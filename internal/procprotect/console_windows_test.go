package procprotect

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The console host's handle (TEN-535 hardening, the tester's D1). These
// victims get a console of their own, hidden, the way a shortcut or the
// logon task gives Tenant one: nobody else is on it, and its host is theirs
// alone, so the test opens no console host but its own victims'.

const (
	envConsole = "PROCPROTECT_CONSOLE" // the victim's streams are its own console: check them
	envLinger  = "PROCPROTECT_LINGER"  // the victim starts a console command that stays, and says which
)

// anchorIfAsked: a victim alone on its console starts this test binary as the
// helper that keeps the console open, as Tenant starts itself.
func anchorIfAsked() {
	if len(os.Args) == 2 && os.Args[1] == anchorArg {
		anchor()
	}
}

// startConsoleVictim starts a victim of kind in a console of its own, with
// the console as its standard input, output and error. Its report comes back
// in a file. It ends with the test.
func startConsoleVictim(t *testing.T, kind string) victimInfo {
	t.Helper()
	return startVictimWith(t, kind, windows.CREATE_NEW_CONSOLE, envConsole+"=1", envLinger+"=1")
}

// commandThatStays is the console command a victim started with envLinger
// says it started, after Protect, the way Tenant starts every command.
func commandThatStays(t *testing.T, v victimInfo) uint32 {
	t.Helper()
	pid, err := strconv.Atoi(v.Self["info:a command that stays"])
	if err != nil {
		t.Fatalf("the victim could not start a command that stays: %q", v.Self["info:a command that stays"])
	}
	return uint32(pid)
}

// startVictimWith starts a victim of kind the way how says (a process
// creation flag: a console of its own, one with no window, or none at all).
// It inherits no handle. Its report comes back in a file. It ends with the
// test.
func startVictimWith(t *testing.T, kind string, how uint32, env ...string) victimInfo {
	t.Helper()
	info := filepath.Join(t.TempDir(), "victim.json")
	var block []uint16
	for _, kv := range append(append(os.Environ(), envHelper+"=victim", envKind+"="+kind, envMarker+"=ten535-"+kind+"-"+t.Name(),
		envInfo+"="+info), env...) {
		block = append(block, windows.StringToUTF16(kv)...)
	}
	block = append(block, 0)
	cmdline := windows.StringToUTF16Ptr(windows.EscapeArg(os.Args[0]))
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Flags: windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE}
	var pi windows.ProcessInformation
	flags := how | windows.CREATE_UNICODE_ENVIRONMENT
	var err error
	if ordinaryToken != 0 { // this test process is protected itself by now: start an ordinary one
		sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: ordinarySD}
		err = windows.CreateProcessAsUser(ordinaryToken, nil, cmdline, sa, sa, false, flags, &block[0], nil, &si, &pi)
	} else {
		err = windows.CreateProcess(nil, cmdline, nil, nil, false, flags, &block[0], nil, &si, &pi)
	}
	if err != nil {
		t.Fatalf("start a victim in a console of its own: %v", err)
	}
	windows.CloseHandle(pi.Thread)
	t.Cleanup(func() {
		_ = windows.TerminateProcess(pi.Process, 1)
		_, _ = windows.WaitForSingleObject(pi.Process, 10_000)
		windows.CloseHandle(pi.Process)
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, err := os.ReadFile(info)
		var v victimInfo
		if err == nil && json.Unmarshal(b, &v) == nil && v.PID != 0 {
			if pid, err := strconv.Atoi(v.Self["info:a command that stays"]); err == nil {
				t.Cleanup(func() { // the command the victim started ends with the test too
					if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
						_ = windows.TerminateProcess(h, 1)
						windows.CloseHandle(h)
					}
				})
			}
			return v
		}
		if ev, _ := windows.WaitForSingleObject(pi.Process, 50); ev == windows.WAIT_OBJECT_0 || time.Now().After(deadline) {
			t.Fatalf("the %s victim in its own console did not report (ended: %v): %v %s", kind, ev == windows.WAIT_OBJECT_0, err, b)
		}
	}
}

