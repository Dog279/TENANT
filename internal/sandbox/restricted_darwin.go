//go:build darwin

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file is the macOS half of the "restricted" backend (SEC-010 / TEN-346):
// OS-primitive confinement with no external runtime, built on Seatbelt via
// /usr/bin/sandbox-exec. It generates an SBPL profile from a Policy, writes it to
// a per-run temp file, and runs `sandbox-exec -f <profile> <shell> <args...>`.
//
// Design notes proven against real sandbox-exec on macOS 15 (arm64):
//
//   - A `(deny default)` profile SIGABRTs the child during dyld bootstrap unless
//     it can (a) read the root directory node itself and (b) stat/traverse the
//     path to its own cwd. So the profile always allows `file-read-metadata`
//     broadly and `file-read*` on the literal "/" (the root node only, never its
//     subtree) plus the system read roots a shell + /usr/bin tools need. Broad
//     metadata leaks existence/size, never file CONTENTS or any write — data
//     read/write confinement stays complete, which is what Filesystem=EnforceFull
//     claims.
//   - Seatbelt matches on the fully symlink-resolved path, and macOS temp dirs
//     live behind /var -> /private/var. Every policy path is therefore resolved
//     (resolvePath) before it is emitted, or legitimate access under a work dir
//     silently fails.
//   - Later rules win in SBPL, so DenyRoots are emitted LAST, after every allow,
//     and win even when ReadRoots is empty (whole-host read) — verified.
//   - Seatbelt canonicalizes ".." before matching, so a `<workdir>/../secret`
//     traversal is contained the same as a direct read.
type seatbelt struct{}

func init() {
	registerRestricted(func() (Backend, error) { return seatbelt{}, nil })
}

func (seatbelt) Name() string         { return "restricted" }
func (seatbelt) Isolation() Isolation { return IsolationProcess }

func (seatbelt) Probe(context.Context) Capabilities {
	return Capabilities{
		Backend:    "seatbelt",
		Isolation:  IsolationProcess,
		Filesystem: EnforceFull,    // read/write roots + deny roots are enforced by the kernel
		Network:    EnforceFull,    // NetNone fully cuts outbound; see Detail for allowlist caveat
		Limits:     EnforcePartial, // wall-clock only
		Env:        EnforceFull,    // env allowlist honoured by the shared materialize()
		Detail: "macOS Seatbelt (sandbox-exec): filesystem read/write roots and full " +
			"network-deny are kernel-enforced; wall-clock timeout is enforced but " +
			"memory and PID caps are NOT (no cgroups on macOS without a container) — " +
			"Limits is wall-only; network allowlist-by-host is best-effort (outbound is " +
			"all-or-nothing at the Seatbelt layer, so NetAllowlist permits and defers " +
			"host filtering to the resolved-address blocker / container backend)",
	}
}

func (seatbelt) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	// A per-run scratch dir doubles as the profile's home and as an extra writable
	// area (Policy allows writes under WorkDir + WriteRoots + a scratch temp).
	scratch, err := os.MkdirTemp("", "tenant-sbx-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(scratch)

	profPath := filepath.Join(scratch, "profile.sb")
	if err := os.WriteFile(profPath, []byte(seatbeltProfile(pol, scratch)), 0o600); err != nil {
		return Result{}, err
	}

	shell, sargs := shellArgs(cmd)
	argv := append([]string{"-f", profPath, shell}, sargs...)
	c := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", argv...)
	if strings.TrimSpace(cmd.Stdin) != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}
	if pol.WorkDir != "" {
		c.Dir = pol.WorkDir
	}
	env := pol.Env.materialize(os.Environ())
	// When the policy scopes the environment, redirect HOME and TMPDIR into the
	// work dir so tools that write there land inside the boundary rather than being
	// denied. A zero EnvPolicy inherits the real env unchanged (host-compatible).
	if envScoped(pol.Env) && pol.WorkDir != "" {
		env = setEnv(env, "HOME", pol.WorkDir)
		env = setEnv(env, "TMPDIR", pol.WorkDir)
	}
	c.Env = env

	out, err := c.CombinedOutput()
	res := Result{Output: clip(string(out), pol.Limits.MaxOutputBytes)}

	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		res.Violations = append(res.Violations, Violation{Kind: "limit", Detail: "wall-clock deadline exceeded"})
		return res, nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			if v, ok := attributeDenial(res.Output); ok {
				res.Violations = append(res.Violations, v)
			}
			return res, nil // a non-zero exit is signal, not an infra error
		}
		// Could not start sandbox-exec at all: an infrastructure failure.
		return res, err
	}
	return res, nil
}

// systemReadRoots are the paths a /bin/sh plus /usr/bin tools must read to load
// and run. The dyld shared cache lives under the Cryptexes tree on Apple silicon;
// /private/var/db/dyld is the legacy location kept for older hosts. /etc and the
// timezone db let common libc-backed tools resolve config and localtime. None of
// these cover a temp-dir secret, so allowing them does not weaken containment.
var systemReadRoots = []string{
	"/usr",
	"/System",
	"/bin",
	"/sbin",
	"/Library",
	"/private/var/db/dyld",
	"/System/Volumes/Preboot/Cryptexes",
	"/private/var/db/timezone",
	"/private/etc",
	"/etc",
}

