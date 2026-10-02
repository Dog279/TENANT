// Package sandbox is Tenant's execution-backend seam: the single boundary every
// process a model turn can spawn passes through. It sits UNDER the approval gate.
// Approval decides WHETHER a command runs; a Backend decides what can happen when
// it does. The classifier (internal/plugins/osys) remains a fast fail-early UX
// layer, never the boundary.
//
// A Backend runs a Command under a Policy. The backends:
//
//   - host       today's behaviour, no filesystem/network confinement (named
//     honestly; still honours work dir, env allowlist, wall-timeout,
//     output cap).
//   - restricted OS primitives, no external runtime: macOS Seatbelt only.
//     Registered via registerRestricted in a build-tagged file, so this
//     package compiles and runs with only the host backend on any
//     platform. (The Linux and Windows bubblewrap backends were removed;
//     docs/plans/docker-sandbox-plan.md.)
//   - jobobject  Windows only: a Job Object that caps resources and scopes the
//     environment, confining no filesystem or network.
//   - container  an ephemeral hardened Docker container per run, on every OS,
//     driven through the docker CLI (container.go; the hardening is
//     docs/plans/docker-sandbox-hardening.md).
//
// The vocabulary (allow/deny read+write roots, allowed domains, resolved-address
// blocking) mirrors Anthropic's sandbox-runtime so profiles are portable and
// familiar.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Isolation is how strong a backend's boundary is. It feeds the security audit's
// execution.isolation field (TEN-331/TEN-338).
type Isolation int

const (
	IsolationNone      Isolation = iota // host: no boundary
	IsolationProcess                    // restricted: OS-primitive confinement
	IsolationContainer                  // container: ephemeral container per run
	IsolationVM                         // reserved; not implemented
)

func (i Isolation) String() string {
	switch i {
	case IsolationProcess:
		return "process"
	case IsolationContainer:
		return "container"
	case IsolationVM:
		return "vm"
	default:
		return "none"
	}
}

// NetworkMode controls a sandboxed command's outbound network.
type NetworkMode int

const (
	NetInherit   NetworkMode = iota // no restriction beyond the host's own
	NetNone                         // no outbound network
	NetAllowlist                    // only Policy.Allow hosts/CIDRs
	NetFull                         // explicit full access
)

func (n NetworkMode) String() string {
	switch n {
	case NetNone:
		return "none"
	case NetAllowlist:
		return "allowlist"
	case NetFull:
		return "full"
	default:
		return "inherit"
	}
}

// ParseNetworkMode maps a settings string to a NetworkMode. Unknown ⇒ inherit,
// ok=false so the caller can warn.
func ParseNetworkMode(s string) (NetworkMode, bool) {
	switch s {
	case "none":
		return NetNone, true
	case "allowlist":
		return NetAllowlist, true
	case "full":
		return NetFull, true
	case "", "inherit":
		return NetInherit, true
	default:
		return NetInherit, false
	}
}

// Enforcement is how completely a backend enforces one policy dimension on the
// current host. Reported by Probe so the audit and doctor never overstate the
// boundary.
type Enforcement int

const (
	EnforceNone    Enforcement = iota // not enforced at all
	EnforcePartial                    // enforced with documented gaps
	EnforceFull                       // fully enforced
)

func (e Enforcement) String() string {
	switch e {
	case EnforceFull:
		return "enforced"
	case EnforcePartial:
		return "partial"
	default:
		return "none"
	}
}

// Limits caps a sandboxed run's resource use. A zero field means "no explicit
// cap" for that dimension.
type Limits struct {
	Wall           time.Duration // wall-clock deadline
	CPU            time.Duration // CPU-time cap (where the backend can enforce it)
	MemoryBytes    int64         // address-space / RSS cap
	MaxPIDs        int           // max processes/threads
	MaxOutputBytes int           // cap on combined output returned to the model
}

