//go:build !windows

package osys

import "os/exec"

// noWindow is Windows' (nowindow_windows.go): nothing opens a window here.
func noWindow(*exec.Cmd) {}
