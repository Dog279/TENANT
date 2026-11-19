package procprotect

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The console host. A process that has a console is a client of a console
// host (conhost.exe, or OpenConsole.exe under Windows Terminal), which runs
// as the same user. The host opens every client with all the access the
// client's list gives it, when the client joins, and keeps the handle. A
// process joins its console before its first instruction, so before Protect:
// the host holds a full-access handle, and any program of the user can open
// the host and copy that handle (TEN-535 hardening, the tester's D1).
//
// The cure is to join again. Once the process's access list is the
// restricted one, the process leaves its console (FreeConsole) and joins the
// same one (AttachConsole): the host closes the old handle and opens a new
// one, which now carries the three rights the owner keeps and no more.
//
// Leaving a console costs three things, and each is put back:
//
//   - The console must outlive the moment nobody is joined to it. Another
//     client keeps it: the parent, when it is on the same console (a shell
//     waiting for the command), otherwise a helper started for the purpose
//     (this program run as anchorArg), which ends as soon as the process is
//     back.
//   - Windows closes the process's console handles: standard input, output
//     and error where they are the console. New ones are opened, and put
//     into the os.File values the Go program already has (os.Stdin and the
//     others), so that everything that captured one of them before main ran
//     still writes to the console. A stream redirected to a pipe or a file
//     is not a console handle and is not touched.
//   - Windows forgets the process's console control handlers. The Go
//     runtime's, which turns Ctrl-C, a closed window, a logoff and a
//     shutdown into signals, is found beforehand and registered again.
//
// What this does not reach: a handle held by whoever started the process
// (the shell), and anything taken in the moment before Protect ran.

// anchorArg is the one argument of the helper process that keeps a console
// open while its only client is away. Protect, in that process, does nothing
// but wait for its standard input to close, and exits: it never runs Tenant.
const anchorArg = "__console-anchor"

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

// consoleRejoined: this process has left and joined its console once.
var consoleRejoined bool

// consoleClients lists the processes joined to this process's console; nil
// when it has none.
func consoleClients() []uint32 {
	buf := make([]uint32, 64)
	for {
		n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 {
			return nil
		}
		if int(n) <= len(buf) {
			return buf[:n]
		}
		buf = make([]uint32, n)
	}
}

// stdStream is one of the three standard streams as the process holds it.
type stdStream struct {
	which   uint32          // STD_INPUT_HANDLE ...
	file    **os.File       // os.Stdin ...
	sys     *syscall.Handle // syscall.Stdin ...
	device  string          // CONIN$ or CONOUT$
	saved   windows.Handle  // what it was before
	console bool            // it was a console handle
}

func stdStreams() []*stdStream {
	return []*stdStream{
		{which: windows.STD_INPUT_HANDLE, file: &os.Stdin, sys: &syscall.Stdin, device: "CONIN$"},
		{which: windows.STD_OUTPUT_HANDLE, file: &os.Stdout, sys: &syscall.Stdout, device: "CONOUT$"},
		{which: windows.STD_ERROR_HANDLE, file: &os.Stderr, sys: &syscall.Stderr, device: "CONOUT$"},
	}
}

func isConsole(h windows.Handle) bool {
	var mode uint32
	return h != 0 && h != windows.InvalidHandle && windows.GetConsoleMode(h, &mode) == nil
}

// sysfd is where f keeps its handle (os.File.file.pfd.Sysfd), or nil when
// this Go's os.File is not laid out as it has been since Go 1.9. Writing the
// new handle there is what lets every holder of os.Stdin, os.Stdout and
// os.Stderr carry on after the console is joined again; the test
// TestTheStandardStreamsSurviveTheConsoleBeingRejoined holds each toolchain
// to it.
func sysfd(f *os.File) (p *syscall.Handle) {
	defer func() {
		if recover() != nil {
			p = nil
		}
	}()
	if f == nil {
		return nil
	}
	fd := reflect.ValueOf(f).Elem().Field(0).Elem().FieldByName("pfd").FieldByName("Sysfd")
	if !fd.IsValid() || fd.Kind() != reflect.Uintptr || !fd.CanAddr() {
		return nil
	}
	p = (*syscall.Handle)(unsafe.Pointer(fd.UnsafeAddr()))
	if *p != syscall.Handle(f.Fd()) {
		return nil
	}
	return p
}

// goCtrlHandler finds the Go runtime's console control handler and takes it
// out of the process's list, to be registered again by the caller. Windows
// has no call to read the list, but removing a handler succeeds only for one
// that is in it; and the runtime's handler and a callback made now are
// entries of the same table in the runtime, the runtime's first. 0: not
// found, and nothing was removed.
func goCtrlHandler() uintptr {
	probe := windows.NewCallback(func(uint32) uintptr { return 0 })
	for back := uintptr(1); back <= 1<<16; back++ {
		if r, _, _ := procSetConsoleCtrlHandler.Call(probe-back, 0); r != 0 {
			return probe - back
		}
	}
	return 0
}

