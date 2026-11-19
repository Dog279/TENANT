//go:build !windows

package procprotect

import "os/exec"

// Child prepares cmd to start as an ordinary process of the user. Only
// Windows has anything to prepare.
func Child(*exec.Cmd) {}

// Ordinary runs f. Only Windows needs more.
func Ordinary(f func()) { f() }
