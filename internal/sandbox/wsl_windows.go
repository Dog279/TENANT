//go:build windows

package sandbox

// wsl_windows.go: the Windows restricted backend. Windows has no native
// filesystem/network confinement primitive wired up yet (the Job Object in
// restricted_windows.go enforces resource limits only), so on Windows the
// restricted backend runs each command inside a WSL2 distro under bubblewrap —
// the same confinement the Linux backend uses, reached through wsl.exe. This is
// what lets "restricted" mean real filesystem + network confinement on Windows
// instead of "limits only, files wide open".
//
// It is written to work across arbitrary open-source installs, not just one
// machine's setup:
//   - It does not assume a distro name. It tries an operator override
//     (TENANT_SANDBOX_WSL_DISTRO), then the WSL default, then every installed
//     distro, and uses the first one whose bubblewrap actually works.
//   - It does not assume bubblewrap is present or functional. It probes for it
//     and runs a real namespace self-check (unprivileged user namespaces can be
//     disabled, and WSL1 cannot do this at all). A distro that cannot confine is
//     not used.
//   - It does not assume the /mnt mount root. It asks wslpath where the Windows
//     drives are mounted, because automount.root can be customised.
//   - If no distro can confine, construction FAILS. The caller then fails closed
//     (the command does not run) rather than running it unconfined. No OS
//     silently runs a "restricted" command with no sandbox.
//
// Discovery is cached once per process (it execs wsl.exe a few times), so
// resolving the backend per command is cheap after the first.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

func init() { registerRestricted(newWindowsRestricted) }

// probeTimeout bounds each wsl.exe discovery call so a wedged distro cannot hang
// backend construction forever.
const probeTimeout = 15 * time.Second

// distroEnvVar lets an operator pin which WSL distro confines commands, for a
// machine whose default distro has no bubblewrap but another does.
const distroEnvVar = "TENANT_SANDBOX_WSL_DISTRO"

// wslInfo is a discovered, confinement-capable WSL target.
type wslInfo struct {
	wslExe      string // absolute path to wsl.exe
	distro      string // "" means the WSL default distro
	bwrap       string // absolute path to bwrap inside the distro
	mountPrefix string // where Windows drives mount, e.g. "/mnt" (so C:\ -> /mnt/c)
}

var (
	wslOnce   sync.Once
	wslCached *wslInfo
	wslErr    error
)

// newWindowsRestricted is the registered restricted-backend factory on Windows.
// It returns a WSL-bubblewrap backend, or an actionable error when no WSL distro
// on this machine can confine a command — in which case restricted surfaces fail
// closed.
func newWindowsRestricted() (Backend, error) {
	wslOnce.Do(func() { wslCached, wslErr = discoverWSL() })
	if wslErr != nil {
		return nil, wslErr
	}
	return &wslBackend{info: *wslCached}, nil
}

// discoverWSL finds the first WSL distro whose bubblewrap can actually create a
// sandbox on this host.
func discoverWSL() (*wslInfo, error) {
	wslExe, err := exec.LookPath("wsl.exe")
	if err != nil {
		return nil, errors.New("sandbox: restricted backend unavailable: WSL is not installed (wsl.exe not found). " +
			"Install WSL2 and a distro with the 'bubblewrap' package to get a restricted sandbox on Windows")
	}

	var tried []string
	for _, distro := range candidateDistros(wslExe) {
		label := distro
		if label == "" {
			label = "(default)"
		}
		bwrap, why := probeDistro(wslExe, distro)
		if bwrap == "" {
			tried = append(tried, fmt.Sprintf("%s: %s", label, why))
			continue
		}
		return &wslInfo{
			wslExe:      wslExe,
			distro:      distro,
			bwrap:       bwrap,
			mountPrefix: probeMountPrefix(wslExe, distro),
		}, nil
	}
	detail := "none found"
	if len(tried) > 0 {
		detail = strings.Join(tried, "; ")
	}
	return nil, fmt.Errorf("sandbox: restricted backend unavailable: no WSL distro has a working bubblewrap sandbox (%s). "+
		"Install 'bubblewrap' in a WSL2 distro and ensure unprivileged user namespaces are enabled, "+
		"or set %s to a distro that has it", detail, distroEnvVar)
}

