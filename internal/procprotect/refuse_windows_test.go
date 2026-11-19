package procprotect

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var controlIDs = []string{"windows.thread-dacl", "windows.process-dacl", "windows.console-host", "windows.wer-exclusion"}

// What the user's other programs may still do to a protected Tenant: Task
// Manager lists it and ends it, and a program can wait for it to exit.
var stillAllowed = map[string]bool{
	"list it and read its path (PROCESS_QUERY_LIMITED_INFORMATION)":                 true,
	"wait for it to exit (SYNCHRONIZE)":                                             true,
	"read its account (its token, TOKEN_QUERY; Task Manager's User name)":           true,
	"copy its token (TOKEN_DUPLICATE; the copy is no more than the attacker's own)": true,
	"read its token as the console host does (TOKEN_READ)":                          true,
	"end it (TerminateProcess, Task Manager's End task)":                            true,
}

// attackerTooStrong: an elevated administrator and SYSTEM are let in by the
// access list itself, by design.
func attackerTooStrong() string {
	if windows.GetCurrentProcessToken().IsElevated() {
		return "running elevated: the access list gives Administrators and SYSTEM full access, which no control here stops"
	}
	return ""
}

// werExclusion reports whether this test binary's name is in the user's list
// of programs Windows Error Reporting leaves alone.
func werExclusion() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, werExcluded, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(filepath.Base(os.Args[0]))
	return err == nil && v != 0
}

// cleanupMachine takes this test binary's name back out of that list: a
// protected victim put it there, and it is the one thing these tests write
// outside their own processes.
func cleanupMachine() {
	name, _ := windows.UTF16PtrFromString(filepath.Base(os.Args[0]))
	_, _, _ = windows.NewLazySystemDLL("wer.dll").NewProc("WerRemoveExcludedApplication").Call(uintptr(unsafe.Pointer(name)), 0)
}

// victimReady makes the Go runtime create more threads, after Protect: each
// goroutine below keeps an operating-system thread to itself for good.
func victimReady() {
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			runtime.LockOSThread()
			wg.Done()
			select {}
		}()
	}
	wg.Wait()
	if os.Getenv("PROCPROTECT_BUSY") != "" {
		busy()
	}
}

// busy is a Tenant at work: several goroutines start commands at once, and
// while each is inside Ordinary, making pipes, it asks the runtime for new
// threads. The threads stay (80 of them), for the attacker to try.
func busy() {
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				_, _ = Output(exec.Command("cmd", "/c", "echo x"))
				Ordinary(func() {
					for range 4 {
						go func() {
							runtime.LockOSThread()
							select {}
						}()
						_ = pipeErr()
					}
				})
			}
		}()
	}
	wg.Wait()
}

func sddl(h windows.Handle) string {
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.Replace(sd.String(), "D:P", "D:", 1) // the entries, protected from inheritance or not
}

func defaultDACL() string {
	acl, err := tokenDefaultDACLOf(windows.GetCurrentProcessToken())
	if err != nil {
		return "unreadable: " + err.Error()
	}
	sd, _ := windows.NewSecurityDescriptor()
	_ = sd.SetDACL(acl, true, false)
	return sd.String()
}

// pipeWorks makes a pipe the way os/exec does and says how that went.
func pipeWorks() string {
	r, w, err := os.Pipe()
	if err != nil {
		return err.Error()
	}
	r.Close()
	w.Close()
	return "ok"
}

func threadIDs(pid uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var out []uint32
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err := windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID == pid {
			out = append(out, te.ThreadID)
		}
	}
	return out
}

