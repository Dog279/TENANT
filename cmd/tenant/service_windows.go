//go:build windows

package main

// service_windows.go (TEN-498): two shapes. The default is a scheduled task
// at logon as whoever installed it (D1): no stored password, an interactive
// token, and a console, hidden by --hidden, because logoff and shutdown
// reach a Windows process as console control events, which the Go runtime
// turns into SIGTERM; a process without a console gets nothing and is just
// killed. --system is a real SCM service as SYSTEM (svc_windows.go), which
// gets a proper stop control and a longer grace; it needs elevation, and it
// keeps runas.go's consequences (browsing off under a service account).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// isElevated is swapped by tests; --system needs an administrator token.
var isElevated = func() bool { return windows.GetCurrentProcessToken().IsElevated() }

// maxTR is schtasks' limit on the /TR string. A .cmd wrapper is no way
// around it: /End would kill cmd.exe and orphan tenant.exe.
const maxTR = 261

// cmdLine is what the task or service runs.
func (s serviceSpec) cmdLine() string {
	line := fmt.Sprintf(`"%s" serve --config "%s" --data "%s" --stop-budget %s`, s.Exe, s.CfgDir, s.DataDir, secs(s.StopBudget))
	if !s.System {
		line += " --hidden"
	}
	return line
}

func (s serviceSpec) unitText() string {
	if s.System {
		return fmt.Sprintf("Windows service %s (automatic start, runs as SYSTEM)\n  %s\n", s.Label, s.cmdLine())
	}
	return fmt.Sprintf("Scheduled task %s (at logon, as the current user)\n  %s\n", s.Label, s.cmdLine())
}

func (s serviceSpec) install(ctx context.Context) error {
	if s.System {
		return s.installService(ctx)
	}
	if n := len(s.cmdLine()); n > maxTR {
		return fmt.Errorf("the task's command line is %d characters and schtasks allows %d: shorten --config/--data", n, maxTR)
	}
	if out, err := runCmd(ctx, "schtasks", "/Create", "/F", "/TN", s.Label, "/SC", "ONLOGON", "/RL", "LIMITED", "/TR", s.cmdLine()); err != nil {
		return cmdErr("schtasks /Create", out, err)
	}
	return s.start(ctx)
}

func (s serviceSpec) installService(ctx context.Context) error {
	if !isElevated() {
		return errors.New("tenant service --system needs an elevated terminal (run it as administrator)")
	}
	config := []string{s.Label, "binPath=", s.cmdLine(), "start=", "auto", "DisplayName=", "Tenant"}
	if out, err := runCmd(ctx, "sc", append([]string{"create"}, config...)...); err != nil {
		if !strings.Contains(out, "1073") { // ERROR_SERVICE_EXISTS: a reinstall
			return cmdErr("sc create", out, err)
		}
		if out, err := runCmd(ctx, "sc", append([]string{"config"}, config...)...); err != nil {
			return cmdErr("sc config", out, err)
		}
	}
	if out, err := runCmd(ctx, "sc", "description", s.Label, "Tenant agent (tenant serve)"); err != nil {
		return cmdErr("sc description", out, err)
	}
	if out, err := runCmd(ctx, "sc", "failure", s.Label, "reset=", "86400", "actions=", "restart/10000/restart/60000/restart/300000"); err != nil {
		return cmdErr("sc failure", out, err)
	}
	if out, err := runCmd(ctx, "sc", "start", s.Label); err != nil {
		if !strings.Contains(out, "1056") { // ERROR_SERVICE_ALREADY_RUNNING
			return cmdErr("sc start", out, err)
		}
		fmt.Println("already running; `tenant service restart` applies the new command line")
	}
	return nil
}

func (s serviceSpec) uninstall(ctx context.Context) error {
	if s.System {
		if err := s.stop(ctx); err != nil {
			return err
		}
		if out, err := runCmd(ctx, "sc", "delete", s.Label); err != nil {
			return cmdErr("sc delete", out, err)
		}
		return nil
	}
	_, _ = runCmd(ctx, "schtasks", "/End", "/TN", s.Label) // not running is fine
	if out, err := runCmd(ctx, "schtasks", "/Delete", "/F", "/TN", s.Label); err != nil {
		return cmdErr("schtasks /Delete", out, err)
	}
	return nil
}

func (s serviceSpec) start(ctx context.Context) error {
	if s.System {
		if out, err := runCmd(ctx, "sc", "start", s.Label); err != nil {
			return cmdErr("sc start", out, err)
		}
		return nil
	}
	if out, err := runCmd(ctx, "schtasks", "/Run", "/TN", s.Label); err != nil {
		return cmdErr("schtasks /Run", out, err)
	}
	return nil
}

// stop ends the task (a hard kill: the budget is 4 s for a reason), or
// sends the service its Stop control and waits for STOPPED.
func (s serviceSpec) stop(ctx context.Context) error {
	if !s.System {
		if out, err := runCmd(ctx, "schtasks", "/End", "/TN", s.Label); err != nil {
			return cmdErr("schtasks /End", out, err)
		}
		return nil
	}
	if out, err := runCmd(ctx, "sc", "stop", s.Label); err != nil && !strings.Contains(out, "1062") { // ERROR_SERVICE_NOT_ACTIVE
		return cmdErr("sc stop", out, err)
	}
	deadline := time.Now().Add(s.StopBudget + 10*time.Second)
	for {
		out, err := runCmd(ctx, "sc", "query", s.Label)
		if err != nil || strings.Contains(out, "STOPPED") {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("sc stop: the service is still running after the stop budget")
		}
		time.Sleep(time.Second)
	}
}

func (s serviceSpec) restart(ctx context.Context) error {
	if err := s.stop(ctx); err != nil {
		return err
	}
	return s.start(ctx)
}

func (s serviceSpec) installed(ctx context.Context) (bool, string) {
	if s.System {
		_, err := runCmd(ctx, "sc", "query", s.Label)
		return err == nil, "Windows service " + s.Label
	}
	_, err := runCmd(ctx, "schtasks", "/Query", "/TN", s.Label)
	return err == nil, "scheduled task " + s.Label
}

// unitCurrent: the registered command line names this binary and these
// dirs. ponytail: a substring check over the manager's own report; a `&`
// in a path shows up XML-escaped in the task and reads as drift.
func (s serviceSpec) unitCurrent(ctx context.Context) (bool, error) {
	var out string
	var err error
	if s.System {
		out, err = runCmd(ctx, "sc", "qc", s.Label)
	} else {
		out, err = runCmd(ctx, "schtasks", "/Query", "/TN", s.Label, "/XML")
	}
	if err != nil {
		return false, cmdErr("read the unit", out, err)
	}
	return strings.Contains(out, s.Exe) && strings.Contains(out, s.CfgDir) && strings.Contains(out, s.DataDir), nil
}

func (s serviceSpec) logs() string {
	return "the log book under " + s.DataDir + `\logs (tenant logs); there is no console output`
}
