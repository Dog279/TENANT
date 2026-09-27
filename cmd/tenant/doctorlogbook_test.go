package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fsperm"
	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
)

// doctorChecks runs `tenant doctor --json` with args and returns its checks
// by name.
func doctorChecks(t *testing.T, args ...string) map[string]checkResult {
	t.Helper()
	out, _ := captureTenantStdout(t, func() error { return cmdDoctor(context.Background(), append(args, "--json")) })
	var report struct {
		Checks []checkResult `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode doctor --json: %v\n%s", err, out)
	}
	byName := map[string]checkResult{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	return byName
}

// TEN-375: doctor reports the log book beside a running serve, read-only:
// its integrity, its size against the budget, who holds the housekeeper
// lease, a process that ran on its text files alone, and what was dropped
// and suppressed. --fix clears the marker of a process that exited that way.
func TestDoctorReportsTheLogbook(t *testing.T) {
	ctx := context.Background()
	cfg, data := t.TempDir(), t.TempDir()
	root := filepath.Join(data, "logs")
	checks := doctorChecks(t, "--config", cfg, "--data", data)
	for _, name := range []string{"log book integrity", "log book size", "log book housekeeper", "log book losses"} {
		if c := checks[name]; c.Stat != "SKIP" || !strings.Contains(c.Detail, "not created yet") {
			t.Errorf("%s on a fresh install = %+v", name, c)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("doctor created the log book: %v", err)
	}

	// A serve that holds the store, its boot lock and the lease.
	const boot = "0123456789abcdef0123456789abcdef"
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{BootID: boot})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Commit(ctx, []event.Event{{Log: "turns", Provider: "turns", Level: event.Info, Msg: "a turn"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginBoot(ctx, store.Boot{ID: boot, PID: 4242, Role: "serve", Host: "box", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	lock, err := datalock.ExclusiveFile(filepath.Join(root, "boots", boot+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if ok, err := st.TryLease(ctx, "housekeeper", time.Minute); !ok || err != nil {
		t.Fatalf("lease: %v, %v", ok, err)
	}
	if err := st.AddLost(ctx, map[string]store.Lost{"turns": {Dropped: 3, Suppressed: 5}}); err != nil {
		t.Fatal(err)
	}
	// A process that died while running on its text files alone.
	const dead = "fedcba9876543210fedcba9876543210"
	marker := filepath.Join(root, "boots", dead+".mirror-only")
	if err := os.WriteFile(marker, []byte(`{"pid":77,"host":"box","since":"2026-09-01T10:00:00Z","why":"disk full"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	checks = doctorChecks(t, "--config", cfg, "--data", data)
	for name, want := range map[string][]string{
		"log book location":    {"OK", root, "the default"},
		"log book integrity":   {"OK", "quick_check OK", "schema version"},
		"log book size":        {"OK", "WAL", "of the 2.0 GB budget", "turns"},
		"log book housekeeper": {"OK", "held by serve pid 4242 on box (boot 01234567)", "runs out in"},
		"log book mirror-only": {"WARN", "pid 77 on box", "exited while on the text files alone"},
		"log book losses":      {"OK", "3 event(s) dropped", "5 suppressed", "turns 3/5"},
	} {
		c := checks[name]
		if c.Stat != want[0] {
			t.Errorf("%s = %s (%s), want %s", name, c.Stat, c.Detail, want[0])
		}
		for _, s := range want[1:] {
			if !strings.Contains(c.Detail, s) {
				t.Errorf("%s detail %q lacks %q", name, c.Detail, s)
			}
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("plain doctor removed the marker: %v", err)
	}
	if n, _ := st.Count(ctx, query.Filter{}); n != 1 {
		t.Errorf("%d events after doctor, want 1", n)
	}

	if c := doctorChecks(t, "--config", cfg, "--data", data, "--fix")["log book mirror-only"]; !c.Fixed {
		t.Errorf("--fix: %+v", c)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("--fix left the stale marker: %v", err)
	}
}

// TEN-375: a log book moved by logs.dir is in doctor's permission audit and
// its --fix, as <data>/logs is; one whose root holds the data dir is not.
func TestDoctorCoversRelocatedLogsDir(t *testing.T) {
	if !fsperm.Supported() {
		t.Skip("POSIX modes only; Windows DACLs are TEN-325")
	}
	cfg, data, moved := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"logs":{"dir":"`+filepath.ToSlash(moved)+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(moved, "system", "2026-09-27.log")
	if err := os.MkdirAll(filepath.Dir(loose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte("a line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}

	checks := doctorChecks(t, "--config", cfg, "--data", data)
	if c := checks["private data permissions"]; c.Stat != "WARN" || !strings.Contains(c.Detail, "exceed owner-only") {
		t.Errorf("private data permissions = %+v, want the relocated log book's loose file", c)
	}
	if c := checks["log book location"]; c.Stat != "OK" || !strings.Contains(c.Detail, moved) || !strings.Contains(c.Detail, "covers it") {
		t.Errorf("log book location = %+v", c)
	}
	if fi, _ := os.Stat(loose); fi.Mode().Perm() != 0o644 {
		t.Fatalf("plain doctor changed %s to %04o", loose, fi.Mode().Perm())
	}
	doctorChecks(t, "--config", cfg, "--data", data, "--fix")
	for _, p := range []string{loose, filepath.Dir(loose)} {
		want := os.FileMode(0o600)
		if p != loose {
			want = 0o700
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm()&^want != 0 {
			t.Errorf("--fix left %s at %04o", p, fi.Mode().Perm())
		}
	}

	// A logs.dir that holds the data dir is left alone, and doctor says so.
	parent := filepath.Dir(data)
	targets, err := privatePermissionTargets(data, cfg, parent)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(targets, func(p permissionTarget) bool { return p.path == parent }) {
		t.Errorf("%s holds the data dir, yet it is a repair target", parent)
	}
	e := &doctorEnv{c: &commonFlags{dataDir: data, cfgDir: cfg, logDir: parent}}
	if c := checkLogbookLocation(context.Background(), e); c.Status != statusWarn || !strings.Contains(c.Detail, "holds the data dir") {
		t.Errorf("log book location for %s = %+v", parent, c)
	}
}
