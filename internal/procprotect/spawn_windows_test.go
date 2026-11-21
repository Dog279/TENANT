package procprotect

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// A Tenant with no console (the service, one another program started
// detached, one that left its console and could not join it again) must not
// put a console window on the operator's desktop for every command it
// starts: Windows gives a console program whose parent has no console a new
// one, window and all. The wrapper every command goes through asks for no
// window there. A Tenant on a console changes nothing: its commands share
// its console, as they always did.
func TestAChildOfATenantWithNoConsoleGetsNoWindow(t *testing.T) {
	prev := hasConsole
	t.Cleanup(func() { hasConsole = prev })
	flags := func(c *exec.Cmd) uint32 {
		Child(c)
		return c.SysProcAttr.CreationFlags
	}
	with := func(f uint32) *exec.Cmd {
		c := exec.Command("cmd", "/c", "exit 0")
		c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: f}
		return c
	}

	hasConsole = func() bool { return false }
	if f := flags(exec.Command("cmd", "/c", "exit 0")); f != windows.CREATE_NO_WINDOW {
		t.Errorf("no console: a command's creation flags = %#x, want CREATE_NO_WINDOW (%#x)", f, uint32(windows.CREATE_NO_WINDOW))
	}
	if f, want := flags(with(windows.CREATE_NEW_PROCESS_GROUP)), uint32(windows.CREATE_NEW_PROCESS_GROUP|windows.CREATE_NO_WINDOW); f != want {
		t.Errorf("no console: a caller's other flags = %#x, want them kept and CREATE_NO_WINDOW added (%#x)", f, want)
	}
	// A caller that chose the child's console itself is left its choice.
	for _, own := range []uint32{windows.CREATE_NEW_CONSOLE, windows.DETACHED_PROCESS, windows.CREATE_NO_WINDOW} {
		if f := flags(with(own)); f != own {
			t.Errorf("no console: a caller's own choice %#x became %#x", own, f)
		}
	}
	// Every way a command is started goes through it.
	for name, start := range map[string]func(*exec.Cmd) error{
		"Start":          func(c *exec.Cmd) error { return errOr(Start(c), c.Wait) },
		"Run":            Run,
		"Output":         func(c *exec.Cmd) error { _, err := Output(c); return err },
		"CombinedOutput": func(c *exec.Cmd) error { _, err := CombinedOutput(c); return err },
	} {
		c := exec.Command("cmd", "/c", "exit 0")
		if err := start(c); err != nil || c.SysProcAttr == nil || c.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
			t.Errorf("no console: %s: %v, attributes %+v; want the command started with CREATE_NO_WINDOW", name, err, c.SysProcAttr)
		}
	}

	hasConsole = func() bool { return true }
	if f := flags(exec.Command("cmd", "/c", "exit 0")); f != 0 {
		t.Errorf("on a console: a command's creation flags = %#x, want none: it shares Tenant's console", f)
	}
}

func errOr(err error, then func() error) error {
	if err != nil {
		return err
	}
	return then()
}