// selfState is what the process can see of itself: its own access list, the
// one new objects get, and the access list of every thread a goroutine can
// be made to run on.
func selfState() map[string]string {
	seen := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread() // a thread of its own; it ends with the goroutine
			h, _ := windows.GetCurrentThread()
			s := sddl(h)
			mu.Lock()
			seen[s] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	var threads []string
	for s := range seen {
		threads = append(threads, s)
	}
	sort.Strings(threads)
	denied := 0
	ids := threadIDs(windows.GetCurrentProcessId())
	for _, id := range ids {
		h, err := windows.OpenThread(windows.THREAD_SET_CONTEXT, false, id)
		if err != nil {
			denied++
			continue
		}
		windows.CloseHandle(h)
	}
	state := map[string]string{
		"process access list":                        sddl(windows.CurrentProcess()),
		"access list new objects get":                defaultDACL(),
		"access lists of its threads":                strings.Join(threads, " | "),
		"a pipe made the way os/exec makes it":       pipeWorks(),
		"info:threads":                               fmt.Sprintf("%d, of which %d refuse the process itself THREAD_SET_CONTEXT by id", len(ids), denied),
		"info:excluded from Windows Error Reporting": fmt.Sprint(werExclusion()),
	}
	if os.Getenv(envConsole) != "" && os.Getenv(envHelper) == "victim" {
		for k, v := range consoleState() {
			state[k] = v
		}
	}
	if os.Getenv(envLinger) != "" && os.Getenv(envHelper) == "victim" {
		// A console command that stays a while, started the way Tenant
		// starts every command. The test ends it.
		c := exec.Command("ping", "-n", "20", "127.0.0.1") // one process: ending it leaves nothing
		if err := Start(c); err != nil {
			state["info:a command that stays"] = err.Error()
		} else {
			state["info:a command that stays"] = fmt.Sprint(c.Process.Pid)
		}
	}
	return state
}

// childState is selfState for a process the victim started, plus whether it
// can still run a command of its own through a pipe.
func childState() map[string]string {
	s := selfState()
	out, err := exec.Command("cmd", "/c", "echo hi | findstr hi").CombinedOutput()
	s["a command of its own, through a pipe"] = fmt.Sprintf("%q %v", strings.TrimSpace(string(out)), err)
	return s
}

// A function every process has at the same address, that takes anything and
// does nothing: what a queued call or a new thread is pointed at.
var harmless = kernel32.NewProc("GetCurrentProcessId").Addr()

