//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func windowsFixture(system bool) serviceSpec {
	s := serviceSpec{Exe: `C:\Program Files\Tenant\tenant.exe`, CfgDir: `C:\Users\Jane Doe\AppData\Roaming\tenant`,
		DataDir: `C:\Users\Jane Doe\AppData\Local\tenant`, StopBudget: 4 * time.Second, Label: `\Tenant\Serve`}
	if system {
		s.System, s.StopBudget, s.Label = true, 120*time.Second, "Tenant"
	}
	return s
}

const userArgs = `serve --config "C:\Users\Jane Doe\AppData\Roaming\tenant" --data "C:\Users\Jane Doe\AppData\Local\tenant" --stop-budget 4s --hidden`
const userTR = `"C:\Program Files\Tenant\tenant.exe" ` + userArgs
const systemCmd = `"C:\Program Files\Tenant\tenant.exe" serve --config "C:\Users\Jane Doe\AppData\Roaming\tenant" --data "C:\Users\Jane Doe\AppData\Local\tenant" --stop-budget 120s`

// What --print shows: the task or service, and the command line it runs.
func TestUnitText(t *testing.T) {
	if got, want := windowsFixture(false).unitText(), "Scheduled task \\Tenant\\Serve (at logon, as the current user)\n  "+userTR+"\n"; got != want {
		t.Errorf("user unit:\n%s\nwant:\n%s", got, want)
	}
	if got, want := windowsFixture(true).unitText(), "Windows service Tenant (automatic start, runs as SYSTEM)\n  "+systemCmd+"\n"; got != want {
		t.Errorf("system unit:\n%s\nwant:\n%s", got, want)
	}
}

func elevated(t *testing.T, yes bool) {
	t.Helper()
	prev := isElevated
	isElevated = func() bool { return yes }
	t.Cleanup(func() { isElevated = prev })
}

const testSID = "S-1-5-21-1004336348-1177238915-682003330-1001"

// taskFile swaps the account and the temporary file for fixed ones, and
// returns where the definition handed to schtasks ends up.
func taskFile(t *testing.T) *string {
	t.Helper()
	def := new(string)
	prevUser, prevWrite := taskUser, writeTaskXML
	taskUser = func() (string, error) { return testSID, nil }
	writeTaskXML = func(d string) (string, func(), error) { *def = d; return "task.xml", func() {}, nil }
	t.Cleanup(func() { taskUser, writeTaskXML = prevUser, prevWrite })
	return def
}

// taskDef is the part of a task definition the tests read back.
type taskDef struct {
	TriggerUser string `xml:"Triggers>LogonTrigger>UserId"`
	User        string `xml:"Principals>Principal>UserId"`
	LogonType   string `xml:"Principals>Principal>LogonType"`
	RunLevel    string `xml:"Principals>Principal>RunLevel"`
	TimeLimit   string `xml:"Settings>ExecutionTimeLimit"`
	NoBattery   string `xml:"Settings>DisallowStartIfOnBatteries"`
	StopBattery string `xml:"Settings>StopIfGoingOnBatteries"`
	Command     string `xml:"Actions>Exec>Command"`
	Arguments   string `xml:"Actions>Exec>Arguments"`
}

func parseTask(t *testing.T, def string) taskDef {
	t.Helper()
	var d taskDef
	// The declaration names UTF-16, which the file is and this string is not.
	if err := xml.Unmarshal([]byte(strings.Replace(def, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)), &d); err != nil {
		t.Fatalf("the task definition is not well-formed: %v\n%s", err, def)
	}
	return d
}

func TestInstallArgv(t *testing.T) {
	rec := &cmdRecorder{}
	rec.install(t)
	def := taskFile(t)
	if err := windowsFixture(false).install(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Never /SC ONLOGON: that is a trigger for any user's logon, which an
	// account that is not an administrator may not register.
	rec.want(t, `schtasks /Create /F /TN \Tenant\Serve /XML task.xml`, `schtasks /Run /TN \Tenant\Serve`)
	want := taskDef{TriggerUser: testSID, User: testSID, LogonType: "InteractiveToken", RunLevel: "LeastPrivilege",
		TimeLimit: "PT0S", NoBattery: "false", StopBattery: "false",
		Command: `C:\Program Files\Tenant\tenant.exe`, Arguments: userArgs}
	if got := parseTask(t, *def); got != want {
		t.Errorf("task definition:\n%+v\nwant:\n%+v", got, want)
	}

	rec.calls = nil
	elevated(t, true)
	if err := windowsFixture(true).install(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "sc create Tenant binPath= "+systemCmd+" start= auto DisplayName= Tenant",
		"sc description Tenant Tenant agent (tenant serve)",
		"sc failure Tenant reset= 86400 actions= restart/10000/restart/60000/restart/300000",
		"sc start Tenant")

	rec.calls = nil
	elevated(t, false)
	if err := windowsFixture(true).install(context.Background()); err == nil || !strings.Contains(err.Error(), "elevated") {
		t.Errorf("--system without elevation: err = %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("called sc without elevation: %v", rec.calls)
	}
}

// recordAsk swaps askTaskToStop: the ask is recorded among the commands, in
// order, and answers whether serve stopped.
func recordAsk(t *testing.T, rec *cmdRecorder, stopped bool) {
	t.Helper()
	prev := askTaskToStop
	askTaskToStop = func(dataDir string, wait time.Duration) bool {
		rec.calls = append(rec.calls, []string{"ask", dataDir, wait.String()})
		return stopped
	}
	t.Cleanup(func() { askTaskToStop = prev })
}

func TestUninstallArgv(t *testing.T) {
	rec := &cmdRecorder{}
	rec.install(t)
	recordAsk(t, rec, false)
	if err := windowsFixture(false).uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, `ask C:\Users\Jane Doe\AppData\Local\tenant 6s`, `schtasks /End /TN \Tenant\Serve`, `schtasks /Delete /F /TN \Tenant\Serve`)

	rec.calls = nil
	rec.reply = func(argv []string) (string, error) {
		if argv[1] == "query" {
			return "        STATE              : 1  STOPPED\n", nil
		}
		return "", nil
	}
	if err := windowsFixture(true).uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "sc stop Tenant", "sc query Tenant", "sc delete Tenant")
}

