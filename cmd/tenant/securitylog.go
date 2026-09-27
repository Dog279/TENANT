package main

// securitylog.go writes the composition root's Security log records
// (docs/design/logging-v2.md §10.1): each command run through the exec seam,
// and the relay's exec mode; and it gives the log book the failure policy
// (logs.security.on_failure) it applies when a record can't be written
// (§10.3). The approval records are internal/approval's.

import (
	"context"
	"strings"

	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/sandbox"
)

// auditDenies reports whether logs.security.on_failure is deny. The log book
// asks only when a record fails, so config.json is read then and an edit
// applies at once; cfg (the value at start) stands in when it won't load.
func auditDenies(cfgDir string, cfg logsConfig) func() bool {
	return func() bool {
		policy := cfg.Security.OnFailure
		if lc, err := loadLaunchConfig(cfgDir); err == nil {
			policy = lc.Logs.Security.OnFailure
		}
		return strings.EqualFold(strings.TrimSpace(policy), "deny")
	}
}

// auditExec writes a command's Security records once it has run: that it
// ran (4201), and what the sandbox refused (4202) or let run outside it
// (4203). The approval that let it run is ctx's: osys opens the slot, the
// approval gate fills it. A turn stopped mid-command doesn't stop its record.
func auditExec(ctx context.Context, backend string, cmd sandbox.Command, enforce bool, res sandbox.Result, err error) {
	actor := "agent"
	if cmd.Surface == surfaceCronShell {
		actor = "cron"
	}
	ctx = context.WithoutCancel(ctx)
	command := logs.Scrub(commandText(cmd))
	who := []any{"actor", actor, "surface", cmd.Surface}
	kv := append([]any{"command", command, "backend", backend, "exit", res.ExitCode, "timed_out", res.TimedOut}, who...)
	if err != nil {
		kv = append(kv, "err", err)
	}
	_ = catalog.ExecRan.Audit(ctx, kv...)
	switch {
	case res.Escalated:
		why := "approved"
		if !enforce {
			why = "dry_run"
		}
		_ = catalog.ExecEscalated.Audit(ctx, append([]any{"command", command, "backend", backend, "violations", describeViolations(res), "why", why}, who...)...)
	case res.Refused():
		_ = catalog.ExecRefused.Audit(ctx, append([]any{"command", command, "backend", backend, "violations", describeViolations(res)}, who...)...)
	}
}

// auditRelayExec records the operator turning the Discord relay's exec mode
// on or off, from surface. (cron-exec's switch is TEN-398's CronExecSwitched.)
func auditRelayExec(on bool, surface string) {
	mode := "off"
	if on {
		mode = "on"
	}
	_ = catalog.RelayExecMode.Audit(context.Background(), "mode", mode, "actor", "operator", "surface", surface)
}