// call calls a kernel32 function that returns a BOOL.
func call(name string, args ...uintptr) error {
	if r, _, err := kernel32.NewProc(name).Call(args...); uint32(r) == 0 {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// suspend is SuspendThread, which returns the thread's suspend count, or -1.
func suspend(h windows.Handle) error {
	if r, _, err := kernel32.NewProc("SuspendThread").Call(uintptr(h)); uint32(r) == ^uint32(0) {
		return fmt.Errorf("SuspendThread: %w", err)
	}
	return nil
}

// openProcess opens the victim with access and runs do on the handle.
func openProcess(v victimInfo, access uint32, do func(windows.Handle) (string, error)) (string, error) {
	h, err := windows.OpenProcess(access, false, uint32(v.PID))
	if err != nil {
		return "", fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(h)
	if do == nil {
		return "opened", nil
	}
	return do(h)
}

// openToken opens the victim's token with access, through the one right on
// the process its owner keeps for that, and runs do on it.
func openToken(v victimInfo, access uint32, do func(windows.Token) (string, error)) (string, error) {
	return openProcess(v, windows.PROCESS_QUERY_LIMITED_INFORMATION, func(h windows.Handle) (string, error) {
		var t windows.Token
		if err := windows.OpenProcessToken(h, access, &t); err != nil {
			return "", fmt.Errorf("OpenProcessToken: %w", err)
		}
		defer t.Close()
		if do == nil {
			return "opened", nil
		}
		return do(t)
	})
}

// everyThread tries to open each of the victim's threads with access, and
// runs do on the first that opens. It is refused only when no thread opens.
func everyThread(v victimInfo, access uint32, do func(windows.Handle) error) (string, error) {
	ids := threadIDs(uint32(v.PID))
	opened := 0
	var doErr error
	for _, id := range ids {
		h, err := windows.OpenThread(access, false, id)
		if err != nil {
			continue
		}
		if opened++; opened == 1 && do != nil {
			doErr = do(h)
		}
		windows.CloseHandle(h)
	}
	if opened == 0 {
		return "", fmt.Errorf("OpenThread: access denied on all %d threads", len(ids))
	}
	return fmt.Sprintf("opened %d of %d threads", opened, len(ids)), doErr
}

// threadContext is CONTEXT on amd64 (1232 bytes, 16-byte aligned), with
// ContextFlags at 0x30. Elsewhere the attempt stops at the open.
func getSetContext(h windows.Handle, set bool) error {
	if runtime.GOARCH != "amd64" {
		return nil
	}
	buf := make([]byte, 1232+16)
	ctx := buf[(16-uintptr(unsafe.Pointer(&buf[0]))%16)%16:]
	binary.LittleEndian.PutUint32(ctx[0x30:], 0x100003) // CONTEXT_CONTROL | CONTEXT_INTEGER
	p := uintptr(unsafe.Pointer(&ctx[0]))
	if err := suspend(h); err != nil {
		return err
	}
	defer windows.ResumeThread(h)
	if err := call("GetThreadContext", uintptr(h), p); err != nil {
		return err
	}
	if set { // the context it already has: a real call that changes nothing
		return call("SetThreadContext", uintptr(h), p)
	}
	return nil
}

const threadGetSetSuspend = windows.THREAD_GET_CONTEXT | windows.THREAD_SET_CONTEXT | windows.THREAD_SUSPEND_RESUME

var attempts = []attempt{
	{"ReadProcessMemory (PROCESS_VM_READ)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.PROCESS_VM_READ, func(h windows.Handle) (string, error) {
			if v.Len == 0 { // a process with no marker (a victim's helper): the open itself is the way in
				return "opened for reading", nil
			}
			b := make([]byte, v.Len)
			var n uintptr
			if err := windows.ReadProcessMemory(h, v.Addr, &b[0], uintptr(len(b)), &n); err != nil {
				return "", err
			}
			if string(b) != v.Marker {
				return "", errors.New("read succeeded but the marker is not in it")
			}
			return "read the marker", nil
		})
	}},
	{"MiniDumpWriteDump (Task Manager's Create dump file, procdump)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, func(h windows.Handle) (string, error) {
			f, err := os.CreateTemp("", "ten535-dump-*.dmp")
			if err != nil {
				return "", err
			}
			defer os.Remove(f.Name())
			defer f.Close()
			// MiniDumpNormal: the smallest dump, which needs the same access as a full one.
			if r, _, err := windows.NewLazySystemDLL("dbghelp.dll").NewProc("MiniDumpWriteDump").Call(
				uintptr(h), uintptr(v.PID), f.Fd(), 0, 0, 0, 0); r == 0 {
				return "", fmt.Errorf("MiniDumpWriteDump: %w", err)
			}
			st, _ := f.Stat()
			return fmt.Sprintf("wrote a %d-byte dump", st.Size()), nil
		})
	}},
	{"GetThreadContext on its threads", func(v victimInfo) (string, error) {
		if _, err := everyThread(v, windows.THREAD_GET_CONTEXT, nil); err != nil {
			return "", err
		}
		return everyThread(v, threadGetSetSuspend, func(h windows.Handle) error { return getSetContext(h, false) })
	}},
	{"SetThreadContext on its threads", func(v victimInfo) (string, error) {
		if _, err := everyThread(v, windows.THREAD_SET_CONTEXT, nil); err != nil {
			return "", err
		}
		return everyThread(v, threadGetSetSuspend, func(h windows.Handle) error { return getSetContext(h, true) })
	}},
	{"SuspendThread on its threads", func(v victimInfo) (string, error) {
		return everyThread(v, windows.THREAD_SUSPEND_RESUME, func(h windows.Handle) error {
			if err := suspend(h); err != nil {
				return err
			}
			_, err := windows.ResumeThread(h)
			return err
		})
	}},
	{"QueueUserAPC on its threads", func(v victimInfo) (string, error) {
		return everyThread(v, windows.THREAD_SET_CONTEXT, func(h windows.Handle) error {
			return call("QueueUserAPC", harmless, uintptr(h), 0)
		})
	}},
	{"CreateRemoteThread", func(v victimInfo) (string, error) {
		const access = windows.PROCESS_CREATE_THREAD | windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_OPERATION | windows.PROCESS_VM_WRITE | windows.PROCESS_VM_READ
		return openProcess(v, access, func(h windows.Handle) (string, error) {
			r, _, err := kernel32.NewProc("CreateRemoteThread").Call(uintptr(h), 0, 0, harmless, 0, 0, 0)
			if r == 0 {
				return "", fmt.Errorf("CreateRemoteThread: %w", err)
			}
			windows.CloseHandle(windows.Handle(r))
			return "started a thread in it", nil
		})
	}},
	{"duplicate its handles (PROCESS_DUP_HANDLE)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.PROCESS_DUP_HANDLE, nil)
	}},
	{"rewrite the process's access list as its owner (WRITE_DAC)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.WRITE_DAC, nil)
	}},
	{"take ownership of the process (WRITE_OWNER)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.WRITE_OWNER, nil)
	}},
	{"rewrite a thread's access list as its owner (WRITE_DAC)", func(v victimInfo) (string, error) {
		return everyThread(v, windows.WRITE_DAC, nil)
	}},
	{"read the process's access list (READ_CONTROL)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.READ_CONTROL, nil)
	}},
	// The process's token is an object of its own, reached through the right
	// to read the process's basic details. It holds the default access list
	// the Go runtime's new threads get: whoever can write it can undo that.
	{"reset the access list its new threads get (its token, TOKEN_ADJUST_DEFAULT)", func(v victimInfo) (string, error) {
		return openToken(v, windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, func(t windows.Token) (string, error) {
			acl, err := tokenDefaultDACLOf(t)
			if err != nil {
				return "", err
			}
			def := tokenDefaultDACL{DefaultDacl: acl} // the list it already has: a real call that changes nothing
			if err := windows.SetTokenInformation(t, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&def)), uint32(unsafe.Sizeof(def))); err != nil {
				return "", err
			}
			return "set its default access list", nil
		})
	}},
	{"rewrite its token's access list as its owner (WRITE_DAC)", func(v victimInfo) (string, error) {
		return openToken(v, windows.WRITE_DAC, nil)
	}},
	{"change its token's privileges (TOKEN_ADJUST_PRIVILEGES)", func(v victimInfo) (string, error) {
		return openToken(v, windows.TOKEN_ADJUST_PRIVILEGES, nil)
	}},
	{"copy its token (TOKEN_DUPLICATE; the copy is no more than the attacker's own)", func(v victimInfo) (string, error) {
		return openToken(v, windows.TOKEN_DUPLICATE, nil)
	}},
	{"read its token as the console host does (TOKEN_READ)", func(v victimInfo) (string, error) {
		return openToken(v, windows.TOKEN_READ, nil)
	}},
	{"read its account (its token, TOKEN_QUERY; Task Manager's User name)", func(v victimInfo) (string, error) {
		return openToken(v, windows.TOKEN_QUERY, func(t windows.Token) (string, error) {
			u, err := t.GetTokenUser()
			if err != nil {
				return "", err
			}
			return u.User.Sid.String(), nil
		})
	}},
	{"list it and read its path (PROCESS_QUERY_LIMITED_INFORMATION)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.PROCESS_QUERY_LIMITED_INFORMATION, func(h windows.Handle) (string, error) {
			buf := make([]uint16, windows.MAX_PATH)
			n := uint32(len(buf))
			if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
				return "", err
			}
			return filepath.Base(windows.UTF16ToString(buf[:n])), nil
		})
	}},
	{"suspend the whole process (NtSuspendProcess)", func(v victimInfo) (string, error) {
		return openProcess(v, 0x0800 /* PROCESS_SUSPEND_RESUME */, func(h windows.Handle) (string, error) {
			ntdll := windows.NewLazySystemDLL("ntdll.dll")
			if st, _, _ := ntdll.NewProc("NtSuspendProcess").Call(uintptr(h)); st != 0 {
				return "", windows.NTStatus(st)
			}
			_, _, _ = ntdll.NewProc("NtResumeProcess").Call(uintptr(h))
			return "suspended, and resumed", nil
		})
	}},
	{"wait for it to exit (SYNCHRONIZE)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.SYNCHRONIZE, nil)
	}},
	// The last two end the victim or may: a debugger that detaches leaves a
	// break-in thread behind.
	{"DebugActiveProcess (attach a debugger)", func(v victimInfo) (string, error) {
		runtime.LockOSThread() // a debugger is one thread
		defer runtime.UnlockOSThread()
		if err := call("DebugActiveProcess", uintptr(v.PID)); err != nil {
			return "", err
		}
		_ = call("DebugSetProcessKillOnExit", 0)
		return "attached", call("DebugActiveProcessStop", uintptr(v.PID))
	}},
	{"end it (TerminateProcess, Task Manager's End task)", func(v victimInfo) (string, error) {
		return openProcess(v, windows.PROCESS_TERMINATE, func(h windows.Handle) (string, error) {
			return "ended", windows.TerminateProcess(h, 1)
		})
	}},
}

