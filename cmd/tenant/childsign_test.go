package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// On macOS a fleet child's binary carries the hardened-runtime flag as
// serve's does (TEN-535, codesign.go): an update's new binary is signed
// before it first runs, and one that cannot be signed is not applied. The
// macOS path runs here on every system, against the fake codesign.
func TestChildUpdateSignsTheNewBinaryBeforeItRuns(t *testing.T) {
	prev := signsBinaries
	signsBinaries = true
	t.Cleanup(func() { signsBinaries = prev })
	for name, fake := range map[string]*fakeCodesign{"signed": {}, "signing fails": {failSign: true}} {
		t.Run(name, func(t *testing.T) {
			u, _ := childUpdaterRig(t)
			ran := false
			prevCmd := childCommand
			t.Cleanup(func() { childCommand = prevCmd })
			childCommand = func(ctx context.Context, exe string, args ...string) *exec.Cmd {
				ran = true
				if !fake.has(exe) {
					t.Errorf("the new binary ran before it was signed")
				}
				return prevCmd(ctx, exe, args...)
			}
			fake.install(t, nil)
			archive := filepath.Join(filepath.Dir(u.exe), "update.part")
			if err := os.WriteFile(archive, archiveOf(t, runtime.GOOS == "windows", map[string][]byte{exeName(): []byte("binary")}), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := u.stage(context.Background(), archive, "9.9.9")
			if fake.failSign {
				if err == nil || !strings.Contains(err.Error(), "could not be signed with the hardened runtime") || ran {
					t.Errorf("stage = %v, ran = %v; want refused unsigned, before it runs", err, ran)
				}
				return
			}
			if !ran { // the fake binary does not run; what matters is that it was signed first
				t.Errorf("stage = %v; the new binary was never run", err)
			}
		})
	}
}
