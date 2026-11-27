//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Where ps is what says which program a process runs, a process is found
// whether ps names it by its path (macOS, started by a path) or by its bare
// name (Linux; on macOS a program started by its name from a shell).
func TestProcessesByPS(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps on this machine")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	wantOtherNotSelf(t, startLiveHelper(t), processesByPS(want))
}
