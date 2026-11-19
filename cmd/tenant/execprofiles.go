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
// safe-env allowlist (envAllowlist). Only macOS has a restricted backend; on
// Linux and Windows, or where it can't run, the built-in backend is the host,
// and probe, audit, doctor and the startup feed say why (builtinExec). Overrides live in settings.<agent>.json under "exec_profiles"
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
	"io"
	"os"
	"os/exec"
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
	// backend: this host has no restricted backend, or it can't run here.
	Fallback string
}

// execProfileStore is the process-wide, live set of operator overrides and
// exceptions: loaded at startup, re-loaded by the settings watcher. No agent
// tool writes it.
type execProfileStore struct {
	mu sync.RWMutex
	m  map[string]execProfileSetting
	ex map[string][]string // exec_exceptions: surface → folders, ~ expanded
	// The install's dirs, for the load-time check of a container profile's
	// grants against what a container may never mount (containerGrantError).
	cfgDir, dataDir string
}

// SetDirs tells the store this install's config and data dirs. Whoever loads
// exec profiles calls it first, so a container profile whose read_roots or
// write_roots overlap them is refused when it loads.
func (s *execProfileStore) SetDirs(cfgDir, dataDir string) {
	s.mu.Lock()
	s.cfgDir, s.dataDir = cfgDir, dataDir
	s.mu.Unlock()
}

