package main

import (
	"os"
	"runtime"
	"testing"
)

// The system's log can be reached by this account: the event source
// registers on Windows, the syslog socket connects elsewhere. Nothing is
// written.
func TestSystemLogOpens(t *testing.T) {
	l, err := openSystemLog()
	if err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("no syslog here (a container, or a system without one): %v", err)
		}
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Error(err)
	}
}

// TestSystemLogLive writes one line to this machine's real system log, under
// a name of its own. Nothing can take a line back out of that log, so it
// runs only when asked: TENANT_LIVE_SYSTEM_LOG=1. Then read it back by hand:
//
//	Windows  Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='TenantTest'} -MaxEvents 1 | Format-List
//	Linux    journalctl -t tenant-test -n 1
//	macOS    log show --last 5m --predicate 'eventMessage CONTAINS "TEN-535"'
func TestSystemLogLive(t *testing.T) {
	if os.Getenv("TENANT_LIVE_SYSTEM_LOG") != "1" {
		t.Skip("TENANT_LIVE_SYSTEM_LOG=1 writes one line to this machine's system log")
	}
	prev := systemLogSource
	t.Cleanup(func() { systemLogSource = prev })
	systemLogSource = map[bool]string{true: "TenantTest", false: "tenant-test"}[runtime.GOOS == "windows"]
	if err := writeSystemLog("Tenant test (TEN-535): written by go test; it means nothing"); err != nil {
		t.Fatal(err)
	}
}
