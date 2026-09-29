package main

// execprofiles.go is the cmd/tenant side of the execution-backend seam
// (internal/sandbox). Every process a model turn can spawn — the os_exec tool and
// cron shell jobs — resolves an execProfile here and runs through a
// sandbox.Backend via sandbox.Execute, so an operator can confine those spawns
// per surface without any spawn site changing.
//
// Surfaces. A turn's surface is stamped on its context where the turn starts
// (withExecSurface: the Discord relay, the iMessage responder, cron) and every
// tool call in the turn inherits it. Unstamped means "local": the TUI, the
// dashboard, `tenant serve` chat — an operator is at the host.
//
// Profiles are OPERATOR-controlled. Built-in profile (TEN-347): the restricted
// backend in dry run on every surface, with no network for cron-shell, and the
// safe-env allowlist (envAllowlist). Where this host's restricted backend can't
// dry-run without blocking or changing commands, the built-in backend is the
// host instead, and probe, audit, doctor and the startup feed say why
// (builtinExec). Overrides live in settings.<agent>.json under "exec_profiles"
// (keyed by surface, or "default"), load at startup and are re-read when the
// file is edited, with no restart. There is deliberately NO agent tool that
// reads or writes a profile: the model must never widen its own boundary. The
// file tools refuse the settings file outright (fileToolGuards), so write
// access does not open a way round that.
//
//	"exec_profiles": {
//	  "default":    {"enforce": true},
//	  "cron-shell": {"backend": "restricted", "network": "none"},
//	  "discord":    {"backend": "host"}
//	}
//
// "default"'s enforce is the one switch from dry run to enforcing: it applies to
// every surface whose own entry neither sets enforce nor names a backend. An
// entry that names a backend keeps the defaults it was written under (enforce
// on, network inherit). "exec_exceptions" (keyed by surface) are folders a
// surface's confined commands may write to: approving an escalation "always"
// adds the refused folder (saveExecException), never a backend switch.
//
// Refusals. When a confining backend refuses a filesystem or network access,
// sandbox.Execute applies the profile's Enforce flag. Enforce asks the turn's
// approver for "os_exec_unsandboxed" — a person on an attended surface, while
// the cron exec approver denies it — and re-runs on the host only if approved.
// Enforce false is dry-run: re-run on the host and log the refusal. A dry run
// never blocks a command, so a dry-run profile whose backend is unavailable
// runs on the host too; an enforcing one fails closed. Every run, on every
// backend, and every refusal is written to the "sandbox" log source.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdreflect "reflect" // aliased: package main already declares reflect (research.go)
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"tenant/internal/approval"
	"tenant/internal/logs"
	"tenant/internal/logs/capture"
	"tenant/internal/logs/event"
	"tenant/internal/plugins/osys"
	"tenant/internal/sandbox"
)

// Exec surfaces: where a process spawn originates.
const (
	surfaceLocal     = "local"      // TUI / dashboard / serve chat
	surfaceDiscord   = "discord"    // the Discord relay
	surfaceIMessage  = "imessage"   // the iMessage responder
	surfaceCronExec  = "cron-exec"  // an exec=true scheduled prompt job
	surfaceCronShell = "cron-shell" // a scheduled shell job
)

// execSurfaces lists every exec surface, in display order.
var execSurfaces = []string{surfaceLocal, surfaceDiscord, surfaceIMessage, surfaceCronExec, surfaceCronShell}

// actionExecUnsandboxed is the approval action raised when a sandbox refused an
// os_exec command and the turn wants it re-run outside the sandbox. categorize
// maps it to its own "sandbox" category (ask by default, deny on iMessage), so a
// session/always grant covers only further escalations; the cron exec approver
// always denies it.
const actionExecUnsandboxed = "os_exec_unsandboxed"

type execSurfaceKey struct{}

// withExecSurface stamps the turn's exec surface on ctx.
func withExecSurface(ctx context.Context, surface string) context.Context {
	return context.WithValue(ctx, execSurfaceKey{}, surface)
}

// execSurfaceFrom returns the stamped exec surface, or "local" when none.
func execSurfaceFrom(ctx context.Context) string {
	if s, _ := ctx.Value(execSurfaceKey{}).(string); s != "" {
		return s
	}
	return surfaceLocal
}

// inheritExecSurface carries the exec surface stamped on from onto to, with
// the offsite approver and lock stamped beside it. A team runtime starts
// sub-agents on its run window's context (a BaseCtx, or the turn that opened
// the window) instead of the spawning turn's, which would otherwise drop the
// stamps: a sub-agent's os_exec would run under the local profile, and a
// Discord turn's sub-agent would ask the local broker, past Discord's lock.
func inheritExecSurface(to, from context.Context) context.Context {
	if c := offsiteConfirmFrom(from); c != nil {
		to = withOffsiteConfirm(to, c)
	}
	if l, _ := from.Value(offsiteLockKey{}).(func(string) bool); l != nil {
		to = withOffsiteLock(to, l)
	}
	if s, _ := from.Value(execSurfaceKey{}).(string); s != "" {
		return withExecSurface(to, s)
	}
	return to
}