func (s *execProfileStore) dirs() (cfgDir, dataDir string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfgDir, s.dataDir
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
		if err := containerGrantError(surface, p); err != nil {
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
	case "", "host", "restricted", "container":
	default:
		return fmt.Errorf("exec_profiles.%s: unknown backend %q (want host, restricted or container)", surface, p.Backend)
	}
	if p.Backend == "container" && p.Enforce != nil && !*p.Enforce {
		// W1-1 (TEN-455): a dry run re-runs a refused command on the host, and a
		// container cannot attribute every refusal (the TEN-436 lesson), so the
		// container backend has no dry run. Refused at load, not silently enforced.
		return fmt.Errorf("exec_profiles.%s: the container backend has no dry run (\"enforce\": false); leave enforce unset or true", surface)
	}
	n, ok := sandbox.ParseNetworkMode(p.Network)
	if !ok {
		return fmt.Errorf("exec_profiles.%s: unknown network %q (want inherit, none, full)", surface, p.Network)
	}
	if n == sandbox.NetAllowlist || len(p.Allow) > 0 {
		return fmt.Errorf("exec_profiles.%s: a network allowlist is not enforced yet (the egress proxy is W3-5/Phase 4); use \"none\" or \"full\"", surface)
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

// containerGrantError refuses a container profile whose read_roots or
// write_roots overlap a folder a container may never mount: Tenant's config
// and data dirs, its binary, the credential stores, or the entry's own
// deny_roots (W1-3, TEN-457). A container sees exactly its mounts, so it has
// no way to mount a folder and hide part of it: a grant that contains a
// protected folder would expose it, and one inside it is it. The run would be
// refused for the same reason (specFor); here the operator hears it when the
// settings load, not from the first command a surface runs. Only an entry
// that names the container backend is checked: on the other backends a deny
// root masks what a grant would show.
func containerGrantError(surface string, p execProfileSetting) error {
	if p.Backend != "container" {
		return nil
	}
	cfgDir, dataDir := defaultExecProfiles.dirs()
	deny := append(tenantDenyRoots(cfgDir, dataDir), credentialStoreRoots()...)
	deny = append(deny, expandRoots(p.DenyRoots)...)
	for _, group := range []struct {
		key   string
		roots []string
	}{{"read_roots", p.ReadRoots}, {"write_roots", p.WriteRoots}} {
		if grant, denied, bad := sandbox.GrantOverlapsDeny(expandRoots(group.roots), deny); bad {
			return fmt.Errorf("exec_profiles.%s: %s entry %s overlaps %s, which a container may never mount "+
				"(it cannot mount a folder and hide part of it); name folders that neither contain it nor sit inside it",
				surface, group.key, grant, denied)
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
	if _, configured := defaultExecProfiles.lookup(surface); !configured && surface != surfaceCronExec && surface != surfaceCronShell {
		// Interactive surfaces run on the host behind their approvals, like
		// Claude Code without /sandbox: a dry run re-ran a refused command on
		// the host, so installers ran twice (TEN-489). Any exec_profiles entry
		// for the surface (or "default") opts back into the sandbox; cron keeps
		// the confined default. The host here is a choice, not a fallback, so
		// there is no reason to report.
		backend, why = "host", ""
	}
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
	p.Policy.WriteRoots = append(expandRoots(o.WriteRoots), liveExceptions(surface)...)
	p.Policy.DenyRoots = dedupeNonEmpty(append(p.Policy.DenyRoots, expandRoots(o.DenyRoots)...)...)
	pass := o.EnvPass
	if p.Backend == "host" {
		// On the host ~/.ssh is readable anyway; the agent socket lets git and
		// ssh use the operator's keys without them leaving the agent.
		pass = append([]string{"SSH_AUTH_SOCK"}, pass...)
	}
	p.Policy.Env = envAllowlist(pass, o.EnvSet)
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
	return chooseBuiltinExec(sandbox.Restricted)
})

// chooseBuiltinExec picks the restricted backend where this OS has one that
// runs a test command (macOS Seatbelt), else the host with why: Linux and
// Windows have no restricted backend (the container backend is planned,
// docs/plans/docker-sandbox-plan.md).
func chooseBuiltinExec(restricted func() (sandbox.Backend, error)) (backend, why string) {
	be, err := restricted()
	if err != nil {
		return "host", err.Error()
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

// osExecConfig is the os service's config: os_exec's commands run through the
// sandbox runner, or on the host through the starter, which lets one outlive
// its tool call (TEN-490); their logs live under <data>/proc.
func osExecConfig(cfgDir, dataDir string, confirm confirmFunc) osys.Config {
	return osys.Config{Runner: sandboxRunner(cfgDir, dataDir, confirm), Starter: sandboxStarter(cfgDir, dataDir),
		ProcDir: filepath.Join(dataDir, "proc")}
}

// sandboxStarter is os_exec's background seam (TEN-490): a command the
// surface's profile runs on the host is started under that profile's policy
// and work dir and watched by osys (process.go), so it can go on past its
// timeout or the turn. A confining backend reaps what it started, so there
// the answer is ErrNoBackground and the runner's kill-at-timeout stands.
func sandboxStarter(cfgDir, dataDir string) func(ctx context.Context, command string, log io.Writer) (osys.Proc, error) {
	return profileStarter(func(ctx context.Context) (execProfile, string, error) {
		surface := execSurfaceFrom(ctx)
		return resolveExecProfile(surface, cfgDir, dataDir, ""), surface, nil
	})
}

// profileStarter is the starter for whatever profile and surface resolve
// names for the call in ctx.
func profileStarter(resolve func(ctx context.Context) (execProfile, string, error)) func(ctx context.Context, command string, log io.Writer) (osys.Proc, error) {
	return func(ctx context.Context, command string, log io.Writer) (osys.Proc, error) {
		p, surface, err := resolve(ctx)
		if err != nil {
			return nil, err
		}
		be, err := profileBackend(ctx, p, surface)
		if err != nil || be.Isolation() != sandbox.IsolationNone {
			return nil, osys.ErrNoBackground
		}
		pol := p.Policy
		pol.Stderr = log
		if pol.WorkDir = osys.WorkDir(ctx); pol.WorkDir == "" {
			pol.WorkDir, _ = os.UserHomeDir()
		}
		return startExec(ctx, be, sandbox.Command{Shell: command, Surface: surface}, pol)
	}
}

// startExec is execute for a command that runs on: the same Run activity,
// exit event, sandbox line and Security record, with the exit filed by a
// goroutine when the process ends rather than before this returns.
func startExec(ctx context.Context, be sandbox.Backend, cmd sandbox.Command, pol sandbox.Policy) (osys.Proc, error) {
	ctx, act := event.Start(ctx, sandbox.Run, "surface", cmd.Surface, "backend", be.Name(),
		"command", clipLog(commandText(cmd)), "background", true)
	pol.Env.Add = capture.New(ctx, "sandbox").Env()
	p, err := sandbox.Start(ctx, be, sandbox.ShellArgv(cmd.Shell), pol)
	if err != nil {
		act.End(&err)
		auditExec(ctx, be.Name(), cmd, pol.Enforce, sandbox.Result{}, err)
		return nil, err
	}
	// The exit is filed inside Wait, which osys's pump calls once the output
	// is drained: so for a foreground command every event is in the log before
	// the tool result returns, in the runner's order, and a background one is
	// filed when it ends.
	return &loggedProc{Process: p, log: func(werr error) {
		res := sandbox.Result{PID: p.Pid()}
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			res.ExitCode, res.ExitStatus = ee.ExitCode(), ee.Error()
		} else if werr != nil {
			res.ExitCode, res.ExitStatus = -1, werr.Error()
		}
		sandbox.Exited.Log(ctx, "surface", cmd.Surface, "backend", be.Name(), "pid", res.PID, "exit", res.ExitCode, "status", exitText(res), "timed_out", false)
		logSandboxRun(ctx, cmd.Surface, be.Name(), pol.Enforce, cmd, res)
		auditExec(ctx, be.Name(), cmd, pol.Enforce, res, nil)
		var none error
		act.End(&none)
	}}, nil
}

// loggedProc files a started command's exit once, from Wait.
type loggedProc struct {
	*sandbox.Process
	once sync.Once
	log  func(err error)
}

func (p *loggedProc) Wait() error {
	err := p.Process.Wait()
	p.once.Do(func() { p.log(err) })
	return err
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

// envSecretNote is what a surface's report says when its env_pass forwards a
// name that looks like a credential.
const envSecretNote = "a sandboxed command can read every variable it is passed, and with the network on can send it anywhere; pass a credential only to a surface whose commands you would trust with it"

// secretLookingEnv returns the names in pass that look like they hold a
// credential (W2-5): a hint for the operator, never a filter. The allowlist
// is the control (envAllowlist); a secret under an innocent name is not
// caught here, and a name caught here is still passed.
func secretLookingEnv(pass []string) []string {
	var out []string
	for _, n := range pass {
		up := strings.ToUpper(n)
		looks := false
		for _, part := range []string{"TOKEN", "SECRET", "PASSW", "CREDENTIAL", "APIKEY", "PRIVATE"} {
			looks = looks || strings.Contains(up, part)
		}
		for _, word := range strings.FieldsFunc(up, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
			switch word {
			case "KEY", "KEYS", "AUTH", "PAT", "PASS", "COOKIE", "CREDS", "DSN":
				looks = true
			}
		}
		if looks && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// envSecretLines is one line per group of surfaces whose env_pass forwards
// the same credential-looking names.
func envSecretLines(ps []surfaceProbe) []string {
	var order []string
	groups := map[string][]string{}
	for _, p := range ps {
		if len(p.EnvSecretLooking) == 0 {
			continue
		}
		names := strings.Join(p.EnvSecretLooking, ", ")
		if _, ok := groups[names]; !ok {
			order = append(order, names)
		}
		groups[names] = append(groups[names], p.Surface)
	}
	out := make([]string, 0, len(order))
	for _, names := range order {
		out = append(out, strings.Join(groups[names], ", ")+": env_pass forwards "+names+", named like a credential ("+envSecretNote+")")
	}
	return out
}

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
	return credentialStoresIn(home, os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"))
}

// credentialStoresIn lists the credential stores of the account whose home
// folder is home. roaming and local are its two AppData folders on Windows;
// an empty one adds nothing.
func credentialStoresIn(home, roaming, local string) []string {
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
		for _, dir := range []string{roaming, local} {
			if dir != "" {
				out = append(out, filepath.Join(dir, "Microsoft", "Credentials"))
			}
		}
		if dir := roaming; dir != "" {
			out = append(out, filepath.Join(dir, "gcloud"), filepath.Join(dir, "Mozilla", "Firefox"))
		}
		if dir := local; dir != "" {
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
// backend it matches osys's realRunner: same shell and cwd, whole output, and
// the same errors ("command timed out after <t>", or exec's own exit status);
// the environment is the profile's allowlist. On a confining backend a refusal
// is appended to the error so the model can correct its command, and an
// escalated run says in its output that it ran outside the sandbox.
func profileRunner(p execProfile, surface string, confirm confirmFunc) runnerFunc {
	return func(ctx context.Context, command string, timeout time.Duration) (string, error) {
		be, err := profileBackend(ctx, p, surface)
		if err != nil {
			return "", fmt.Errorf("sandbox: the %s exec profile's backend %q is unavailable, so the command was not run (fail closed): %w",
				surface, p.Backend, err)
		}
		pol := p.Policy
		pol.Limits.Wall = timeout
		// os_exec's workdir (default: home) is the cwd on the host. A confining
		// backend's WorkDir is also a writable root, so there a model-chosen
		// folder is refused rather than quietly opened for writing.
		if be.Isolation() == sandbox.IsolationNone {
			if pol.WorkDir = osys.WorkDir(ctx); pol.WorkDir == "" {
				pol.WorkDir, _ = os.UserHomeDir()
			}
		} else if osys.WorkDir(ctx) != "" {
			return "", fmt.Errorf("workdir is only used when commands run on the host; the %s surface runs them in the %s sandbox, so cd inside the command instead", surface, be.Name())
		}
		cmd := sandbox.Command{Shell: command, Surface: surface}
		res, err := execute(ctx, be, cmd, pol, execEscalator(confirm, pol, be))
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
	if err == nil {
		// A backend that is known but not ready here (the container backend when
		// Docker is down or the image is unbuilt) is unavailable too, so it fails
		// closed / falls back the same way an unknown backend does (W1-1).
		if a, ok := be.(sandbox.Available); ok {
			err = a.Available(ctx)
		}
	}
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
// prompt names exactly what the boundary refused, where an approval would
// re-run the command (W1-6: inside the sandbox with one grant, or on the host
// for a program refusal only — sandbox.RiskSummary writes that line, never the
// model), and what "always" would save: the refused folder as an exception for
// the surface (pol is the profile's, for its deny roots), or nothing, and why.
// The command is shown whole, with control characters escaped, so nothing
// hides behind a carriage return or a terminal escape.
//
// The yes this returns covers one exact run: sandbox.Execute takes the
// backend's digest of the granted run (interpreter, command, mounts, network,
// image) before it calls here and again before the run, and runs nothing if
// they differ (TEN-460, sandbox.EscalationBinder). The approval broker carries
// no ticket: the binding is held where the run starts.
func execEscalator(confirm confirmFunc, pol sandbox.Policy, be sandbox.Backend) sandbox.Escalator {
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
		return confirm(ctx, actionExecUnsandboxed, sandbox.EscapeControl(commandText(cmd))+
			"  [SANDBOX REFUSED: "+describeViolations(refused)+" — "+sandbox.RiskSummary(be, cmd, refused)+"; "+always+why+"]")
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
// absolute path has one: that path if it is a folder, else its folder,
// symlinks resolved (what is saved is the real folder, so a link swapped in
// later can't redirect it: liveExceptions). The command wrote that output, so
// the folder is never inside a deny root (no exception reopens one), never a
// system folder or a root (systemScope), and never the home folder or above
// it, which would open every dotfile to later commands.
func exceptionScope(res sandbox.Result, deny []string) (scope, why string) {
	kind := func(k string) func(sandbox.Violation) bool {
		return func(v sandbox.Violation) bool { return v.Kind == k }
	}
	if !slices.ContainsFunc(res.Violations, kind("filesystem")) {
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
		// A sandbox can't grant a folder that isn't there.
		return "", scope + " does not exist yet; create it, or add it to exec_exceptions by hand"
	}
	if r, err := filepath.EvalSymlinks(scope); err == nil {
		scope = r
	}
	if slices.ContainsFunc(deny, func(d string) bool { return inside(scope, d) }) {
		return "", scope + " is Tenant's own or a credential store, which no exception opens"
	}
	if systemScope(runtime.GOOS, scope, resolvedPath) {
		return "", scope + " is a system folder, a drive or volume root, or above one, which no exception opens"
	}
	if home, err := os.UserHomeDir(); err == nil && inside(home, scope) {
		return "", scope + " would open your whole home folder; add a narrower write root by hand if you mean it"
	}
	return scope, ""
}

// systemDirs are the top-level system folders an exception may never be, nor
// be above; a folder under one (/opt/app/data, /var/lib/app) is fine.
var systemDirs = map[string][]string{
	"posix": {"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/opt", "/var", "/tmp",
		"/private", "/private/etc", "/private/var", "/private/tmp", "/System", "/Library", "/Applications",
		"/Users", "/home", "/root", "/dev", "/proc", "/sys", "/run", "/boot", "/srv", "/mnt", "/Volumes"},
	"windows": {`C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`, `C:\ProgramData`, `C:\Users`},
}

// systemScope reports whether dir is a filesystem or drive root, a volume's
// mount (/mnt/<x>, /Volumes/<x>), or a system folder (systemDirs, and on
// Windows where its environment puts them) or above one. Names compare after
// resolve (symlinks: /etc is /private/etc on macOS) and case-folded where the
// filesystem ignores case (Windows, macOS). goos and resolve are parameters so
// every OS's rules are tested on any.
func systemScope(goos, dir string, resolve func(string) string) bool {
	sep, dirs := "/", systemDirs["posix"]
	if goos == "windows" {
		sep, dirs = `\`, slices.Clone(systemDirs["windows"])
		for _, env := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
			if v := os.Getenv(env); v != "" {
				dirs = append(dirs, v)
			}
		}
		if pub := os.Getenv("PUBLIC"); strings.LastIndex(pub, `\`) > 0 {
			dirs = append(dirs, pub[:strings.LastIndex(pub, `\`)]) // C:\Users\Public → C:\Users
		}
	}
	norm := func(p string) string {
		p = strings.TrimRight(resolve(p), sep)
		if goos == "windows" || goos == "darwin" {
			p = strings.ToLower(p)
		}
		return p
	}
	d := norm(dir) // "" for "/"; "c:" for C:\
	if d == "" || (goos == "windows" && len(d) == 2 && d[1] == ':') {
		return true // a filesystem or drive root
	}
	if i := strings.LastIndex(d, "/"); goos != "windows" && i > 0 && (strings.EqualFold(d[:i], "/mnt") || strings.EqualFold(d[:i], "/volumes")) {
		return true // a volume's mount: /mnt/c, /Volumes/Data
	}
	if goos == "darwin" && (d == "/system/volumes/data" || strings.HasPrefix(d, "/system/volumes/data/")) {
		return true // the data volume's own path to every firmlinked folder: /Users, /Applications, /private, …
	}
	for _, s := range dirs {
		s = norm(s)
		if s != "" && (d == s || strings.HasPrefix(s, d+sep)) { // the folder, or above it
			return true
		}
	}
	return false
}

// liveExceptions is surface's exceptions that are still the folders they were
// saved as (their real paths, exceptionScope). One that is gone, is no longer
// a folder, or now resolves elsewhere (a symlink swapped in, perhaps to the
// home folder) is left out, and said once in the sandbox log: a write root
// must never follow a link planted after the approval.
func liveExceptions(surface string) []string {
	var out []string
	for _, p := range defaultExecProfiles.exceptions(surface) {
		if why := exceptionDrift(runtime.GOOS, p, filepath.EvalSymlinks, os.Stat); why != "" {
			if _, said := skippedExceptions.LoadOrStore(surface+"\x00"+p+"\x00"+why, true); !said {
				sandbox.ExceptionSkipped.Log(context.Background(), "surface", surface, "path", p, "why", why)
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

// skippedExceptions holds the drifted exceptions already reported.
var skippedExceptions sync.Map

// exceptionDrift says why exception p is not the folder it was saved as, or ""
// when it still is. Case-folded where the filesystem ignores case.
func exceptionDrift(goos, p string, eval func(string) (string, error), stat func(string) (os.FileInfo, error)) string {
	r, err := eval(p)
	if err != nil {
		return "it no longer exists"
	}
	same := r == p
	if goos == "windows" || goos == "darwin" {
		same = strings.EqualFold(r, p)
	}
	if !same {
		return "it now resolves to " + r + "; save that real path instead if it is meant"
	}
	if fi, err := stat(r); err != nil || !fi.IsDir() {
		return "it is no longer a folder"
	}
	return ""
}

// resolvedPath is p with symlinks resolved, or cleaned when it can't be.
func resolvedPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// inside reports whether p is root or inside it, both symlink-resolved, as the
// file tools compare paths.
func inside(p, root string) bool { return osys.Roots{Deny: []string{root}}.Check(p, false) != nil }

// refusedPath is the absolute path a refusal names in a command's output: on
// a line that says "operation not permitted" (Seatbelt) or "read-only file
// system" (a read-only mount), the path in quotes ("cannot touch '/a/b'", Python's
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
			if c = strings.Trim(strings.TrimSpace(c), "'\"`,:;()"); strings.HasPrefix(c, "/") || filepath.IsAbs(c) {
				return filepath.Clean(c)
			}
		}
	}
	return ""
}

// runSandboxShell runs a cron shell job through the seam: cmd /C on the
// Windows host and sh -c everywhere else (cronShellArgv), the profile's
// environment allowlist, the given work dir, output trimmed then capped, and
// "command failed: <exit status>" on a non-zero exit (the pre-seam
// runCronShell's text). Cron is unattended, so a refusal never reaches a
// person: with enforce on it fails the job (named in the error and the sandbox
// log); in dry-run it re-runs outside the sandbox and logs what would have
// been refused.
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

// cronShellArgv is the shell a cron job runs in: cmd /C on Windows (cron shell
// jobs are cmd syntax, not PowerShell), sh -c everywhere else.
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
		return "sandbox dry-run: this would have been refused (" + describeViolations(res) + "); it ran outside the sandbox"
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
// operator what commands run under, a line per group of surfaces, and on
// Windows which cron shell jobs now run as Linux sh.
func loadExecSettings(c *commonFlags, stg *settings, mu *sync.Mutex, notify func(string)) {
	cfgDir, agentID := c.cfgDir, c.agent
	defaultExecProfiles.SetDirs(c.cfgDir, c.dataDir) // before Load: it checks container grants against them
	n, skipped := defaultExecProfiles.Load(stg.ExecProfiles)
	ne, skippedEx := defaultExecProfiles.LoadExceptions(stg.ExecExceptions)
	if skipped = append(skipped, skippedEx...); n > 0 || ne > 0 || len(skipped) > 0 {
		msg := fmt.Sprintf("sandbox: %d exec profile(s), %d exception(s) active", n, ne)
		if len(skipped) > 0 {
			msg += fmt.Sprintf(", %d invalid skipped: %s", len(skipped), strings.Join(skipped, "; "))
		}
		notify(msg)
	}
	probes := probeExecSurfaces(context.Background(), cfgDir, "")
	for _, ln := range execProfileLines(probes) {
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
func loadExecProfilesFromSettings(cfgDir, dataDir, agentID string) (int, []string, error) {
	stg, err := loadSettings(cfgDir, agentID)
	if err != nil {
		return 0, nil, err
	}
	defaultExecProfiles.SetDirs(cfgDir, dataDir) // before Load: it checks container grants against them
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
