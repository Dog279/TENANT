package sandbox

// container_engine.go is the container backend's engine seam (W1-1 / TEN-455,
// docs/plans/docker-sandbox-hardening.md §4): how the docker CLI is launched,
// which engine endpoints it may talk to, the version and feature gates the run
// is refused below, and the probe cache that keeps a slow or absent Docker
// Desktop from being re-probed on every command.
//
// The rules, each with its test in container_engine_test.go:
//
//   - Clean launch. The CLI starts from an absolute path resolved once, with an
//     environment Tenant builds: every ambient DOCKER_* and NVIDIA_* is stripped
//     (cliEnv), and the endpoint is passed explicitly with -H (command), so a
//     DOCKER_HOST or DOCKER_CONTEXT in Tenant's own environment can never
//     redirect a run to another engine.
//   - Endpoints. The precedence is the CLI's own — an explicit endpoint, then
//     DOCKER_HOST, then the docker context — but tcp:// is refused without TLS
//     and anything but npipe, unix, ssh or mutual-TLS tcp is refused
//     (resolveEndpoint). A daemon answering on 127.0.0.1:2375 in the clear
//     refuses the run outright (plaintextDaemonAnswers): that setting makes the
//     engine root-for-anyone on the box.
//   - Gates. The floors in §3 refuse (not warn) below the CVE-fixed releases of
//     Engine, runc and containerd, and below Docker Desktop 4.44.3; newer
//     floors warn. seccomp must be on. An unparseable version warns rather than
//     refuses, so a CLI whose output format changes cannot wedge the backend.
//   - Probe cache. Only success is cached (60 s); a failure is retried with
//     back-off, because Docker Desktop starts after Tenant at login (the
//     TEN-439 lesson: never cache a failed discovery for the life of the
//     process).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// windowsDesktopPipe is Docker Desktop's Linux engine on Windows: the vetted
	// default when nothing names an endpoint.
	windowsDesktopPipe = "npipe:////./pipe/dockerDesktopLinuxEngine"
	// unixDefaultSocket is the engine's socket on Linux. macOS Desktop publishes
	// its socket through the docker context (desktop-linux), which
	// resolveEndpoint consults before this default.
	unixDefaultSocket = "unix:///var/run/docker.sock"
	// plaintextDaemonAddr is Docker Desktop's "Expose daemon on tcp://localhost:2375
	// without TLS" setting. While anything answers there, the run is refused.
	plaintextDaemonAddr = "127.0.0.1:2375"
)

// Version floors (hardening plan §3; the Desktop floor is D1, decided
// 2026-09-30). "refuse" floors are the CVE-fixed releases; "latest" are the
// newest releases known when this file was last updated and only warn, so a
// stale constant can never stop a run.
const (
	minEngineVersion    = "29.4.3" // CVE-2026-31431 (Copy Fail)
	latestEngineVersion = "29.8.0" // as of 2026-09-30
	minDesktopVersion   = "4.44.3" // CVE-2025-9074: a networked container could take over the host
	warnDesktopVersion  = "4.76.0"
	latestDesktop       = "4.93.0" // as of 2026-09-30
)

// runcFloors and containerdFloors are the fixed release per line (the Nov 2025
// runc trio; containerd's matching fixes). A line older than the oldest listed
// is refused outright; a line newer than the newest listed carries the fixes.
var (
	runcFloors       = lineFloors{oldest: "1.2", floors: map[string]string{"1.2": "1.2.8", "1.3": "1.3.3", "1.4": "1.4.0"}}
	containerdFloors = lineFloors{oldest: "1.6", floors: map[string]string{"1.6": "1.6.38", "1.7": "1.7.27", "2.0": "2.0.4"}}
)

// --- endpoint and launch -----------------------------------------------------

// resolveEndpoint picks the engine endpoint the CLI is told to use (-H), in the
// precedence the docker CLI itself applies — explicit, then DOCKER_HOST, then
// the docker context's host — and vets it. With nothing named, the vetted OS
// default is used. tlsVerify is DOCKER_TLS_VERIFY (or the operator's setting):
// tcp:// is allowed only with it.
func resolveEndpoint(explicit, envHost, contextHost string, tlsVerify bool, goos string) (string, error) {
	for _, c := range []struct{ ep, source string }{
		{explicit, "container.endpoint"},
		{envHost, "DOCKER_HOST"},
		{contextHost, "the docker context"},
	} {
		if strings.TrimSpace(c.ep) == "" {
			continue
		}
		return vetEndpoint(strings.TrimSpace(c.ep), tlsVerify, c.source)
	}
	if goos == "windows" {
		return windowsDesktopPipe, nil
	}
	return unixDefaultSocket, nil
}