const restricted = "D:(A;;0x1fffff;;;SY)(A;;0x1fffff;;;BA)(A;;0x101001;;;OW)"

// TestWhatAProcessSeesOfItself: after Protect the process and every thread
// the Go runtime made, before and after, carry the restricted access list;
// a process it starts the way Tenant starts a command is the same as one an
// unprotected process starts, and can run commands of its own.
func TestWhatAProcessSeesOfItself(t *testing.T) {
	if Disabled {
		t.Skip("built with -tags debugattach: the controls are compiled out")
	}
	plain, v := startVictim(t, "plain"), startVictim(t, "protected")
	want := map[string]string{
		"process access list":         restricted,
		"access list new objects get": "D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x101001;;;OW)",
		"access lists of its threads": restricted,
	}
	for k, w := range want {
		if v.Self[k] != w {
			t.Errorf("victim: %s = %q, want %q", k, v.Self[k], w)
		}
	}
	t.Logf("victim: threads: %s", v.Self["info:threads"])
	t.Logf("victim: a pipe made the way os/exec makes it, outside Ordinary: %s", v.Self["a pipe made the way os/exec makes it"])
	if v.Child["error"] != "" {
		t.Fatalf("the victim could not start a command: %s", v.Child["error"])
	}
	for k, w := range plain.Child {
		if !strings.HasPrefix(k, "info:") && v.Child[k] != w {
			t.Errorf("a command the protected victim started: %s = %q; one an unprotected process starts has %q", k, v.Child[k], w)
		}
	}
	if got := v.Child["a command of its own, through a pipe"]; got != `"hi" <nil>` {
		t.Errorf("a command the victim started ran one of its own through a pipe: %s", got)
	}
	t.Logf("a command the victim started: %v", v.Child)
}

