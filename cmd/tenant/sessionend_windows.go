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
// So serve has a window. It is never shown. It answers yes to
// WM_QUERYENDSESSION, and on WM_ENDSESSION it starts the same stop a
// SIGTERM starts and holds the message until serve has returned, because
// Windows may end the process as soon as the message is answered. Windows
// allows a window about five seconds; the wait is a little under that.
//
// The same window is how `tenant service stop`, `restart` and `uninstall`
// (and so `tenant update`) ask the logon task's serve to stop before they
// end the task: WM_CLOSE starts the stop. Ending a task is a hard kill, and
// until 0.2.0-rc.3 every update left "The previous run ended unexpectedly"
// in the log for that reason. The window's title names the data dir, so the
// command finds the serve of its own install and no other.
//
// The window is not in serve's own process (TEN-535 hardening). A thread
// that runs a window's messages can be made to load another program's code:
// any program of the same user may set a hook on it (SetWindowsHookEx), and
// Windows then maps that program's library into the process that owns the
// window, with no handle to it and whatever its access list says. So the
// window, and the call that hides the console, live in a helper: this
// program started again as sessionWatchArg, with nothing of serve's
// environment, no config and no secret. The two talk over the helper's
// standard input and output, a word a line: the helper says "ready" and
// "ending", serve says "finished". Code a hook runs in the helper can say
// "ending", which stops serve, as ending the process does; it finds nothing
// to read. serve itself has no window, and no thread of it reads window
// messages, so a hook set on one of its threads never runs. (user32.dll is
// still in serve: the clipboard library the terminal UI uses loads it when
// the program starts. That is also why Windows does not tell serve about a
// logoff as a console event, and the helper's window is how it hears.)
//
// The helper asks Windows to be told about the end of the session before
// serve is (SetProcessShutdownParameters), so that serve has the whole of
// the held message to stop in.
//
// A service (`--system`) is told by the service control manager
// (svc_windows.go) and gets no window and no helper.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"tenant/internal/procprotect"
)

// sessionWatchArg is the first argument of the helper process that owns
// serve's window: `tenant __session-watch hide|show <window title>`.
const sessionWatchArg = "__session-watch"

const (
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmDestroyWatch    = 0x8000 + 1 // WM_APP+1, ours: serve has returned, take the window down
)