// seatbeltProfile renders the SBPL profile for one run. scratch is the per-run
// writable scratch directory.
func seatbeltProfile(pol Policy, scratch string) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	w("(version 1)")
	w(";; Tenant restricted backend — generated per run, do not edit.")
	w("(deny default)")

	// Process + system essentials so the shell can fork/exec and read sysctls.
	w("(allow process-fork)")
	w("(allow process-exec)")
	w("(allow sysctl-read)")

	// Broad metadata (stat/getcwd/path traversal): required or the child cannot
	// even resolve its own cwd. Leaks existence/size only — never contents.
	w("(allow file-read-metadata)")

	// The loader reads the root node during bootstrap; a literal grants only "/".
	w(`(allow file-read* (literal "/"))`)

	// System read roots a shell + /usr/bin tools need to load.
	w("(allow file-read*")
	for _, p := range systemReadRoots {
		w("  (subpath " + sbplStr(p) + ")")
	}
	w(")")

	// Character devices shells and tools open.
	w(`(allow file-read* file-write* (literal "/dev/null"))`)
	w(`(allow file-read* (literal "/dev/zero"))`)
	w(`(allow file-read* (literal "/dev/random"))`)
	w(`(allow file-read* (literal "/dev/urandom"))`)
	w(`(allow file-read* file-write* (literal "/dev/dtracehelper"))`)
	w(`(allow file-read* file-write* (literal "/dev/tty"))`)

	// Reads: explicit ReadRoots, or the whole host when empty (DenyRoots mask it).
	reads := resolveAll(pol.ReadRoots)
	if len(reads) == 0 {
		w(`(allow file-read* (subpath "/"))`)
	} else {
		for _, p := range reads {
			w("(allow file-read* (subpath " + sbplStr(p) + "))")
		}
	}

	// Writes: WriteRoots + WorkDir + scratch. WorkDir and scratch are also readable
	// (cwd + scratch space); other WriteRoots are write-only unless ReadRoots also
	// grant them.
	writes := resolveAll(pol.WriteRoots)
	rw := dedupPaths([]string{resolvePath(pol.WorkDir), resolvePath(scratch)})
	writes = append(writes, rw...)
	for _, p := range dedupPaths(writes) {
		w("(allow file-write* (subpath " + sbplStr(p) + "))")
	}
	for _, p := range rw {
		w("(allow file-read* (subpath " + sbplStr(p) + "))")
	}

	// Deny roots LAST so deny wins (later rules win in SBPL), even over subpath "/".
	for _, p := range resolveAll(pol.DenyRoots) {
		w("(deny file-read* (subpath " + sbplStr(p) + "))")
		w("(deny file-write* (subpath " + sbplStr(p) + "))")
	}

	// Network.
	switch pol.Network {
	case NetNone:
		w("(deny network*)")
	case NetAllowlist:
		// Seatbelt cannot allowlist by hostname; outbound is all-or-nothing here, so
		// permit it and let the resolved-address blocker / container backend enforce
		// the allowlist. Probe reports this as a best-effort gap.
		w("(allow network*)")
	default: // NetFull, NetInherit
		w("(allow network*)")
	}

	return b.String()
}

// envScoped reports whether an EnvPolicy restricts the child's environment (the
// same condition materialize() uses to switch off inherit-all).
func envScoped(e EnvPolicy) bool {
	return e.Isolate || len(e.Pass) > 0 || len(e.Set) > 0
}

// setEnv returns env with any existing key removed and key=val appended, so the
// override is unambiguous regardless of what materialize() produced.
func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+val)
}

// attributeDenial makes a best-effort Violation from a Seatbelt denial. Seatbelt
// reports a blocked operation to the child as EPERM ("operation not permitted"),
// so a non-zero exit whose output carries that signature is very likely a
// containment event. Classification is heuristic; Contained() never depends on it.
func attributeDenial(output string) (Violation, bool) {
	low := strings.ToLower(output)
	if !strings.Contains(low, "operation not permitted") && !strings.Contains(low, "sandbox") {
		return Violation{}, false
	}
	if strings.Contains(low, "/dev/tcp") || strings.Contains(low, "connect") ||
		strings.Contains(low, "network") || strings.Contains(low, "socket") {
		return Violation{Kind: "network", Detail: "sandbox blocked a network operation"}, true
	}
	return Violation{Kind: "filesystem", Detail: "sandbox blocked access outside the policy's read/write roots"}, true
}

// sbplStr renders a Go string as an SBPL string literal (C-like escaping).
func sbplStr(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// resolvePath returns p as an absolute, symlink-resolved path — the form Seatbelt
// matches against. For a path that does not exist yet it resolves the deepest
// existing ancestor and re-joins the remaining tail, so /var-rooted temp paths
// canonicalize to /private/var even before their leaf is created.
func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	var tail []string
	cur := abs
	for {
		parent := filepath.Dir(cur)
		tail = append([]string{filepath.Base(cur)}, tail...)
		if parent == cur {
			return abs // reached root without resolving anything
		}
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(append([]string{r}, tail...)...)
		}
		cur = parent
	}
}

func resolveAll(ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if r := resolvePath(p); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func dedupPaths(ps []string) []string {
	seen := make(map[string]bool, len(ps))
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
