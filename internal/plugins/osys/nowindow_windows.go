package osys

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: the program gets a console that has no
// window.
const createNoWindow = 0x08000000

// noWindow starts c with no console window of its own. tasklist is a console
// program, and a Tenant with no console (a service, a fleet child under one)
// would otherwise have Windows open a window for it each time it runs.
func noWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
