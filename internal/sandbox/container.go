package sandbox

// container.go is the container backend (docs/plans/docker-sandbox-plan.md and
// docs/plans/docker-sandbox-hardening.md): one ephemeral, hardened container per
// command, driven through the `docker` CLI. It is one backend for every OS, so
// this file carries no build tag; the OS-specific part is mount-source
// canonicalisation and the container path a host path maps to
// (container_paths_*.go).
//
// The seam calls it through Backend.Run / Starter.Start / Backend.Probe like any
// other backend. The hardening the live Windows probe motivated lives here:
//
//   - the argv is built from a fixed schema and never takes an argument from
//     config (buildRunArgv, forbidden by construction) — W1-2;
//   - a missing image never reaches a registry (--pull never) — W1-2;
//   - mount sources are canonical and free of reparse points, re-checked just
//     before the run (canonicalMountSource) — W1-3;
//   - a cancelled or timed-out command's container is killed and swept, never
//     left running (reap, sweepLeftovers) — W1-4;
//   - inherit and allowlist network modes fail closed to none; only an operator's
//     explicit full opens the bridge (dockerNetwork) — W1-5;
//   - a refusal is attributed conservatively and a limit never escalates
//     (attributeContainer) — W1-6/W1-9;
//   - Probe never overstates the boundary and gates on engine/runtime versions
//     (probe, checkFloors) — W1-1/W1-11.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Version floors the backend refuses to run below (docker-sandbox-hardening.md
// §3, decided 2026-09-30). Engine and runc come from `docker version`; the
// Docker Desktop floor is checked in the OS-specific probe where the product
// version is legible (the registry on Windows). A version that cannot be parsed
// is a warning, never a refusal: the CLI's own format changing must not wedge
// the backend.
const (
	minEngineVersion  = "29.4.3"
	minRuncVersion    = "1.2.8"
	minDesktopVersion = "4.44.3"
)

// containerLabelKey and friends tag every container the backend starts, so the
// reaper and the startup sweep can find this instance's leftovers and never
// another instance's or another program's (W1-4).
const (
	labelSandbox  = "tenant.sandbox"  // =1 on every sandbox container
	labelTrace    = "tenant.trace"    // the run's trace id
	labelInstance = "tenant.instance" // this Tenant process's id
)

// defaultImage is the sandbox image the backend runs when a profile names none.
// It is a local build, pinned by image id at build time (tenant sandbox image
// build); the backend runs only a pinned reference and never pulls.
const defaultImage = "tenant-sandbox:latest"

// containerBackend runs commands in a fresh hardened container each time.
type containerBackend struct {
	cli      string // the docker-compatible binary: "docker", "podman", or a path
	image    string // the pinned image reference
	instance string // this process's instance id, for the container labels
	// clock and lookPath are seams for tests; nil uses the real ones.
	lookPath func(string) (string, error)
}

// ContainerOptions configures a container backend. A zero value is the default:
// the docker CLI on PATH, the default image, a random instance id.
type ContainerOptions struct {
	CLI      string
	Image    string
	Instance string
}

// Container returns a container backend. It does not touch the engine; Probe and
// Run do. For resolves "container" through here with defaults.
func Container(opts ContainerOptions) *containerBackend {
	b := &containerBackend{
		cli:      firstNonEmptyStr(opts.CLI, "docker"),
		image:    firstNonEmptyStr(opts.Image, defaultImage),
		instance: firstNonEmptyStr(opts.Instance, defaultInstanceID()),
	}
	return b
}

