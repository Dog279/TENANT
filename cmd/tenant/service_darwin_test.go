//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func darwinFixture(t *testing.T) serviceSpec {
	t.Helper()
	home := "/Users/Jane Doe"
	t.Setenv("HOME", home)
	return serviceSpec{Exe: home + "/.local/bin/tenant", CfgDir: home + "/.config/tenant",
		DataDir: home + "/Library/Application Support/tenant", LogDir: home + "/Library/Logs/tenant",
		StopBudget: 90 * time.Second, Label: "com.tenant.serve"}
}

// The plist for paths with spaces: the old template's keys plus ExitTimeOut.
func TestUnitText(t *testing.T) {
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.tenant.serve</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/Jane Doe/.local/bin/tenant</string>
    <string>serve</string>
    <string>--config</string>
    <string>/Users/Jane Doe/.config/tenant</string>
    <string>--data</string>
    <string>/Users/Jane Doe/Library/Application Support/tenant</string>
    <string>--stop-budget</string>
    <string>90s</string>
  </array>
  <key>KeepAlive</key>
  <dict>
    <key>Crashed</key>
    <true/>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ExitTimeOut</key>
  <integer>100</integer>
  <key>WorkingDirectory</key>
  <string>/Users/Jane Doe/Library/Application Support/tenant</string>
  <key>StandardOutPath</key>
  <string>/Users/Jane Doe/Library/Logs/tenant/serve.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/Jane Doe/Library/Logs/tenant/serve.err.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>/Users/Jane Doe</string>
  </dict>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
`
	if got := darwinFixture(t).unitText(); got != want {
		t.Errorf("plist:\n%s\nwant:\n%s", got, want)
	}
}

// liveSpec is a spec under a temp home, outside the TCC dirs.
func liveSpec(t *testing.T) serviceSpec {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	useRealUnitDir(t) // under the temp home
	spec, err := specFor("darwin", filepath.Join(home, ".local", "bin", "tenant"), filepath.Join(home, ".config", "tenant"),
		filepath.Join(home, "Library", "Application Support", "tenant"), false)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// loadedTimes replies as launchd would: `print` succeeds n times, then the
// label is gone.
func loadedTimes(n int) func([]string) (string, error) {
	return func(argv []string) (string, error) {
		if argv[1] == "print" {
			if n--; n >= 0 {
				return "", nil
			}
			return "", errors.New("Could not find service")
		}
		return "", nil
	}
}

func TestInstallArgv(t *testing.T) {
	spec := liveSpec(t)
	gui := fmt.Sprintf("gui/%d", os.Getuid())
	target := gui + "/" + spec.Label

	rec := &cmdRecorder{reply: loadedTimes(0)}
	rec.install(t)
	if err := spec.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "launchctl print "+target, "launchctl bootstrap "+gui+" "+spec.unitPath())
	if b, err := os.ReadFile(spec.unitPath()); err != nil || string(b) != spec.unitText() {
		t.Errorf("plist on disk: %v", err)
	}
	if fi, err := os.Stat(spec.LogDir); err != nil || !fi.IsDir() {
		t.Errorf("log dir: %v", err)
	}

	// A reinstall unloads first: launchd keeps the plist it loaded.
	rec.calls, rec.reply = nil, loadedTimes(1)
	if err := spec.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "launchctl print "+target, "launchctl bootout "+target, "launchctl print "+target, "launchctl bootstrap "+gui+" "+spec.unitPath())

	rec.calls = nil
	tcc := spec
	tcc.DataDir = filepath.Join(os.Getenv("HOME"), "Desktop", "tenant")
	if err := tcc.install(context.Background()); err == nil || !strings.Contains(err.Error(), "Desktop") {
		t.Errorf("a data dir on the Desktop: err = %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("TCC refusal still called launchctl: %v", rec.calls)
	}
}

func TestUninstallArgv(t *testing.T) {
	spec := liveSpec(t)
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), spec.Label)
	os.MkdirAll(filepath.Dir(spec.unitPath()), 0o755)
	os.WriteFile(spec.unitPath(), []byte("x"), 0o644)

	rec := &cmdRecorder{reply: loadedTimes(1)}
	rec.install(t)
	if err := spec.uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "launchctl print "+target, "launchctl bootout "+target, "launchctl print "+target)
	if _, err := os.Stat(spec.unitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still there: %v", err)
	}
	if ok, _ := spec.installed(context.Background()); ok {
		t.Error("installed() after uninstall")
	}

	rec.calls, rec.reply = nil, loadedTimes(0) // never loaded, no file: still fine
	if err := spec.uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "launchctl print "+target)
}

func TestRestartArgv(t *testing.T) {
	spec := liveSpec(t)
	gui := fmt.Sprintf("gui/%d", os.Getuid())
	target := gui + "/" + spec.Label
	rec := &cmdRecorder{reply: loadedTimes(1)}
	rec.install(t)
	if err := spec.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "launchctl print "+target, "launchctl kickstart -k "+target)

	rec.calls, rec.reply = nil, loadedTimes(0) // stopped (booted out): load it again
	if err := spec.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "launchctl print "+target, "launchctl bootstrap "+gui+" "+spec.unitPath())
}

// TestServiceLive installs a real LaunchAgent under a test label that runs
// this test binary (which exits at once on serve's flags), then removes it.
func TestServiceLive(t *testing.T) {
	if os.Getenv("TENANT_LIVE_SERVICE") != "1" {
		t.Skip("TENANT_LIVE_SERVICE=1 runs this against launchd")
	}
	liveServiceManager(t)
	useRealUnitDir(t) // a unit of its own, under a test label
	exe, _ := os.Executable()
	spec, err := specFor("darwin", exe, t.TempDir(), t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	spec.Label = "com.tenant.test"
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
// after pointing HOME at a temporary directory or giving the unit a
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
	t.Setenv("HOME", t.TempDir()) // stands in for the real one
	own := filepath.Join(realUnitDir(), "com.tenant.serve.plist")
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	const mine = "the developer's own unit\n"
	if err := os.WriteFile(own, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if spec, err := specFor("darwin", "/opt/tenant/tenant", t.TempDir(), t.TempDir(), false); err != nil {
		t.Fatal(err)
	} else if ok, where := spec.installed(context.Background()); ok {
		t.Errorf("a test sees a unit installed at %s without asking for the real unit dir", where)
	}
	runDoctorJSON(t, "--config", t.TempDir(), "--data", t.TempDir(), "--fix")
	if b, err := os.ReadFile(own); err != nil || string(b) != mine {
		t.Errorf("doctor --fix under test changed the account's own unit: %q, %v", b, err)
	}
}