// execProfile is the resolved execution profile for one surface: which sandbox
// backend to run on, and the Policy that backend enforces.
type execProfile struct {
	Backend string
	Policy  sandbox.Policy
	// Fallback says why the surface runs on the host when nothing chose a
	// backend: this host's restricted backend can't dry-run here.
	Fallback string
}

// execProfileStore is the process-wide, live set of operator overrides and
// exceptions: loaded at startup, re-loaded by the settings watcher. No agent
// tool writes it.
type execProfileStore struct {
	mu sync.RWMutex
	m  map[string]execProfileSetting
	ex map[string][]string // exec_exceptions: surface → folders, ~ expanded
}

var defaultExecProfiles = &execProfileStore{m: map[string]execProfileSetting{}}

// Load replaces the overrides with the valid entries of in. It returns how many
// were applied and one message per skipped entry.
func (s *execProfileStore) Load(in map[string]execProfileSetting) (int, []string) {
	valid, skipped := validExecProfiles(in)
	s.mu.Lock()
	s.m = valid
	s.mu.Unlock()
	return len(valid), skipped
}

// Snapshot returns a copy of the live overrides.
func (s *execProfileStore) Snapshot() map[string]execProfileSetting {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]execProfileSetting, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// lookup returns the surface's override, else the "default" override.
func (s *execProfileStore) lookup(surface string) (execProfileSetting, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.m[surface]; ok {
		return p, true
	}
	p, ok := s.m["default"]
	return p, ok
}

// defaultEnforce is the switch: "default"'s enforce, or on when "default" names
// a backend without one; nil when it says nothing.
func (s *execProfileStore) defaultEnforce() *bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.m["default"]
	if d.Enforce == nil && d.Backend != "" {
		on := true
		return &on
	}
	return d.Enforce
}

// LoadExceptions replaces the exceptions with the valid entries of in: a known
// surface, and an absolute path (or one under ~). It returns how many paths
// were applied and one message per skipped entry.
func (s *execProfileStore) LoadExceptions(in map[string][]string) (int, []string) {
	valid, skipped := validExecExceptions(in)
	n := 0
	for surface, paths := range valid {
		valid[surface] = expandRoots(paths)
		n += len(paths)
	}
	s.mu.Lock()
	s.ex = valid
	s.mu.Unlock()
	return n, skipped
}

// exceptions returns a surface's exception folders.
func (s *execProfileStore) exceptions(surface string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.ex[surface])
}

// ExceptionsSnapshot returns a copy of the live exceptions, as loaded.
func (s *execProfileStore) ExceptionsSnapshot() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string, len(s.ex))
	for k, v := range s.ex {
		out[k] = slices.Clone(v)
	}
	return out
}

func validExecExceptions(in map[string][]string) (map[string][]string, []string) {
	valid := make(map[string][]string, len(in))
	var skipped []string
	for surface, paths := range in {
		if !slices.Contains(execSurfaces, surface) {
			skipped = append(skipped, fmt.Sprintf("exec_exceptions.%s: unknown surface (want %s)", surface, strings.Join(execSurfaces, ", ")))
			continue
		}
		for _, p := range paths {
			if !filepath.IsAbs(expandHome(p)) {
				skipped = append(skipped, fmt.Sprintf("exec_exceptions.%s: %q must be absolute (or start with ~)", surface, p))
				continue
			}
			valid[surface] = append(valid[surface], p)
		}
	}
	sort.Strings(skipped)
	return valid, skipped
}

// validExecProfiles keeps the entries that validate and describes the rest.
func validExecProfiles(in map[string]execProfileSetting) (map[string]execProfileSetting, []string) {
	valid := make(map[string]execProfileSetting, len(in))
	var skipped []string
	for surface, p := range in {
		if err := validateExecProfileSetting(surface, p); err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		valid[surface] = p
	}
	sort.Strings(skipped)
	return valid, skipped
}

func validateExecProfileSetting(surface string, p execProfileSetting) error {
	known := surface == "default"
	for _, s := range execSurfaces {
		known = known || s == surface
	}
	if !known {
		return fmt.Errorf("exec_profiles.%s: unknown surface (want default, %s)", surface, strings.Join(execSurfaces, ", "))
	}
	switch p.Backend {
	case "", "host", "restricted":
	case "container":
		return fmt.Errorf("exec_profiles.%s: the container backend is not implemented", surface)
	default:
		return fmt.Errorf("exec_profiles.%s: unknown backend %q (want host or restricted)", surface, p.Backend)
	}
	n, ok := sandbox.ParseNetworkMode(p.Network)
	if !ok {
		return fmt.Errorf("exec_profiles.%s: unknown network %q (want inherit, none, full)", surface, p.Network)
	}
	if n == sandbox.NetAllowlist || len(p.Allow) > 0 {
		return fmt.Errorf("exec_profiles.%s: a network allowlist is not enforced by any backend yet (an egress proxy is a follow-up); use \"none\" or \"full\"", surface)
	}
	if p.CPUSeconds < 0 || p.MemoryMB < 0 || p.MaxPIDs < 0 {
		return fmt.Errorf("exec_profiles.%s: limits must not be negative", surface)
	}
	for _, group := range [][]string{p.ReadRoots, p.WriteRoots, p.DenyRoots} {
		for _, r := range group {
			if !filepath.IsAbs(expandHome(r)) {
				return fmt.Errorf("exec_profiles.%s: root %q must be absolute (or start with ~)", surface, r)
			}
		}
	}
	return nil
}

