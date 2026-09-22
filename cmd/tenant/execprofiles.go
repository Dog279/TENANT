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
// Profiles are OPERATOR-controlled. Built-in default: the "host" backend on every
// surface, byte-for-byte the pre-seam behaviour. Overrides live in
// settings.<agent>.json under "exec_profiles" (keyed by surface, or "default"),
// load at startup and are re-read when the file is edited, so a surface moves
// onto the restricted backend with no restart. There is deliberately NO agent
// tool that reads or writes a profile: the model must never widen its own
// boundary.
//
//	"exec_profiles": {
//	  "default":    {"backend": "restricted"},
//	  "cron-shell": {"backend": "restricted", "network": "none"},
//	  "discord":    {"backend": "restricted", "enforce": false}
//	}
//
// Refusals. When a confining backend refuses a filesystem or network access,
// sandbox.Execute applies the profile's Enforce flag. Enforce (the default) asks
// the turn's approver for "os_exec_unsandboxed" — a person on an attended
// surface, while the cron exec approver denies it — and re-runs on the host only
// if approved. Enforce false is dry-run: re-run on the host and log the refusal.
// Every confined run and every refusal is written to the "sandbox" log source.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdreflect "reflect" // aliased: package main already declares reflect (research.go)
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/logs"
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
// maps it to the destructive category explicitly: always-ask on the host broker,
// denied by the cron exec approver, and never covered by a blanket exec grant.
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

// inheritExecSurface carries the exec surface stamped on from onto to. A team
// runtime with a BaseCtx starts sub-agents on that long-lived context instead of
// the spawning turn's, which would otherwise drop the stamp and run a sub-agent's
// os_exec under the local profile: a way around a stricter per-surface boundary.
func inheritExecSurface(to, from context.Context) context.Context {
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
}

// execProfileSetting is one operator override under "exec_profiles", keyed by a
// surface or "default" (every surface without its own entry). Unset fields keep
// the built-in default. deny_roots ADD to the built-in deny set; nothing can
// remove Tenant's own dirs from it.
type execProfileSetting struct {
	Backend    string            `json:"backend,omitempty"`     // host | restricted
	Enforce    *bool             `json:"enforce,omitempty"`     // default true; false = dry-run
	Network    string            `json:"network,omitempty"`     // inherit | none | allowlist | full
	Allow      []string          `json:"allow,omitempty"`       // hosts/CIDRs for network=allowlist
	ReadRoots  []string          `json:"read_roots,omitempty"`  // empty = whole host readable (deny roots still mask)
	WriteRoots []string          `json:"write_roots,omitempty"` // writable, in addition to the work dir
	DenyRoots  []string          `json:"deny_roots,omitempty"`  // added to the built-in deny set
	EnvPass    []string          `json:"env_pass,omitempty"`    // env allowlist: variable names to forward
	EnvSet     map[string]string `json:"env_set,omitempty"`     // env: fixed values
	CPUSeconds int               `json:"cpu_seconds,omitempty"`
	MemoryMB   int               `json:"memory_mb,omitempty"`
	MaxPIDs    int               `json:"max_pids,omitempty"`
}