// TestThreadsStayClosedWhileCommandsStart: Ordinary lends one thread the
// user's ordinary default for as long as a command takes to start. No thread
// that starts meanwhile, on any goroutine, may get that default.
func TestThreadsStayClosedWhileCommandsStart(t *testing.T) {
	if Disabled {
		t.Skip("built with -tags debugattach")
	}
	if why := attackerTooStrong(); why != "" {
		t.Skip(why)
	}
	v := startVictim(t, "protected", "PROCPROTECT_BUSY=1")
	if got := v.Self["access lists of its threads"]; got != restricted {
		t.Errorf("the victim's threads carry %q, want only %q", got, restricted)
	}
	t.Logf("victim: threads: %s", v.Self["info:threads"])
	got := attack(t, v)
	for _, a := range attempts {
		if o := got[a.name]; o.OK && !stillAllowed[a.name] {
			t.Errorf("%q was NOT refused (got %q)", a.name, o.Got)
		} else if !o.OK {
			t.Logf("refused: %s: %s", a.name, o.Err)
		}
	}
}

// TestExcludedFromWindowsErrorReporting: Protect puts the program's name in
// the user's exclusion list. This test binary's own name is what goes in,
// and it comes out again here and when the tests end.
func TestExcludedFromWindowsErrorReporting(t *testing.T) {
	if Disabled {
		t.Skip("built with -tags debugattach")
	}
	cleanupMachine()
	if werExclusion() {
		t.Fatal("could not clear the exclusion before the test")
	}
	startVictim(t, "plain")
	if werExclusion() {
		t.Fatal("a process that did not call Protect registered the exclusion")
	}
	startVictim(t, "protected")
	if !werExclusion() {
		t.Fatalf(`after Protect, HKCU\%s has no value %q`, werExcluded, filepath.Base(os.Args[0]))
	}
	cleanupMachine()
	if werExclusion() {
		t.Fatal("the exclusion is still there after the test removed it")
	}
}

