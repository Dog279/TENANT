//go:build !windows && !linux

package main

import "testing"

// undoFromOutside: nothing this test can undo from another process here. On
// macOS the soft core limit of another process cannot be set (there is no
// prlimit), and whether an attach is still refused cannot be asked.
func undoFromOutside(*testing.T, string) (env []string, undo func(t *testing.T, pid int), found string) {
	return nil, nil, ""
}