// resolveExecProfile returns the execution profile for a surface. It reads only
// trusted inputs: the surface, Tenant's own dirs, a work dir, and the operator's
// exec_profiles overrides and exec_exceptions.
//
// The built-in profile is builtinExec's backend in dry run (Enforce off), with
// no network on cron-shell; an entry changes what it sets (see the file
// comment for enforce). Every profile runs commands with the safe-env
// allowlist plus its env_pass and env_set (envAllowlist). DenyRoots ALWAYS
// include tenantDenyRoots: the config dir, the data dir, the Security log's
// key and anchor, and the running executable FILE (its directory may be
// shared, e.g. /usr/local/bin, and denying it would break every tool installed
// beside Tenant). A confining backend additionally denies the well-known
// credential stores (credentialStoreRoots).
func resolveExecProfile(surface, cfgDir, dataDir, workDir string) execProfile {
	backend, why := builtinExec()
	p := execProfile{
		Backend:  backend,
		Fallback: why,
		Policy: sandbox.Policy{
			WorkDir:   workDir,
			DenyRoots: tenantDenyRoots(cfgDir, dataDir),
		},
	}
	if surface == surfaceCronShell {
		p.Policy.Network = sandbox.NetNone // a scheduled shell job has nobody to ask about the network
	}
	o, _ := defaultExecProfiles.lookup(surface)
	if o.Backend != "" {
		// An entry that names its backend is a profile of the operator's own,
		// with the defaults it was written under: enforce on, network inherited.
		p.Backend, p.Fallback = o.Backend, ""
		p.Policy.Enforce, p.Policy.Network = true, sandbox.NetInherit
	}
	switch {
	case o.Enforce != nil:
		p.Policy.Enforce = *o.Enforce
	case o.Backend == "":
		if on := defaultExecProfiles.defaultEnforce(); on != nil {
			p.Policy.Enforce = *on // the one switch: exec_profiles.default.enforce
		}
	}
	if o.Network != "" {
		p.Policy.Network, _ = sandbox.ParseNetworkMode(o.Network) // validated at load
	}
	p.Policy.Allow = o.Allow
	p.Policy.ReadRoots = expandRoots(o.ReadRoots)
	p.Policy.WriteRoots = append(expandRoots(o.WriteRoots), defaultExecProfiles.exceptions(surface)...)
	p.Policy.DenyRoots = dedupeNonEmpty(append(p.Policy.DenyRoots, expandRoots(o.DenyRoots)...)...)
	p.Policy.Env = envAllowlist(o.EnvPass, o.EnvSet)
	p.Policy.Limits.CPU = time.Duration(o.CPUSeconds) * time.Second
	p.Policy.Limits.MemoryBytes = int64(o.MemoryMB) << 20
	p.Policy.Limits.MaxPIDs = o.MaxPIDs
	if p.Backend != "host" {
		applyConfinedDefaults(&p)
	}
	return p
}

// builtinExec is the backend a surface runs on when no exec_profiles entry
// names one, and why when that is the host. Chosen once per process; the
// cmd/tenant tests pin it.
var builtinExec = sync.OnceValues(func() (backend, why string) {
	return chooseBuiltinExec(runtime.GOOS, sandbox.Restricted)
})

// chooseBuiltinExec picks the restricted backend where it can run the built-in
// dry run without blocking or changing a command, else the host with why. A
// dry run re-runs on the host only what the backend attributes as refused, so
// a refusal it can't attribute fails the command, and a backend that runs
// commands differently changes them all:
//   - Windows: restricted is WSL2 bubblewrap, which runs Linux sh, not
//     PowerShell or cmd (not asked: discovering WSL takes seconds).
//   - Linux: bubblewrap hides a deny root as an empty dir, so a read of one
//     fails with nothing attributed, and /tmp is private to each command.
//   - elsewhere (macOS): a test command must run under it.
func chooseBuiltinExec(goos string, restricted func() (sandbox.Backend, error)) (backend, why string) {
	if goos == "windows" {
		return "host", "the Windows restricted backend runs commands as Linux sh inside WSL2, not PowerShell or cmd, so it would change what every command does; name it in exec_profiles to use it anyway"
	}
	be, err := restricted()
	if err != nil {
		return "host", err.Error()
	}
	if goos == "linux" {
		return "host", "bubblewrap can't dry-run without breaking commands yet: a read of a deny root (~/.ssh for git) fails with nothing refused, so it is never re-run on the host, and /tmp is private to each command (TEN-346); name it in exec_profiles to use it anyway"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := be.Run(ctx, sandbox.Command{Shell: "exit 0"}, sandbox.Policy{Enforce: true})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("%s: %s", exitText(res), strings.TrimSpace(res.Output))
	}
	if err != nil {
		return "host", "the restricted backend could not run a test command: " + err.Error()
	}
	return "restricted", ""
}

