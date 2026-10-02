//go:build !windows

package sandbox

import (
	"os/exec"
	"syscall"
)

// detach starts c in a session of its own (TEN-489): with no controlling
// terminal, sudo and other /dev/tty readers fail at once instead of grabbing
// the operator's terminal or hanging to the timeout. The timeout kills the
// whole group, so a command's children don't outlive it.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}
