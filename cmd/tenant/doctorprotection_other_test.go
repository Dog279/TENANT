//go:build !windows && !linux

package main

import "testing"

// doneByDoctorItself: no control a doctor's own start applies is one a
// running Tenant reads back from a shared place here (Windows only).
const doneByDoctorItself = ""

// undoFromOutside: nothing this test can undo from another process here. On
// macOS the soft core limit of another process cannot be set (there is no
// prlimit), and whether an attach is still refused cannot be asked.
func undoFromOutside(*testing.T, string) (env []string, undo func(t *testing.T, pid int), found string) {
	return nil, nil, ""
}