// tenantDenyRoots is what no sandboxed command, MCP server or plugin child may
// read or write: Tenant's config and data dirs, the Security log's key and
// anchor (TEN-396; inside the config dir, and named too so no change to that
// entry can expose them), and the running executable.
func tenantDenyRoots(cfgDir, dataDir string, more ...string) []string {
	roots := []string{cfgDir, dataDir, exePath()}
	if cfgDir != "" {
		roots = append(roots, chainKeyPath(cfgDir), chainAnchorPath(cfgDir))
	}
	return dedupeNonEmpty(append(roots, more...)...)
}

// withBackend returns p moved onto backend, with the defaults every confining
// profile gets (see applyConfinedDefaults). `tenant sandbox run --backend` uses
// it so a forced trial run matches what a real profile with that backend does.
func withBackend(p execProfile, backend string) execProfile {
	p.Backend = backend
	if backend != "host" {
		applyConfinedDefaults(&p)
	}
	return p
}

// applyConfinedDefaults adds what every confining profile gets on top of the
// operator's settings: the credential/browser-store deny baseline, and the
// safe-env allowlist if the profile somehow has none (a confined command must
// not inherit every token in Tenant's own environment, red-team #13).
func applyConfinedDefaults(p *execProfile) {
	p.Policy.DenyRoots = dedupeNonEmpty(append(p.Policy.DenyRoots, credentialStoreRoots()...)...)
	if !envScoped(p.Policy.Env) {
		p.Policy.Env = envAllowlist(nil, nil)
	}
}

// safeEnv is what every exec profile passes a command whatever it adds: where
// things are, who and where the user is, how to print. Nothing in it is a
// credential. Everything else in Tenant's environment — an API key, a token, a
// proxy URL with a password in it, SSH_AUTH_SOCK — reaches a command only when
// its profile names it in env_pass. LC_* is passed too.
var safeEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "USERNAME", "SHELL", "LANG", "LANGUAGE", "TZ", "TERM", "TMPDIR", "TEMP", "TMP"}

// safeEnvWindows is Windows' own safe set: the system and profile folders and
// names that programs, cmd and PowerShell look up (without SystemRoot even
// networking fails).
var safeEnvWindows = []string{"SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT", "PSModulePath",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "ProgramData", "ALLUSERSPROFILE", "PUBLIC",
	"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432",
	"USERDOMAIN", "COMPUTERNAME", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS"}

// envAllowlist is a command's environment: the safe set plus pass, forwarded
// from Tenant's own, plus set (TEN-345; replaces the KEY/TOKEN/SECRET
// name-denylist, which leaked any secret with an innocent name).
func envAllowlist(pass []string, set map[string]string) sandbox.EnvPolicy {
	return envAllowlistFrom(runtime.GOOS, os.Environ(), pass, set)
}

// envAllowlistFrom forwards the wanted names by the names parent actually has:
// on Windows they match in any case ("Path" is PATH).
func envAllowlistFrom(goos string, parent, pass []string, set map[string]string) sandbox.EnvPolicy {
	key := func(n string) string {
		if goos == "windows" {
			return strings.ToUpper(n)
		}
		return n
	}
	want := map[string]bool{}
	for _, n := range slices.Concat(safeEnv, pass) {
		want[key(n)] = true
	}
	if goos == "windows" {
		for _, n := range safeEnvWindows {
			want[key(n)] = true
		}
	}
	var names []string
	for _, kv := range parent {
		if n, _, ok := strings.Cut(kv, "="); ok && n != "" && (want[key(n)] || strings.HasPrefix(n, "LC_")) {
			names = append(names, n)
		}
	}
	// Isolate: an empty list is still an allowlist, never inherit-all.
	return sandbox.EnvPolicy{Isolate: true, Pass: names, Set: set}
}

// credentialStoreRoots are well-known local credential stores. A confining
// backend denies them outright: reading ~/.ssh or ~/.aws is the first thing a
// prompt-injected command reaches for, and using those credentials from a
// sandboxed command should be an explicit, approved escalation.
func credentialStoreRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	rel := []string{".ssh", ".aws", ".gnupg", ".netrc", ".git-credentials", ".config/gh", ".config/gcloud",
		".azure", ".kube", ".docker", ".npmrc", ".pypirc",
		// Browser profiles: session cookies and saved logins (red-team #8).
		".mozilla", ".config/google-chrome", ".config/chromium", ".config/BraveSoftware", ".config/microsoft-edge",
		// macOS: Keychains, cookie jars and browser profiles.
		"Library/Keychains", "Library/Cookies", "Library/Safari",
		"Library/Application Support/Google/Chrome", "Library/Application Support/Firefox",
		"Library/Application Support/Microsoft Edge", "Library/Application Support/BraveSoftware"}
	out := make([]string, 0, len(rel)+3)
	for _, r := range rel {
		out = append(out, filepath.Join(home, filepath.FromSlash(r)))
	}
	if runtime.GOOS == "windows" {
		for _, env := range []string{"APPDATA", "LOCALAPPDATA"} {
			if dir := os.Getenv(env); dir != "" {
				out = append(out, filepath.Join(dir, "Microsoft", "Credentials"))
			}
		}
		if dir := os.Getenv("APPDATA"); dir != "" {
			out = append(out, filepath.Join(dir, "gcloud"), filepath.Join(dir, "Mozilla", "Firefox"))
		}
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			out = append(out,
				filepath.Join(dir, "Google", "Chrome", "User Data"),
				filepath.Join(dir, "Microsoft", "Edge", "User Data"),
				filepath.Join(dir, "BraveSoftware", "Brave-Browser", "User Data"))
		}
	}
	return out
}