// execProfileStore is the process-wide, live set of operator overrides: loaded
// at startup, re-loaded by the settings watcher. No agent tool writes it.
type execProfileStore struct {
	mu sync.RWMutex
	m  map[string]execProfileSetting
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
		return fmt.Errorf("exec_profiles.%s: the container backend is not implemented yet (TEN-318)", surface)
	default:
		return fmt.Errorf("exec_profiles.%s: unknown backend %q (want host or restricted)", surface, p.Backend)
	}
	if _, ok := sandbox.ParseNetworkMode(p.Network); !ok {
		return fmt.Errorf("exec_profiles.%s: unknown network %q (want inherit, none, allowlist, full)", surface, p.Network)
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
// exec_profiles overrides.
//
// The built-in default is the "host" backend with Enforce on, so a routed spawn
// behaves exactly as before. DenyRoots ALWAYS include the config dir, data dir
// and the running executable's directory. A confining backend additionally
// denies the well-known credential stores (credentialStoreRoots).
func resolveExecProfile(surface, cfgDir, dataDir, workDir string) execProfile {
	p := execProfile{
		Backend: "host",
		Policy: sandbox.Policy{
			WorkDir:   workDir,
			DenyRoots: dedupeNonEmpty(cfgDir, dataDir, exeDir()),
			Enforce:   true,
		},
	}
	o, ok := defaultExecProfiles.lookup(surface)
	if !ok {
		return p
	}
	if o.Backend != "" {
		p.Backend = o.Backend
	}
	if o.Enforce != nil {
		p.Policy.Enforce = *o.Enforce
	}
	if n, ok := sandbox.ParseNetworkMode(o.Network); ok {
		p.Policy.Network = n
	}
	p.Policy.Allow = o.Allow
	p.Policy.ReadRoots = expandRoots(o.ReadRoots)
	p.Policy.WriteRoots = expandRoots(o.WriteRoots)
	deny := append(p.Policy.DenyRoots, expandRoots(o.DenyRoots)...)
	if p.Backend != "host" {
		deny = append(deny, credentialStoreRoots()...)
	}
	p.Policy.DenyRoots = dedupeNonEmpty(deny...)
	if len(o.EnvPass) > 0 || len(o.EnvSet) > 0 {
		p.Policy.Env = sandbox.EnvPolicy{Isolate: true, Pass: o.EnvPass, Set: o.EnvSet}
	}
	p.Policy.Limits.CPU = time.Duration(o.CPUSeconds) * time.Second
	p.Policy.Limits.MemoryBytes = int64(o.MemoryMB) << 20
	p.Policy.Limits.MaxPIDs = o.MaxPIDs
	return p
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
		".azure", ".kube", ".docker", ".npmrc", ".pypirc", "Library/Keychains"}
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
			out = append(out, filepath.Join(dir, "gcloud"))
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
		be, err := sandbox.For(p.Backend)
		if err != nil {
			return "", fmt.Errorf("sandbox: the %s exec profile's backend %q is unavailable, so the command was not run (fail closed): %w",
				surface, p.Backend, err)
		}
		pol := p.Policy
		pol.Limits.Wall = timeout
		cmd := sandbox.Command{Shell: command, Surface: surface}
		res, err := sandbox.Execute(ctx, be, cmd, pol, execEscalator(confirm))
		logSandboxRun(surface, be.Name(), pol.Enforce, cmd, res, err)
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

// execEscalator turns the approval callback into a sandbox.Escalator whose
// prompt names exactly what the boundary refused.
func execEscalator(confirm confirmFunc) sandbox.Escalator {
	if confirm == nil {
		return nil
	}
	return func(ctx context.Context, cmd sandbox.Command, refused sandbox.Result) bool {
		return confirm(ctx, actionExecUnsandboxed, commandText(cmd)+
			"  [SANDBOX REFUSED: "+describeViolations(refused)+" — approve to re-run it OUTSIDE the sandbox]")
	}
}

// runSandboxShell runs a cron shell job through the seam. On the default host
// profile it is the pre-seam runCronShell exactly: cmd /C on Windows and sh -c
// elsewhere, the scrubbed environment, the given work dir, output trimmed then
// capped, and "command failed: <exit status>" on a non-zero exit. Cron is
// unattended, so a refusal never reaches a person: with enforce on it fails the
// job (named in the error and the sandbox log); in dry-run it re-runs on the host
// and logs what would have been refused.
func runSandboxShell(ctx context.Context, command string, p execProfile) (string, error) {
	be, err := sandbox.For(p.Backend)
	if err != nil {
		return "", fmt.Errorf("sandbox: the cron-shell exec profile's backend %q is unavailable, so the job was not run (fail closed): %w",
			p.Backend, err)
	}
	pol := p.Policy
	if !envScoped(pol.Env) {
		pol.Env = scrubbedEnvPolicy() // the legacy scrubbedEnv default
	}
	cmd := sandbox.Command{Args: cronShellArgv(command), Surface: surfaceCronShell}
	res, err := sandbox.Execute(ctx, be, cmd, pol, nil) // unattended: never escalate to a person
	logSandboxRun(surfaceCronShell, be.Name(), pol.Enforce, cmd, res, err)
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
// survive the scrub, reproducing the pre-seam cron environment.
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

var sandboxLog = logs.For("sandbox")

// logSandboxRun records every confined run and every refusal. Plain host runs
// with nothing refused are the pre-seam path and are not logged here.
func logSandboxRun(surface, backend string, enforce bool, cmd sandbox.Command, res sandbox.Result, err error) {
	if backend == "host" && err == nil && len(res.Violations) == 0 {
		return
	}
	attrs := []any{"surface", surface, "backend", backend, "exit", res.ExitCode, "timed_out", res.TimedOut,
		"command", clipLog(commandText(cmd))}
	if len(res.Violations) > 0 {
		attrs = append(attrs, "violations", describeViolations(res))
	}
	switch {
	case err != nil:
		sandboxLog.Error("sandbox could not start the command", append(attrs, "err", err.Error())...)
	case res.Escalated && !enforce:
		sandboxLog.Warn("dry-run: the sandbox would have refused this; it ran on the host", attrs...)
	case res.Escalated:
		sandboxLog.Warn("refused, then approved to re-run outside the sandbox", attrs...)
	case res.Refused():
		sandboxLog.Warn("refused by the sandbox", attrs...)
	default:
		sandboxLog.Info("ran in the sandbox", attrs...)
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
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// execProfilesEqual compares two override sets.
func execProfilesEqual(a, b map[string]execProfileSetting) bool { return stdreflect.DeepEqual(a, b) }

// loadExecProfilesFromSettings loads the operator overrides for commands that do
// not run the settings watcher (one-shot CLIs such as `tenant os`).
func loadExecProfilesFromSettings(cfgDir, agentID string) (int, []string, error) {
	stg, err := loadSettings(cfgDir, agentID)
	if err != nil {
		return 0, nil, err
	}
	n, skipped := defaultExecProfiles.Load(stg.ExecProfiles)
	return n, skipped, nil
}

// exeDir is the directory of the running executable, or "" when it can't be
// resolved. Always part of the deny set so a sandboxed command can't reach
// Tenant's own binary.
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