func (b *containerBackend) Name() string         { return "container" }
func (b *containerBackend) Isolation() Isolation { return IsolationContainer }

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
// stand in for the engine client (W1-2). A CLI that cannot be found is returned
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
// engine answers, and the sandbox image is built. It is the fail-closed gate
// (W1-1) — a profile naming container never silently runs on the host, and a dry
// run falls back to the host only when this returns an error.
func (b *containerBackend) Available(ctx context.Context) error {
	bin, err := b.resolvedBin()
	if err != nil {
		return fmt.Errorf("the %s CLI was not found on PATH", b.bin())
	}
	if _, err := dockerVersion(ctx, bin); err != nil {
		return fmt.Errorf("the container engine is not reachable (is Docker running?): %s", oneLine(err.Error()))
	}
	if err := imagePresent(ctx, bin, b.image); err != nil {
		return fmt.Errorf("the sandbox image %s is not built (run `tenant sandbox image build`)", b.image)
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
	bin, err := b.resolvedBin()
	if err != nil {
		caps.Detail = "the " + b.bin() + " CLI was not found on PATH; install Docker (or set container.cli)"
		return caps
	}
	info, err := dockerVersion(ctx, bin)
	if err != nil {
		caps.Detail = "the container engine is not reachable (" + oneLine(err.Error()) + "); is Docker running?"
		return caps
	}
	var notes []string
	if warn := checkFloors(info); warn != "" {
		notes = append(notes, warn)
	}
	if warn := b.probeDesktop(ctx); warn != "" {
		notes = append(notes, warn)
	}
	imgErr := imagePresent(ctx, bin, b.image)
	if imgErr != nil {
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
	detail := fmt.Sprintf("%s engine %s, runtime %s: read-only rootfs, non-root user, all capabilities dropped, "+
		"no-new-privileges, default seccomp, per-run pids/memory caps; filesystem is deny-by-default (only mounts exist). "+
		"Limits is partial: --cpus is a rate, not a CPU-time budget.",
		info.clientName(), info.Server.Version, info.runcVersion())
	if len(notes) > 0 {
		detail += " NOTE: " + strings.Join(notes, "; ")
	}
	caps.Detail = detail
	return caps
}

// --- Run -------------------------------------------------------------------

func (b *containerBackend) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	bin, err := b.resolvedBin()
	if err != nil {
		return Result{}, fmt.Errorf("container: %s CLI not found: %w", b.bin(), err)
	}
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

	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	argv, err := buildRunArgv(spec)
	if err != nil {
		return Result{Violations: []Violation{{Kind: "policy", Detail: oneLine(err.Error())}}}, err
	}

	c := exec.CommandContext(ctx, bin, argv...)
	if strings.TrimSpace(cmd.Stdin) != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}
	// Killing the CLI does not stop the container (the live probe showed the
	// orphan), so Cancel runs `docker kill <name>` and the deferred reap sweeps
	// anything left (W1-4).
	c.Cancel = func() error { return b.reap(spec.name) }
	c.WaitDelay = 3 * time.Second
	defer b.reap(spec.name)

	out := &cappedBuffer{max: pol.Limits.MaxOutputBytes}
	c.Stdout = out
	c.Stderr = out
	if pol.Stderr != nil {
		c.Stderr = teeWriter{out, pol.Stderr}
	}

	runErr := c.Run()
	res := Result{Output: out.String(), PID: pidOf(c)}
	if out.Truncated() {
		res.Violations = append(res.Violations, Violation{Kind: "limit", Detail: "output exceeded the cap and was truncated"})
		_ = b.reap(spec.name)
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
		return res, fmt.Errorf("container: could not run %s: %w", bin, runErr)
	}
	return res, nil
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
	var mounts []mount
	for _, r := range dedupPathList(pol.ReadRoots) {
		if r == pol.WorkDir {
			continue
		}
		m, err := containerMount(r, true)
		if err != nil {
			return containerSpec{}, fmt.Errorf("read root %s: %w", r, err)
		}
		mounts = append(mounts, m)
	}
	for _, r := range dedupPathList(pol.WriteRoots) {
		if r == pol.WorkDir {
			continue
		}
		m, err := containerMount(r, false)
		if err != nil {
			return containerSpec{}, fmt.Errorf("write root %s: %w", r, err)
		}
		mounts = append(mounts, m)
	}
	// Deny roots (plan §3.4). The container enforces them by never mounting them,
	// so a read/write root that overlaps a deny root in EITHER direction is
	// refused: inside one would expose denied content, containing one would expose
	// it through the parent. The work dir is the carve-out — it may sit INSIDE a
	// deny root (cron's work dir lives under the data dir), since only the work-dir
	// subtree is mounted; but a work dir that IS or CONTAINS a deny root fails
	// closed.
	for _, d := range pol.DenyRoots {
		if d == "" {
			continue
		}
		// Resolve the deny root the same way mount sources are resolved
		// (canonForCompare == what containerMount does), or a symlinked deny root
		// (macOS /var -> /private/var, a symlinked home) would not match a resolved
		// mount and the guard would silently miss.
		d = canonForCompare(d)
		if withinHost(d, work.host) { // deny root at or inside the work dir
			return containerSpec{}, fmt.Errorf("work dir %s is or contains the deny root %s", work.host, d)
		}
		for _, m := range mounts {
			if withinHost(m.host, d) || withinHost(d, m.host) {
				return containerSpec{}, fmt.Errorf("mount %s overlaps the deny root %s", m.host, d)
			}
		}
	}
	sh := cmd.Shell
	if sh == "" {
		sh = strings.Join(cmd.Args, " ")
	}
	return containerSpec{
		image:    b.image,
		name:     containerName(),
		trace:    cmd.Origin,
		instance: b.instance,
		work:     work,
		mounts:   mounts,
		network:  dockerNetwork(pol.Network),
		envNames: envNamesFor(pol.Env),
		limits:   pol.Limits,
		command:  sh,
	}, nil
}

// reap kills and removes the named container. It is safe to call more than once
// and on a container that has already gone (--rm), so Cancel, the output-cap
// path and the deferred cleanup can all call it.
func (b *containerBackend) reap(name string) error {
	bin, err := b.resolvedBin()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, "kill", name).Run()
}

