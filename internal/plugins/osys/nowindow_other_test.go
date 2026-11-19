//go:build !windows

package osys

import (
	"os/exec"
	"testing"
)

// checkNoWindow is Windows' (nowindow_windows_test.go).
func checkNoWindow(*testing.T, *exec.Cmd) {}