// expandHome turns a leading "~" into the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func expandRoots(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, r := range in {
		out = append(out, expandHome(r))
	}
	return out
}

// runnerFunc is osys's runner shape; confirmFunc is the approval callback shape.
type (
	runnerFunc  = func(ctx context.Context, command string, timeout time.Duration) (string, error)
	confirmFunc = func(ctx context.Context, action, detail string) bool
)

// sandboxRunner is the os_exec runner for the live tool mux. It resolves the
// profile PER CALL from the turn's surface, so one shared os service gives each
// surface its own boundary and a hand edit to exec_profiles applies to the next
// command. confirm is the mux's approval callback (origin-routed); nil denies
// every escalation.
func sandboxRunner(cfgDir, dataDir string, confirm confirmFunc) runnerFunc {
	return func(ctx context.Context, command string, timeout time.Duration) (string, error) {
		surface := execSurfaceFrom(ctx)
		return profileRunner(resolveExecProfile(surface, cfgDir, dataDir, ""), surface, confirm)(ctx, command, timeout)
	}
}

// profileRunner runs os_exec commands under one fixed profile. On the host
// backend it is indistinguishable from osys's realRunner: same shell, inherited
// environment and cwd, whole output, and the same errors ("command timed out
// after <t>", or exec's own exit status). On a confining backend a refusal is
// appended to the error so the model can correct its command, and an escalated
// run says in its output that it ran outside the sandbox.
func profileRunner(p execProfile, surface string, confirm confirmFunc) runnerFunc {
	return func(ctx context.Context, command string, timeout time.Duration) (string, error) {
		be, err := profileBackend(ctx, p, surface)
		if err != nil {
			return "", fmt.Errorf("sandbox: the %s exec profile's backend %q is unavailable, so the command was not run (fail closed): %w",
				surface, p.Backend, err)
		}
		pol := p.Policy
		pol.Limits.Wall = timeout
		cmd := sandbox.Command{Shell: command, Surface: surface}
		res, err := execute(ctx, be, cmd, pol, execEscalator(confirm, pol))
		if err != nil {
			if be.Isolation() == sandbox.IsolationNone {
				return res.Output, err // host: realRunner returned the start error unwrapped
			}
			return res.Output, fmt.Errorf("sandbox: the %s backend could not start the command: %w", be.Name(), err)
		}
		out := res.Output
		if res.Escalated {
			out += "\n[" + escalationNote(res, pol.Enforce) + "]"
		}
		var msg string
		switch {
		case res.TimedOut:
			msg = fmt.Sprintf("command timed out after %s", timeout)
		case res.ExitCode != 0:
			msg = exitText(res)
		}
		if res.Refused() && !res.Escalated {
			msg = strings.TrimSpace(msg + " " + refusalNote(res))
		}
		if msg == "" {
			return out, nil
		}
		return out, errors.New(msg)
	}
}

// profileBackend is p's backend. One that is unavailable here fails an
// enforcing profile closed (the error), but a dry run never blocks a command:
// it runs on the host, and the sandbox log says so.
func profileBackend(ctx context.Context, p execProfile, surface string) (sandbox.Backend, error) {
	be, err := sandbox.For(p.Backend)
	switch {
	case err == nil:
		return be, nil
	case p.Policy.Enforce:
		sandbox.BackendUnavailable.Log(ctx, "surface", surface, "backend", p.Backend, "err", err)
		return nil, err
	}
	sandbox.DryRunOnHost.Log(ctx, "surface", surface, "backend", p.Backend, "err", err)
	return sandbox.Host(), nil
}

// execEscalator turns the approval callback into a sandbox.Escalator whose
// prompt names exactly what the boundary refused, and what "always" would
// save: the refused folder as an exception for the surface (pol is the
// profile's, for its deny roots), or nothing, and why.
func execEscalator(confirm confirmFunc, pol sandbox.Policy) sandbox.Escalator {
	if confirm == nil {
		return nil
	}
	return func(ctx context.Context, cmd sandbox.Command, refused sandbox.Result) bool {
		always := "\"always\" approves this time only: "
		scope, why := exceptionScope(refused, pol.DenyRoots)
		if scope != "" {
			ctx = context.WithValue(ctx, execExceptionKey{}, execException{surface: cmd.Surface, path: scope})
			why = "\"always\" also lets " + cmd.Surface + " commands write under " + scope + " from now on"
			always = ""
		}
		return confirm(ctx, actionExecUnsandboxed, commandText(cmd)+
			"  [SANDBOX REFUSED: "+describeViolations(refused)+" — approve to re-run it OUTSIDE the sandbox; "+always+why+"]")
	}
}

