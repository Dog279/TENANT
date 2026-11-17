//go:build windows

package main

import (
	"testing"
	"time"
)

// sendWindow sends a message the way Windows does at the end of a session
// and returns the window's answer; it blocks until the window answers.
func sendWindow(hwnd uintptr, msg uint32, wparam uintptr) uintptr {
	r, _, _ := procSendMessageW.Call(hwnd, uintptr(msg), wparam, 0)
	return r
}

func newTestSessionWatch(t *testing.T) *sessionWatch {
	t.Helper()
	w, err := newSessionWatch()
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
