package fleet

import (
	"os/exec"
	"syscall"
)

// noWindow makes a helper process the tests start one that can never show a
// console window. A console program started from a process that has no
// console of its own is given a new one, in a window that takes the focus
// from whatever the machine's owner is doing; whether the test binary has a
// console depends on who started it, so the helper is told outright.
func noWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000 // CREATE_NO_WINDOW
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