// TestProtectThisProcess runs Protect in the test process itself: first with
// every control forced to fail (nothing changes), then for real.
func TestProtectThisProcess(t *testing.T) {
	before := selfState()
	for _, id := range controlIDs {
		ForceFail(id, "forced")
	}
	if m := Protect().Missing(); !Disabled && (len(m) != len(controlIDs) || m[0].Reason != "forced") {
		t.Fatalf("forced: missing = %+v", m)
	}
	if after := selfState(); after["process access list"] != before["process access list"] || after["access list new objects get"] != before["access list new objects get"] {
		t.Fatalf("a forced failure still changed the process: %v", after)
	}
	for _, id := range controlIDs {
		ForceFail(id, "")
	}
	if Disabled {
		t.Skip("built with -tags debugattach: nothing to apply")
	}
	r := Protect()
	if len(r.Missing()) != 0 || len(r.Controls) != len(controlIDs) {
		t.Fatalf("Protect() = %+v", r)
	}
	if r2 := Protect(); len(r2.Missing()) != 0 { // a second call changes nothing
		t.Fatalf("a second Protect() = %+v", r2)
	}
	s := selfState()
	if s["process access list"] != restricted || s["access lists of its threads"] != restricted {
		t.Fatalf("after Protect: %v", s)
	}
	// The side effect every command Tenant starts has to allow for: a pipe
	// made with the default access list cannot be opened by a user who is
	// not an administrator. Ordinary is where one can be.
	if !windows.GetCurrentProcessToken().IsElevated() && !errors.Is(pipeErr(), windows.ERROR_ACCESS_DENIED) {
		t.Errorf("os.Pipe outside Ordinary: %v, want access denied", pipeErr())
	}
	Ordinary(func() {
		if err := pipeErr(); err != nil {
			t.Errorf("os.Pipe inside Ordinary: %v", err)
		}
		Ordinary(func() { // nested: the inner one must not end the outer one's
			if err := pipeErr(); err != nil {
				t.Errorf("os.Pipe inside a nested Ordinary: %v", err)
			}
		})
		if err := pipeErr(); err != nil {
			t.Errorf("os.Pipe after a nested Ordinary returned: %v", err)
		}
	})
	// What Tenant does in its own process still works: Windows copies the
	// process's token to answer a group question, the data-protection
	// service is asked to seal a secret, and names resolve. And all of it
	// works inside Ordinary, which is where every command and every tool
	// runs (cmd/tenant's main, toolmux's Dispatch).
	everyday := func(where string) {
		users, _ := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
		if _, err := windows.Token(0).IsMember(users); err != nil {
			t.Errorf("%sCheckTokenMembership on the process's own token: %v", where, err)
		}
		if _, err := user.Current(); err != nil {
			t.Errorf("%suser.Current: %v", where, err)
		}
		secret := []byte("ten535")
		in := windows.DataBlob{Size: uint32(len(secret)), Data: &secret[0]}
		var sealed, opened windows.DataBlob
		if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &sealed); err != nil {
			t.Errorf("%sCryptProtectData: %v", where, err)
		} else {
			if err := windows.CryptUnprotectData(&sealed, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &opened); err != nil ||
				string(unsafe.Slice(opened.Data, opened.Size)) != "ten535" {
				t.Errorf("%sCryptUnprotectData: %v", where, err)
			}
			windows.LocalFree(windows.Handle(unsafe.Pointer(sealed.Data)))
			if opened.Data != nil {
				windows.LocalFree(windows.Handle(unsafe.Pointer(opened.Data)))
			}
		}
		if _, err := net.LookupHost("localhost"); err != nil {
			t.Errorf("%sLookupHost: %v", where, err)
		}
		if ln, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Errorf("%sListen: %v", where, err)
		} else {
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Errorf("%sDial: %v", where, err)
			} else {
				c.Close()
			}
			ln.Close()
		}
	}
	everyday("")
	Ordinary(func() { everyday("inside Ordinary: ") })
	// A thread Windows itself starts from inside Ordinary (CreateThread, as
	// a system library does) still gets the restricted list: a new thread's
	// list comes from the process's token, not from the one its creator's
	// thread carries. Running whole commands inside Ordinary rests on this.
	startedInside := "CreateThread failed"
	Ordinary(func() {
		if h, _, _ := kernel32.NewProc("CreateThread").Call(0, 0, kernel32.NewProc("Sleep").Addr(), 200, 0, 0); h != 0 {
			startedInside = sddl(windows.Handle(h))
			windows.CloseHandle(windows.Handle(h))
		}
	})
	if startedInside != restricted {
		t.Errorf("a thread Windows starts from inside Ordinary carries %q, want %q", startedInside, restricted)
	}
	f, err := os.CreateTemp(t.TempDir(), "made-after-protect")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	f.Close()
	if g, err := os.OpenFile(f.Name(), os.O_RDWR, 0); err != nil {
		t.Errorf("a file made after Protect cannot be opened again for writing: %v", err)
	} else {
		g.Close()
	}
	// Every way a command is started still works, and its child is ordinary.
	echo := func() *exec.Cmd { return exec.Command("cmd", "/c", "echo ok") }
	if out, err := Output(echo()); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Errorf("Output: %q, %v", out, err)
	}
	if out, err := CombinedOutput(echo()); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Errorf("CombinedOutput: %q, %v", out, err)
	}
	if err := Run(echo()); err != nil {
		t.Errorf("Run: %v", err)
	}
	c := echo()
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP} // a caller's own attributes are kept
	if err := Start(c); err != nil {
		t.Errorf("Start: %v", err)
	} else if err := c.Wait(); err != nil || c.SysProcAttr.CreationFlags != windows.CREATE_NEW_PROCESS_GROUP {
		t.Errorf("Start: wait %v, flags %#x", err, c.SysProcAttr.CreationFlags)
	}
}

