package main

import (
	"os/exec"
	"syscall"
)

// hideWindow starts a test's helper process without a console window of its
// own (CREATE_NO_WINDOW): nothing a test starts may flash a window open on
// the desktop of whoever is using this computer. What the helper starts in
// turn inherits its windowless console.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
