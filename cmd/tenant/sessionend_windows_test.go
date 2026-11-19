//go:build windows

package main

import (
	"bufio"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sendWindow sends a message the way Windows does at the end of a session
// and returns the window's answer; it blocks until the window answers.
func sendWindow(hwnd uintptr, msg uint32, wparam uintptr) uintptr {
	r, _, _ := procSendMessageW.Call(hwnd, uintptr(msg), wparam, 0)
	return r
}

func newTestSessionWatch(t *testing.T) *sessionWatch {
	t.Helper()
	w, err := newSessionWatch(serveWindowTitle(t.TempDir()))
	if err != nil {
		t.Fatalf("no window: %v", err)
	}
	t.Cleanup(w.done)
	return w
}

// The logon-task finding (Windows Server 2025, 2026-11-17): a restart or a
// logoff ended serve without one shutdown step. The end of the session must
// start the stop, and Windows must not hear "done" before serve has returned,
// because it may end the process as soon as it does.
func TestSessionEndStartsTheStopAndWaitsForServe(t *testing.T) {
	w := newTestSessionWatch(t)
	if r := sendWindow(w.hwnd, wmQueryEndSession, 0); r != 1 {
		t.Errorf("WM_QUERYENDSESSION answered %d, want 1 (serve never holds up a logoff)", r)
	}
	select {
	case <-w.ending:
		t.Fatal("the question alone started the stop")
	default:
	}
	answered := make(chan struct{})
	go func() { sendWindow(w.hwnd, wmEndSession, 1); close(answered) }()
	select {
	case <-w.ending:
	case <-time.After(5 * time.Second):
		t.Fatal("WM_ENDSESSION did not start the stop")
	}
	select {
	case <-answered:
		t.Fatal("WM_ENDSESSION was answered before serve returned")
	case <-time.After(300 * time.Millisecond):
	}
	w.done()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("WM_ENDSESSION was not answered after serve returned")
	}
}

// WM_ENDSESSION with wParam 0 says the logoff was cancelled: serve goes on.
func TestSessionEndCancelledChangesNothing(t *testing.T) {
	w := newTestSessionWatch(t)
	sendWindow(w.hwnd, wmEndSession, 0)
	select {
	case <-w.ending:
		t.Fatal("a cancelled logoff started the stop")
	case <-time.After(200 * time.Millisecond):
	}
}

// A serve that does not finish in time must not hold the logoff forever.
func TestSessionEndGivesUpAfterTheWait(t *testing.T) {
	prev := sessionEndWait
	sessionEndWait = 200 * time.Millisecond
	t.Cleanup(func() { sessionEndWait = prev })
	w := newTestSessionWatch(t)
	answered := make(chan struct{})
	start := time.Now()
	go func() { sendWindow(w.hwnd, wmEndSession, 1); close(answered) }()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("WM_ENDSESSION was held past the wait")
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("answered after %v, before the wait had passed", d)
	}
	select {
	case <-w.ending:
	default:
		t.Error("the stop was not started")
	}
}

// findWindow is the handle of the serve window for dataDir, 0 for none.
func findWindow(t *testing.T, dataDir string) uintptr {
	t.Helper()
	title, err := windows.UTF16PtrFromString(serveWindowTitle(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(sessionClassName)), uintptr(unsafe.Pointer(title)))
	return h
}

// `tenant service stop` and `restart` ask through the window (WM_CLOSE). The
// ask starts the stop and leaves the window up: serve is still running, and
// a logoff in the next second must still find it.
func TestServeWindowTakesAStopRequest(t *testing.T) {
	dir := t.TempDir()
	w, err := newSessionWatch(serveWindowTitle(dir))
	if err != nil {
		t.Fatalf("no window: %v", err)
	}
	t.Cleanup(w.done)
	if h := findWindow(t, dir); h != w.hwnd {
		t.Fatalf("the window for the data dir is %#x, want %#x", h, w.hwnd)
	}
	// The same folder spelled another way is the same install.
	if h := findWindow(t, strings.ToUpper(dir)+string(filepath.Separator)+"."); h != w.hwnd {
		t.Errorf("the window was not found under another spelling of the data dir")
	}
	if h := findWindow(t, t.TempDir()); h != 0 {
		t.Errorf("another install's data dir found this window")
	}
	sendWindow(w.hwnd, wmClose, 0)
	select {
	case <-w.ending:
	case <-time.After(5 * time.Second):
		t.Fatal("WM_CLOSE did not start the stop")
	}
	if findWindow(t, dir) == 0 {
		t.Error("the stop request took the window down while serve was still running")
	}
	w.done()
	deadline := time.Now().Add(5 * time.Second)
	for findWindow(t, dir) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the window is still there after serve returned")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serve reads the helper's lines up to a bound (TEN-535 round three): code a
// hook runs in the helper must not grow serve's memory with a line that never
// ends. A line past the bound ends the reading; one within it is read.
func TestHelperLinesAreBounded(t *testing.T) {
	endings := 0
	ending := func() { endings++ }
	long := strings.Repeat("x", maxHelperLine+1)
	if err := followHelper(helperLines(strings.NewReader("ending\n"+long+"\nending\n")), ending); !errors.Is(err, bufio.ErrTooLong) || endings != 1 {
		t.Errorf("a line past the bound: err %v, %d endings; want bufio.ErrTooLong after the first ending", err, endings)
	}
	endings = 0
	within := strings.Repeat("y", maxHelperLine-1)
	if err := followHelper(helperLines(strings.NewReader(within+"\nending\n")), ending); err != nil || endings != 1 {
		t.Errorf("a line within the bound: err %v, %d endings; want it read, and the ending after it", err, endings)
	}
}

// A command must never stop itself, and finds nothing where no serve runs.
func TestAskServeToStopWithNoOtherServe(t *testing.T) {
	if askServeToStop(t.TempDir(), 50*time.Millisecond) {
		t.Error("asked a serve that does not exist, and it stopped")
	}
	dir := t.TempDir()
	w, err := newSessionWatch(serveWindowTitle(dir))
	if err != nil {
		t.Fatalf("no window: %v", err)
	}
	t.Cleanup(w.done)
	if askServeToStop(dir, 50*time.Millisecond) {
		t.Error("the window belongs to this process, and the ask reported a stop")
	}
	select {
	case <-w.ending:
		t.Error("a process asked itself to stop")
	case <-time.After(100 * time.Millisecond):
	}
}