func pipeErr() error {
	r, w, err := os.Pipe()
	if err == nil {
		r.Close()
		w.Close()
	}
	return err
}

// TestNowReadsTheStateAgain: a control another program of the user undoes
// while Tenant runs is not reported as in force any more (TEN-535 hardening:
// the tester deleted the error-reporting exclusion under a running serve, and
// the report did not change). Now reads again what can be read, and says
// which controls it could not.
func TestNowReadsTheStateAgain(t *testing.T) {
	if Disabled {
		if r := Now(); !r.Disabled || len(r.Controls) != 0 {
			t.Fatalf("a debugattach build's Now() = %+v", r)
		}
		t.Skip("built with -tags debugattach: nothing to read again")
	}
	if m := Protect().Missing(); len(m) != 0 {
		t.Fatalf("Protect: %+v", m)
	}
	t.Cleanup(cleanupMachine)
	now := Now()
	if len(now.Missing()) != 0 || now.PID != os.Getpid() || len(now.System) != 1 {
		t.Fatalf("Now() = %+v", now)
	}
	asAtStart := map[string]bool{}
	for _, c := range now.Controls {
		asAtStart[c.ID] = c.AsAtStart
	}
	// Read again in full: the process's list and the exclusion. Not, or not
	// wholly: the threads that exist, and the console host's handle.
	want := map[string]bool{"windows.thread-dacl": true, "windows.process-dacl": false, "windows.console-host": true, "windows.wer-exclusion": false}
	if fmt.Sprint(asAtStart) != fmt.Sprint(want) {
		t.Errorf("as at start: %v, want %v", asAtStart, want)
	}

	// What the tester did: the value is the user's own to delete.
	k, err := registry.OpenKey(registry.CURRENT_USER, werExcluded, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	err = k.DeleteValue(filepath.Base(os.Args[0]))
	k.Close()
	if err != nil {
		t.Fatal(err)
	}
	m := Now().Missing()
	const reason = "it was in force when Tenant started and is not any more: this program is not on the user's Windows Error Reporting exclusion list"
	if len(m) != 1 || m[0].ID != "windows.wer-exclusion" || !m[0].Undone || m[0].Reason != reason || m[0].Leaves == "" {
		t.Fatalf("with the exclusion deleted, Now() reports as not in force: %+v", m)
	}
	if again := Protect(); len(again.Missing()) != 0 { // what a restart does
		t.Fatalf("Protect, again: %+v", again.Missing())
	}
	if m := Now().Missing(); len(m) != 0 {
		t.Errorf("with the exclusion back, Now() still reports: %+v", m)
	}

	// An access list that lets the user in is not the restricted one.
	for sddl, closed := range map[string]bool{
		restrictedSDDL:               true,
		"D:(A;;GA;;;SY)(A;;GA;;;BA)": true,
		"D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x101001;;;OW)(A;;GA;;;WD)": false,
		"D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x1fffff;;;OW)":             false,
		"D:(A;;GA;;;SY)(D;;GA;;;WD)":                               false, // not a list Protect writes
	} {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		if dacl, _, _ := sd.DACL(); closedToTheUser(dacl) != closed {
			t.Errorf("closedToTheUser(%s) = %v, want %v", sddl, !closed, closed)
		}
	}
	if closedToTheUser(nil) {
		t.Error("no access list at all counts as closed")
	}
}
