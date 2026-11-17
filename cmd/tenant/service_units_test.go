//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// realUnitDir is where this account's units really live. Under test unitDir
// names a folder that does not exist instead, so no test sees, reads or
// rewrites a unit the developer installed, unless it asks (useRealUnitDir)
// after pointing unitHomeEnv at a temporary directory or giving the unit a
// test name. unitHomeEnv and ownUnitFile are each OS's
// (service_linux_test.go, service_darwin_test.go).
var realUnitDir = unitDir

func init() {
	unitDir = func() string { return filepath.Join(os.TempDir(), "tenant-test-no-units") }
}

func useRealUnitDir(t *testing.T) {
	t.Helper()
	prev := unitDir
	unitDir = realUnitDir
	t.Cleanup(func() { unitDir = prev })
}

// The tests that run `tenant doctor --fix` used to see the developer's own
// unit, find that it did not run the test binary with the test's temporary
// dirs, and rewrite it. Reproduced on Linux (2026-11-17): a unit standing
// where the real one would be came back as ExecStart="…/tenant.test" serve.
// It must come through a doctor --fix untouched.
func TestDoctorFixLeavesTheAccountsOwnUnitAlone(t *testing.T) {
	t.Setenv(unitHomeEnv, t.TempDir()) // stands in for the real one
	own := filepath.Join(realUnitDir(), ownUnitFile)
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	const mine = "the developer's own unit\n"
	if err := os.WriteFile(own, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if spec, err := specFor(runtime.GOOS, "/opt/tenant/tenant", t.TempDir(), t.TempDir(), false); err != nil {
		t.Fatal(err)
	} else if ok, where := spec.installed(context.Background()); ok {
		t.Errorf("a test sees a unit installed at %s without asking for the real unit dir", where)
	}
	runDoctorJSON(t, "--config", t.TempDir(), "--data", t.TempDir(), "--fix")
	if b, err := os.ReadFile(own); err != nil || string(b) != mine {
		t.Errorf("doctor --fix under test changed the account's own unit: %q, %v", b, err)
	}
}
