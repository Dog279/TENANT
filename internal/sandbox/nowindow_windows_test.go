//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// consoleWindow makes this process look as if it has, or has no, console
// window for the rest of the test.
func consoleWindow(t *testing.T, has bool) {
	t.Helper()
	prev := hasConsoleWindow
	hasConsoleWindow = func() bool { return has }
	t.Cleanup(func() { hasConsoleWindow = prev })
}

// A Tenant with no console window of its own (a service, a fleet child
// under one) must not have Windows open one for each command it runs: every
// way the sandbox starts a process asks for a console without a window, and
// never for a new console or for none at all, either of which would open
// windows further down.
func TestCommandsGetNoConsoleWindowWhenTenantHasNone(t *testing.T) {
	consoleWindow(t, false)
	const opens = windows.CREATE_NEW_CONSOLE | windows.DETACHED_PROCESS
	c := exec.Command("anything")
	detach(c)
	if a := c.SysProcAttr; a == nil || a.CreationFlags&windows.CREATE_NO_WINDOW == 0 || !a.HideWindow || a.CreationFlags&opens != 0 {
		t.Fatalf("a host command is created with %+v", a)
	}
	if a := procAttr(windows.CREATE_SUSPENDED); a.CreationFlags != windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW || !a.HideWindow {
		t.Fatalf("a job object command is created with %+v", a)
	}

	// Created that way, a command still runs and its output still arrives:
	// on the host, in a job object, and started to run on.
	for _, name := range []string{"host", "jobobject"} {
		be, err := For(name)
		if err != nil {
			t.Fatal(err)
		}
		res, err := be.Run(context.Background(), Command{Shell: "Write-Output windowless"}, Policy{Limits: Limits{Wall: time.Minute}})
		if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "windowless") {
			t.Errorf("%s: run = %+v, %v", name, res, err)
		}
		p, err := Start(context.Background(), be, helperArgv(), helperPolicy("echo"))
		if err != nil {
			t.Fatalf("%s: start: %v", name, err)
		}
		fmt.Fprintln(p.Stdin(), "no-window")
		if got := readLine(t, p); got != "echo:no-window" {
			t.Errorf("%s: a started process answered %q", name, got)
		}
		_ = p.Close(5 * time.Second)
	}
}

// A Tenant in a terminal, or with the logon task's hidden console, shares
// that console with its commands as it always has.
func TestCommandsShareTenantsConsoleWhenItHasOne(t *testing.T) {
	consoleWindow(t, true)
	c := exec.Command("anything")
	detach(c)
	if a := c.SysProcAttr; a == nil || a.CreationFlags != 0 || a.HideWindow {
		t.Fatalf("a host command is created with %+v", a)
	}
	if a := procAttr(windows.CREATE_SUSPENDED); a.CreationFlags != windows.CREATE_SUSPENDED || a.HideWindow {
		t.Fatalf("a job object command is created with %+v", a)
	}
}
