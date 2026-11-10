package sandbox

// container.go is the container backend (docs/plans/docker-sandbox-plan.md and
// docs/plans/docker-sandbox-hardening.md): one ephemeral, hardened container per
// command, driven through the `docker` CLI. It is one backend for every OS, so
// this file carries no build tag; the OS-specific part is mount-source
// canonicalisation and the container path a host path maps to
// (container_paths_*.go), and the Desktop and WSL probes.
//
// The seam calls it through Backend.Run / Starter.Start / Backend.Probe like any
// other backend. The hardening the live Windows probe motivated lives here and
// in its siblings:
//
//   - the CLI is launched clean, on a vetted endpoint, and the run is refused
//     below the version floors or without seccomp (container_engine.go) — W1-1;
//   - the argv is built from a fixed schema and never takes an argument from
//     config (buildRunArgv, forbidden by construction) — W1-2;
//   - a missing image never reaches a registry (--pull never) — W1-2;
//   - mount sources are canonical and free of reparse points, re-checked just
//     before the run (containerMount) — W1-3;
//   - a cancelled, timed-out or over-cap command's container is killed and
//     swept, never left running, and dies on its own even if Tenant does
//     (reap, SweepLeftovers, the in-container timeout) — W1-4;
//   - inherit and allowlist network modes fail closed to none; only an operator's
//     explicit full opens the bridge (dockerNetwork) — W1-5;
//   - a refusal is attributed conservatively, a limit never escalates, and an
//     approved escalation widens the boundary by ONE grant and re-runs in a
//     container (attributeContainer, RunGranted) — W1-6.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// containerLabelKey and friends tag every container the backend starts, so the
// reaper and the startup sweep can find this instance's leftovers and never
// another instance's or another program's (W1-4).
const (
	labelSandbox  = "tenant.sandbox"  // =1 on every sandbox container
	labelTrace    = "tenant.trace"    // the run's trace id
	labelInstance = "tenant.instance" // this Tenant install's id (InstanceID)
)

// defaultImage is the sandbox image the backend runs when a profile names none.
// It is a local build, pinned by image id at build time (tenant sandbox image
// build); the backend runs only a pinned reference and never pulls.
const defaultImage = "tenant-sandbox:latest"

// containerBackend runs commands in a fresh hardened container each time.
type containerBackend struct {
	cli      string // the docker-compatible binary: "docker", "podman", or a path
	image    string // the pinned image reference
	instance string // this install's instance id, for the container labels
	// imageSeen is the id the image tag resolved to at the last readiness
	// check (Available), for the run record. Shared by pointer with nothing:
	// each backend value keeps its own, and a zero value means "not known".
	imageSeen  atomic.Value // string
	explicitEP string       // an operator-named engine endpoint (-H); "" resolves
	tlsVerify  bool         // the operator vouches for mutual TLS on a tcp:// endpoint
	tlsCertDir string       // where ca.pem, cert.pem and key.pem are, for a tcp:// endpoint; "" resolves

	epMu  sync.Mutex
	ep    string      // the resolved endpoint, cached on success only
	cache *probeCache // the engine probe, cached on success with back-off on failure

	// Seams for tests; nil uses the real ones.
	lookPath      func(string) (string, error)
	contextHostFn func(context.Context) string
	hostExists    func(string) bool
}

// ContainerOptions configures a container backend. A zero value is the default:
// the docker CLI on PATH, the default image, the process's own instance id, the
// endpoint resolved from DOCKER_HOST, the docker context, or the OS default.
type ContainerOptions struct {
	CLI       string
	Image     string
	Instance  string
	Endpoint  string // an explicit engine endpoint, passed as -H; vetted like any other
	TLSVerify bool   // allow a tcp:// Endpoint (mutual TLS); otherwise tcp:// is refused
	// TLSCertDir holds ca.pem, cert.pem and key.pem for a tcp:// endpoint, the
	// layout the docker CLI uses. Naming it allows a tcp:// endpoint, like
	// TLSVerify. "" falls back to DOCKER_CERT_PATH, then ~/.docker.
	TLSCertDir string
}

// containerDefaults are the options For("container") builds with. A process
// sets them once at startup (SetContainerDefaults) so every surface's backend
// shares one instance id, one endpoint and one probe cache.
var (
	containerDefaultsMu sync.Mutex
	containerDefaultOpt ContainerOptions
)

// SetContainerDefaults sets the options For("container") uses from now on:
// serve and the TUI call it once with the install's InstanceID, so the startup
// sweep finds a crashed run's leftovers, and with the operator's endpoint.
func SetContainerDefaults(o ContainerOptions) {
	containerDefaultsMu.Lock()
	defer containerDefaultsMu.Unlock()
	containerDefaultOpt = o
}

func containerDefaults() ContainerOptions {
	containerDefaultsMu.Lock()
	defer containerDefaultsMu.Unlock()
	return containerDefaultOpt
}

// InstanceID is the stable id an install labels its containers with: derived
// from the data dir, so a restart (or a crash and restart) sweeps the previous
// process's leftovers, while another install on the same machine, with its own
// data dir, is never touched. The data lock (internal/datalock) already keeps
// two live processes off one data dir.
func InstanceID(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return "inst-" + hex.EncodeToString(sum[:6])
}

// processInstance is the per-process fallback id when no install id was set
// (tests, `tenant sandbox run` with no data dir).
var processInstance = sync.OnceValue(func() string {
	return "proc-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
})