// --- exceptions (approve-always on an escalation) ---

func init() { approval.OnAlways(actionExecUnsandboxed, saveExecException) }

type execExceptionKey struct{}

// execException is what "always" on one escalation saves: a folder the
// surface's confined commands may write to.
type execException struct{ surface, path string }

// persistExecException saves an exception to the settings file and the live
// store, and reports a failed save itself. serve and the TUI install it over
// their live settings (loadExecSettings); unset, "always" approves the one time.
var persistExecException func(surface, path string) error

// saveExecException is "always" on os_exec_unsandboxed: it saves the refused
// folder the prompt named, never a backend switch or the sandbox category's
// allow. No agent tool reaches it: only an operator's decision does.
func saveExecException(ctx context.Context) {
	ex, ok := ctx.Value(execExceptionKey{}).(execException)
	if ok && persistExecException != nil && persistExecException(ex.surface, ex.path) == nil {
		sandbox.ExceptionSaved.Log(ctx, "surface", ex.surface, "path", ex.path)
	}
}

// exceptionScope is the folder an exception for a refusal would open, or ""
// and why none can be saved. Only a filesystem refusal whose output names an
// absolute path has one: that path if it is a folder, else its folder. Never
// inside a deny root (no exception reopens one), and never the home folder or
// above it, which would open every dotfile to later commands.
func exceptionScope(res sandbox.Result, deny []string) (scope, why string) {
	fs := slices.ContainsFunc(res.Violations, func(v sandbox.Violation) bool { return v.Kind == "filesystem" })
	if !fs {
		return "", "only a refused folder can be saved, and no backend limits the network to one host yet"
	}
	path := refusedPath(res.Output)
	if path == "" {
		return "", "the refusal names no absolute path to save"
	}
	scope = path
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		scope = filepath.Dir(path)
	}
	if fi, err := os.Stat(scope); err != nil || !fi.IsDir() {
		// bubblewrap can't bind a folder that isn't there: every command would fail.
		return "", scope + " does not exist yet; create it, or add it to exec_exceptions by hand"
	}
	if slices.ContainsFunc(deny, func(d string) bool { return inside(scope, d) }) {
		return "", scope + " is Tenant's own or a credential store, which no exception opens"
	}
	if home, err := os.UserHomeDir(); err == nil && inside(home, scope) {
		return "", scope + " would open your whole home folder; add a narrower write root by hand if you mean it"
	}
	return scope, ""
}

// inside reports whether p is root or inside it, both symlink-resolved, as the
// file tools compare paths.
func inside(p, root string) bool { return osys.Roots{Deny: []string{root}}.Check(p, false) != nil }

// refusedPath is the absolute path a refusal names in a command's output: on
// a line that says "operation not permitted" (Seatbelt) or "read-only file
// system" (bubblewrap), the path in quotes ("cannot touch '/a/b'", Python's
// "…: '/a/b'", Node's ", open '/a/b'"), else a ": "-separated field
// ("touch: /a/b: Operation not permitted"), else a word. "" when none is
// absolute. The command wrote the output, so the prompt shows the folder the
// operator is asked to open.
func refusedPath(output string) string {
	for _, line := range strings.Split(output, "\n") {
		low := strings.ToLower(line)
		if !strings.Contains(low, "operation not permitted") && !strings.Contains(low, "read-only file system") {
			continue
		}
		var cands []string
		for _, q := range []string{"'", `"`} {
			parts := strings.Split(line, q)
			for i := 1; i < len(parts)-1; i += 2 {
				cands = append(cands, parts[i])
			}
		}
		cands = append(cands, strings.Split(line, ": ")...)
		cands = append(cands, strings.Fields(line)...)
		for _, c := range cands {
			if c = strings.Trim(strings.TrimSpace(c), "'\"`,:;()"); filepath.IsAbs(c) {
				return filepath.Clean(c)
			}
		}
	}
	return ""
}

// runSandboxShell runs a cron shell job through the seam: cmd /C on Windows and
// sh -c elsewhere, the profile's environment allowlist, the given work dir,
// output trimmed then capped, and "command failed: <exit status>" on a
// non-zero exit (the pre-seam runCronShell's text). Cron is unattended, so a
// refusal never reaches a person: with enforce on it fails the job (named in
// the error and the sandbox log); in dry-run it re-runs on the host and logs
// what would have been refused.
func runSandboxShell(ctx context.Context, command string, p execProfile) (string, error) {
	be, err := profileBackend(ctx, p, surfaceCronShell)
	if err != nil {
		return "", fmt.Errorf("sandbox: the cron-shell exec profile's backend %q is unavailable, so the job was not run (fail closed): %w",
			p.Backend, err)
	}
	pol := p.Policy
	if !envScoped(pol.Env) {
		pol.Env = envAllowlist(nil, nil) // never the whole environment
	}
	cmd := sandbox.Command{Args: cronShellArgv(command), Surface: surfaceCronShell}
	res, err := execute(ctx, be, cmd, pol, nil) // unattended: never escalate to a person
	s := strings.TrimSpace(res.Output)
	if len(s) > cronShellOutputCap {
		s = s[:cronShellOutputCap] + "\n…[truncated]"
	}
	if err != nil {
		return s, fmt.Errorf("command failed: %v", err)
	}
	if res.Escalated {
		s += "\n[" + escalationNote(res, pol.Enforce) + "]"
	}
	if res.ExitCode != 0 || res.TimedOut {
		status := exitText(res)
		if res.Refused() && !res.Escalated {
			status += " " + refusalNote(res)
		}
		return s, fmt.Errorf("command failed: %s", status)
	}
	return s, nil
}

