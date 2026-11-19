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