// String lists the caps set, for a log line: "wall=1m0s cpu=30s memory=256MB
// pids=16 output=16000B", or "none".
func (l Limits) String() string {
	var parts []string
	if l.Wall > 0 {
		parts = append(parts, "wall="+l.Wall.String())
	}
	if l.CPU > 0 {
		parts = append(parts, "cpu="+l.CPU.String())
	}
	if l.MemoryBytes > 0 {
		parts = append(parts, "memory="+strconv.FormatInt(l.MemoryBytes>>20, 10)+"MB")
	}
	if l.MaxPIDs > 0 {
		parts = append(parts, "pids="+strconv.Itoa(l.MaxPIDs))
	}
	if l.MaxOutputBytes > 0 {
		parts = append(parts, "output="+strconv.Itoa(l.MaxOutputBytes)+"B")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

// EnvPolicy is an ALLOWLIST for a child's environment (the Hermes
// docker_forward_env model), replacing the old name-substring denylist that
// leaked any secret whose variable name lacked KEY/TOKEN/SECRET/PASSWORD.
//
// A zero EnvPolicy (Isolate false, empty Pass and Set) inherits the full parent
// environment unchanged — this preserves the pre-sandbox os_exec behaviour so
// the default host profile is a no-op. A profile that scopes the environment
// sets Isolate true (or a non-empty Pass/Set): the child then sees only the
// forwarded names plus Set.
type EnvPolicy struct {
	Isolate bool              // force the allowlist even when Pass and Set are empty
	Pass    []string          // parent variable NAMES to forward
	Set     map[string]string // fixed values (added, overriding a forwarded name)
	// Add is NAME=value pairs added in every mode, inherit-all included: the
	// IDs that join a child's own logs to the work that started it
	// (internal/logs/capture). It never scopes the environment.
	Add []string
}

// materialize resolves the effective environment for a child given the parent
// environment (usually os.Environ()).
func (e EnvPolicy) materialize(parent []string) []string {
	if !e.Isolate && len(e.Pass) == 0 && len(e.Set) == 0 {
		return append(slices.Clip(parent), e.Add...) // inherit-all: compatibility with pre-sandbox os_exec
	}
	want := make(map[string]bool, len(e.Pass))
	for _, n := range e.Pass {
		want[n] = true
	}
	merged := make(map[string]string, len(e.Pass)+len(e.Set))
	for _, kv := range parent {
		name, _, ok := cut(kv, '=')
		if ok && want[name] {
			merged[name] = kv
		}
	}
	out := make([]string, 0, len(merged)+len(e.Set))
	for _, kv := range merged {
		out = append(out, kv)
	}
	for k, v := range e.Set {
		out = append(out, k+"="+v)
	}
	return append(out, e.Add...)
}

// Policy is the boundary applied to one run. Filesystem roots use the same
// symlink-resolved semantics as osys.Roots. Precedence, strongest last:
//
//  1. ReadRoots / WriteRoots grant access (empty ReadRoots = whole host readable).
//  2. DenyRoots revoke it: nothing under a DenyRoot is readable or writable,
//     whatever the grants above say.
//  3. WorkDir (and a backend's per-run scratch) stays readable and writable even
//     when it sits INSIDE a DenyRoot. This carve-out is what lets cron's work dir
//     live under Tenant's data dir. It never reopens a DenyRoot that is itself at
//     or inside WorkDir, so a WorkDir equal to a DenyRoot stays denied (fail
//     closed).
type Policy struct {
	WorkDir    string      // cwd; always usable (carve-out above); HOME/TMPDIR point here when env is scoped
	ReadRoots  []string    // empty ⇒ whole host readable (DenyRoots still mask it)
	WriteRoots []string    // writes confined to these plus WorkDir
	DenyRoots  []string    // never readable or writable; always includes Tenant's cfg/data/binary dirs
	Network    NetworkMode //
	Allow      []string    // hosts/CIDRs when Network == NetAllowlist
	Env        EnvPolicy   //
	Limits     Limits      //
	// Stderr, when set, also receives what a long-lived process (Start)
	// writes to stderr, as it is written: its parent files it
	// (internal/logs/capture). It observes; it bounds nothing. Run's output
	// is combined and returned whole.
	Stderr io.Writer
	// Enforce selects what Execute does when the boundary refuses something:
	// true asks the Escalator (attended surface: a person; unattended: deny);
	// false is dry-run, which re-runs on the host and reports the refusal.
	// Backends always enforce; this flag is consumed by Execute, not Backend.Run,
	// except that the macOS backend points HOME and TMPDIR into the work dir only
	// when enforcing, so a dry run sees the command's real paths.
	Enforce bool
}

// Command is one thing to run on a surface.
type Command struct {
	Shell   string   // a shell command line (sh -c / powershell -Command); preferred
	Args    []string // explicit argv, used only when Shell == ""
	Stdin   string   //
	Surface string   // "tui","dashboard","cron-shell",... for logging + escalation
	Origin  string   // free-form origin tag
}

// Violation is one thing the boundary refused (or, in dry-run, would have
// refused). Best-effort: some backends surface a denied access only as a failed
// command, in which case Violations may be empty and containment is observed
// behaviourally by the escape suite.
type Violation struct {
	Kind   string `json:"kind"`   // "filesystem" | "network" | "limit" | "policy"
	Detail string `json:"detail"` // model/operator-readable, e.g. "wrote outside write roots: /x"
}

// Result is the outcome of a run. A non-zero ExitCode is NOT an error (it is
// signal); err is reserved for infrastructure failures (the backend could not
// start the command).
type Result struct {
	Output   string
	PID      int // the process that ran (the backend's launcher, where it has one); 0 if none started
	ExitCode int
	// ExitStatus is the OS's own description of a non-zero exit ("exit status 3",
	// "signal: killed"), so adapters can reproduce pre-seam error text exactly.
	ExitStatus string
	TimedOut   bool
	Violations []Violation
	// Escalated is set by Execute when a refused command was re-run on the host,
	// after an approved escalation or in dry-run. Violations then records what the
	// boundary refused on the first attempt.
	Escalated bool
}

// Contained reports whether the boundary stopped the command from succeeding:
// it recorded a violation, or (the common Seatbelt case) the denied access
// made the command exit non-zero. An escalated result was not contained. Used by
// the escape suite, which calls Backend.Run directly.
func (r Result) Contained() bool {
	if r.Escalated {
		return false
	}
	return len(r.Violations) > 0 || r.ExitCode != 0 || r.TimedOut
}

// Refused reports whether the boundary refused a filesystem or network access.
// Limit violations (a wall-clock timeout, reaped background processes) are
// reported but are not refusals: re-running on the host would not change them,
// so Execute never escalates on them.
func (r Result) Refused() bool {
	for _, v := range r.Violations {
		if v.Kind == "filesystem" || v.Kind == "network" {
			return true
		}
	}
	return false
}

// Capabilities is what a backend actually enforces on the current host.
type Capabilities struct {
	Backend    string
	Isolation  Isolation
	Filesystem Enforcement
	Network    Enforcement
	Limits     Enforcement
	Env        Enforcement
	// Shell is the command interpreter the backend runs commands under —
	// "sh" or "powershell". It is usually the host's native shell, but a
	// backend that runs commands in a different environment reports that
	// environment's shell instead (a Linux container on a Windows host runs
	// sh). The escape suite reads this to pick the right adversary/twin
	// command forms for the backend it is probing.
	Shell  string
	Detail string
}

// Backend runs commands under a Policy.
type Backend interface {
	Name() string
	Isolation() Isolation
	Probe(ctx context.Context) Capabilities
	Run(ctx context.Context, cmd Command, pol Policy) (Result, error)
}

// Available is an optional backend check for whether it can actually run here,
// now — beyond merely being a known backend. The container backend implements it
// (the engine must be reachable and its image built); a backend that is always
// ready does not. A caller treats a non-nil result exactly as it treats For
// returning an error: an enforcing profile fails closed, a dry run falls back to
// the host.
type Available interface {
	Available(ctx context.Context) error
}

// jobObjectFactory is installed on Windows: the Job Object backend, which caps
// resources and scopes the environment but confines no filesystem or network.
var jobObjectFactory func() Backend

func registerJobObject(f func() Backend) { jobObjectFactory = f }

// restrictedFactory is installed by an OS-specific file's init() when this
// platform/build has a restricted backend. nil ⇒ none available here.
var restrictedFactory func() (Backend, error)

// registerRestricted is called from a build-tagged restricted_<goos>.go init().
func registerRestricted(f func() (Backend, error)) { restrictedFactory = f }

// Restricted returns the OS-native restricted backend, or an error when this
// build/host has none (callers on unattended surfaces fail closed).
func Restricted() (Backend, error) {
	if restrictedFactory == nil {
		return nil, fmt.Errorf("sandbox: no restricted backend on %s: restricted is macOS-only; use host, or the container backend once it ships", runtime.GOOS)
	}
	return restrictedFactory()
}

// For resolves a backend by profile name: "" / "host", "restricted",
// "jobobject" (Windows only), or "container" (not yet implemented). An
// unknown or unavailable backend is an error the caller surfaces.
func For(name string) (Backend, error) {
	switch name {
	case "", "host":
		return Host(), nil
	case "restricted":
		return Restricted()
	case "jobobject":
		if jobObjectFactory == nil {
			return nil, fmt.Errorf("sandbox: the jobobject backend exists only on Windows")
		}
		return jobObjectFactory(), nil
	case "container":
		return Container(ContainerOptions{}), nil
	default:
		return nil, fmt.Errorf("sandbox: unknown backend %q", name)
	}
}

// Escalator decides whether a command the boundary refused may re-run on the
// host, unconfined. It is the approval seam: an attended surface asks a person,
// an unattended surface denies. A nil Escalator denies.
type Escalator func(ctx context.Context, cmd Command, refused Result) bool

// Execute runs cmd on b under pol and applies the refusal policy. Callers should
// use Execute; Backend.Run is the raw primitive.
//
//   - Nothing refused: the backend's result, unchanged. The host backend never
//     refuses, so on host Execute is exactly Run.
//   - Refused, pol.Enforce false (dry-run): re-run on the host so behaviour is
//     unchanged. The result carries the first attempt's Violations and
//     Escalated=true so the refusal is still reported.
//   - Refused, pol.Enforce true: esc decides. Approved ⇒ re-run on the host
//     (Escalated=true, Violations kept). Denied or nil ⇒ the confined result,
//     Violations intact.
//
// Re-running after a refusal means whatever the command did BEFORE the boundary
// stopped it can happen twice. The first attempt could only write inside the
// policy's writable roots, but a side effect that precedes the first refusal (a
// network call on a profile that allows network, an append in WorkDir) is not
// idempotent. Attended surfaces show the refusal before asking; for dry-run, vet
// non-idempotent jobs with `tenant sandbox run` first.
func Execute(ctx context.Context, b Backend, cmd Command, pol Policy, esc Escalator) (Result, error) {
	res, err := b.Run(ctx, cmd, pol)
	if err != nil || !res.Refused() {
		return res, err
	}
	if pol.Enforce && (esc == nil || !esc(ctx, cmd, res)) {
		return res, nil
	}
	hres, herr := Host().Run(ctx, cmd, pol)
	hres.Violations = append(append([]Violation(nil), res.Violations...), hres.Violations...)
	hres.Escalated = true
	return hres, herr
}

// --- shared helpers used by every backend ---

// shellName is the native shell for a GOOS: "powershell" on Windows, "sh"
// elsewhere. Backends report it in Capabilities.Shell unless they run commands
// in a different environment (a Linux container runs sh on a Windows host).
func shellName(goos string) string {
	if goos == "windows" {
		return "powershell"
	}
	return "sh"
}

// shellArgs picks the platform shell for a Command. An explicit Args argv wins
// over Shell. Mirrors osys.shellArgs: PowerShell (non-interactive, no profile)
// on Windows, sh -c on Unix.
func shellArgs(cmd Command) (string, []string) {
	if cmd.Shell == "" && len(cmd.Args) > 0 {
		return cmd.Args[0], cmd.Args[1:]
	}
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", cmd.Shell}
	}
	return "sh", []string{"-c", cmd.Shell}
}

// pidOf is c's process id, or 0 when it never started.
func pidOf(c *exec.Cmd) int {
	if c.Process == nil {
		return 0
	}
	return c.Process.Pid
}

// clip truncates output to n bytes with a marker. n <= 0 ⇒ no cap.
// clip keeps a third of n from the head and the rest from the tail, where a
// command's errors and summary usually are.
func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	head := n / 3
	return s[:head] + "\n…[truncated]…\n" + s[len(s)-(n-head):]
}

// cut is strings.Cut for a byte separator (kept local to avoid an import churn).
func cut(s string, sep byte) (before, after string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// within reports whether path p is root itself or inside it. Both should already
// be absolute and cleaned; backends resolve symlinks first. Case-insensitive on
// Windows, where C:\TENANT and c:\tenant are one path.
func within(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		p, root = strings.ToLower(p), strings.ToLower(root)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
