package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/datalock"
)

// useRealDataLock installs the process lock for one test and drops every
// held lock before the test's temp dirs are removed.
func useRealDataLock(t *testing.T) {
	t.Helper()
	prev := holdDataDir
	holdDataDir = lockDataDirForProcess
	t.Cleanup(func() {
		releaseDataLocks()
		holdDataDir = prev
	})
}

func TestResolveDirsHoldsTheSharedDataLock(t *testing.T) {
	root := t.TempDir()
	useRealDataLock(t)
	c := &commonFlags{dataDir: filepath.Join(root, "data"), cfgDir: filepath.Join(root, "cfg")}
	if err := c.resolveDirs(); err != nil {
		t.Fatal(err)
	}
	if _, err := datalock.Exclusive(c.dataDir); !errors.Is(err, datalock.ErrBusy) {
		t.Fatalf("a restore got the lock while a command held it: err = %v", err)
	}
	// A second resolve in the same process reuses the held lock.
	again := &commonFlags{dataDir: c.dataDir, cfgDir: c.cfgDir}
	if err := again.resolveDirs(); err != nil {
		t.Fatal(err)
	}

	releaseDataLocks()
	x, err := datalock.Exclusive(c.dataDir)
	if err != nil {
		t.Fatalf("exclusive after the command let go: %v", err)
	}
	blocked := &commonFlags{dataDir: c.dataDir, cfgDir: c.cfgDir}
	if err := blocked.resolveDirs(); err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("a command started during a restore: err = %v", err)
	}
	restore := &commonFlags{dataDir: c.dataDir, cfgDir: c.cfgDir, skipDataLock: true}
	if err := restore.resolveDirs(); err != nil {
		t.Fatalf("restore's own resolve must not take the lock: %v", err)
	}
	if err := x.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditOnlyResolveCreatesNoLockFile(t *testing.T) {
	root := t.TempDir()
	useRealDataLock(t)
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	c := &commonFlags{dataDir: data, cfgDir: filepath.Join(root, "cfg"), auditOnly: true}
	if err := c.resolveDirs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(datalock.Path(data)); !os.IsNotExist(err) {
		t.Fatalf("read-only resolve created the lock file: %v", err)
	}
}