// consoleState is what a victim whose streams are its own console can see of
// them, after Protect or without it: every value is "ok" when the console
// still works.
func consoleState() map[string]string {
	s := map[string]string{}
	say := func(k string, err error) {
		s[k] = "ok"
		if err != nil {
			s[k] = err.Error()
		}
	}
	for name, f := range map[string]*os.File{"stdin": os.Stdin, "stdout": os.Stdout, "stderr": os.Stderr} {
		var mode uint32
		say("console: os."+name+" is the console", windows.GetConsoleMode(windows.Handle(f.Fd()), &mode))
	}
	_, err := fmt.Fprintln(os.Stdout, "a line on the console")
	say("console: a line written to os.Stdout", err)
	// The log package made its logger before main ran, around the os.Stderr of then.
	say("console: a line through the log package's own logger", log.Default().Output(0, "a line from the log package"))
	var mode uint32
	in := windows.Handle(os.Stdin.Fd())
	err = windows.GetConsoleMode(in, &mode)
	if err == nil {
		err = windows.SetConsoleMode(in, mode)
	}
	say("console: the input mode read and set (what the terminal UI does)", err)
	conout, err := windows.CreateFile(windows.StringToUTF16Ptr("CONOUT$"), windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err == nil {
		var info windows.ConsoleScreenBufferInfo
		err = windows.GetConsoleScreenBufferInfo(conout, &info)
		windows.CloseHandle(conout)
	}
	say("console: its size read through CONOUT$ (how a resize is seen)", err)

	// A command given the console's own streams, as a command that needs a console is.
	c := exec.Command("cmd", "/c", "echo a command on the console")
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	say("console: a command started on the console's streams", Run(c))

	// A line typed at the console reaches os.Stdin.
	type keyRecord struct { // INPUT_RECORD holding a KEY_EVENT_RECORD
		EventType uint16
		_         uint16
		KeyDown   int32
		Repeat    uint16
		VKey      uint16
		Scan      uint16
		Char      uint16
		Control   uint32
	}
	var keys []keyRecord
	for _, r := range "typed\r" {
		vk := uint16(0)
		if r == '\r' {
			vk = 0x0D
		}
		keys = append(keys, keyRecord{EventType: 1, KeyDown: 1, Repeat: 1, VKey: vk, Char: uint16(r)},
			keyRecord{EventType: 1, KeyDown: 0, Repeat: 1, VKey: vk, Char: uint16(r)})
	}
	var n uint32
	if r, _, e := kernel32.NewProc("WriteConsoleInputW").Call(uintptr(in), uintptr(unsafe.Pointer(&keys[0])), uintptr(len(keys)), uintptr(unsafe.Pointer(&n))); r == 0 {
		say("console: a line typed at the console is read from os.Stdin", e)
	} else {
		line := make(chan string, 1)
		go func() {
			l, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			line <- l
		}()
		select {
		case l := <-line:
			err = nil
			if strings.TrimSpace(l) != "typed" {
				err = fmt.Errorf("read %q", l)
			}
		case <-time.After(5 * time.Second):
			err = errors.New("nothing was read in 5s")
		}
		say("console: a line typed at the console is read from os.Stdin", err)
	}

	// Ctrl-Break, as the console sends it: the Go runtime turns it into a signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	err = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0) // this console only: the victim's own
	if err == nil {
		select {
		case <-sig:
		case <-time.After(5 * time.Second):
			err = errors.New("no signal arrived in 5s")
		}
	}
	say("console: Ctrl-Break reaches the Go runtime as a signal", err)
	return s
}

// TestTheStandardStreamsSurviveTheConsoleBeingRejoined: Protect leaves the
// console and joins it again. A process whose streams are the console still
// writes to it, through os.Stdout and through what captured os.Stderr before
// main ran; reads a typed line; sets its input mode; reads its size; starts
// a command on it; and still gets Ctrl-Break as a signal. The same checks
// run in a process that did not call Protect, so that a failure here is the
// rejoining and not the check.
func TestTheStandardStreamsSurviveTheConsoleBeingRejoined(t *testing.T) {
	if Disabled {
		t.Skip("built with -tags debugattach")
	}
	for _, kind := range []string{"plain", "protected"} {
		v := startConsoleVictim(t, kind)
		checked := 0
		for k, got := range v.Self {
			if !strings.HasPrefix(k, "console: ") {
				continue
			}
			checked++
			if got != "ok" {
				t.Errorf("%s victim: %s: %s", kind, strings.TrimPrefix(k, "console: "), got)
			}
		}
		if checked < 10 {
			t.Errorf("%s victim reported only %d console checks: %v", kind, checked, v.Self)
		}
		// A command started after the console was left and joined again runs
		// on that same console, which is hidden here: it gets no console of
		// its own, and nothing of the victim's is shown on the desktop.
		stays := commandThatStays(t, v)
		tree := processTree(uint32(v.PID))
		if tree[stays] != "ping.exe" || len(consoleHostsOf(stays)) != 0 {
			t.Errorf("%s victim: the command it started is not there, or was given a console of its own (hosts %v): %v", kind, consoleHostsOf(stays), tree)
		}
		if shown := visibleWindows(tree); len(shown) != 0 {
			t.Errorf("%s victim: windows of its processes are shown on the desktop: %v", kind, shown)
		}
		if kind == "protected" {
			var c Control
			for _, x := range v.Report.Controls {
				if x.ID == "windows.console-host" {
					c = x
				}
			}
			if !c.OK || c.Note != "the console was left and joined again" {
				t.Errorf("the console control of a victim alone on its console = %+v", c)
			}
		}
		if v.Child["error"] != "" {
			t.Errorf("%s victim: a command through a pipe: %s", kind, v.Child["error"])
		}
	}
}

// TestProtectWithNoConsoleAndWithNoWindow: the other ways Tenant is started.
// A service has no console at all: nothing is left, nothing is joined, and
// the control says so. A console with no window (CREATE_NO_WINDOW, how a
// program that hides its commands starts them) is a console all the same,
// with a host that holds a handle: it is left and joined like any other.
func TestProtectWithNoConsoleAndWithNoWindow(t *testing.T) {
	if Disabled {
		t.Skip("built with -tags debugattach")
	}
	if why := attackerTooStrong(); why != "" {
		t.Skip(why)
	}
	const name = "copy the console host's handle to it (conhost.exe)"
	console := func(v victimInfo) (c Control) {
		for _, x := range v.Report.Controls {
			if x.ID == "windows.console-host" {
				c = x
			}
		}
		return c
	}

	none := startVictimWith(t, "protected", windows.DETACHED_PROCESS, envLinger+"=1")
	if c := console(none); !c.OK || c.Note != "this process has no console" || len(none.Report.Missing()) != 0 {
		t.Errorf("a victim with no console: the console control = %+v, missing %+v", c, none.Report.Missing())
	}
	// A command it starts gets a console, since it is a console program, and
	// that console has no window: nothing of the victim's is shown on the
	// desktop. (Windows would otherwise give the command a new console with
	// a window, which takes the keyboard from whatever the operator does.)
	stays := commandThatStays(t, none)
	time.Sleep(500 * time.Millisecond) // a window, if there were one, is up by now
	tree := processTree(uint32(none.PID))
	if tree[stays] != "ping.exe" || len(consoleHostsOf(stays)) == 0 {
		t.Errorf("the command the victim started is not there, or has no console at all: %v", tree)
	}
	if shown := visibleWindows(tree); len(shown) != 0 {
		t.Errorf("a victim with no console put windows on the desktop for a command it started: %v (its processes: %v)", shown, tree)
	}
	if hosts := consoleHostsOf(uint32(none.PID)); len(hosts) != 0 {
		t.Errorf("a victim started with no console has console hosts %v", hosts)
	}
	if none.Child["error"] != "" {
		t.Errorf("a victim with no console: a command through a pipe: %s", none.Child["error"])
	}
	if got, ok := attack(t, none)["ReadProcessMemory (PROCESS_VM_READ)"]; !ok || got.OK {
		t.Errorf("a victim with no console: its memory was read, or the attempt was not made: %+v", got)
	}

	control := attack(t, startVictimWith(t, "plain", windows.CREATE_NO_WINDOW))[name]
	t.Logf("a console with no window, no Protect (control): %s %s", verdict(control), control.Got)
	if !control.OK {
		t.Skipf("NOT SHOWN HERE: the host of a console with no window holds nothing that reads an unprotected process: %s", control.Err)
	}
	windowless := startVictimWith(t, "protected", windows.CREATE_NO_WINDOW)
	if c := console(windowless); !c.OK || c.Note != "the console was left and joined again" {
		t.Errorf("a victim on a console with no window: the console control = %+v", c)
	}
	got := attack(t, windowless)[name]
	t.Logf("a console with no window, after Protect: %s", verdict(got))
	if got.OK || !strings.Contains(got.Err, "0x101001") {
		t.Errorf("%q against a victim on a console with no window: let through %v (%q), refused with %q; want the owner's three rights, 0x101001", name, got.OK, got.Got, got.Err)
	}
}

// TestTheConsoleHostsHandleIsNoWayIn: the console host of a process that did
// not call Protect holds a handle that reads its memory, and any program of
// the user can copy it. After Protect the host holds the owner's three
// rights and no more.
func TestTheConsoleHostsHandleIsNoWayIn(t *testing.T) {
	if Disabled {
		t.Skip("built with -tags debugattach")
	}
	if why := attackerTooStrong(); why != "" {
		t.Skip(why)
	}
	const name = "copy the console host's handle to it (conhost.exe)"
	control := attack(t, startConsoleVictim(t, "plain"))[name]
	t.Logf("no Protect (control): %s %s", verdict(control), control.Got)
	if !control.OK {
		t.Skipf("NOT SHOWN HERE: the console host's handle to an unprotected process reads nothing either: %s", control.Err)
	}
	got := attack(t, startConsoleVictim(t, "protected"))[name]
	t.Logf("after Protect: %s", verdict(got))
	if got.OK {
		t.Errorf("%q was NOT refused after Protect (got %q)", name, got.Got)
	}
	if !strings.Contains(got.Err, "0x101001") {
		t.Errorf("the console host's handle after Protect: %s; want the owner's three rights, 0x101001", got.Err)
	}
}

// processTree is root and every process started by it or by one it started,
// console hosts among them, as the system lists them now.
func processTree(root uint32) map[uint32]string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	parent, name := map[uint32]uint32{}, map[uint32]string{}
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		parent[pe.ProcessID], name[pe.ProcessID] = pe.ParentProcessID, strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
	}
	tree := map[uint32]string{root: name[root]}
	for grew := true; grew; {
		grew = false
		for pid, p := range parent {
			if _, in := tree[p]; in && tree[pid] == "" && pid != p {
				tree[pid], grew = name[pid], true
			}
		}
	}
	return tree
}