// SweepLeftovers kills this instance's leftover sandbox containers at startup:
// the TEN-401 lesson (an orphaned headless Chrome). It matches only this
// instance's label, never another live Tenant's containers (W1-4).
func (b *containerBackend) SweepLeftovers(ctx context.Context) (int, error) {
	bin, err := b.resolvedBin()
	if err != nil {
		return 0, err
	}
	filter := labelInstance + "=" + b.instance
	list, err := exec.CommandContext(ctx, bin, "ps", "-aq", "--filter", "label="+filter).Output()
	if err != nil {
		return 0, err
	}
	ids := strings.Fields(string(list))
	for _, id := range ids {
		_ = exec.CommandContext(ctx, bin, "rm", "-f", id).Run()
	}
	return len(ids), nil
}

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

func (v dockerVersionInfo) runcVersion() string {
	for _, c := range v.Server.Components {
		if strings.EqualFold(c.Name, "runc") {
			return c.Version
		}
	}
	return "unknown"
}

func dockerVersion(ctx context.Context, bin string) (dockerVersionInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version", "--format", "{{json .}}").Output()
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

// checkFloors returns a warning when the engine or runtime is below the version
// floor, or "" when both clear it or cannot be parsed. It never refuses: a floor
// is advisory here, and a hard refusal on an unparseable version would wedge the
// backend on a CLI whose format changed.
func checkFloors(info dockerVersionInfo) string {
	var warn []string
	if cmp, ok := compareVersions(info.Server.Version, minEngineVersion); ok && cmp < 0 {
		warn = append(warn, fmt.Sprintf("engine %s is below the floor %s (CVE-2026-31431 Copy Fail)", info.Server.Version, minEngineVersion))
	}
	if rv := info.runcVersion(); rv != "unknown" {
		if cmp, ok := compareVersions(rv, minRuncVersion); ok && cmp < 0 {
			warn = append(warn, fmt.Sprintf("runc %s is below the floor %s (the Nov 2025 runc trio)", rv, minRuncVersion))
		}
	}
	return strings.Join(warn, "; ")
}

func imagePresent(ctx context.Context, bin, image string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, "image", "inspect", image).Run()
}

// attributeContainer makes a conservative Violation from a failed run (W1-9).
// Only EROFS is a filesystem refusal; only a network error under `none` is a
// network refusal; OOM and "cannot fork" are limits and never escalate.
// Anything else stays unattributed, so it fails closed rather than being
// re-run on the host.
func attributeContainer(res Result, spec containerSpec) (Violation, bool) {
	low := strings.ToLower(res.Output)
	// A Windows program invoked in a Linux container exits 127 "not found"; name it
	// a program refusal so the seam can escalate it to the host (W1-6/H19).
	if res.ExitCode == 127 && strings.Contains(low, "not found") {
		if prog, ok := windowsProgramInvoked(spec.command); ok {
			return Violation{Kind: "program", Detail: "a Windows program (" + prog + ") cannot run in a Linux container"}, true
		}
	}
	if res.ExitCode == 137 || strings.Contains(low, "out of memory") || strings.Contains(low, "cannot fork") ||
		strings.Contains(low, "resource temporarily unavailable") {
		return Violation{Kind: "limit", Detail: "the container hit a resource cap (memory, pids or CPU)"}, true
	}
	if strings.Contains(low, "read-only file system") {
		return Violation{Kind: "filesystem", Detail: "wrote outside the writable roots (read-only file system)"}, true
	}
	if spec.network == "none" {
		for _, sig := range netCutSigns {
			if strings.Contains(low, sig) {
				return Violation{Kind: "network", Detail: "the container has no network (network disabled for this profile)"}, true
			}
		}
	}
	return Violation{}, false
}

var netCutSigns = []string{
	"network is unreachable", "could not resolve host", "temporary failure in name resolution",
	"no such host", "name or service not known", "connection refused", "no route to host",
}

// --- small utilities -------------------------------------------------------

var instanceCounter atomic.Uint64

func defaultInstanceID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func containerName() string {
	return fmt.Sprintf("tenant-sbx-%s-%d", defaultInstanceID(), instanceCounter.Add(1))
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

// canonForCompare resolves p to the absolute, symlink-resolved form that mount
// sources are compared in, so a deny root and a mount that name the same place
// through different symlinks still match. Falls back to the cleaned abs path when
// the target does not exist.
func canonForCompare(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
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

// cappedBuffer keeps at most max bytes and records whether more arrived, so a
// run's output can be capped as it streams (W1-8). max <= 0 means no cap.
type cappedBuffer struct {
	max       int
	buf       strings.Builder
	total     int64
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	if c.max <= 0 {
		c.buf.Write(p)
		return len(p), nil
	}
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return c.buf.String() + "\n…[truncated]"
	}
	return c.buf.String()
}

func (c *cappedBuffer) Truncated() bool { return c.truncated }

// teeWriter writes to both w and the observer (Policy.Stderr).
type teeWriter struct {
	a, b interface{ Write([]byte) (int, error) }
}

func (t teeWriter) Write(p []byte) (int, error) {
	t.a.Write(p)
	return t.b.Write(p)
}