// cronShellArgv is the pre-seam cron shell: cmd /C on Windows (existing jobs are
// cmd syntax, NOT PowerShell) and sh -c elsewhere.
func cronShellArgv(command string) []string { return cronShellArgvFor(runtime.GOOS, command) }

func cronShellArgvFor(goos, command string) []string {
	if goos == "windows" {
		return []string{"cmd", "/C", command}
	}
	return []string{"sh", "-c", command}
}

// scrubbedEnvPolicy expresses cron's legacy scrubbedEnv() secret-name denylist as
// an EnvPolicy allowlist: it forwards exactly the parent variable NAMES that
// survive the scrub. Exec profiles use envAllowlist; this is the base of a
// local MCP server's environment (mcpstdio.go).
func scrubbedEnvPolicy() sandbox.EnvPolicy {
	kept := scrubbedEnv()
	pass := make([]string, 0, len(kept))
	for _, kv := range kept {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			pass = append(pass, kv[:i])
		}
	}
	// Isolate forces the allowlist even if pass is empty, so an empty environment
	// never silently falls back to inherit-all.
	return sandbox.EnvPolicy{Isolate: true, Pass: pass}
}

// envScoped reports whether an EnvPolicy restricts the environment (sandbox's
// own inherit-all condition, inverted).
func envScoped(e sandbox.EnvPolicy) bool {
	return e.Isolate || len(e.Pass) > 0 || len(e.Set) > 0
}

// --- reporting ---

// execute runs cmd through the seam as a sandbox Run activity (logging v2
// §8.4), on every backend, the host included: its start, with the command,
// scrubbed, and the limits; its exit (sandbox 1001), with the pid, the exit
// code and the tail of the output; what the boundary did (logSandboxRun);
// then its stop, or its fail when the backend could not start it. The run is
// related to the turn or job in ctx, and the child's environment gets the
// trace and the run's activity.
func execute(ctx context.Context, be sandbox.Backend, cmd sandbox.Command, pol sandbox.Policy, esc sandbox.Escalator) (res sandbox.Result, err error) {
	ctx, act := event.Start(ctx, sandbox.Run, "surface", cmd.Surface, "backend", be.Name(),
		"command", clipLog(commandText(cmd)), "limits", pol.Limits.String())
	defer act.End(&err)
	pol.Env.Add = capture.New(ctx, "sandbox").Env()
	res, err = sandbox.Execute(ctx, be, cmd, pol, esc)
	if err == nil {
		kv := []any{"surface", cmd.Surface, "backend", be.Name(), "pid", res.PID, "exit", res.ExitCode, "status", exitText(res), "timed_out", res.TimedOut}
		if len(res.Violations) > 0 {
			kv = append(kv, "violations", describeViolations(res))
		}
		if tail := outputTail(res.Output); tail != "" {
			kv = append(kv, "tail", tail)
		}
		sandbox.Exited.Log(ctx, kv...)
	}
	logSandboxRun(ctx, cmd.Surface, be.Name(), pol.Enforce, cmd, res)
	auditExec(ctx, be.Name(), cmd, pol.Enforce, res, err) // the Security log's record (securitylog.go)
	return res, err
}

// outputTail is the end of a run's output for its exit event, scrubbed
// before it is cut so no secret is left half-cut (TEN-361). Run merges
// stdout and stderr on one pipe, so it is the tail of both, as the model sees
// them.
func outputTail(s string) string {
	const n = 512
	if len(s) > n+8<<10 {
		s = s[len(s)-(n+8<<10):] // the scrubber sees whole any secret shorter than 8 KiB that crosses the cut
	}
	s = strings.TrimSpace(logs.Scrub(s))
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}

// logSandboxRun records what the boundary did to a run, one event per
// outcome: refused, escalated, or refused in a dry run. violations names what
// it refused, as the backend attributes it.
func logSandboxRun(ctx context.Context, surface, backend string, enforce bool, cmd sandbox.Command, res sandbox.Result) {
	if !res.Escalated && !res.Refused() {
		return
	}
	kv := []any{"surface", surface, "backend", backend, "command", clipLog(commandText(cmd)), "exit", res.ExitCode,
		"violations", describeViolations(res)}
	switch {
	case res.Escalated && !enforce:
		sandbox.WouldRefuse.Log(ctx, kv...)
	case res.Escalated:
		sandbox.Escalated.Log(ctx, kv...)
	default:
		sandbox.Refused.Log(ctx, kv...)
	}
}