// visibleWindows lists the top-level windows on the desktop that are shown
// and belong to a process of tree: "class of program (pid)".
func visibleWindows(tree map[uint32]string) (shown []string) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	each := windows.NewCallback(func(hwnd, _ uintptr) uintptr {
		if v, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd); v == 0 {
			return 1
		}
		var pid uint32
		_, _, _ = user32.NewProc("GetWindowThreadProcessId").Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if program, in := tree[pid]; in {
			class := make([]uint16, 128)
			_, _, _ = user32.NewProc("GetClassNameW").Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
			shown = append(shown, fmt.Sprintf("%s of %s (%d)", windows.UTF16ToString(class), program, pid))
		}
		return 1
	})
	_, _, _ = user32.NewProc("EnumWindows").Call(each, 0)
	return shown
}

// consoleHostsOf lists the console hosts started for pid: its children named
// conhost.exe, or OpenConsole.exe under Windows Terminal.
func consoleHostsOf(pid uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var out []uint32
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		name := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		if pe.ParentProcessID == pid && (name == "conhost.exe" || name == "openconsole.exe") {
			out = append(out, pe.ProcessID)
		}
	}
	return out
}

// handleEntry is SYSTEM_HANDLE_TABLE_ENTRY_INFO_EX: one handle of one process.
type handleEntry struct {
	Object     uintptr
	PID        uintptr
	Handle     uintptr
	Granted    uint32
	BackTrace  uint16
	TypeIndex  uint16
	Attributes uint32
	Reserved   uint32
}

