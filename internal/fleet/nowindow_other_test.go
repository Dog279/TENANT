//go:build !windows

package fleet

import "os/exec"

// noWindow is for Windows, where a helper process can open a console window.
func noWindow(*exec.Cmd) {}