func commandText(cmd sandbox.Command) string {
	if cmd.Shell != "" {
		return cmd.Shell
	}
	return strings.Join(cmd.Args, " ")
}

func describeViolations(res sandbox.Result) string {
	parts := make([]string, 0, len(res.Violations))
	for _, v := range res.Violations {
		parts = append(parts, v.Kind+": "+v.Detail)
	}
	return strings.Join(parts, "; ")
}

func refusalNote(res sandbox.Result) string {
	return "(sandbox refused: " + describeViolations(res) + ")"
}

func escalationNote(res sandbox.Result, enforce bool) string {
	if !enforce {
		return "sandbox dry-run: this would have been refused (" + describeViolations(res) + "); it ran on the host"
	}
	return "ran OUTSIDE the sandbox after approval; the sandbox had refused: " + describeViolations(res)
}

func exitText(res sandbox.Result) string {
	if res.ExitStatus != "" {
		return res.ExitStatus
	}
	return fmt.Sprintf("exit status %d", res.ExitCode)
}

func clipLog(s string) string {
	s = logs.Scrub(s) // before the cut, so no secret is left half-cut (TEN-361)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// execProfilesEqual compares two override sets.
func execProfilesEqual(a, b map[string]execProfileSetting) bool { return stdreflect.DeepEqual(a, b) }

// execExceptionsEqual compares a file's valid exceptions with the live set,
// which holds them ~-expanded.
func execExceptionsEqual(file, live map[string][]string) bool {
	if len(file) != len(live) {
		return false
	}
	for s, ps := range file {
		if !slices.Equal(expandRoots(ps), live[s]) {
			return false
		}
	}
	return true
}

// loadExecSettings is serve's and the TUI's start: it loads stg's exec
// profiles and exceptions, installs the save for an approved exception
// (through stg under mu, like every other save of the file), and tells the
// operator what commands run under, each surface in a line.
func loadExecSettings(stg *settings, mu *sync.Mutex, cfgDir, agentID string, notify func(string)) {
	n, skipped := defaultExecProfiles.Load(stg.ExecProfiles)
	ne, skippedEx := defaultExecProfiles.LoadExceptions(stg.ExecExceptions)
	if skipped = append(skipped, skippedEx...); n > 0 || ne > 0 || len(skipped) > 0 {
		msg := fmt.Sprintf("sandbox: %d exec profile(s), %d exception(s) active", n, ne)
		if len(skipped) > 0 {
			msg += fmt.Sprintf(", %d invalid skipped: %s", len(skipped), strings.Join(skipped, "; "))
		}
		notify(msg)
	}
	for _, ln := range execProfileLines(probeExecSurfaces(context.Background(), cfgDir, "")) {
		notify("sandbox: " + ln)
	}
	persistExecException = func(surface, path string) error {
		mu.Lock()
		defer mu.Unlock()
		prev := stg.ExecExceptions[surface]
		if slices.Contains(prev, path) {
			return nil
		}
		if stg.ExecExceptions == nil {
			stg.ExecExceptions = map[string][]string{}
		}
		stg.ExecExceptions[surface] = append(slices.Clone(prev), path)
		if err := stg.Save(cfgDir, agentID); err != nil {
			stg.ExecExceptions[surface] = prev
			sysNotSaved.Log(context.Background(), "what", "the sandbox exception for "+path, "path", settingsPath(cfgDir, agentID), "err", err)
			notify(errNotSaved("sandbox: the exception for "+path, err).Error())
			return err
		}
		defaultExecProfiles.LoadExceptions(stg.ExecExceptions)
		notify("sandbox: exception saved: " + surface + " commands may now write under " + path + " (exec_exceptions)")
		return nil
	}
}

// loadExecProfilesFromSettings loads the operator overrides and exceptions for
// commands that do not run the settings watcher (one-shot CLIs such as
// `tenant os`). It returns how many overrides applied and every skipped entry.
func loadExecProfilesFromSettings(cfgDir, agentID string) (int, []string, error) {
	stg, err := loadSettings(cfgDir, agentID)
	if err != nil {
		return 0, nil, err
	}
	n, skipped := defaultExecProfiles.Load(stg.ExecProfiles)
	_, skippedEx := defaultExecProfiles.LoadExceptions(stg.ExecExceptions)
	return n, append(skipped, skippedEx...), nil
}

// exePath is the running executable, or "" when it can't be resolved. Always in
// the sandbox deny set so a confined command can't read or tamper with Tenant's
// own binary.
func exePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// exeDir is the directory of the running executable, or "" when it can't be
// resolved. The cron exec approver refuses writes into it (planting a library
// beside the binary is a hijack vector), which is narrower than denying reads.
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// dedupeNonEmpty returns the distinct, non-empty inputs in order.
func dedupeNonEmpty(vals ...string) []string {
	seen := make(map[string]bool, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
