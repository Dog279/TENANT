//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"tenant/internal/fleet"
)

// A fleet child's logon task (`tenant child run --hidden`) is told about a
// logoff or a shutdown as serve is: through the window of a helper process
// (sessionend_windows.go), which TEN-535 moved out of the process. This runs
// the real binary as a child with a child.json that does not load (it holds
// no link and retries), ends the session at the helper's window, and wants a
// clean exit, with Windows answered only after it.
func TestFleetChildStopsAtSessionEndThroughItsHelpersWindow(t *testing.T) {
	first, _ := forcedControls(t) // the error-reporting exclusion, forced off: nothing is written to the registry
	bin := protectTestBinary(t)
	u := newTenantUnderTest(t, bin)
	dir := filepath.Join(u.home, "child")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fleet.ChildPath(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := u.command(ctx, []string{"TENANT_TEST_FORCE_FAIL=" + first + "=forced by the test"}, "child", "run", "--dir", dir)
	var errb syncBuffer
	cmd.Stdout, cmd.Stderr = &errb, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	var exit error
	go func() { exit = cmd.Wait(); close(ended) }()
	t.Cleanup(func() { // also when the test fails: the child, and with it its helper
		_ = cmd.Process.Kill()
		<-ended
	})

	title, err := windows.UTF16PtrFromString(sessionWindowTitle("child", dir))
	if err != nil {
		t.Fatal(err)
	}
	find := func() uintptr {
		h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(sessionClassName)), uintptr(unsafe.Pointer(title)))
		return h
	}
	var hwnd uintptr
	for deadline := time.Now().Add(60 * time.Second); hwnd == 0; time.Sleep(50 * time.Millisecond) {
		if hwnd = find(); hwnd == 0 && time.Now().After(deadline) {
			t.Fatalf("no window for the child's directory\n%s", errb.String())
		}
	}
	// Its own title: a serve whose data dir is the child's directory does
	// not find it (and `tenant service stop` cannot stop the child).
	if h := findWindow(t, dir); h != 0 {
		t.Errorf("the child's window answers to serve's title for the same folder")
	}
	var owner uint32
	_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
	if child := uint32(cmd.Process.Pid); owner == child || !slices.Contains(childrenOf(t, child), owner) {
		t.Fatalf("the window belongs to process %d; want a child of the fleet child (%d), not the child itself", owner, child)
	}

	if r := sendWindow(hwnd, wmQueryEndSession, 0); r != 1 {
		t.Errorf("WM_QUERYENDSESSION answered %d, want 1", r)
	}
	sendWindow(hwnd, wmEndSession, 1) // answered once the child has returned, or after the wait
	select {
	case <-ended:
		if exit != nil {
			t.Errorf("the child ended with %v, want a clean exit\n%s", exit, errb.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the child is still running after the end of the session\n%s", errb.String())
	}
	for deadline := time.Now().Add(10 * time.Second); find() != 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the helper's window is still there after the child ended")
		}
	}
	if strings.Contains(errb.String(), "without a clean stop") {
		t.Errorf("the child says it has no window:\n%s", errb.String())
	}
}