func TestRestartArgv(t *testing.T) {
	rec := &cmdRecorder{}
	rec.install(t)
	recordAsk(t, rec, false)
	if err := windowsFixture(false).restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, `ask C:\Users\Jane Doe\AppData\Local\tenant 6s`, `schtasks /End /TN \Tenant\Serve`, `schtasks /Run /TN \Tenant\Serve`)

	rec.calls = nil
	queries := 0
	rec.reply = func(argv []string) (string, error) {
		if argv[1] == "query" { // running once, then stopped
			if queries++; queries == 1 {
				return "        STATE              : 4  RUNNING\n", nil
			}
			return "        STATE              : 1  STOPPED\n", nil
		}
		return "", nil
	}
	if err := windowsFixture(true).restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.want(t, "sc stop Tenant", "sc query Tenant", "sc query Tenant", "sc start Tenant")
}

// A command line longer than the 261 characters `schtasks /TR` allowed is
// installed, and a path with characters XML reserves comes back as written.
func TestInstallLongAndReservedPaths(t *testing.T) {
	rec := &cmdRecorder{}
	rec.install(t)
	def := taskFile(t)
	spec := windowsFixture(false)
	spec.CfgDir = `C:\Users\R&D <team>\tenant`
	spec.DataDir = `D:\` + strings.Repeat("x", 400)
	if err := spec.install(context.Background()); err != nil || len(rec.calls) != 2 {
		t.Fatalf("err = %v, %d calls", err, len(rec.calls))
	}
	if strings.Contains(*def, "R&D <team>") {
		t.Error("the definition carries the path unescaped")
	}
	if got := parseTask(t, *def); got.Arguments != spec.args() || got.Command != spec.Exe {
		t.Errorf("read back:\n%s %s\nwant:\n%s %s", got.Command, got.Arguments, spec.Exe, spec.args())
	}
}

// The definition reaches schtasks as UTF-16 with a byte-order mark, in a
// file that is removed afterwards.
func TestWriteTaskXML(t *testing.T) {
	const def = `<?xml version="1.0" encoding="UTF-16"?><Task>D:\données\tenant</Task>`
	path, remove, err := writeTaskXML(def)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) < 2 || len(raw)%2 != 0 {
		t.Fatalf("read %d bytes: %v", len(raw), err)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	if units[0] != 0xFEFF || string(utf16.Decode(units[1:])) != def {
		t.Errorf("the file holds %q", string(utf16.Decode(units)))
	}
	remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}
}

// TestServiceLive registers a real logon task under a test name that runs
// this test binary (which exits at once on serve's flags), then removes it;
// from an elevated terminal, the --system service too. The task half must
// pass from a terminal that is not elevated: that is the default install.
func TestServiceLive(t *testing.T) {
	if os.Getenv("TENANT_LIVE_SERVICE") != "1" {
		t.Skip("TENANT_LIVE_SERVICE=1 runs this against the Task Scheduler and the SCM")
	}
	liveServiceManager(t)
	exe, _ := os.Executable()
	ctx := context.Background()
	modes := []bool{false}
	if isElevated() {
		modes = append(modes, true)
	}
	for _, system := range modes {
		spec, err := specFor("windows", exe, t.TempDir(), t.TempDir(), system)
		if err != nil {
			t.Fatal(err)
		}
		if system {
			spec.Label = "TenantTest"
		} else {
			spec.Label = `\Tenant\Test`
		}
		if err := spec.install(ctx); err != nil {
			t.Fatalf("system=%v: %v", system, err)
		}
		if ok, where := spec.installed(ctx); !ok {
			t.Errorf("system=%v: installed() = false, %s", system, where)
		}
		if err := spec.uninstall(ctx); err != nil {
			t.Fatalf("system=%v uninstall: %v", system, err)
		}
		if ok, _ := spec.installed(ctx); ok {
			t.Errorf("system=%v: installed() after uninstall", system)
		}
	}
}

// Ending a task is a hard kill: until this was fixed every `tenant update`
// and `tenant service restart` left "The previous run ended unexpectedly" in
// the log (seen with 0.2.0-rc.3, 2026-11-17). stop asks serve first, for the
// stop budget and a margin. A serve that stopped has already ended its task,
// so schtasks finding nothing to end is then not an error.
func TestStopAsksTheTaskBeforeItEndsIt(t *testing.T) {
	nothingToEnd := func(argv []string) (string, error) {
		return "ERROR: There are no running instances of the task.", errors.New("exit status 1")
	}
	for _, tc := range []struct {
		name    string
		stopped bool
		reply   func([]string) (string, error)
		wantErr bool
	}{
		{"serve stopped when asked, nothing left to end", true, nothingToEnd, false},
		{"serve did not stop, the task is ended", false, nil, false},
		{"no serve and no running task", false, nothingToEnd, true},
	} {
		rec := &cmdRecorder{reply: tc.reply}
		rec.install(t)
		recordAsk(t, rec, tc.stopped)
		err := windowsFixture(false).stop(context.Background())
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: stop = %v, want an error: %v", tc.name, err, tc.wantErr)
		}
		rec.want(t, `ask C:\Users\Jane Doe\AppData\Local\tenant 6s`, `schtasks /End /TN \Tenant\Serve`)
	}
}