// rejoinConsole makes the console host open this process anew, now that its
// access list is the restricted one. note says what was done, for a control
// that is in force.
func rejoinConsole() (note string, err error) {
	if consoleRejoined {
		return "", nil
	}
	clients := consoleClients()
	if len(clients) == 0 {
		return "this process has no console", nil
	}
	streams := stdStreams()
	for _, s := range streams {
		s.saved, _ = windows.GetStdHandle(s.which)
		if s.console = isConsole(s.saved); s.console && sysfd(*s.file) == nil {
			return "", errors.New("this Go toolchain's standard streams cannot be pointed at the console again, so the console was not left")
		}
	}
	handler := goCtrlHandler()
	if handler == 0 {
		return "", errors.New("the Go runtime's console control handler was not found, so the console was not left")
	}
	defer procSetConsoleCtrlHandler.Call(handler, 1)

	// Who keeps the console while this process is away.
	target := uint32(os.Getppid())
	if !slices.Contains(clients, target) || target == windows.GetCurrentProcessId() {
		release, pid, err := startAnchor()
		if err != nil {
			return "", &failure{reason: "a helper to keep the console open could not be started, so the console was not left: " + err.Error()}
		}
		defer release()
		target = pid
	}

	if r, _, e := procFreeConsole.Call(); r == 0 {
		return "", fmt.Errorf("FreeConsole: %w", e)
	}
	r, _, e := procAttachConsole.Call(uintptr(target))
	if r == 0 {
		r, _, _ = procAttachConsole.Call(^uintptr(0)) // ATTACH_PARENT_PROCESS: the last way back
	}
	if r == 0 {
		// No console any more. The streams that were the console go
		// nowhere, rather than to whatever takes their handle values next.
		for _, s := range streams {
			if s.console {
				nul, _ := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ|windows.GENERIC_WRITE,
					windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
				s.set(nul)
			}
		}
		return "", fmt.Errorf("this process left its console and could not join it again: AttachConsole: %w", e)
	}
	consoleRejoined = true
	for _, s := range streams {
		cur, _ := windows.GetStdHandle(s.which)
		if !s.console { // redirected: exactly as it was
			if cur != s.saved {
				_ = windows.SetStdHandle(s.which, s.saved)
			}
			continue
		}
		// Opened here, for reading and writing as the handle a process is
		// born with is: the one Windows puts in its place on joining reads
		// only, and typing into the console or flushing its input needs more.
		sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
		h, err := windows.CreateFile(windows.StringToUTF16Ptr(s.device), windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &sa, windows.OPEN_EXISTING, 0, 0)
		switch {
		case err == nil:
			if isConsole(cur) && cur != h {
				windows.CloseHandle(cur)
			}
		case isConsole(cur):
			h = cur
		default:
			return "", fmt.Errorf("this process joined its console again but could not open it (%s): %w", s.device, err)
		}
		s.set(h)
	}
	return "the console was left and joined again", nil
}

// set makes h the stream's handle everywhere the process looks for it.
func (s *stdStream) set(h windows.Handle) {
	_ = windows.SetStdHandle(s.which, h)
	*s.sys = syscall.Handle(h)
	if p := sysfd(*s.file); p != nil {
		*p = syscall.Handle(h)
	}
}

// startAnchor starts the helper that keeps the console open, and returns
// when it has joined the console. release ends it.
func startAnchor() (release func(), pid uint32, err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, 0, err
	}
	c := exec.Command(exe, anchorArg)
	c.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot")} // nothing of this process's environment
	var in io.WriteCloser
	var out io.ReadCloser
	Ordinary(func() {
		if in, err = c.StdinPipe(); err == nil {
			if out, err = c.StdoutPipe(); err == nil {
				Child(c)
				err = c.Start()
			}
		}
	})
	if err != nil {
		return nil, 0, err
	}
	release = func() {
		in.Close()
		_ = c.Wait()
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(out).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = errors.New("it did not say it was ready")
		}
		ready <- err
	}()
	select {
	case err = <-ready:
	case <-time.After(10 * time.Second):
		err = errors.New("it did not come up in time")
	}
	if err != nil {
		_ = c.Process.Kill()
		release()
		return nil, 0, err
	}
	return release, uint32(c.Process.Pid), nil
}

// anchor is the helper's whole life: say it has joined the console, wait for
// the process that started it to close its input, and exit.
func anchor() {
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