// probeCaches shares one probe cache per (CLI, endpoint) across every backend
// value built with the same options, so For("container") on each surface does
// not re-probe the engine.
var (
	probeCachesMu sync.Mutex
	probeCaches   = map[string]*probeCache{}
)

func sharedProbeCache(key string) *probeCache {
	probeCachesMu.Lock()
	defer probeCachesMu.Unlock()
	c, ok := probeCaches[key]
	if !ok {
		c = &probeCache{}
		probeCaches[key] = c
	}
	return c
}

// Container returns a container backend. It does not touch the engine; Probe and
// Run do. For resolves "container" through here with the process defaults.
func Container(opts ContainerOptions) *containerBackend {
	b := &containerBackend{
		cli:        firstNonEmptyStr(opts.CLI, "docker"),
		image:      firstNonEmptyStr(opts.Image, defaultImage),
		instance:   firstNonEmptyStr(opts.Instance, processInstance()),
		explicitEP: opts.Endpoint,
		tlsVerify:  opts.TLSVerify || opts.TLSCertDir != "",
		tlsCertDir: opts.TLSCertDir,
	}
	b.cache = sharedProbeCache(b.cli + "\x00" + b.explicitEP)
	return b
}

// SweepContainers kills the leftover sandbox containers of this install at
// startup, using the process defaults (SetContainerDefaults first). It is the
// TEN-401 lesson applied to containers: a crash must not leave a `sleep 3600`
// running under our label. A missing CLI or engine is not an error here —
// there is nothing to sweep — so startup never fails on it.
//
// It runs before any surface starts work (a later sweep would remove this
// run's own containers, which carry the same label), so start-up waits for it:
// the whole sweep is held to sweepTimeout, and a CLI that does not answer is
// an error the caller logs and carries on from, never a start-up that hangs.
func SweepContainers(ctx context.Context) (int, error) {
	b := Container(containerDefaults())
	if _, err := b.resolvedBin(); err != nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	n, err := b.SweepLeftovers(ctx)
	return n, stalled(ctx, "the start-up sweep", err)
}

// sweepTimeout bounds the whole start-up sweep: finding the endpoint, listing
// and removing. A variable so tests can shorten it.
var sweepTimeout = 20 * time.Second

func (b *containerBackend) Name() string         { return "container" }
func (b *containerBackend) Isolation() Isolation { return IsolationContainer }

// CommandShell is the interpreter a Command.Shell line runs under: sh, in a
// Linux container, whatever the host (Execute reads it so an sh line is never
// re-run under PowerShell; W1-6).
func (b *containerBackend) CommandShell() string { return "sh" }

// ContainerPath maps a host path to the path a command uses to name it inside the
// container (C:\x -> /mnt/c/x), the same mapping the mounts use. It satisfies
// escapecore.PathMapper so the escape suite names files by their container paths.
// A path that will not canonicalise is returned unchanged, so it simply will not
// exist inside — which is containment, not a false pass.
func (b *containerBackend) ContainerPath(hostPath string) string {
	m, err := containerMount(hostPath, false)
	if err != nil {
		return hostPath
	}
	return m.ctr
}

func (b *containerBackend) bin() string {
	if b.cli == "" {
		return "docker"
	}
	return b.cli
}

