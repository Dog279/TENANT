//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tenant/internal/fleet"
)

// No test sees or writes the machine's own unit directory: like unitDir
// (service_units_test.go), systemUnitDir names a folder that does not exist
// unless a test points it at a temporary one.
func init() {
	systemUnitDir = func() string { return filepath.Join(os.TempDir(), "tenant-test-no-system-units") }
}

// childServiceSeams gives an install under test unit directories of its own.
func childServiceSeams(t *testing.T) {
	t.Helper()
	prevUser, prevSystem := unitDir, systemUnitDir
	user, system := filepath.Join(t.TempDir(), "user-units"), filepath.Join(t.TempDir(), "system-units")
	unitDir, systemUnitDir = func() string { return user }, func() string { return system }
	t.Cleanup(func() { unitDir, systemUnitDir = prevUser, prevSystem })
}

// childHelperSeams is childServiceSeams for a helper process, which has no
// test to clean up after: it ends when it is done.
func childHelperSeams() {
	if units := os.Getenv("TENANT_CHILD_HELPER_UNITS"); units != "" {
		unitDir = func() string { return filepath.Join(units, "user") }
		systemUnitDir = func() string { return filepath.Join(units, "system") }
	}
}

func fakeElevated(t *testing.T, yes bool) {
	t.Helper()
	prev := childElevated
	childElevated = func() bool { return yes }
	t.Cleanup(func() { childElevated = prev })
}

// hideWindow is Windows' (child_windows_test.go): nothing opens a window here.
func hideWindow(*exec.Cmd) {}

// With --system the key, the certificate and child.json are in the machine's
// own location, their account's alone, before the service is first started;
// the unit is the machine's, not an account's.
func TestChildInstallSystemUnix(t *testing.T) {
	m := newFleetMaster(t)
	root := realTemp(t)
	sysDir := filepath.Join(root, "etc", "tenant", "child")
	prevDir, prevExe := childSystemDir, checkChildExe
	childSystemDir, checkChildExe = func() string { return sysDir }, func(string) error { return nil }
	t.Cleanup(func() { childSystemDir, checkChildExe = prevDir, prevExe })
	childServiceSeams(t)
	fakeElevated(t, true)

	start := "systemctl restart tenant-child.service"
	if runtime.GOOS == "darwin" {
		start = "launchctl bootstrap system " + filepath.Join(systemUnitDir(), "com.tenant.child.plist")
	}
	sealedAtStart := false
	rec := &cmdRecorder{reply: func(argv []string) (string, error) {
		if strings.Join(argv, " ") == start {
			// The moment the service would first run.
			sealedAtStart = true
			for p, want := range map[string]os.FileMode{sysDir: 0o700, fleet.ChildPath(sysDir): 0o600, fleet.ChildIdentityPath(sysDir): 0o600, childDataDir(sysDir): 0o700} {
				if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
					sealedAtStart = false
					t.Errorf("when the service starts, %s: %v, %v; want mode %04o", p, fi, err, want)
				}
			}
		}
		if argv[0] == "launchctl" && argv[1] == "print" {
			return "", os.ErrNotExist // not loaded yet
		}
		return "", nil
	}}
	rec.install(t)
	var out bytes.Buffer
	err := childInstall(context.Background(), strings.NewReader(m.code("server")), &out, sysDir, childInstallOpts{system: true, ceiling: "no-exec", sandbox: "host"})
	if err != nil {
		t.Fatalf("install --system: %v\n%s", err, out.String())
	}
	if !sealedAtStart {
		t.Errorf("the service was started before its files were checked, or never: %v", rec.calls)
	}
	spec, _ := childSpecFor(runtime.GOOS, "", sysDir, true)
	unit := mustRead(t, spec.unitPath())
	if filepath.Dir(spec.unitPath()) != systemUnitDir() || !bytes.Contains(unit, []byte(sysDir)) || bytes.Contains(unit, []byte("--user")) {
		t.Errorf("the unit at %s:\n%s", spec.unitPath(), unit)
	}
	for _, c := range rec.calls {
		if line := strings.Join(c, " "); strings.Contains(line, "--user") || strings.Contains(line, "gui/") || c[0] == "loginctl" {
			t.Errorf("a machine's unit was managed as an account's: %s", line)
		}
	}
	if mach, err := m.store.ByName("server"); err != nil || mach.Reported.User != "root" || mach.Reported.Ceiling != "no-exec" {
		t.Errorf("the master was told %+v, %v", mach.Reported, err)
	}
	// Files that are open to another account are not a system child's.
	if err := os.Chmod(fleet.ChildPath(sysDir), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sealSystemChild(sysDir); err == nil || !strings.Contains(err.Error(), "open to other accounts") {
		t.Errorf("child.json readable by everyone passed the check: %v", err)
	}
}

// A binary an account other than root can replace is not one to run as
// root. The test binary is exactly that.
func TestChildExeSafeUnix(t *testing.T) {
	if err := childExeSafe("/bin/sh"); err != nil {
		t.Errorf("a binary only root can replace: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("as root the test binary is root's own")
	}
	exe, _ := os.Executable()
	if err := childExeSafe(exe); err == nil || !strings.Contains(err.Error(), "runs this binary as root") {
		t.Errorf("a binary this account owns: %v", err)
	}
	// And the real command refuses before the code is spent.
	m := newFleetMaster(t)
	sysDir := filepath.Join(t.TempDir(), "child")
	prev := childSystemDir
	childSystemDir = func() string { return sysDir }
	t.Cleanup(func() { childSystemDir = prev })
	fakeElevated(t, true)
	code := m.code("box")
	var out bytes.Buffer
	err := childInstall(context.Background(), strings.NewReader(code), &out, sysDir, childInstallOpts{system: true, ceiling: "full", sandbox: "host"})
	if exitCode(err) != 2 || exists(sysDir) {
		t.Errorf("install --system with a binary of this account's: %v", err)
	}
	if _, digits, err := enrollWith(t, code, childInstallOpts{}); err != nil || digits == "" {
		t.Errorf("the code was spent by the refusal: %v", err)
	}
}

// From a root shell a child is the machine's own: one in a directory someone
// names would run as root with neither its binary nor its directory checked.
func TestChildInstallDirAsRootIsRefused(t *testing.T) {
	fakeElevated(t, true)
	dir := filepath.Join(t.TempDir(), "c")
	err := cmdChild(context.Background(), []string{"install", "--dir", dir})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "from a root shell") || exists(dir) {
		t.Fatalf("install --dir as root: %v (exit %d), directory made: %v", err, exitCode(err), exists(dir))
	}
}
