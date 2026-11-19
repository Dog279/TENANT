package procprotect

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
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
//     (this program run as anchorArg), which is ended as soon as the process
//     is back.
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
// The helper is the one thing here another program of the user could act on,
// so nothing depends on what it does (TEN-535 round three: a program that
// suspended it made the control fail, and then answered Tenant's question
// about that at the console). It is born with the restricted access list, on
// its process and on every thread it will ever have, so it cannot be
// suspended, read or redirected; whether it is on the console is read from
// the console's own list of its programs, not from anything it says; and
// Tenant ends it through the handle it got when it started it. What is left
// to another program is to end it, or to starve the machine so that it is
// late. Either way, and whenever the console cannot be joined again, Tenant
// leaves its console for good: the host then holds no handle at all, which
// is what the control promises, and a Tenant with no console asks nobody
// anything. No failure another program can bring about leaves the old handle
// in place with Tenant running on.
//
// What this does not reach: a handle held by whoever started the process
// (the shell), and anything taken in the moment before Protect ran.

// anchorArg is the one argument of the helper process that keeps a console
// open while its only client is away. Protect, in that process, does nothing
// but wait to be ended: it never runs Tenant.
const anchorArg = "__console-anchor"

// anchorWait is how long the helper has to appear on the console. It is
// long: nothing another program can do makes the helper slow (it cannot be
// suspended), so only a machine under load is waited for, and giving up
// costs the console. A variable, so that a test can make it late.
var anchorWait = 30 * time.Second

// attachWait is how long joining the console again is retried.
const attachWait = 3 * time.Second

// anchorUp, when a test sets it, is called once the helper is on the console
// and before this process leaves it.
var anchorUp func(pid uint32)

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
	why := ""
	target := uint32(os.Getppid())
	if slices.Contains(clients, target) && target != windows.GetCurrentProcessId() {
		// Held until the end, so that its process id names it and nothing
		// else for as long as this process may join it.
		if h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, target); err == nil {
			defer windows.CloseHandle(h)
		}
	} else {
		end, pid, err := startAnchor()
		if err != nil {
			why = "the helper that keeps a console open " + err.Error()
		} else {
			defer end()
			target = pid
			if anchorUp != nil {
				anchorUp(pid)
			}
		}
	}

	if r, _, e := procFreeConsole.Call(); r == 0 {
		return "", fmt.Errorf("FreeConsole: %w", e)
	}
	consoleRejoined = true
	// From here to the end the console streams' old handle values are closed,
	// and Windows gives closed values to the next handles it makes. Nothing
	// in this program may write through them meanwhile.
	for _, s := range streams {
		if s.console {
			s.set(windows.InvalidHandle)
		}
	}
	if why == "" {
		// Only the console it left. Another console (the parent's, for one)
		// would hand this process's input to whoever owns that one. Joining
		// can fail for a moment with the console still there ("A device
		// attached to the system is not functioning", about one start in a
		// few hundred), so it is tried again for a short while. The target's
		// process id is not reused meanwhile: a handle to it is held here.
		for deadline := time.Now().Add(attachWait); ; time.Sleep(10 * time.Millisecond) {
			r, _, e := procAttachConsole.Call(uintptr(target))
			if r != 0 {
				why = ""
				break
			}
			why = "AttachConsole: " + e.Error()
			if time.Now().After(deadline) {
				break
			}
		}
	}
	for _, s := range streams {
		if !s.console { // redirected: exactly as it was
			_ = windows.SetStdHandle(s.which, s.saved)
			continue
		}
		// Opened here, for reading and writing as the handle a process is
		// born with is. What Windows has in the stream's place by now is not
		// looked at and never closed: a process started with its streams
		// named (STARTF_USESTDHANDLES, which is how Windows Terminal starts
		// one) is left the old, closed value there, and by this point that
		// value may be one of the handles opened in this very loop.
		device := s.device
		if why != "" {
			device = "NUL" // no console any more: its streams go nowhere
		}
		sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
		h, err := windows.CreateFile(windows.StringToUTF16Ptr(device), windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &sa, windows.OPEN_EXISTING, 0, 0)
		if err != nil && why == "" {
			// On the console, and it cannot be opened: left for good, as below.
			why = "opening " + s.device + ": " + err.Error()
			_, _, _ = procFreeConsole.Call()
			h, _ = windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ|windows.GENERIC_WRITE,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &sa, windows.OPEN_EXISTING, 0, 0)
		}
		s.set(h)
	}
	if why != "" {
		// The console host holds no handle to a process that is on no
		// console: the control is in force, at the cost of the console.
		return "this process left its console and did not join it again: " + why, nil
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
// once the console lists it among its programs. end ends it.
//
// It is not given the user's ordinary token, as a command is (Child): it
// keeps this process's, whose default for new objects is the restricted list
// by now, and its process and first thread are given that list by name. So
// from its first instant no other program of the user can suspend it or get
// into it. It needs nothing the restricted default takes away: it opens
// nothing, and the pipe it reads was made here.
func startAnchor() (end func(), pid uint32, err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, 0, errors.New("could not be started: " + err.Error())
	}
	sd, err := windows.SecurityDescriptorFromString(restrictedSDDL)
	if err != nil {
		return nil, 0, errors.New("could not be started: " + err.Error())
	}
	closed := &syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: uintptr(unsafe.Pointer(sd))}
	c := exec.Command(exe, anchorArg)
	c.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot")} // nothing of this process's environment
	c.SysProcAttr = &syscall.SysProcAttr{ProcessAttributes: closed, ThreadAttributes: closed}
	var in io.WriteCloser
	Ordinary(func() { in, err = c.StdinPipe() }) // a pipe can only be made there
	if err == nil {
		err = c.Start()
	}
	runtime.KeepAlive(sd)
	if err != nil {
		return nil, 0, errors.New("could not be started: " + err.Error())
	}
	gone := make(chan struct{})
	go func() {
		_ = c.Wait()
		close(gone)
	}()
	end = func() {
		_ = c.Process.Kill() // through the handle it was started with
		in.Close()
		<-gone
	}
	pid = uint32(c.Process.Pid)
	// A process is on the console's list once it has run its first steps; one
	// that has not run yet is not.
	for deadline := time.Now().Add(anchorWait); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if slices.Contains(consoleClients(), pid) {
			return end, pid, nil
		}
		select {
		case <-gone:
			in.Close()
			return nil, 0, errors.New("ended before it was on the console")
		default:
		}
	}
	end()
	return nil, 0, errors.New("was not on the console in time")
}

// anchor is the helper's whole life: stay on the console until the process
// that started it ends it, or is gone itself (its input closes then).
func anchor() {
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
