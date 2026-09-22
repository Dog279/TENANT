package main

// execprofiles.go is the cmd/tenant side of the execution-backend seam
// (internal/sandbox). Every process a model turn can spawn — the os_exec tool
// and cron shell jobs — resolves an execProfile here and runs through a
// sandbox.Backend, so an operator can later confine those spawns per surface
// without any spawn site changing.
//
// The seam is OPERATOR-controlled: resolveExecProfile reads only trusted inputs
// (surface, the config/data dirs, a work dir) and there is deliberately NO agent
// tool to change a profile — the model must never widen its own boundary. Every
// surface currently defaults to the "host" backend, which is byte-for-byte
// today's behaviour, so wiring the seam changes nothing until a profile opts
// into a stronger backend.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/sandbox"
)

// execProfile is the resolved execution profile for one surface: which sandbox
// backend to run on, and the Policy that backend enforces.
type execProfile struct {
	Backend string
	Policy  sandbox.Policy
}

// resolveExecProfile returns the execution profile for a surface. It is simple
// and pure: it maps trusted inputs to a backend + Policy and reads nothing the
// model controls.
//
// DEFAULT Backend is "host" for EVERY surface, so a routed spawn site behaves
// exactly as it does today. The Policy's DenyRoots ALWAYS include Tenant's own
// config dir, data dir, and the running executable's directory (de-duplicated,
// empties dropped) so that even a future confined backend can never be pointed
// at Tenant's own state or binary. WorkDir is set when a work dir is supplied.
func resolveExecProfile(surface, cfgDir, dataDir, workDir string) execProfile {
	p := execProfile{
		Backend: "host", // every surface defaults to host ⇒ no behaviour change
		Policy: sandbox.Policy{
			DenyRoots: dedupeNonEmpty(cfgDir, dataDir, exeDir()),
		},
	}
	if workDir != "" {
		p.Policy.WorkDir = workDir
	}
	// TODO(TEN-347): operator exec_profiles override (per-surface backend +
	// policy) from settings.<agent>.json, applied here by surface. It stays
	// operator-only — no agent tool resolves or mutates a profile.
	_ = surface
	return p
}

// sandboxRunner adapts a resolved execProfile to osys's runner shape so the
// os_exec spawn site can run through the backend seam. The wall-clock timeout
// osys passes becomes the Policy's wall limit and Enforce is turned on. An
// unavailable/unknown backend is a hard error (fail closed) rather than a silent
// fall-through to the host. A non-zero exit is returned as output only (the host
// runner's behaviour); err is reserved for a backend that could not start.
func sandboxRunner(p execProfile) func(ctx context.Context, command string, timeout time.Duration) (string, error) {
	return func(ctx context.Context, command string, timeout time.Duration) (string, error) {
		be, err := sandbox.For(p.Backend)
		if err != nil {
			return "", fmt.Errorf("sandbox: os_exec backend %q unavailable: %w", p.Backend, err)
		}
		pol := p.Policy
		pol.Limits.Wall = timeout
		pol.Enforce = true
		res, err := be.Run(ctx, sandbox.Command{Shell: command, Surface: "os-exec"}, pol)
		if err != nil {
			return res.Output, fmt.Errorf("sandbox: os_exec backend %q could not start command: %w", p.Backend, err)
		}
		return res.Output, nil
	}
}

// runSandboxShell runs a cron shell command through the backend seam. With the
// default host profile it reproduces the pre-seam runCronShell behaviour: the
// scrubbed environment (expressed as an env allowlist), the supplied working
// dir, combined output trimmed and capped, and a non-zero exit surfaced as an
// error. A profile that already scopes the environment is honoured as-is.
func runSandboxShell(ctx context.Context, command string, p execProfile) (string, error) {
	be, err := sandbox.For(p.Backend)
	if err != nil {
		return "", fmt.Errorf("sandbox: cron-shell backend %q unavailable: %w", p.Backend, err)
	}
	pol := p.Policy
	pol.Enforce = true
	if !envScoped(pol.Env) {
		pol.Env = scrubbedEnvPolicy() // preserve the legacy scrubbedEnv default
	}
	if pol.Limits.MaxOutputBytes == 0 {
		pol.Limits.MaxOutputBytes = cronShellOutputCap
	}
	res, err := be.Run(ctx, sandbox.Command{Shell: command, Surface: "cron-shell"}, pol)
	s := strings.TrimSpace(res.Output)
	if err != nil {
		return s, fmt.Errorf("command failed: %v", err)
	}
	if res.ExitCode != 0 || res.TimedOut {
		return s, fmt.Errorf("command failed: exit status %d", res.ExitCode)
	}
	return s, nil
}

// scrubbedEnvPolicy expresses cron's legacy scrubbedEnv() secret-name denylist as
// a sandbox EnvPolicy allowlist: it forwards exactly the parent variable NAMES
// that survive the scrub, so the host backend reproduces the pre-seam cron
// environment. Used as the cron host-profile default.
func scrubbedEnvPolicy() sandbox.EnvPolicy {
	kept := scrubbedEnv() // []string "NAME=VALUE" with secret-bearing names removed
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

// envScoped reports whether an EnvPolicy actually restricts the environment. It
// mirrors sandbox's own inherit-all condition: a zero EnvPolicy does not scope,
// so the cron default scrub applies.
func envScoped(e sandbox.EnvPolicy) bool {
	return e.Isolate || len(e.Pass) > 0 || len(e.Set) > 0
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
