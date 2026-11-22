package osys

import (
	"os/exec"
	"runtime"
	"testing"
)

// TEN-531: `tenant child …` changes whether a machine serves its master, so
// a command that runs it is flagged like `tenant service …`; an ordinary
// mention of the words is not.
func TestClassifyTenantChild(t *testing.T) {
	for _, tc := range []struct {
		cmd       string
		dangerous bool
	}{
		{"tenant child uninstall", true},
		{"tenant child resume", true},
		{"tenant child pause --dir /etc/tenant/child", true},
		{"/usr/local/bin/tenant child install --system", true},
		{`C:\Tenant\tenant.exe child uninstall`, true},
		{"TENANT CHILD UNINSTALL", true},
		{"tenant children", false},
		{"echo tenant child", false},
		{"grep tenant child.log", false},
	} {
		if d, reason := Classify(tc.cmd); d != tc.dangerous {
			t.Errorf("Classify(%q) = %v (%s), want %v", tc.cmd, d, reason, tc.dangerous)
		}
	}
}

// TEN-531: `tenant fleet …` on a master changes which machines it owns and
// what the agent may do on them (permissions, relay, revoke, enroll), or
// what runs there (update), so an
// agent with host exec on allow does not run it unasked; an ordinary mention
// of the words is not that. The rule is TEN-534's for `tenant service`
// (selfCommand): wherever in the line the command runs.
func TestClassifyTenantFleet(t *testing.T) {
	for _, tc := range []struct {
		cmd       string
		dangerous bool
	}{
		{"tenant fleet permissions web1 set exec allow", true},
		{"tenant fleet relay discord on", true},
		{"tenant fleet revoke web1 --delete-scheme", true},
		{"tenant fleet enroll web2", true},
		{"tenant fleet init --listen 0.0.0.0:9777", true},
		{"tenant fleet list", true},
		{"tenant fleet update web1 --version v9.9.9", true}, // TEN-533: a new Tenant on a machine
		{"tenant fleet update --all --allow-older", true},
		{`C:\Tenant\tenant.exe fleet permissions --defaults add "os_exec(*)" allow`, true},
		{"./tenant fleet confirm web1", true},
		{"TENANT FLEET RELAY IMESSAGE ON", true},
		{"tenant fleets", false},
		{"echo tenant fleet", false},
		{"grep tenant fleet.json", false},
	} {
		if d, reason := Classify(tc.cmd); d != tc.dangerous {
			t.Errorf("Classify(%q) = %v (%s), want %v", tc.cmd, d, reason, tc.dangerous)
		}
	}
}

// The Fleet's verbs in TEN-534's selfVerbs: `tenant child|fleet` is
// dangerous wherever in a line it runs (after &&, after ;, under sudo,
// through Start-Process), and a mention of it stays data, read-only where
// it was.
func TestClassifyFleetVerbsAnywhereInALine(t *testing.T) {
	for _, cmd := range []string{
		"true && tenant fleet revoke web1",
		"cd /tmp; tenant child uninstall",
		"sudo tenant fleet permissions web1 exec=allow",
		"Start-Process tenant -ArgumentList fleet,enroll,x",
	} {
		if d, reason := Classify(cmd); !d {
			t.Errorf("Classify(%q) = false (%s), want dangerous", cmd, reason)
		}
		if ReadOnly(cmd) {
			t.Errorf("ReadOnly(%q) = true", cmd)
		}
	}
	for _, cmd := range []string{`grep "tenant fleet" docs`, "echo tenant child status"} {
		if d, reason := Classify(cmd); d {
			t.Errorf("Classify(%q) = dangerous (%s), want not", cmd, reason)
		}
		if !ReadOnly(cmd) {
			t.Errorf("ReadOnly(%q) = false, want read-only as before", cmd)
		}
	}
}

// os_processes runs a console program. Under a Tenant with no console of its
// own, Windows would open a window for it unless it is created without one.
func TestProcessListOpensNoConsoleWindow(t *testing.T) {
	c := exec.Command("tasklist")
	noWindow(c)
	if runtime.GOOS != "windows" {
		if c.SysProcAttr != nil {
			t.Fatalf("noWindow changed how a process starts on %s: %+v", runtime.GOOS, c.SysProcAttr)
		}
		return
	}
	checkNoWindow(t, c)
}