// candidateDistros is the order to try: an operator override, then the WSL
// default (empty name), then every installed distro (best-effort enumeration).
func candidateDistros(wslExe string) []string {
	if pin := strings.TrimSpace(os.Getenv(distroEnvVar)); pin != "" {
		return []string{pin}
	}
	seen := map[string]bool{"": true}
	out := []string{""} // the default distro first
	for _, d := range listDistros(wslExe) {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// listDistros enumerates installed distros via `wsl.exe -l -q`. That output is
// UTF-16 with Windows line endings, so it is decoded tolerantly; any failure
// yields nil and discovery falls back to the default distro alone.
func listDistros(wslExe string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, wslExe, "-l", "-q").Output()
	if err != nil {
		return nil
	}
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(decodeWSLText(out)))
	for sc.Scan() {
		if n := strings.TrimSpace(sc.Text()); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// probeDistro reports the bubblewrap path in a distro when bubblewrap is present
// AND a real namespace self-check passes; otherwise it returns "" and a short
// reason. The self-check is the honest test: a distro where unprivileged user
// namespaces are disabled (or WSL1) has bwrap on disk but cannot confine, and
// must not be used.
func probeDistro(wslExe, distro string) (bwrap, why string) {
	path := strings.TrimSpace(runWSL(wslExe, distro, "sh", "-c", "command -v bwrap 2>/dev/null"))
	if path == "" {
		return "", "bubblewrap not installed"
	}
	// A minimal confinement that touches every namespace the backend relies on.
	// If this exits non-zero, bubblewrap cannot build a sandbox here.
	check := runWSLCode(wslExe, distro, path,
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--unshare-net", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
		"--die-with-parent", "--", "true")
	if check != 0 {
		return "", "bubblewrap present but cannot create namespaces (unprivileged user namespaces disabled, or WSL1)"
	}
	return path, ""
}

// probeMountPrefix asks the distro where the Windows drives are mounted, so path
// translation respects a custom automount.root. Defaults to /mnt on any doubt.
func probeMountPrefix(wslExe, distro string) string {
	out := strings.TrimSpace(runWSL(wslExe, distro, "wslpath", "-u", `C:\`))
	// out is like "/mnt/c/" or "/c/"; the prefix is its parent ("/mnt" or "/").
	out = strings.TrimRight(out, "/")
	if out == "" || !strings.HasPrefix(out, "/") {
		return "/mnt"
	}
	if p := path.Dir(out); p != "" {
		return p
	}
	return "/mnt"
}

// runWSL runs `wsl.exe [-d distro] -e <args...>` and returns its combined output.
func runWSL(wslExe, distro string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, wslExe, wslArgv(distro, args...)...).CombinedOutput()
	return string(out)
}

// runWSLCode runs a command in the distro and returns just its exit code (-1 on
// a start/timeout failure), for the namespace self-check.
func runWSLCode(wslExe, distro string, args ...string) int {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	err := exec.CommandContext(ctx, wslExe, wslArgv(distro, args...)...).Run()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// wslArgv builds the wsl.exe argument vector: an optional "-d <distro>" then
// "-e" and the program+args to exec directly (no login shell).
func wslArgv(distro string, args ...string) []string {
	argv := make([]string, 0, len(args)+3)
	if distro != "" {
		argv = append(argv, "-d", distro)
	}
	argv = append(argv, "-e")
	return append(argv, args...)
}

// decodeWSLText normalises wsl.exe management output, which is UTF-16LE with a
// BOM. ASCII distro names decode by dropping the high (NUL) byte of each unit;
// plain UTF-8 (no interleaved NULs) is returned unchanged. Tolerant by design:
// enumeration is best-effort.
func decodeWSLText(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte{0xFF, 0xFE}) // UTF-16LE BOM
	if !bytes.Contains(b, []byte{0x00}) {
		return b // already single-byte text
	}
	out := make([]byte, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if b[i] != 0x00 { // low byte of a UTF-16LE unit; skip the NUL high byte
			out = append(out, b[i])
		}
	}
	return out
}

// wslBackend confines a command by running it inside a WSL distro under
// bubblewrap. The confinement flags are the same OS-agnostic argv the Linux
// backend uses (bwrapArgs in spec.go); only the invocation (via wsl.exe) and the
// Windows->Linux path translation are Windows-specific.
type wslBackend struct{ info wslInfo }

func (*wslBackend) Name() string         { return "restricted" }
func (*wslBackend) Isolation() Isolation { return IsolationProcess }

func (b *wslBackend) Probe(context.Context) Capabilities {
	distro := b.info.distro
	if distro == "" {
		distro = "the default distro"
	}
	return Capabilities{
		Backend:    "restricted",
		Isolation:  IsolationProcess,
		Filesystem: EnforceFull,    // bubblewrap bind/tmpfs confinement
		Network:    EnforceFull,    // NetNone cuts the network namespace
		Limits:     EnforcePartial, // wall-clock + output cap only (no cgroup caps through WSL yet)
		Env:        EnforcePartial, // WSL's default env, not yet a strict allowlist through the wsl.exe boundary
		Shell:      "sh",           // commands run as POSIX sh inside WSL, even though the host is Windows
		Detail: "Windows via WSL2 bubblewrap (" + distro + "): filesystem read/write/deny roots and " +
			"network-none are enforced by bubblewrap; Windows paths are translated to their WSL mount " +
			"(" + b.info.mountPrefix + "); commands run as Linux sh, not PowerShell; wall-clock timeout only " +
			"(no memory/CPU/PID caps through the WSL boundary); env is WSL's default, not a strict allowlist yet",
	}
}

func (b *wslBackend) Run(ctx context.Context, cmd Command, pol Policy) (Result, error) {
	if pol.Limits.Wall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.Limits.Wall)
		defer cancel()
	}

	tpol := b.translatePolicy(pol)
	line := commandLine(cmd)
	bwrapArgv := bwrapArgs(tpol, "sh", []string{"-c", line})
	argv := wslArgv(b.info.distro, append([]string{b.info.bwrap}, bwrapArgv...)...)

	c := exec.CommandContext(ctx, b.info.wslExe, argv...)
	if strings.TrimSpace(cmd.Stdin) != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}

	out, err := c.CombinedOutput()
	res := Result{Output: clip(string(out), pol.Limits.MaxOutputBytes)}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		res.ExitStatus = ee.Error()
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
		return res, nil
	}
	if err != nil && ee == nil {
		// wsl.exe itself could not run (vanished after discovery): infra error.
		return res, fmt.Errorf("sandbox: wsl.exe could not run the command: %w", err)
	}
	if err != nil {
		if v, ok := attributeBwrapDenial(res.Output, tpol); ok {
			res.Violations = append(res.Violations, v)
		}
	}
	return res, nil // a non-zero exit is signal, not an infra error
}

// translatePolicy rewrites the Windows paths in a Policy to their WSL locations
// so bubblewrap binds/masks the right places inside the distro. Roots already in
// Linux form pass through, so an operator can write either.
func (b *wslBackend) translatePolicy(pol Policy) Policy {
	tp := pol
	tp.WorkDir = b.toLinuxPath(pol.WorkDir)
	tp.ReadRoots = b.toLinuxPaths(pol.ReadRoots)
	tp.WriteRoots = b.toLinuxPaths(pol.WriteRoots)
	tp.DenyRoots = b.toLinuxPaths(pol.DenyRoots)
	return tp
}

func (b *wslBackend) toLinuxPaths(ps []string) []string {
	if len(ps) == 0 {
		return ps
	}
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, b.toLinuxPath(p))
	}
	return out
}

var driveRe = regexp.MustCompile(`^([A-Za-z]):[\\/](.*)$`)

// toLinuxPath maps a Windows path to its location inside the distro:
// C:\a\b -> <mountPrefix>/c/a/b. A path already in Linux form (leading "/") is
// returned unchanged; anything else is passed through best-effort.
func (b *wslBackend) toLinuxPath(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	m := driveRe.FindStringSubmatch(p)
	if m == nil {
		return p
	}
	rest := strings.ReplaceAll(m[2], `\`, "/")
	return path.Clean(b.info.mountPrefix + "/" + strings.ToLower(m[1]) + "/" + rest)
}

// commandLine extracts the shell command line to run under sh -c. It prefers the
// Shell field; for an Args argv it unwraps a "<shell> -c/-C <line>" form (cron's
// Windows argv is cmd /C <line>, which becomes sh -c <line> under WSL), else it
// joins the argv.
func commandLine(cmd Command) string {
	if strings.TrimSpace(cmd.Shell) != "" {
		return cmd.Shell
	}
	if n := len(cmd.Args); n >= 2 {
		switch strings.ToLower(cmd.Args[n-2]) {
		case "-c", "/c":
			return cmd.Args[n-1]
		}
	}
	return strings.Join(cmd.Args, " ")
}
