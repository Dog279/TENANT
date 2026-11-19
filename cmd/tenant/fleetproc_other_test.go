//go:build !windows

package main

import "os/exec"

// hideWindow is Windows' concern (fleetproc_windows_test.go): elsewhere a
// child process opens no window.
func hideWindow(*exec.Cmd) {}