// resolvedBin is the CLI's absolute path, so a planted `docker` on PATH cannot
// stand in for the engine client (W1-1). A CLI that cannot be found is returned
// unresolved; Probe and Run surface that as unavailable.
func (b *containerBackend) resolvedBin() (string, error) {
	lp := b.lookPath
	if lp == nil {
		lp = exec.LookPath
	}
	if strings.ContainsAny(b.bin(), `/\`) {
		return b.bin(), nil // an explicit path is used as given
	}
	return lp(b.bin())
}

// Available reports whether the backend can run here now: the CLI is found, the
// endpoint is vetted, no plaintext daemon is exposed, the engine answers and
// clears every floor, and the sandbox image is built. It is the fail-closed
// gate (W1-1) — a profile naming container never silently runs on the host.
func (b *containerBackend) Available(ctx context.Context) error {
	if _, err := b.resolvedBin(); err != nil {
		return unavailable(fmt.Errorf("the %s CLI was not found on PATH", b.bin()), fixInstallEngine)
	}
	if _, err := b.facts(ctx); err != nil {
		return err // probeOnce attached the fix
	}
	// One call answers "is the image built" and "which image is it": the id
	// goes into every run's record until the next check.
	id, err := b.imageID(ctx)
	if err != nil {
		var u *Unavailable
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return unavailable(err, fixStuckEngine) // the CLI stalled: that is not "the image is missing"
		case errors.As(err, &u):
			return err // the endpoint or the CLI, with its own fix
		}
		return unavailable(fmt.Errorf("the sandbox image %s is not built (run `tenant sandbox image build`)", b.image), fixBuildImage)
	}
	b.imageSeen.Store(id)
	// The engine answers and the image is there; can it start a container?
	f, _ := b.facts(ctx) // cached: the call above just succeeded
	if _, err := b.containerCheck(ctx, f); err != nil {
		return err
	}
	return nil
}

// --- Probe -----------------------------------------------------------------

func (b *containerBackend) Probe(ctx context.Context) Capabilities {
	caps := Capabilities{
		Backend:   "container",
		Isolation: IsolationContainer,
		Shell:     "sh", // a Linux container runs sh, even on a Windows host
	}
	if _, err := b.resolvedBin(); err != nil {
		caps.Detail = "the " + b.bin() + " CLI was not found on PATH; install Docker (or set container.cli)"
		return caps
	}
	f, err := b.facts(ctx)
	if err != nil {
		caps.Detail = oneLine(err.Error())
		return caps
	}
	ic, done, err := b.control(ctx, "an image check", "image", "inspect", b.image)
	if err != nil {
		caps.Detail = oneLine(err.Error())
		return caps
	}
	if imgErr := done(imagePresent(ic)); imgErr != nil {
		if errors.Is(imgErr, context.DeadlineExceeded) {
			caps.Detail = oneLine(imgErr.Error()) // the CLI stalled: that is not "the image is missing"
			return caps
		}
		caps.Detail = "the sandbox image " + b.image + " is not built (run `tenant sandbox image build`): " + oneLine(imgErr.Error())
		return caps
	}
	// The boundary the container enforces, reported honestly. Limits is partial
	// because --cpus is a rate, not a CPU-time budget (W2-1), and attribution has
	// documented gaps.
	caps.Filesystem = EnforceFull
	caps.Network = EnforceFull // none fully cuts outbound; full is the bridge, gated elsewhere
	caps.Limits = EnforcePartial
	caps.Env = EnforceFull
	detail := "read-only rootfs, non-root user, all capabilities dropped, no-new-privileges, seccomp, " +
		"per-run pids/memory caps and an in-container deadline; filesystem is deny-by-default (only mounts exist). " +
		"Limits is partial: --cpus is a rate, not a CPU-time budget. Probe: " + f.summary() + "."
	if c := b.cache.lastCheck(); c != nil {
		detail += " " + c.summary() + "."
	} else {
		detail += " Container check: not run yet (it runs before the first command)."
	}
	if w := f.gates.warning(); w != "" {
		detail += " NOTE: " + w
	}
	caps.Detail = detail
	return caps
}

// --- Run -------------------------------------------------------------------

func (b *containerBackend) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	// os_exec resolves with no work dir; give it a fresh scratch dir the backend
	// owns and removes, so the backend never falls back to its own cwd (W1-3).
	if pol.WorkDir == "" {
		dir, err := os.MkdirTemp("", "tenant-sbx-work-")
		if err != nil {
			return Result{}, fmt.Errorf("container: scratch work dir: %w", err)
		}
		defer os.RemoveAll(dir)
		pol.WorkDir = dir
	}

	spec, err := b.specFor(cmd, pol)
	if err != nil {
		// A policy the backend cannot express safely (a mount source that will not
		// canonicalise, a bad env name) fails closed.
		return Result{Violations: []Violation{{Kind: "policy", Detail: oneLine(err.Error())}}}, err
	}
	return b.runSpec(ctx, cmd, pol, spec)
}

// runSpec starts one container from spec, which is everything about the run
// that was decided: Run builds it and starts it, and a pinned re-run
// (RunGrantedAs) starts the one it hashed.
func (b *containerBackend) runSpec(ctx context.Context, cmd Command, pol Policy, spec containerSpec) (Result, error) {
	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	argv, err := buildRunArgv(spec)
	if err != nil {
		return Result{Violations: []Violation{{Kind: "policy", Detail: oneLine(err.Error())}}}, err
	}

	c, err := b.command(ctx, argv...)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(cmd.Stdin) != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}
	// Killing the CLI does not stop the container (the live probe showed the
	// orphan), so Cancel runs `docker kill <name>` and the deferred reap sweeps
	// anything left (W1-4).
	c.Cancel = func() error { return b.reap(spec.name) }
	c.WaitDelay = 3 * time.Second
	defer b.reap(spec.name)

	// Output is capped as it arrives, and the first byte over the cap kills the
	// container (W1-4 / TEN-438): a flood never grows Tenant's memory. The
	// refusal witness sees every byte BEFORE the cap, so attribution is stable
	// even when the refusal line lands past MaxOutputBytes (W1-6). Stdout and
	// stderr get the SAME writer value so os/exec keeps them on one pipe, merged
	// by the kernel at write boundaries: a refusal message is one atomic write,
	// so the witness always sees it whole.
	out := &cappedBuffer{max: pol.Limits.MaxOutputBytes, onCap: func() { go b.reap(spec.name) }}
	wit := &refusalWitness{}
	merged := io.MultiWriter(out, wit)
	c.Stdout = merged
	c.Stderr = merged
	if pol.Stderr != nil {
		c.Stderr = io.MultiWriter(out, wit, pol.Stderr)
	}

	started := time.Now()
	runErr := c.Run()
	wit.flush()
	spec.witness = wit
	res := Result{Output: out.String(), PID: pidOf(c)}
	res.Confinement = b.confinement(spec, out, time.Since(started))
	if out.Truncated() {
		res.Violations = append(res.Violations, Violation{Kind: "limit", Detail: "output exceeded the cap and was truncated; the container was killed"})
	}

	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		res.Violations = append(res.Violations, Violation{Kind: "limit", Detail: "wall-clock deadline exceeded"})
		return res, nil
	}

	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		res.ExitCode = ee.ExitCode()
		res.ExitStatus = ee.Error()
		// 125 is docker's own "could not run the container" (a daemon or config
		// error), never the command's exit. Fail closed: it is infrastructure.
		if res.ExitCode == 125 {
			return res, fmt.Errorf("container: the engine refused to start the container: %s", oneLine(res.Output))
		}
		if v, ok := attributeContainer(res, spec); ok {
			res.Violations = append(res.Violations, v)
		}
	} else if runErr != nil {
		return res, fmt.Errorf("container: could not run %s: %w", b.bin(), runErr)
	}
	return res, nil
}

// confinement describes the boundary a run had, from the spec it was started
// with (W2-7).
func (b *containerBackend) confinement(spec containerSpec, out *cappedBuffer, elapsed time.Duration) *Confinement {
	describe := func(m mount) string {
		return m.host + " -> " + m.ctr + " (" + boolWord(m.ro, "ro", "rw") + ")"
	}
	mounts := []string{describe(spec.work)}
	for _, m := range spec.mounts {
		mounts = append(mounts, describe(m))
	}
	pids, memMB := spec.limits.MaxPIDs, spec.limits.MemoryBytes>>20
	if pids <= 0 {
		pids = defaultMaxPIDs
	}
	if memMB <= 0 {
		memMB = defaultMemoryMB
	}
	limits := fmt.Sprintf("pids=%d memory=%dMB cpus=%s", pids, memMB, defaultCPUs)
	if spec.limits.CPU > 0 {
		limits += " cpu=" + spec.limits.CPU.String() + "/process"
	}
	if spec.limits.Wall > 0 {
		limits += " wall=" + spec.limits.Wall.String()
	}
	if spec.limits.MaxOutputBytes > 0 {
		limits += fmt.Sprintf(" output=%dB", spec.limits.MaxOutputBytes)
	}
	id, _ := b.imageSeen.Load().(string)
	if spec.image != b.image {
		id = spec.image // a pinned re-run is started by the id itself
	}
	return &Confinement{
		Image: b.image, ImageID: id, Mounts: mounts, Network: spec.network, Limits: limits,
		Env:         append([]string(nil), spec.envNames...),
		OutputBytes: out.Total(), Capped: out.Truncated(), Elapsed: elapsed,
	}
}

// RunGranted is the within-tier escalation (W1-6, D3): the same command, in a
// fresh container, with exactly ONE grant added — the refused folder as a
// writable root, or the network for this one command. It never touches the
// host. A refusal that names no grantable path cannot be escalated this way
// and is an error, so the caller fails closed rather than widening blindly.
func (b *containerBackend) RunGranted(ctx context.Context, cmd Command, pol Policy, refused Result) (Result, error) {
	granted, err := grantedPolicy(pol, refused)
	if err != nil {
		return refused, err
	}
	// specFor re-checks the grant against the deny roots, so an escalation can
	// never open Tenant's own dirs or a credential store.
	return b.Run(ctx, cmd, granted)
}

// grantedPolicy is pol with the one grant an approval of refused adds: the
// network for a network refusal, or the refused folder as a writable root for
// a filesystem refusal that names a path. Nothing grantable is an error.
func grantedPolicy(pol Policy, refused Result) (Policy, error) {
	granted := false
	for _, v := range refused.Violations {
		switch v.Kind {
		case "network":
			pol.Network = NetFull
			granted = true
		case "filesystem":
			if v.Path == "" {
				continue
			}
			scope := v.Path
			if fi, err := os.Stat(scope); err != nil || !fi.IsDir() {
				scope = filepath.Dir(scope)
			}
			pol.WriteRoots = append(append([]string(nil), pol.WriteRoots...), scope)
			granted = true
		}
	}
	if !granted {
		return pol, errors.New("container: the refusal names nothing the sandbox can grant for one command (no path, or a limit)")
	}
	return pol, nil
}

// GrantedRunHash is the digest of the run an approval of refused would start
// (EscalationBinder, W1-6; see RunGrantedAs, which starts it): sh, the command line, every mount as the engine
// would get it (canonical source, destination, mode), the network, and the
// image's id as the engine resolves the tag right now. A re-pointed mount
// source or a rebuilt or re-tagged image is a different digest. A scratch work
// dir is a fresh temp folder on every run, so it is hashed as "a scratch work
// dir", not by its path.
func (b *containerBackend) GrantedRunHash(ctx context.Context, cmd Command, pol Policy, refused Result) (string, error) {
	run, err := b.grantedRun(ctx, cmd, pol, refused)
	if err != nil {
		return "", err
	}
	run.cleanup()
	return run.digest, nil
}

// RunGrantedAs is the approved re-run, started from the description whose
// digest is approved (PinnedEscalator): the spec is built once, hashed, and
// that spec is what runs, with the image named by the id that was hashed. A
// tag moved after the last check, or a mount that now canonicalises
// elsewhere, is a different digest here and nothing starts.
func (b *containerBackend) RunGrantedAs(ctx context.Context, cmd Command, pol Policy, refused Result, approved string) (Result, error) {
	run, err := b.grantedRun(ctx, cmd, pol, refused)
	if err != nil {
		return refused, err
	}
	defer run.cleanup()
	if approved == "" || run.digest != approved {
		return refused, errApprovalStale
	}
	run.spec.image = run.imageID
	return b.runSpec(ctx, cmd, run.pol, run.spec)
}

// grantedRunPlan is one approved re-run, pinned down: the policy with its
// grant, the spec the engine would get, the id the image tag resolves to now,
// and the digest of all of it. cleanup removes a scratch work dir.
type grantedRunPlan struct {
	pol     Policy
	spec    containerSpec
	imageID string
	digest  string
	cleanup func()
}

// grantedRun pins down the run an approval of refused would start.
func (b *containerBackend) grantedRun(ctx context.Context, cmd Command, pol Policy, refused Result) (grantedRunPlan, error) {
	granted, err := grantedPolicy(pol, refused)
	if err != nil {
		return grantedRunPlan{}, err
	}
	cleanup := func() {}
	scratch := granted.WorkDir == ""
	if scratch {
		dir, derr := os.MkdirTemp("", "tenant-sbx-work-")
		if derr != nil {
			return grantedRunPlan{}, fmt.Errorf("container: scratch work dir: %w", derr)
		}
		cleanup = func() { os.RemoveAll(dir) }
		granted.WorkDir = dir
	}
	// specFor re-checks the grant against the deny roots, so an escalation can
	// never open Tenant's own dirs or a credential store.
	spec, err := b.specFor(cmd, granted)
	if err != nil {
		cleanup()
		return grantedRunPlan{}, err
	}
	describe := func(m mount) string {
		return m.host + "\x00" + m.ctr + "\x00" + boolWord(m.ro, "ro", "rw")
	}
	mounts := []string{"work\x00" + describe(spec.work)}
	if scratch {
		mounts[0] = "work\x00<scratch>" // its path, and so its destination, is new every run
	}
	for _, m := range spec.mounts {
		mounts = append(mounts, describe(m))
	}
	id, err := b.imageID(ctx)
	if err != nil {
		cleanup()
		return grantedRunPlan{}, err
	}
	return grantedRunPlan{
		pol: granted, spec: spec, imageID: id, cleanup: cleanup,
		digest: EscalationHash("sh", []string{spec.command}, mounts, spec.network, id),
	}, nil
}

// imageID is the id the engine resolves the sandbox image's tag to right now.
func (b *containerBackend) imageID(ctx context.Context) (string, error) {
	c, done, err := b.control(ctx, "an image check", "image", "inspect", "--format", "{{.Id}}", b.image)
	if err != nil {
		return "", unavailable(err, fixEndpoint)
	}
	out, err := c.Output()
	if err = done(err); err != nil {
		return "", fmt.Errorf("container: could not read the id of image %s: %w", b.image, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("container: the engine reported no id for image %s", b.image)
	}
	return id, nil
}

// specFor turns a Policy into a validated container spec: the canonical mounts,
// the network value, the env names, and the limits. It resolves and re-checks
// every mount source here, immediately before the run (W1-3).
func (b *containerBackend) specFor(cmd Command, pol Policy) (containerSpec, error) {
	if pol.WorkDir == "" {
		return containerSpec{}, errors.New("container: a work dir is required (the backend never uses its own cwd)")
	}
	work, err := containerMount(pol.WorkDir, false)
	if err != nil {
		return containerSpec{}, fmt.Errorf("work dir: %w", err)
	}
	// One mount per destination, or the engine refuses the run ("Duplicate
	// mount point"). A folder named twice is mounted once, by its widest
	// grant: the work dir over anything, writable over read-only. That is what
	// an approved re-run asks for when the refused folder was already granted
	// read-only, and on a real engine that re-run never started before this.
	taken := map[string]bool{work.ctr: true}
	var writes []mount
	for _, r := range dedupPathList(pol.WriteRoots) {
		m, err := containerMount(r, false)
		if err != nil {
			return containerSpec{}, fmt.Errorf("write root %s: %w", r, err)
		}
		if !taken[m.ctr] {
			taken[m.ctr] = true
			writes = append(writes, m)
		}
	}
	var mounts []mount
	for _, r := range dedupPathList(pol.ReadRoots) {
		m, err := containerMount(r, true)
		if err != nil {
			return containerSpec{}, fmt.Errorf("read root %s: %w", r, err)
		}
		if !taken[m.ctr] {
			taken[m.ctr] = true
			mounts = append(mounts, m)
		}
	}
	mounts = append(mounts, writes...)
	if err := grantsClearDenyRoots(work, mounts, pol.DenyRoots); err != nil {
		return containerSpec{}, err
	}
	for _, m := range append([]mount{work}, mounts...) {
		if !m.ro && bareRepoLayout(m.host) {
			return containerSpec{}, fmt.Errorf("container: %s is laid out as a bare git repository, so it cannot be a writable folder of a sandboxed command: its hooks and its config would be the command's to write, and git runs them on the host the next time it is used there. Use a folder that holds a checkout, or grant this one read-only", m.host)
		}
	}
	sh := cmd.Shell
	if sh == "" {
		sh = strings.Join(cmd.Args, " ")
	}
	exists := b.hostExists
	if exists == nil {
		exists = func(p string) bool { _, err := os.Lstat(p); return err == nil }
	}
	return containerSpec{
		image:      b.image,
		name:       containerName(),
		trace:      cmd.Origin,
		instance:   b.instance,
		work:       work,
		mounts:     mounts,
		network:    dockerNetwork(pol.Network),
		envNames:   envNamesFor(pol.Env),
		limits:     pol.Limits,
		command:    sh,
		hostExists: exists,
	}, nil
}

// grantsClearDenyRoots refuses a grant that overlaps a deny root (plan §3.4).
// The container enforces deny roots by never mounting them, so a read/write
// root that overlaps a deny root in EITHER direction is refused: inside one
// would expose denied content, containing one would expose it through the
// parent. The work dir is the carve-out — it may sit INSIDE a deny root (cron's
// work dir lives under the data dir), since only the work-dir subtree is
// mounted; but a work dir that IS or CONTAINS a deny root fails closed.
//
// Both sides compare in ONE canonical form (canonForCompare), or they disagree
// when the same place is named differently: a macOS symlink (/var ->
// /private/var) or a Windows 8.3 short name (RUNNER~1 vs the long name) on one
// side only would let the guard silently miss.
//
// It is a function of its own so the load-time check (exec_profiles, TEN-457)
// can run the same rule before any command does.
func grantsClearDenyRoots(work mount, mounts []mount, deny []string) error {
	dwork := canonForCompare(work.host)
	for _, d := range deny {
		if d == "" {
			continue
		}
		dc := canonForCompare(d)
		if withinHost(dc, dwork) { // deny root at or inside the work dir
			return fmt.Errorf("work dir %s is or contains the deny root %s", work.host, d)
		}
		for _, m := range mounts {
			mc := canonForCompare(m.host)
			if withinHost(mc, dc) || withinHost(dc, mc) {
				return fmt.Errorf("mount %s overlaps the deny root %s", m.host, d)
			}
		}
	}
	return nil
}

// GrantOverlapsDeny reports the first of grants (folders a container profile
// would mount) that overlaps one of deny, and which: the grant is the deny
// root, sits inside it, or contains it. It is the rule grantsClearDenyRoots
// applies to a run's mounts, in the same canonical form, for whoever loads a
// profile to apply before any command runs (W1-3, TEN-457). The work dir's
// carve-out is not part of it: a profile's grants are not a work dir.
func GrantOverlapsDeny(grants, deny []string) (grant, denied string, overlaps bool) {
	for _, d := range deny {
		if d == "" {
			continue
		}
		dc := canonForCompare(d)
		for _, g := range grants {
			if g == "" {
				continue
			}
			gc := canonForCompare(g)
			if withinHost(gc, dc) || withinHost(dc, gc) {
				return g, d, true
			}
		}
	}
	return "", "", false
}

// reap kills and removes the named container. It is safe to call more than once
// and on a container that has already gone (--rm), so Cancel, the output-cap
// path and the deferred cleanup can all call it.
func (b *containerBackend) reap(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := b.command(ctx, "kill", name)
	if err != nil {
		return err
	}
	return c.Run()
}

// SweepLeftovers kills this install's leftover sandbox containers at startup:
// the TEN-401 lesson (an orphaned headless Chrome). It matches only this
// instance's label, never another live Tenant's containers (W1-4).
func (b *containerBackend) SweepLeftovers(ctx context.Context) (int, error) {
	ids, err := b.instanceContainers(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if rc, done, err := b.control(ctx, "a container removal", "rm", "-f", id); err == nil {
			_ = done(rc.Run())
		}
	}
	return len(ids), nil
}

// instanceContainers lists the containers, running or not, that carry this
// install's instance label. Every container is listed with that label's value
// and matched here: the engine's own `--filter label=…` is what stalled (see
// controlTimeout), and an unfiltered list answers where it does not. An exact
// match on the value, so another install's containers are never returned.
func (b *containerBackend) instanceContainers(ctx context.Context) ([]string, error) {
	if b.instance == "" {
		return nil, nil
	}
	return b.labelledContainers(ctx, labelInstance, b.instance)
}

// labelledContainers lists the containers, running or not, whose label has
// exactly the value want, matched here and not by the engine (see
// instanceContainers).
func (b *containerBackend) labelledContainers(ctx context.Context, label, want string) ([]string, error) {
	lc, done, err := b.control(ctx, "a container listing", "ps", "-a", "--no-trunc", "--format", `{{.ID}}`+"\t"+`{{.Label "`+label+`"}}`)
	if err != nil {
		return nil, err
	}
	list, err := lc.Output()
	if err = done(err); err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(string(list), "\n") {
		id, value, _ := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if id = strings.TrimSpace(id); id != "" && strings.TrimSpace(value) == want {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// StopAllContainers removes every sandbox container on the engine, running or
// not: the operator's stop for "something in a sandbox is doing what it should
// not" (W2-7, `tenant sandbox stop-all`). Every container the backend starts
// carries tenant.sandbox=1 whatever install or process started it, and that
// label is the match, so this stops other installs' sandbox containers on the
// same engine too, and nothing that is not a Tenant sandbox. A command whose
// container is removed ends as if it had been killed. It returns how many
// were found and how many of those could not be removed.
func StopAllContainers(ctx context.Context) (found, failed int, err error) {
	b := Container(containerDefaults())
	if _, err := b.resolvedBin(); err != nil {
		return 0, 0, unavailable(fmt.Errorf("the %s CLI was not found on PATH", b.bin()), fixInstallEngine)
	}
	ctx, cancel := context.WithTimeout(ctx, stopAllTimeout)
	defer cancel()
	ids, err := b.labelledContainers(ctx, labelSandbox, "1")
	if err != nil {
		return 0, 0, stalled(ctx, "the stop", err)
	}
	for _, id := range ids {
		rc, done, cerr := b.control(ctx, "a container removal", "rm", "-f", id)
		if cerr == nil {
			cerr = done(rc.Run())
		}
		if cerr != nil {
			failed++
		}
	}
	return len(ids), failed, nil
}

// stopAllTimeout bounds StopAllContainers. A variable so tests can shorten it.
var stopAllTimeout = 60 * time.Second

// --- helpers ---------------------------------------------------------------

// windowsProgramInvoked reports whether the command INVOKES a Windows-native
// program (its first token), which a Linux container cannot run. It checks the
// invoked program, not any substring, so `python build.py --out app.exe` is not
// flagged — only `cmd.exe /c ver` or `app.exe` is (W1-6/H19).
func windowsProgramInvoked(command string) (string, bool) {
	tok := strings.TrimSpace(command)
	if i := strings.IndexAny(tok, " \t"); i >= 0 {
		tok = tok[:i]
	}
	low := strings.ToLower(tok)
	switch low {
	case "cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return tok, true
	}
	for _, suf := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if strings.HasSuffix(low, suf) {
			return tok, true
		}
	}
	return "", false
}

// dockerNetwork maps a NetworkMode to a docker --network value, failing closed:
// inherit and allowlist become none until the Phase 4 proxy and W2-9 land; only
// an explicit full opens the default bridge (W1-5). The bridge choice is the
// operator's, gated at the profile layer, so the model can never select it.
func dockerNetwork(n NetworkMode) string {
	switch n {
	case NetFull:
		return "bridge"
	default: // NetNone, NetAllowlist, NetInherit
		return "none"
	}
}

// envNamesFor is the environment variable NAMES that cross into the container,
// dropping host-only names that mean nothing in a Linux container and any name
// the argv schema forbids (DOCKER_*, NVIDIA_*). Values never appear on the argv;
// buildRunArgv passes each name with `-e NAME`, so docker reads the value from
// the CLI's own environment (W1-2/H20).
func envNamesFor(e EnvPolicy) []string {
	skip := map[string]bool{
		"PATH": true, "HOME": true, "SHELL": true, "TMPDIR": true, "TEMP": true, "TMP": true,
		"SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true,
		"PSMODULEPATH": true, "USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true, "APPDATA": true,
		"LOCALAPPDATA": true, "PROGRAMDATA": true, "ALLUSERSPROFILE": true, "PUBLIC": true,
		"USERNAME": true, "USERDOMAIN": true, "COMPUTERNAME": true, "OS": true,
	}
	var names []string
	seen := map[string]bool{}
	for _, n := range e.Pass {
		up := strings.ToUpper(n)
		if skip[up] || strings.HasPrefix(up, "DOCKER") || strings.HasPrefix(up, "NVIDIA") {
			continue
		}
		if !validEnvName(n) || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	// Add carries the log-join IDs; forward them by name too (they are set on the
	// CLI's environment by execute()).
	for _, kv := range e.Add {
		if name, _, ok := cut(kv, '='); ok && validEnvName(name) && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// dockerVersionInfo is the slice of `docker version --format {{json .}}` the
// backend reads.
type dockerVersionInfo struct {
	Client struct {
		Version string
	}
	Server struct {
		Version    string
		Platform   struct{ Name string }
		Components []struct {
			Name    string
			Version string
		}
	}
}

func (v dockerVersionInfo) clientName() string {
	if v.Client.Version == "" {
		return "docker"
	}
	return "docker " + v.Client.Version
}

// component is the version of a named server component (runc, containerd), or
// "" when the engine did not report it.
func (v dockerVersionInfo) component(name string) string {
	for _, c := range v.Server.Components {
		if strings.EqualFold(c.Name, name) {
			return c.Version
		}
	}
	return ""
}

func (v dockerVersionInfo) runcVersion() string { return orUnknown(v.component("runc")) }

// dockerVersion reads the engine's version through c (a prepared `docker -H …
// version --format {{json .}}` command).
func dockerVersion(c *exec.Cmd) (dockerVersionInfo, error) {
	out, err := c.Output()
	if err != nil {
		return dockerVersionInfo{}, err
	}
	var info dockerVersionInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return dockerVersionInfo{}, fmt.Errorf("parse docker version: %w", err)
	}
	if info.Server.Version == "" {
		return dockerVersionInfo{}, errors.New("the engine reported no server version (is the daemon running?)")
	}
	return info, nil
}

// imagePresent runs a prepared `docker -H … image inspect <image>`.
func imagePresent(c *exec.Cmd) error { return c.Run() }

// attributeContainer makes a conservative Violation from a failed run (W1-6):
//
//   - program: exit 127 for a Windows program invoked in a Linux container;
//   - limit, never escalated: exit 137 / OOM, "cannot fork", EAGAIN;
//   - filesystem: EROFS, or ENOENT on a path that exists on the host but is not
//     mounted (mapped back through the mount table); the violation carries the
//     host path so RunGranted can grant exactly it;
//   - network: a resolution or reachability error, only under `none`.
//
// Anything else stays unattributed, so it fails closed rather than being
// re-run anywhere.
//
// The signals are read from the run's refusalWitness (output.go), which scanned
// the whole stream before the output cap, so the verdict for a given command is
// stable: a refusal line that fell past MaxOutputBytes is still seen. Without a
// witness (a spec built outside Run) the capped output is scanned instead.
func attributeContainer(res Result, spec containerSpec) (Violation, bool) {
	var erofs, enoent, limitSig, netCut, notFound bool
	erofsLine, enoentLine := res.Output, res.Output
	if w := spec.witness; w != nil {
		erofs, erofsLine = w.erofs, w.erofsLine
		enoent, enoentLine = w.enoent, w.enoentLine
		limitSig, netCut, notFound = w.limit, w.netCut, w.notFound
	} else {
		low := strings.ToLower(res.Output)
		erofs = strings.Contains(low, "read-only file system")
		enoent = strings.Contains(low, "no such file or directory")
		limitSig = strings.Contains(low, "out of memory") || strings.Contains(low, "cannot fork") ||
			strings.Contains(low, "resource temporarily unavailable")
		notFound = strings.Contains(low, "not found")
		for _, sig := range netCutSigns {
			if strings.Contains(low, sig) {
				netCut = true
				break
			}
		}
	}

	if res.ExitCode == 127 && notFound {
		if prog, ok := windowsProgramInvoked(spec.command); ok {
			return Violation{Kind: "program", Detail: "a Windows program (" + prog + ") cannot run in a Linux container"}, true
		}
	}
	if res.ExitCode == 137 || limitSig {
		return Violation{Kind: "limit", Detail: "the container hit a resource cap (memory, pids or CPU)"}, true
	}
	if erofs {
		v := Violation{Kind: "filesystem", Detail: "wrote outside the writable roots (read-only file system)"}
		if p := refusedPathIn(erofsLine); p != "" {
			v.Path = spec.hostPathOf(p)
		}
		return v, true
	}
	if enoent {
		// ENOENT is a refusal only when the path exists on the host and is not
		// mounted: then the container hid it. A genuinely missing file is the
		// command's own error and stays unattributed.
		if p := refusedPathIn(enoentLine); p != "" && spec.hostExists != nil {
			host := spec.hostPathOf(p)
			if host != "" && spec.hostExists(host) && !spec.mounted(host) {
				return Violation{Kind: "filesystem", Detail: "read outside the readable roots (" + host + " exists on the host but is not mounted)", Path: host}, true
			}
		}
	}
	if spec.network == "none" && netCut {
		return Violation{Kind: "network", Detail: "the container has no network (network disabled for this profile)"}, true
	}
	return Violation{}, false
}

var netCutSigns = []string{
	"network is unreachable", "could not resolve host", "temporary failure in name resolution",
	"no such host", "name or service not known", "connection refused", "no route to host",
}

// --- small utilities -------------------------------------------------------

var instanceCounter atomic.Uint64

func containerName() string {
	return fmt.Sprintf("tenant-sbx-%s-%d", strconv.FormatInt(time.Now().UnixNano(), 36), instanceCounter.Add(1))
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// goosOf is runtime.GOOS behind a function, so the endpoint default can be
// read where runtime is not otherwise imported.
func goosOf() string { return runtime.GOOS }

// bareRepoLayout reports whether dir is itself a git directory (a bare
// repository, or a checkout's .git handed over directly): git's own test, a
// HEAD file beside objects and refs folders. In a checkout those sit one
// level down, under .git; here the hooks folder and the config file are at
// the top of what the command may write (W2-2, H16).
func bareRepoLayout(dir string) bool {
	isDir := func(name string) bool {
		fi, err := os.Stat(filepath.Join(dir, name))
		return err == nil && fi.IsDir()
	}
	head, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil && head.Mode().IsRegular() && isDir("objects") && isDir("refs")
}

// canonForCompare resolves p to the absolute, symlink-resolved form that mount
// sources are compared in, so a deny root and a mount that name the same place
// through different symlinks still match. A path that does not exist yet (a
// grant for a folder still to be made) is resolved through its nearest
// ancestor that does, with the rest kept as written. Without that, such a
// path kept every symlink and short name on the way to it (/var on macOS,
// RUNNER~1 on Windows) while an existing deny root lost them, and the two
// never compared equal: the overlap check silently missed it.
func canonForCompare(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	probe, tail := abs, ""
	for {
		if r, err := filepath.EvalSymlinks(probe); err == nil {
			return filepath.Join(r, tail)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return abs // nothing on the path exists
		}
		tail = filepath.Join(filepath.Base(probe), tail)
		probe = parent
	}
}

func dedupPathList(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// validEnvName reports whether n is a POSIX-style environment name the schema
// allows: [A-Za-z_][A-Za-z0-9_]*.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validEnvName(n string) bool { return envNameRe.MatchString(n) }

// compareVersions compares two dotted numeric versions (a leading 'v' and any
// pre-release or build suffix after the numbers are ignored). It returns -1/0/1
// and ok=false when either side has no numeric component to compare.
func compareVersions(a, b string) (int, bool) {
	pa, oka := numericParts(a)
	pb, okb := numericParts(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// numericParts extracts the leading dotted-integer components of a version, so
// "29.8.0", "v1.2.8" and "1.4.0-rc.3" parse to their numbers.
func numericParts(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var parts []int
	for _, f := range strings.Split(s, ".") {
		n := 0
		i := 0
		for i < len(f) && f[i] >= '0' && f[i] <= '9' {
			n = n*10 + int(f[i]-'0')
			i++
		}
		if i == 0 {
			break // a non-numeric component ends the comparable prefix
		}
		parts = append(parts, n)
		if i < len(f) {
			break // trailing suffix like "-rc" ends it
		}
	}
	return parts, len(parts) > 0
}
