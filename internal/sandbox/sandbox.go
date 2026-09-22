// Package sandbox is Tenant's execution-backend seam: the single boundary every
// process a model turn can spawn passes through. It sits UNDER the approval gate.
// Approval decides WHETHER a command runs; a Backend decides what can happen when
// it does. The classifier (internal/plugins/osys) remains a fast fail-early UX
// layer, never the boundary.
//
// A Backend runs a Command under a Policy. Three backends are planned:
//
//   - host       today's behaviour, no filesystem/network confinement (named
//                honestly; still honours work dir, env allowlist, wall-timeout,
//                output cap).
//   - restricted OS primitives, no external runtime: macOS Seatbelt, Linux
//                bubblewrap/Landlock, Windows dedicated account + ACE/WFP/Job
//                Object. Registered per-OS via registerRestricted in a
//                build-tagged file, so this package compiles and runs with only
//                the host backend on any platform.
//   - container  an ephemeral container per run (TEN-318; not yet implemented).
//
// The vocabulary (allow/deny read+write roots, allowed domains, resolved-address
// blocking) mirrors Anthropic's sandbox-runtime so profiles are portable and
// familiar.
package sandbox

import (
	"context"
	"fmt"
	"runtime"
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
}

// materialize resolves the effective environment for a child given the parent
// environment (usually os.Environ()).
func (e EnvPolicy) materialize(parent []string) []string {
	if !e.Isolate && len(e.Pass) == 0 && len(e.Set) == 0 {
		return parent // inherit-all: compatibility with pre-sandbox os_exec
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
	return out
}

// Policy is the boundary applied to one run. Filesystem roots use the same
// symlink-resolved semantics as osys.Roots; DenyRoots is checked first and wins.
type Policy struct {
	WorkDir    string      // the only implicitly-writable dir; cwd; TMP/TEMP/HOME point here
	ReadRoots  []string    // empty ⇒ whole host readable (DenyRoots still masks)
	WriteRoots []string    // writes confined to these plus WorkDir
	DenyRoots  []string    // never readable or writable; always includes cfg/data/binary/wiki
	Network    NetworkMode //
	Allow      []string    // hosts/CIDRs when Network == NetAllowlist
	Env        EnvPolicy   //
	Limits     Limits      //
	Enforce    bool        // false ⇒ dry-run: violations are reported but the command still runs
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
	Kind   string // "filesystem" | "network" | "limit" | "policy"
	Detail string // model/operator-readable, e.g. "wrote outside write roots: /x"
}

// Result is the outcome of a run. A non-zero ExitCode is NOT an error (it is
// signal); err is reserved for infrastructure failures (the backend could not
// start the command).
type Result struct {
	Output     string
	ExitCode   int
	TimedOut   bool
	Violations []Violation
}

// Contained reports whether the boundary stopped the command from succeeding —
// either it recorded a violation, or (the common Seatbelt/bwrap case) the denied
// access made the command exit non-zero. Used by the escape suite.
func (r Result) Contained() bool {
	return len(r.Violations) > 0 || r.ExitCode != 0 || r.TimedOut
}

// Capabilities is what a backend actually enforces on the current host.
type Capabilities struct {
	Backend    string
	Isolation  Isolation
	Filesystem Enforcement
	Network    Enforcement
	Limits     Enforcement
	Env        Enforcement
	Detail     string
}

// Backend runs commands under a Policy.
type Backend interface {
	Name() string
	Isolation() Isolation
	Probe(ctx context.Context) Capabilities
	Run(ctx context.Context, cmd Command, pol Policy) (Result, error)
}

// restrictedFactory is installed by an OS-specific file's init() when this
// platform/build has a restricted backend. nil ⇒ none available here.
var restrictedFactory func() (Backend, error)

// registerRestricted is called from a build-tagged restricted_<goos>.go init().
func registerRestricted(f func() (Backend, error)) { restrictedFactory = f }

// Restricted returns the OS-native restricted backend, or an error when this
// build/host has none (callers on unattended surfaces fail closed).
func Restricted() (Backend, error) {
	if restrictedFactory == nil {
		return nil, fmt.Errorf("sandbox: no restricted backend on %s", runtime.GOOS)
	}
	return restrictedFactory()
}

// For resolves a backend by profile name: "" / "host", "restricted", or
// "container" (not yet implemented). An unknown or unavailable backend is an
// error the caller surfaces.
func For(name string) (Backend, error) {
	switch name {
	case "", "host":
		return Host(), nil
	case "restricted":
		return Restricted()
	case "container":
		return nil, fmt.Errorf("sandbox: container backend not yet implemented (TEN-318)")
	default:
		return nil, fmt.Errorf("sandbox: unknown backend %q", name)
	}
}

// --- shared helpers used by every backend ---

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

// clip truncates output to n bytes with a marker. n <= 0 ⇒ no cap.
func clip(s string, n int) string {
	if n > 0 && len(s) > n {
		return s[:n] + "\n…[truncated]"
	}
	return s
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
