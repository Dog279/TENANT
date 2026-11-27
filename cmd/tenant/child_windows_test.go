//go:build windows

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"tenant/internal/fleet"
)

// childServiceSeams keeps an install under test away from this account's SID
// lookup and from a temporary task file: the definition is handed over in
// memory.
func childServiceSeams(t *testing.T) { taskFile(t) }

// childHelperSeams is childServiceSeams for a helper process, which has no
// test to clean up after: it ends when it is done.
func childHelperSeams() {}

func fakeElevated(t *testing.T, yes bool) { elevated(t, yes) }

// hideWindow starts a process of a test's without a console window: nothing
// a test starts may open one on this machine's desktop.
func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// A child's own unit: a logon task as the account that installed it, or
// with --system a service; both run `tenant child run` on its directory.
func TestChildUnitWindows(t *testing.T) {
	dir := `C:\Users\Jane Doe\AppData\Roaming\tenant\child`
	user, err := childSpecFor("windows", `C:\Program Files\Tenant\tenant.exe`, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Scheduled task \\Tenant\\Child (at logon, as the current user)\n  \"C:\\Program Files\\Tenant\\tenant.exe\" child run --dir \"" + dir + "\" --hidden\n"; user.unitText() != want {
		t.Errorf("user unit:\n%s\nwant:\n%s", user.unitText(), want)
	}
	def := parseTask(t, user.taskXML(testSID))
	if def.Command != `C:\Program Files\Tenant\tenant.exe` || def.Arguments != `child run --dir "`+dir+`" --hidden` || def.User != testSID {
		t.Errorf("task: %+v", def)
	}
	if !strings.Contains(user.taskXML(testSID), "<Description>Tenant fleet child (tenant child run)</Description>") {
		t.Error("the task does not say it is a fleet child")
	}

	sysDir := `C:\ProgramData\tenant\child`
	sys, err := childSpecFor("windows", `C:\Program Files\Tenant\tenant.exe`, sysDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Windows service TenantChild (automatic start, runs as SYSTEM)\n  \"C:\\Program Files\\Tenant\\tenant.exe\" child run --dir \"" + sysDir + "\"\n"; sys.unitText() != want {
		t.Errorf("system unit:\n%s\nwant:\n%s", sys.unitText(), want)
	}
	// Serve's own units are what they were.
	if got := windowsFixture(false).args(); got != userArgs {
		t.Errorf("serve's task now runs %q", got)
	}

	elevated(t, true)
	rec := &cmdRecorder{}
	rec.install(t)
	if err := sys.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmdLine := `"C:\Program Files\Tenant\tenant.exe" child run --dir "` + sysDir + `"`
	rec.want(t,
		"sc create TenantChild binPath= "+cmdLine+" start= auto DisplayName= Tenant fleet child",
		"sc description TenantChild Tenant fleet child (tenant child run)",
		"sc failure TenantChild reset= 86400 actions= restart/10000/restart/60000/restart/300000",
		"sc start TenantChild")
	rec.calls = nil
	if err := user.uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, `schtasks /End /TN \Tenant\Child`, `schtasks /Delete /F /TN \Tenant\Child`)
}

// sids lists whom path's access list names, and whether the list is closed
// to inheritance.
func sids(t *testing.T, path string) (names []string, protected bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the access list of %s: %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("%s has no access list: %v", path, err)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("%s: an entry of type %d", path, ace.Header.AceType)
		}
		// A directory's entry comes twice: once for itself, once for what is
		// made in it.
		if sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String(); !slices.Contains(names, sid) {
			names = append(names, sid)
		}
	}
	return names, control&windows.SE_DACL_PROTECTED != 0
}

// unseal gives this account its files back so the test's directory can be
// removed: sealed, they are SYSTEM's and the administrators'.
func unseal(t *testing.T, root string) {
	t.Helper()
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	open := func(p string) {
		acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
			AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeValue: windows.TrusteeValueFromSID(me.User.Sid)},
		}}, nil)
		if err == nil {
			err = windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
		}
		if err != nil {
			t.Logf("could not reopen %s: %v", p, err)
		}
	}
	// Each directory before what is in it: sealed, it cannot be listed.
	_ = filepath.WalkDir(root, func(p string, _ os.DirEntry, _ error) error {
		open(p)
		return nil
	})
}