// systemHandles lists every handle on the system, with the process that
// holds it and the access it carries. Any user may ask.
func systemHandles() ([]handleEntry, error) {
	size := uint32(1 << 22)
	for {
		buf := make([]byte, size)
		var n uint32
		err := windows.NtQuerySystemInformation(64 /* SystemExtendedHandleInformation */, unsafe.Pointer(&buf[0]), size, &n)
		if err == windows.STATUS_INFO_LENGTH_MISMATCH {
			size = n + 1<<20
			continue
		}
		if err != nil {
			return nil, err
		}
		count := *(*uintptr)(unsafe.Pointer(&buf[0]))
		return unsafe.Slice((*handleEntry)(unsafe.Pointer(&buf[2*unsafe.Sizeof(uintptr(0))])), count), nil
	}
}

func init() {
	// The attempt: open the victim's console host, copy the handle it holds
	// on the victim, and read the marker through it. First in the table: the
	// last attempts end the victim.
	attempts = append([]attempt{{"copy the console host's handle to it (conhost.exe)", func(v victimInfo) (string, error) {
		hosts := consoleHostsOf(uint32(v.PID))
		if len(hosts) == 0 {
			return "", errors.New("it has no console host of its own (it shares a console someone else started)")
		}
		mine, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(v.PID))
		if err != nil {
			return "", err
		}
		defer windows.CloseHandle(mine)
		all, err := systemHandles()
		if err != nil {
			return "", err
		}
		processType := uint16(0) // the type number of a process handle, from the one just opened
		for _, e := range all {
			if uint32(e.PID) == windows.GetCurrentProcessId() && windows.Handle(e.Handle) == mine {
				processType = e.TypeIndex
			}
		}
		same := windows.NewLazySystemDLL("kernelbase.dll").NewProc("CompareObjectHandles")
		var held []string
		for _, host := range hosts {
			hp, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE, false, host)
			if err != nil {
				return "", fmt.Errorf("open its console host: %w", err)
			}
			defer windows.CloseHandle(hp)
			for _, e := range all {
				if uint32(e.PID) != host || e.TypeIndex != processType {
					continue
				}
				var dup windows.Handle
				if windows.DuplicateHandle(hp, windows.Handle(e.Handle), windows.CurrentProcess(), &dup, 0, false, windows.DUPLICATE_SAME_ACCESS) != nil {
					continue
				}
				if r, _, _ := same.Call(uintptr(dup), uintptr(mine)); r == 0 {
					windows.CloseHandle(dup)
					continue
				}
				held = append(held, fmt.Sprintf("%#x", e.Granted))
				if e.Granted&windows.PROCESS_VM_READ != 0 {
					b := make([]byte, v.Len)
					var n uintptr
					err := windows.ReadProcessMemory(dup, v.Addr, &b[0], uintptr(len(b)), &n)
					windows.CloseHandle(dup)
					if err == nil && string(b) == v.Marker {
						return fmt.Sprintf("read the marker through the console host's handle (%#x)", e.Granted), nil
					}
					return "", fmt.Errorf("the console host's handle carries %#x but the read failed: %v", e.Granted, err)
				}
				windows.CloseHandle(dup)
			}
		}
		return "", fmt.Errorf("the console host's handle to it carries %s: nothing to read with", strings.Join(held, ", "))
	}}}, attempts...)
}