// sessionEndWait is how long WM_ENDSESSION is held for serve to finish.
// Tests shorten it.
var sessionEndWait = 4500 * time.Millisecond

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procFindWindowW              = user32.NewProc("FindWindowW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procSendMessageW             = user32.NewProc("SendMessageW")
	procGetModuleHandleW         = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
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

// sessionHelper is serve's end of the helper process that owns its window.
type sessionHelper struct {
	cmd      *exec.Cmd
	say      io.WriteCloser // the helper's standard input
	ending   chan struct{}  // closed when the helper says the session is ending, or a command asked for a stop
	endOnce  sync.Once
	finished atomic.Bool
}

// serveSessionHelper is the helper of the serve this process runs, if any.
var serveSessionHelper atomic.Pointer[sessionHelper]

// sessionHelperStart is how long the helper has to make its window.
const sessionHelperStart = 15 * time.Second

// maxHelperLine bounds a line serve reads from the helper.
const maxHelperLine = 4096

// serveWindowTitle is the title of the window of the serve that uses
// dataDir: how a command of the same install finds it.
func serveWindowTitle(dataDir string) string { return sessionWindowTitle("serve", dataDir) }

// sessionWindowTitle names the window of what ("serve", or "child" for a
// fleet child, childctl.go) by its directory: a serve and a child on one
// machine never share a title, whatever their directories.
func sessionWindowTitle(what, dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return "tenant " + what + " " + strings.ToLower(filepath.Clean(dir))
}

// watchSessionEnd gives serve, or a fleet child (what, with the directory
// that names its window), a channel that is closed when the user's session
// is ending or a command asks it to stop, and with hide takes the console
// window off the screen (--hidden). Both are the helper's work. With no
// helper (a service, or one that did not come up) the channel is nil and
// err says why, when there is something to say.
func watchSessionEnd(what, dir string, hide bool) (ending <-chan struct{}, err error) {
	if ok, _ := svc.IsWindowsService(); ok {
		return nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	mode := "show"
	if hide {
		mode = "hide"
	}
	c := exec.Command(exe, sessionWatchArg, mode, sessionWindowTitle(what, dir))
	c.Env = append([]string{"SystemRoot=" + os.Getenv("SystemRoot")}, sessionHelperEnv...) // nothing of serve's environment
	// The helper shares serve's console. For a serve that has none, Child
	// asks for no window, as it does for every command of such a serve.
	h := &sessionHelper{cmd: c, ending: make(chan struct{})}
	var out io.ReadCloser
	procprotect.Child(c)
	procprotect.Ordinary(func() { // its pipes are made here
		if h.say, err = c.StdinPipe(); err == nil {
			if out, err = c.StdoutPipe(); err == nil {
				err = c.Start()
			}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("the helper that owns serve's window did not start: %w", err)
	}
	lines := helperLines(out)
	ready := make(chan error, 1)
	go func() {
		var err error
		switch {
		case !lines.Scan():
			if err = lines.Err(); err == nil {
				err = io.ErrUnexpectedEOF
			}
		case strings.TrimSpace(lines.Text()) != "ready":
			err = errors.New(strings.TrimSpace(lines.Text()))
		}
		ready <- err
	}()
	select {
	case err = <-ready:
	case <-time.After(sessionHelperStart):
		err = errors.New("it did not make its window in time")
	}
	if err != nil {
		_ = c.Process.Kill()
		_ = c.Wait()
		return nil, fmt.Errorf("the helper that owns serve's window: %w", err)
	}
	go func() {
		if followHelper(lines, func() { h.endOnce.Do(func() { close(h.ending) }) }) != nil {
			_ = c.Process.Kill() // a line past the bound: nothing more is read from it
		}
		_ = c.Wait()
		if !h.finished.Load() { // it went while serve runs: the window went with it
			sysUnavailable.Log(context.Background(), "component", "the helper that owns serve's window",
				"effect", "serve runs on, but a logoff, a shutdown and `tenant service stop` now end it without a clean stop", "err", "it ended")
		}
	}()
	serveSessionHelper.Store(h)
	return h.ending, nil
}

// helperLines reads the helper's output a line at a time. A line is a word,
// or the reason the helper could not make its window; one longer than
// maxHelperLine stops the reading with bufio.ErrTooLong.
func helperLines(out io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(out)
	s.Buffer(make([]byte, 0, 256), maxHelperLine)
	return s
}

// followHelper reads what the helper says after "ready" until its output
// ends: each "ending" calls ending. The error is the reading's, nil at the
// end of the output.
func followHelper(lines *bufio.Scanner, ending func()) error {
	for lines.Scan() {
		if strings.TrimSpace(lines.Text()) == "ending" {
			ending()
		}
	}
	return lines.Err()
}

// sessionEndFinished is main's word that serve has returned and everything
// it deferred has run: the helper answers a held WM_ENDSESSION. The helper
// itself stays until this process has ended (its input closes then), so
// that a command waiting for the window's process waits for serve.
func sessionEndFinished() {
	if h := serveSessionHelper.Load(); h != nil && !h.finished.Swap(true) {
		_, _ = io.WriteString(h.say, "finished\n")
	}
}

// runSessionWatcher is the helper process, when this process is one: it
// makes the window, hides the console when asked, tells serve when the
// session is ending or a stop was asked for, and answers Windows once serve
// says it has finished. It ends when serve does. True: this process was the
// helper, and code is its exit code.
func runSessionWatcher() (was bool, code int) {
	if len(os.Args) != 4 || os.Args[1] != sessionWatchArg {
		return false, 0
	}
	// Ctrl-C and Ctrl-Break at the console it shares with serve are for
	// serve: it stops, and the helper goes when serve has gone. (The Go
	// runtime ends a program that does not take the signal.)
	signal.Notify(make(chan os.Signal, 1), os.Interrupt)
	// Told about the end of the session before serve is: 0x280 is where a
	// program starts, and a higher level is asked first.
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetProcessShutdownParameters").Call(0x3ff, 0)
	if os.Args[2] == "hide" {
		if hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call(); hwnd != 0 {
			_, _, _ = user32.NewProc("ShowWindow").Call(hwnd, 0) // SW_HIDE
		}
	}
	w, err := newSessionWatch(os.Args[3])
	if err != nil {
		fmt.Println(strings.ReplaceAll(err.Error(), "\n", " "))
		return true, 1
	}
	fmt.Println("ready")
	go func() {
		<-w.ending
		fmt.Println("ending")
	}()
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadString('\n')
		if strings.TrimSpace(line) == "finished" {
			w.done()
		}
		if err != nil { // serve has ended
			break
		}
	}
	w.done()
	return true, 0
}

func newSessionWatch(title string) (*sessionWatch, error) {
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return nil, err
	}
	w := &sessionWatch{ending: make(chan struct{}), finished: make(chan struct{})}
	ready := make(chan error, 1)
	go w.loop(t, ready)
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
		_, _, _ = procPostMessageW.Call(w.hwnd, wmDestroyWatch, 0, 0)
	})
}

// loop makes the window and runs its messages. A window belongs to the
// thread that made it, so the goroutine keeps its thread until it ends.
func (w *sessionWatch) loop(title *uint16, ready chan<- error) {
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
	hwnd, _, e := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(sessionClassName)), uintptr(unsafe.Pointer(title)),
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
		case wmClose: // `tenant service stop`, or anything that closes windows politely
			w.endOnce.Do(func() { close(w.ending) })
			return 0 // the window stays: serve is still running
		case wmDestroyWatch:
			_, _, _ = procDestroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			_, _, _ = procPostQuitMessage.Call(0)
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
}

// askServeToStop asks the serve that uses dataDir to stop, through its
// window, and waits for the process to end. False when there is no such
// window (no serve, an older one, or one on another desktop) or the process
// is still there after wait; the caller then ends the task the hard way.
func askServeToStop(dataDir string, wait time.Duration) bool {
	title, err := windows.UTF16PtrFromString(serveWindowTitle(dataDir))
	if err != nil {
		return false
	}
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(sessionClassName)), uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return false
	}
	var pid uint32
	_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 || pid == windows.GetCurrentProcessId() {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if ok, _, _ := procPostMessageW.Call(hwnd, wmClose, 0, 0); ok == 0 {
		return false
	}
	ev, err := windows.WaitForSingleObject(h, uint32(wait/time.Millisecond))
	return err == nil && ev == windows.WAIT_OBJECT_0
}
