//go:build windows

package main

// service_windows.go (TEN-498): two shapes. The default is a scheduled task
// at logon as whoever installed it (D1): no stored password, an interactive
// token, and a console window hidden by --hidden. A logoff or a shutdown
// reaches that serve through a window of its own (sessionend_windows.go),
// not as a console event: hiding the console window loads user32.dll, and
// Windows sends a process that has it loaded window messages instead.
// --system is a real SCM service as SYSTEM (svc_windows.go), which
// gets a proper stop control and a longer grace; it needs elevation, and it
// keeps runas.go's consequences (browsing off under a service account).
//
// The task is registered from XML (taskXML), not with `schtasks /SC ONLOGON`.
// That switch makes a trigger for any user's logon, which only an
// administrator may register ("Access is denied" for everyone else), and it
// leaves schtasks' defaults in place: the task is ended after 72 hours, is
// not started on battery power and is ended when the machine goes onto it.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// isElevated is swapped by tests; --system needs an administrator token.
var isElevated = func() bool { return windows.GetCurrentProcessToken().IsElevated() }

// taskUser names whoever is installing the task, as the SID of this
// process's token: no account lookup, so it works off the domain, and it is
// the same account whatever it is renamed to. Tests swap it.
var taskUser = func() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read this account's SID: %w", err)
	}
	return u.User.Sid.String(), nil
}

// writeTaskXML puts the task's definition in a temporary file for schtasks
// to read, as UTF-16 with a byte-order mark, the encoding the XML declares.
// Tests swap it to see the definition.
var writeTaskXML = func(def string) (path string, remove func(), err error) {
	f, err := os.CreateTemp("", "tenant-task-*.xml")
	if err != nil {
		return "", nil, err
	}
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, append([]uint16{0xFEFF}, utf16.Encode([]rune(def))...))
	_, werr := f.Write(b.Bytes())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return "", nil, werr
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// args is what follows the binary on the command line.
func (s serviceSpec) args() string {
	a := fmt.Sprintf(`serve --config "%s" --data "%s" --stop-budget %s`, s.CfgDir, s.DataDir, secs(s.StopBudget))
	if !s.System {
		a += " --hidden"
	}
	return a
}

// cmdLine is what the task or service runs.
func (s serviceSpec) cmdLine() string { return `"` + s.Exe + `" ` + s.args() }

// taskXML is the logon task: started when user (a SID) logs on, as that
// user with their interactive token and no elevation, which is what an
// unelevated account may register for itself. It runs until it is stopped:
// no execution time limit and no battery conditions.
func (s serviceSpec) taskXML(user string) string {
	esc := func(v string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(v))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Tenant agent (tenant serve)</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + esc(user) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(user) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(s.Exe) + `</Command>
      <Arguments>` + esc(s.args()) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
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
	user, err := taskUser()
	if err != nil {
		return err
	}
	file, remove, err := writeTaskXML(s.taskXML(user))
	if err != nil {
		return fmt.Errorf("write the task's definition: %w", err)
	}
	defer remove()
	if out, err := runCmd(ctx, "schtasks", "/Create", "/F", "/TN", s.Label, "/XML", file); err != nil {
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
