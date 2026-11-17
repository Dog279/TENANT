//go:build windows

package main

// sessionend_windows.go: how a logoff or a shutdown reaches a serve that
// runs in a user's session (the logon task, or `tenant serve` in a terminal).
//
// A console program is told about them through its console control handler,
// which the Go runtime turns into SIGTERM. Windows stops doing that for a
// process that has user32.dll or gdi32.dll loaded: it treats the process as
// a windowed application and sends WM_QUERYENDSESSION and WM_ENDSESSION to
// its top-level windows instead. serve --hidden loads user32.dll to hide
// its console window, so the logon task got neither: on Windows Server 2025
// (2026-11-17) every restart and every logoff ended it without one shutdown
// step, and the next start logged "The previous run ended unexpectedly".
//
// So serve owns a window. It is never shown. It answers yes to
// WM_QUERYENDSESSION, and on WM_ENDSESSION it starts the same stop a
// SIGTERM starts and holds the message until serve has returned, because
// Windows may end the process as soon as the message is answered. Windows
// allows a window about five seconds; the wait is a little under that.
//
// A service (`--system`) is told by the service control manager
// (svc_windows.go) and gets no window.

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

const (
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
)

// sessionEndWait is how long WM_ENDSESSION is held for serve to finish.
// Tests shorten it.
var sessionEndWait = 4500 * time.Millisecond

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procGetModuleHandleW = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	X, Y    int32
	Private uint32
}

// sessionWatch is one hidden window and what it tells serve.
type sessionWatch struct {
	hwnd     uintptr
	ending   chan struct{} // closed when Windows says the session is ending
	finished chan struct{} // closed when serve has returned
	endOnce  sync.Once
	finOnce  sync.Once
}

var (
	sessionClassOnce sync.Once
	sessionClassErr  error
	sessionClassName = func() *uint16 { p, _ := windows.UTF16PtrFromString("TenantSessionEnd"); return p }()
	sessionWatches   sync.Map // hwnd -> *sessionWatch
)

// serveSessionWatch is the window of the serve this process runs, if any.
var serveSessionWatch atomic.Pointer[sessionWatch]

// watchSessionEnd gives serve a channel that is closed when the user's
// session is ending. With no window (a service, or Windows refusing one) the
// channel is nil and err says why, when there is something to say.
func watchSessionEnd() (ending <-chan struct{}, err error) {
	if ok, _ := svc.IsWindowsService(); ok {
		return nil, nil
	}
	w, err := newSessionWatch()
	if err != nil {
		return nil, err
	}
	serveSessionWatch.Store(w)
	return w.ending, nil
}

// sessionEndFinished is main's word that serve has returned and everything
// it deferred has run: a held WM_ENDSESSION is answered.
func sessionEndFinished() {
	if w := serveSessionWatch.Load(); w != nil {
		w.done()
	}
}

func newSessionWatch() (*sessionWatch, error) {
	w := &sessionWatch{ending: make(chan struct{}), finished: make(chan struct{})}
	ready := make(chan error, 1)
	go w.loop(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return w, nil
}

// done says serve has returned: a held WM_ENDSESSION is answered, and the
// window is closed.
func (w *sessionWatch) done() {
	w.finOnce.Do(func() {
		close(w.finished)
		_, _, _ = procPostMessageW.Call(w.hwnd, wmClose, 0, 0)
	})
}

// loop makes the window and runs its messages. A window belongs to the
// thread that made it, so the goroutine keeps its thread until it ends.
func (w *sessionWatch) loop(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	inst, _, _ := procGetModuleHandleW.Call(0)
	sessionClassOnce.Do(func() {
		wc := wndClassEx{WndProc: windows.NewCallback(sessionWndProc), Instance: inst, ClassName: sessionClassName}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if atom, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			sessionClassErr = fmt.Errorf("RegisterClassEx: %w", e)
		}
	})
	if sessionClassErr != nil {
		ready <- sessionClassErr
		return
	}
	// Top-level (no parent) and never shown: a message-only window is not
	// sent the end-of-session messages.
	hwnd, _, e := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(sessionClassName)), uintptr(unsafe.Pointer(sessionClassName)),
		0, 0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("CreateWindowEx: %w", e)
		return
	}
	w.hwnd = hwnd
	sessionWatches.Store(hwnd, w)
	defer sessionWatches.Delete(hwnd)
	ready <- nil
	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // WM_QUIT, or an error
			return
		}
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func sessionWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	if v, ok := sessionWatches.Load(hwnd); ok {
		w := v.(*sessionWatch)
		switch msg {
		case wmQueryEndSession:
			return 1 // go ahead: serve never holds up a logoff or a shutdown
		case wmEndSession:
			if wparam != 0 { // 0: someone cancelled it, the session goes on
				w.endOnce.Do(func() { close(w.ending) })
				select {
				case <-w.finished:
				case <-time.After(sessionEndWait):
				}
			}
			return 0
		case wmDestroy:
			_, _, _ = procPostQuitMessage.Call(0)
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
}
