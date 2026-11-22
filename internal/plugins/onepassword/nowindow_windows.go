package onepassword

import (
	"os/exec"
	"syscall"
)

// noWindow starts c with no console window of its own. op is a console
// program, and a Tenant with no console (a service, a scheduled task) would
// otherwise have Windows open a window for it on every call.
func noWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
