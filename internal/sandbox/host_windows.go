package sandbox

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hasConsoleWindow reports whether this process has a console with a window:
// the terminal Tenant was started in, or the one the logon task's --hidden
// hid. A Tenant started by the service control manager has none, and neither
// has a fleet child running under it. Tests replace it.
var hasConsoleWindow = func() bool {
	hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	return hwnd != 0
}

// procAttr is how every command is created on Windows, with flags added. A
// Tenant that has a console window shares it with what it starts, and nothing
// opens. One that has none would have Windows open a new console window for
// every command: on a service's own desktop, or, started without a console
// in somebody's session, in front of them. Such a Tenant gives its commands
// a console that has no window (CREATE_NO_WINDOW) instead.
func procAttr(flags uint32) *syscall.SysProcAttr {
	if hasConsoleWindow() {
		return &syscall.SysProcAttr{CreationFlags: flags}
	}
	return &syscall.SysProcAttr{CreationFlags: flags | windows.CREATE_NO_WINDOW, HideWindow: true}
}

// detach has no controlling terminal to escape on Windows; what it keeps a
// command from is a console window of its own (procAttr).
func detach(c *exec.Cmd) { c.SysProcAttr = procAttr(0) }
