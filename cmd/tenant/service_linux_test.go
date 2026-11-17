//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"
)

// The unit for paths with spaces, quoted as systemd wants them.
func TestUnitText(t *testing.T) {
	spec := serviceSpec{Exe: "/home/jane doe/.local/bin/tenant", CfgDir: "/home/jane doe/.config/tenant",
		DataDir: "/home/jane doe/.local/share/tenant", StopBudget: 90 * time.Second, Label: "tenant"}
	want := `[Unit]
Description=Tenant agent (tenant serve)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart="/home/jane doe/.local/bin/tenant" serve --config "/home/jane doe/.config/tenant" --data "/home/jane doe/.local/share/tenant" --stop-budget 90s
WorkingDirectory=/home/jane doe/.local/share/tenant
Restart=on-failure
RestartSec=10
TimeoutStopSec=100

[Install]
WantedBy=default.target
`
	if got := spec.unitText(); got != want {
		t.Errorf("unit:\n%s\nwant:\n%s", got, want)
	}
}

// liveSpec is a spec whose unit lands under a temp XDG config home.
func liveSpec(t *testing.T) serviceSpec {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	useRealUnitDir(t) // under the temp XDG config home
	spec, err := specFor("linux", filepath.Join(t.TempDir(), "tenant"), t.TempDir(), t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func userName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func TestInstallArgv(t *testing.T) {
	spec := liveSpec(t)
	rec := &cmdRecorder{reply: func(argv []string) (string, error) {
		if argv[0] == "loginctl" {
			return "no\n", nil
		}
		return "", nil
	}}
	rec.install(t)
	if err := spec.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "systemctl --user daemon-reload", "systemctl --user enable tenant.service", "systemctl --user restart tenant.service",
		"loginctl show-user "+userName()+" -p Linger --value")
	if b, err := os.ReadFile(spec.unitPath()); err != nil || string(b) != spec.unitText() {
		t.Errorf("unit on disk: %v", err)
	}
	if ok, where := spec.installed(context.Background()); !ok || where != spec.unitPath() {
		t.Errorf("installed() = %v, %s", ok, where)
	}
}

func TestUninstallArgv(t *testing.T) {
	spec := liveSpec(t)
	os.MkdirAll(filepath.Dir(spec.unitPath()), 0o755)
	os.WriteFile(spec.unitPath(), []byte("x"), 0o644)
	rec := &cmdRecorder{}
	rec.install(t)
	if err := spec.uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "systemctl --user disable --now tenant.service", "systemctl --user daemon-reload")
	if _, err := os.Stat(spec.unitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit still there: %v", err)
	}
}

func TestRestartArgv(t *testing.T) {
	spec := liveSpec(t)
	rec := &cmdRecorder{}
	rec.install(t)
	if err := spec.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "systemctl --user restart tenant.service")
}

// TestServiceLive installs a real user unit under a test name that runs
// this test binary (which exits at once on serve's flags), then removes it.
func TestServiceLive(t *testing.T) {
	if os.Getenv("TENANT_LIVE_SERVICE") != "1" {
		t.Skip("TENANT_LIVE_SERVICE=1 runs this against systemd --user")
	}
	liveServiceManager(t)
	useRealUnitDir(t) // a unit of its own, under a test name
	exe, _ := os.Executable()
	spec, err := specFor("linux", exe, t.TempDir(), t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	spec.Label = "tenant-test"
	ctx := context.Background()
	if err := spec.install(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, where := spec.installed(ctx); !ok {
		t.Errorf("installed() = false, %s", where)
	}
	if err := spec.uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := spec.installed(ctx); ok {
		t.Error("installed() after uninstall")
	}
}

// realUnitDir is where this account's units really live. Under test unitDir
// names a folder that does not exist instead, so no test sees, reads or
// rewrites a unit the developer installed, unless it asks (useRealUnitDir)
// after pointing XDG_CONFIG_HOME at a temporary directory or giving the unit a
// test name.
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
// dirs, and rewrite it (seen on Windows, 2026-11-17, where the scheduled
// task was pointed at the test binary). A unit standing where the real one
// would be must come through a doctor --fix untouched.
func TestDoctorFixLeavesTheAccountsOwnUnitAlone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // stands in for the real one
	own := filepath.Join(realUnitDir(), "tenant.service")
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	const mine = "the developer's own unit\n"
	if err := os.WriteFile(own, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if spec, err := specFor("linux", "/opt/tenant/tenant", t.TempDir(), t.TempDir(), false); err != nil {
		t.Fatal(err)
	} else if ok, where := spec.installed(context.Background()); ok {
		t.Errorf("a test sees a unit installed at %s without asking for the real unit dir", where)
	}
	runDoctorJSON(t, "--config", t.TempDir(), "--data", t.TempDir(), "--fix")
	if b, err := os.ReadFile(own); err != nil || string(b) != mine {
		t.Errorf("doctor --fix under test changed the account's own unit: %q, %v", b, err)
	}
}