// With --system the key, the certificate and child.json are in the machine's
// own location, SYSTEM's and the administrators' alone, before the service
// is first started.
func TestChildInstallSystemWindows(t *testing.T) {
	m := newFleetMaster(t)
	root := realTemp(t)
	sysDir := filepath.Join(root, "ProgramData", "tenant", "child")
	prevDir, prevExe := childSystemDir, checkChildExe
	childSystemDir, checkChildExe = func() string { return sysDir }, func(string) error { return nil }
	t.Cleanup(func() { childSystemDir, checkChildExe = prevDir, prevExe })
	t.Cleanup(func() { unseal(t, filepath.Dir(sysDir)) })
	elevated(t, true)

	const system, admins = "S-1-5-18", "S-1-5-32-544"
	sealedAtStart := false
	rec := &cmdRecorder{reply: func(argv []string) (string, error) {
		if argv[0] == "sc" && argv[1] == "start" {
			// The moment the service would first run.
			sealedAtStart = true
			for _, p := range []string{sysDir, fleet.ChildPath(sysDir), fleet.ChildIdentityPath(sysDir), childDataDir(sysDir)} {
				names, protected := sids(t, p)
				if !protected || len(names) != 2 || !slices.Contains(names, system) || !slices.Contains(names, admins) {
					sealedAtStart = false
					t.Errorf("when the service starts, %s is open to %v (closed to inheritance: %v); want SYSTEM and Administrators alone", p, names, protected)
				}
			}
		}
		return "", nil
	}}
	rec.install(t)
	var out bytes.Buffer
	err := childInstall(context.Background(), strings.NewReader(m.code("server")), &out, sysDir, childInstallOpts{system: true, ceiling: "full", sandbox: "host"})
	if err != nil {
		t.Fatalf("install --system: %v\n%s", err, out.String())
	}
	if !sealedAtStart {
		t.Error("the service was started before its files were checked, or never")
	}
	if len(rec.calls) == 0 || strings.Join(rec.calls[0][:3], " ") != "sc create TenantChild" || !strings.Contains(strings.Join(rec.calls[0], " "), `child run --dir "`+sysDir+`"`) {
		t.Errorf("the service manager was asked: %v", rec.calls)
	}
	if mach, err := m.store.ByName("server"); err != nil || mach.Reported.User != "SYSTEM" {
		t.Errorf("the master was told the machine runs as %q, %v", mach.Reported.User, err)
	}
	// A pause typed by an administrator leaves the directory as closed as it was.
	if windows.GetCurrentProcessToken().IsElevated() {
		if err := childPause(&out, sysDir, true); err != nil {
			t.Fatal(err)
		}
		if names, protected := sids(t, sysDir); !protected || len(names) != 2 {
			t.Errorf("after a pause the directory is open to %v", names)
		}
	}
}

// A binary an ordinary account can replace is not one to run as SYSTEM.
func TestChildExeSafeWindows(t *testing.T) {
	exe, _ := os.Executable() // the test binary, under this account's temp folder
	if err := childExeSafe(exe); err == nil || !strings.Contains(err.Error(), "runs this binary as SYSTEM") {
		t.Errorf("a binary under a user's profile: %v", err)
	}
	if err := childExeSafe(`C:\Program Files\Tenant\tenant.exe`); err != nil {
		t.Errorf("a binary under Program Files: %v", err)
	}
	// A folder made on the system drive takes the drive's access list, which
	// lets every signed-in account change its files: not a place to run from.
	if err := childExeSafe(`C:\Tenant\tenant.exe`); err == nil || !strings.Contains(err.Error(), "runs this binary as SYSTEM") {
		t.Errorf("a binary in a folder of its own on the system drive: %v", err)
	}
	// And the real command refuses before the code is spent.
	m := newFleetMaster(t)
	sysDir := filepath.Join(t.TempDir(), "child")
	prev := childSystemDir
	childSystemDir = func() string { return sysDir }
	t.Cleanup(func() { childSystemDir = prev })
	elevated(t, true)
	code := m.code("box")
	var out bytes.Buffer
	err := childInstall(context.Background(), strings.NewReader(code), &out, sysDir, childInstallOpts{system: true, ceiling: "full", sandbox: "host"})
	if exitCode(err) != 2 || exists(sysDir) {
		t.Errorf("install --system from a user's folder: %v", err)
	}
	if _, digits, err := enrollWith(t, code, childInstallOpts{}); err != nil || digits == "" {
		t.Errorf("the code was spent by the refusal: %v", err)
	}
}

// A real child under a service has no console window, and nothing it runs
// for its master may open one. The run loop's commands go through the
// sandbox's host backend, whose creation flags the sandbox's own tests
// assert; this is the child's side of it, end to end: a command started by a
// child that has no console window is created with one that has no window.
func TestChildCommandsOpenNoWindow(t *testing.T) {
	hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	if hwnd != 0 {
		t.Skip("this test process has a console window, as a child under a service has not")
	}
	m, _, mach := upChild(t, childInstallOpts{})
	// The command reports the window it owns: none, for a console program
	// whose console has no window on any desktop.
	const probe = `'window=' + (Get-Process -Id $PID).MainWindowHandle`
	if res := m.ok(mach.ID, "os_exec", map[string]any{"command": probe, "timeout": 120}); !strings.Contains(res.Text, "window=0") {
		t.Errorf("a command a child ran has a console window: %q", res.Text)
	}
}