// vetEndpoint refuses an engine endpoint the plan forbids: tcp:// without TLS
// (the engine API is root on the box), and any scheme that is not a local pipe
// or socket, ssh, or mutual-TLS tcp.
func vetEndpoint(ep string, tlsVerify bool, source string) (string, error) {
	u, err := url.Parse(ep)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("container: %s names an endpoint that is not a URL: %q", source, ep)
	}
	switch strings.ToLower(u.Scheme) {
	case "npipe", "unix", "ssh":
		return ep, nil
	case "tcp":
		if !tlsVerify {
			return "", fmt.Errorf("container: %s is %s, a tcp:// endpoint without TLS; the engine API is root on that host, so only ssh:// or mutual TLS (DOCKER_TLS_VERIFY=1) is allowed", source, ep)
		}
		return ep, nil
	default:
		return "", fmt.Errorf("container: %s is %s; only npipe://, unix://, ssh:// or a mutual-TLS tcp:// endpoint is allowed", source, ep)
	}
}

// cliEnv is the environment the docker CLI runs with: parent minus every
// DOCKER_* (an ambient DOCKER_HOST, DOCKER_CONTEXT or DOCKER_CONFIG must not
// redirect a run; the endpoint travels on -H) and every NVIDIA_* (under the
// NVIDIA runtime, NVIDIA_VISIBLE_DEVICES requests GPUs by itself). Everything
// else stays, because `-e NAME` makes docker read each forwarded value from
// this environment.
func cliEnv(parent []string) []string {
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		name, _, ok := cut(kv, '=')
		if !ok || name == "" {
			continue
		}
		up := strings.ToUpper(name)
		if strings.HasPrefix(up, "DOCKER_") || strings.HasPrefix(up, "NVIDIA_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// plaintextDaemonAnswers reports whether something accepts TCP connections on
// 127.0.0.1:2375 — Docker Desktop's "without TLS" exposure. A variable so tests
// can pin it without a listener.
var plaintextDaemonAnswers = func(ctx context.Context) bool {
	d := net.Dialer{Timeout: 300 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", plaintextDaemonAddr)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// endpoint resolves the vetted engine endpoint once per backend and caches
// only success. The docker context is consulted (without -H, since that is
// what we are resolving) only when neither the operator nor DOCKER_HOST named
// one.
func (b *containerBackend) endpoint(ctx context.Context) (string, error) {
	b.epMu.Lock()
	defer b.epMu.Unlock()
	if b.ep != "" {
		return b.ep, nil
	}
	envHost := os.Getenv("DOCKER_HOST")
	tls := b.tlsVerify || os.Getenv("DOCKER_TLS_VERIFY") != ""
	var contextHost string
	if b.explicitEP == "" && envHost == "" {
		contextHost = b.contextHost(ctx)
	}
	ep, err := resolveEndpoint(b.explicitEP, envHost, contextHost, tls, goosOf())
	if err != nil {
		return "", err
	}
	b.ep = ep
	return ep, nil
}

// contextHost asks the CLI for the current docker context's engine host, with
// the clean environment and no -H. "" when the CLI cannot say (then the OS
// default applies). A test seam (b.contextHostFn) replaces it.
func (b *containerBackend) contextHost(ctx context.Context) string {
	if b.contextHostFn != nil {
		return b.contextHostFn(ctx)
	}
	bin, err := b.resolvedBin()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bin, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	c.Env = cliEnv(os.Environ())
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// command is the one way the backend runs the docker CLI: the absolute binary,
// the clean environment, and the vetted endpoint first on the argv (`-H <ep>`
// precedes the subcommand buildRunArgv renders, so the golden argv tests are
// unchanged). Run, Probe, the reaper and the sweep all go through here.
func (b *containerBackend) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	bin, err := b.resolvedBin()
	if err != nil {
		return nil, fmt.Errorf("container: %s CLI not found: %w", b.bin(), err)
	}
	ep, err := b.endpoint(ctx)
	if err != nil {
		return nil, err
	}
	full := append([]string{"-H", ep}, args...)
	c := exec.CommandContext(ctx, bin, full...)
	c.Env = cliEnv(os.Environ())
	return c, nil
}

// controlTimeout bounds one docker CLI call that manages the engine (version,
// info, image inspect, ps, rm) rather than runs the operator's command, whose
// limit is the policy's wall clock. The CLI can stall with the engine up: on
// one Docker Desktop a `ps --filter label=…` never returned, and a caller that
// passed a context with no deadline (serve's start-up sweep) waited on it for
// good. A variable so tests can shorten it.
var controlTimeout = 15 * time.Second

// control is command for an engine-management call: the same CLI, environment
// and endpoint, under controlTimeout. The caller runs the command and hands
// its error to done, which releases the deadline and, when the deadline is
// what ended the call, returns an error that says so (what names the call:
// "an image check"). WaitDelay makes Output return soon after the deadline
// kills the CLI even if something it started still holds the pipe.
func (b *containerBackend) control(ctx context.Context, what string, args ...string) (c *exec.Cmd, done func(error) error, err error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	c, err = b.command(ctx, args...)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	c.WaitDelay = 2 * time.Second
	return c, func(runErr error) error {
		defer cancel()
		return stalled(ctx, what, runErr)
	}, nil
}

// stalled rewrites err when ctx's deadline is what ended a docker CLI call,
// so the operator reads why instead of "signal: killed".
func stalled(ctx context.Context, what string, err error) error {
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("the docker CLI did not answer %s in time (is Docker Desktop stuck?): %w", what, context.DeadlineExceeded)
	}
	return err
}

// --- gates -------------------------------------------------------------------

// gateReport is what the version and feature checks found: refusals stop the
// run (Available returns them); warnings reach Probe's Detail and doctor.
type gateReport struct {
	refuse []string
	warn   []string
}

func (g *gateReport) add(o gateReport) {
	g.refuse = append(g.refuse, o.refuse...)
	g.warn = append(g.warn, o.warn...)
}

// refused is the refusals as one error, or nil.
func (g gateReport) refused() error {
	if len(g.refuse) == 0 {
		return nil
	}
	return errors.New(strings.Join(g.refuse, "; "))
}

func (g gateReport) warning() string { return strings.Join(g.warn, "; ") }

// lineFloors is a per-release-line floor table (runc 1.2.x needs 1.2.8, 1.3.x
// needs 1.3.3, …). A line older than oldest is refused; a line newer than any
// listed carries the fixes and is fine.
type lineFloors struct {
	oldest string            // the oldest fixed line, major.minor
	floors map[string]string // major.minor → fixed release
}

// check returns the refusal for v, or "". An unparseable v is not a refusal.
func (lf lineFloors) check(component, v string) (refuse string, parsed bool) {
	parts, ok := numericParts(v)
	if !ok || len(parts) < 2 {
		return "", false
	}
	line := fmt.Sprintf("%d.%d", parts[0], parts[1])
	if cmp, ok := compareVersions(line, lf.oldest); ok && cmp < 0 {
		return fmt.Sprintf("%s %s is older than every fixed release line (needs %s or later)", component, v, lf.floors[lf.oldest]), true
	}
	floor, listed := lf.floors[line]
	if !listed {
		return "", true // a newer line than any we know: it carries the fixes
	}
	if cmp, ok := compareVersions(v, floor); ok && cmp < 0 {
		return fmt.Sprintf("%s %s is below the floor %s for its line", component, v, floor), true
	}
	return "", true
}

// checkFloors gates on what `docker version` reports: the engine, runc and
// containerd floors refuse; a version below the latest known warns; an
// unparseable version warns. (Before W1-1 closed, this only ever warned.)
func checkFloors(info dockerVersionInfo) gateReport {
	var g gateReport
	ev := info.Server.Version
	if cmp, ok := compareVersions(ev, minEngineVersion); !ok {
		g.warn = append(g.warn, fmt.Sprintf("engine version %q could not be parsed; the floor %s was not checked", ev, minEngineVersion))
	} else if cmp < 0 {
		g.refuse = append(g.refuse, fmt.Sprintf("engine %s is below the floor %s (CVE-2026-31431 Copy Fail)", ev, minEngineVersion))
	} else if cmp, _ := compareVersions(ev, latestEngineVersion); cmp < 0 {
		g.warn = append(g.warn, fmt.Sprintf("engine %s is below the latest known release %s", ev, latestEngineVersion))
	}
	if rv := info.component("runc"); rv != "" {
		if r, parsed := runcFloors.check("runc", rv); r != "" {
			g.refuse = append(g.refuse, r+" (the Nov 2025 runc trio)")
		} else if !parsed {
			g.warn = append(g.warn, fmt.Sprintf("runc version %q could not be parsed; its floor was not checked", rv))
		}
	} else {
		g.warn = append(g.warn, "the engine did not report a runc component; its floor was not checked")
	}
	if cv := info.component("containerd"); cv != "" {
		if r, parsed := containerdFloors.check("containerd", cv); r != "" {
			g.refuse = append(g.refuse, r)
		} else if !parsed {
			g.warn = append(g.warn, fmt.Sprintf("containerd version %q could not be parsed; its floor was not checked", cv))
		}
	}
	return g
}

// checkDesktopFloor gates on the Docker Desktop product version (the registry's
// DisplayVersion on Windows, read by probeDesktop). "" (no Desktop: a headless
// engine or Podman) checks nothing.
func checkDesktopFloor(ver string) gateReport {
	var g gateReport
	if ver == "" {
		return g
	}
	cmp, ok := compareVersions(ver, minDesktopVersion)
	switch {
	case !ok:
		g.warn = append(g.warn, fmt.Sprintf("Docker Desktop version %q could not be parsed; the floor %s was not checked", ver, minDesktopVersion))
	case cmp < 0:
		g.refuse = append(g.refuse, fmt.Sprintf("Docker Desktop %s is below the floor %s (CVE-2025-9074: a networked container could take over the host)", ver, minDesktopVersion))
	default:
		if c, _ := compareVersions(ver, warnDesktopVersion); c < 0 {
			g.warn = append(g.warn, fmt.Sprintf("Docker Desktop %s is below %s; update it", ver, warnDesktopVersion))
		} else if c, _ := compareVersions(ver, latestDesktop); c < 0 {
			g.warn = append(g.warn, fmt.Sprintf("Docker Desktop %s is below the latest known release %s", ver, latestDesktop))
		}
	}
	return g
}

// engineFacts is the slice of `docker info --format {{json .}}` the gates read:
// the security options (seccomp must be on; userns and an LSM are recorded),
// the registered runtimes (nvidia is recorded, never used), the image store
// and the kernel the engine runs on.
type engineFacts struct {
	SecurityOptions []string                   `json:"SecurityOptions"`
	Runtimes        map[string]json.RawMessage `json:"Runtimes"`
	DefaultRuntime  string                     `json:"DefaultRuntime"`
	Driver          string                     `json:"Driver"`
	DriverStatus    [][2]string                `json:"DriverStatus"`
	KernelVersion   string                     `json:"KernelVersion"`
	OperatingSystem string                     `json:"OperatingSystem"`
	OSType          string                     `json:"OSType"`
	Architecture    string                     `json:"Architecture"`
}

func (f engineFacts) hasSecurityOption(name string) bool {
	for _, o := range f.SecurityOptions {
		if o == "name="+name || strings.HasPrefix(o, "name="+name+",") {
			return true
		}
	}
	return false
}

// seccompProfile is the profile the engine reports ("builtin", "default", or ""
// when seccomp is off or unstated).
func (f engineFacts) seccompProfile() string {
	for _, o := range f.SecurityOptions {
		if !strings.HasPrefix(o, "name=seccomp") {
			continue
		}
		for _, kv := range strings.Split(o, ",") {
			if v, ok := strings.CutPrefix(kv, "profile="); ok {
				return v
			}
		}
		return "on"
	}
	return ""
}

// lsm names the mandatory-access-control module the engine reports, or "none"
// (Docker Desktop's VM has neither AppArmor nor SELinux).
func (f engineFacts) lsm() string {
	switch {
	case f.hasSecurityOption("apparmor"):
		return "apparmor"
	case f.hasSecurityOption("selinux"):
		return "selinux"
	}
	return "none"
}

// imageStore says which image store the engine uses: the containerd snapshotter
// store, or the classic graph driver.
func (f engineFacts) imageStore() string {
	for _, kv := range f.DriverStatus {
		if kv[0] == "driver-type" && strings.Contains(kv[1], "containerd") {
			return "containerd (" + f.Driver + ")"
		}
	}
	if f.Driver == "" {
		return "unknown"
	}
	return f.Driver
}

// runtimeNames lists the engine's registered OCI runtimes, sorted, for the
// record (the argv always pins --runtime runc).
func (f engineFacts) runtimeNames() []string {
	names := make([]string, 0, len(f.Runtimes))
	for n := range f.Runtimes {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// checkFacts gates on the engine's features: seccomp must be on, the engine
// must run Linux containers (Windows containers are out of scope), and a
// registered nvidia runtime is noted so an operator knows the GPU path exists
// and is never taken.
func checkFacts(f engineFacts) gateReport {
	var g gateReport
	if f.OSType != "" && !strings.EqualFold(f.OSType, "linux") {
		g.refuse = append(g.refuse, fmt.Sprintf("the engine runs %s containers; the sandbox needs a Linux engine (switch Docker Desktop to Linux containers)", f.OSType))
	}
	if !f.hasSecurityOption("seccomp") {
		g.refuse = append(g.refuse, "the engine does not enforce seccomp (SecurityOptions lacks name=seccomp)")
	}
	if _, ok := f.Runtimes["nvidia"]; ok {
		g.warn = append(g.warn, "the nvidia runtime is registered; the sandbox pins --runtime runc and never passes --gpus or NVIDIA_* variables")
	}
	return g
}

// dockerInfo reads the engine facts through c (a prepared `docker -H … info
// --format {{json .}}` command).
func dockerInfo(c *exec.Cmd) (engineFacts, error) {
	out, err := c.Output()
	if err != nil {
		return engineFacts{}, err
	}
	var f engineFacts
	if err := json.Unmarshal(out, &f); err != nil {
		return engineFacts{}, fmt.Errorf("parse docker info: %w", err)
	}
	return f, nil
}

// --- probe cache ----------------------------------------------------------------

// probeFacts is everything one full probe learned, for Available, Probe and
// doctor to render without re-asking the engine.
type probeFacts struct {
	version    dockerVersionInfo
	engine     engineFacts
	desktop    string // Docker Desktop product version, "" off Desktop
	wsl        string // WSL version on Windows, "" elsewhere
	wslKernel  string // the WSL kernel on Windows, "" elsewhere
	gates      gateReport
	probedAt   time.Time
	endpoint   string
	userns     bool   // ECI / a user namespace in effect, from the engine's security options
	lsm        string // apparmor | selinux | none
	seccomp    string // the seccomp profile the engine reports
	imageStore string
}

// summary is the one-line record Probe puts in Capabilities.Detail: the
// versions, the namespace and LSM state, the seccomp profile and the image
// store (W1-1: "the probe records…").
func (f probeFacts) summary() string {
	parts := []string{
		"engine " + f.version.Server.Version,
		"runc " + orUnknown(f.version.component("runc")),
		"containerd " + orUnknown(f.version.component("containerd")),
	}
	if f.desktop != "" {
		parts = append(parts, "Docker Desktop "+f.desktop)
	}
	if f.wsl != "" {
		parts = append(parts, "WSL "+f.wsl+" kernel "+orUnknown(f.wslKernel))
	}
	if f.engine.KernelVersion != "" {
		parts = append(parts, "engine kernel "+f.engine.KernelVersion)
	}
	parts = append(parts,
		"userns "+boolWord(f.userns, "on", "off (no ECI)"),
		"LSM "+f.lsm,
		"seccomp "+orUnknown(f.seccomp),
		"image store "+f.imageStore,
		"endpoint "+f.endpoint,
	)
	return strings.Join(parts, ", ")
}

// Probe cache timings: a success is trusted for probeSuccessTTL; a failure is
// not retried before an exponential back-off from probeBackoffBase, capped at
// probeBackoffMax, has passed.
const (
	probeSuccessTTL  = 60 * time.Second
	probeBackoffBase = 2 * time.Second
	probeBackoffMax  = 60 * time.Second
)

// probeCache caches a probe's result: success for a TTL, failure with
// back-off. It never caches a failure for the life of the process (TEN-439).
type probeCache struct {
	mu       sync.Mutex
	facts    *probeFacts
	okAt     time.Time
	lastErr  error
	failAt   time.Time
	failures int
	now      func() time.Time // test seam; nil is time.Now
}

func (c *probeCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// cached returns the facts or error to use without probing, and ok=true, when
// the cache has an answer that is still valid; ok=false means probe now.
func (c *probeCache) cached() (*probeFacts, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.facts != nil && now.Sub(c.okAt) < probeSuccessTTL {
		return c.facts, nil, true
	}
	if c.lastErr != nil && now.Before(c.failAt.Add(c.backoff())) {
		return nil, c.lastErr, true
	}
	return nil, nil, false
}

func (c *probeCache) backoff() time.Duration {
	d := probeBackoffBase
	for i := 1; i < c.failures && d < probeBackoffMax; i++ {
		d *= 2
	}
	return min(d, probeBackoffMax)
}

func (c *probeCache) recordOK(f *probeFacts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.facts, c.okAt = f, c.clock()
	c.lastErr, c.failures = nil, 0
}

func (c *probeCache) recordErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.facts = nil
	c.lastErr, c.failAt = err, c.clock()
	c.failures++
}

// facts runs the full engine probe (or serves it from the cache): the endpoint,
// the plaintext-daemon check, `docker version`, `docker info`, the Desktop and
// WSL versions where this OS has them, and every gate over them. A gate
// refusal is the error, so Available fails closed on it; Probe renders the
// same facts with the refusal in Detail.
func (b *containerBackend) facts(ctx context.Context) (*probeFacts, error) {
	if f, err, ok := b.cache.cached(); ok {
		return f, err
	}
	f, err := b.probeOnce(ctx)
	if err != nil {
		b.cache.recordErr(err)
		return nil, err
	}
	b.cache.recordOK(f)
	return f, nil
}

func (b *containerBackend) probeOnce(ctx context.Context) (*probeFacts, error) {
	if plaintextDaemonAnswers(ctx) {
		return nil, errors.New("a Docker daemon answers on " + plaintextDaemonAddr + " without TLS; turn off \"Expose daemon on tcp://localhost:2375 without TLS\" in Docker Desktop's settings (the engine API is root on this machine)")
	}
	vc, done, err := b.control(ctx, "a version check", "version", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	info, err := dockerVersion(vc)
	if err = done(err); err != nil {
		return nil, fmt.Errorf("the container engine is not reachable (%s); is Docker running?", oneLine(err.Error()))
	}
	ic, done, err := b.control(ctx, "an info request", "info", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	eng, err := dockerInfo(ic)
	if err = done(err); err != nil {
		return nil, fmt.Errorf("docker info failed (%s)", oneLine(err.Error()))
	}
	ep, _ := b.endpoint(ctx)
	f := &probeFacts{
		version:    info,
		engine:     eng,
		desktop:    desktopVersionProbe(),
		probedAt:   b.cache.clock(),
		endpoint:   ep,
		userns:     eng.hasSecurityOption("userns"),
		lsm:        eng.lsm(),
		seccomp:    eng.seccompProfile(),
		imageStore: eng.imageStore(),
	}
	f.wsl, f.wslKernel = probeWSL(ctx)
	f.gates.add(checkFloors(info))
	f.gates.add(checkDesktopFloor(f.desktop))
	f.gates.add(checkFacts(eng))
	if f.wsl != "" {
		f.gates.add(checkWSLFloors(f.wsl, f.wslKernel))
	}
	// TODO(TEN-455): cross-check the WSL kernel line against `uname -r` inside a
	// container, and read ECI from /proc/self/uid_map there, once the live probe
	// runs on the Windows box; both need a run, which Probe must not do.
	if err := f.gates.refused(); err != nil {
		return nil, fmt.Errorf("the engine does not meet the sandbox's floors: %w", err)
	}
	return f, nil
}

// --- small helpers -----------------------------------------------------------------

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func boolWord(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
