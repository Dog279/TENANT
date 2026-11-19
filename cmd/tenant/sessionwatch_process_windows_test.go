//go:build windows

package main

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// serve's window is in a helper process (TEN-535 hardening): serve itself
// has no window and no thread that reads window messages, so a window hook
// another program sets on one of its threads never runs. These run the real
// binary, as two processes; stop serve the two ways the window exists for,
// a stop a command asks for and the end of the user's session; and set a
// hook on every thread of serve.
//
// serve runs without --hidden here: hiding the console is the helper's too,
// and the console of a test is the one the test was started from.

// childrenOf lists the processes parent started.
func childrenOf(t *testing.T, parent uint32) (pids []uint32) {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snap)
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ParentProcessID == parent {
			pids = append(pids, pe.ProcessID)
		}
	}
	return pids
}

// loadedModules lists the libraries loaded in the process, lower case. A
// process whose access list is the restricted one refuses to be asked.
func loadedModules(pid uint32) ([]string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPMODULE|windows.TH32CS_SNAPMODULE32, pid)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var out []string
	me := windows.ModuleEntry32{Size: uint32(unsafe.Sizeof(windows.ModuleEntry32{}))}
	for err := windows.Module32First(snap, &me); err == nil; err = windows.Module32Next(snap, &me) {
		out = append(out, strings.ToLower(windows.UTF16ToString(me.Module[:])))
	}
	return out, nil
}

// threadsOf lists the thread ids of the process.
func threadsOf(t *testing.T, pid uint32) (ids []uint32) {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snap)
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err := windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID == pid {
			ids = append(ids, te.ThreadID)
		}
	}
	return ids
}

func TestServeStopsThroughItsHelpersWindow(t *testing.T) {
	first, second := forcedControls(t) // the error-reporting exclusion, forced off: nothing is written to the registry
	bin := protectTestBinary(t)
	const loaded = "a window hook on every thread of serve"
	for _, how := range []string{"a stop asked for by a command (WM_CLOSE)", "the end of the session (WM_ENDSESSION)", loaded} {
		t.Run(how, func(t *testing.T) {
			force := first + "=forced by the test"
			if how == loaded { // its libraries can be listed only with its access list left open
				force += ";" + second + "=forced by the test"
			}
			u := newTenantUnderTest(t, bin)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := u.command(ctx, []string{"TENANT_TEST_TERMINAL=1", "TENANT_TEST_FORCE_FAIL=" + force}, u.startArgs("serve")...)
			var errb, said syncBuffer
			cmd.Stdin, cmd.Stderr = strings.NewReader("y\n"), &errb
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			up, ended := make(chan bool, 1), make(chan struct{})
			var exit error
			go func() {
				sc := bufio.NewScanner(pipe)
				told := false
				for sc.Scan() {
					_, _ = said.Write([]byte(sc.Text() + "\n"))
					if !told && strings.Contains(sc.Text(), "running 24/7") {
						told = true
						up <- true
					}
				}
				if !told {
					up <- false
				}
				exit = cmd.Wait()
				close(ended)
			}()
			t.Cleanup(func() { // also when the test fails: serve, and with it its helper
				_ = cmd.Process.Kill()
				<-ended
			})
			if !<-up {
				t.Fatalf("serve did not come up:\nstdout:\n%s\nstderr:\n%s", said.String(), errb.String())
			}
			serve := uint32(cmd.Process.Pid)

			// The window is the helper's, a child of serve; serve has none.
			hwnd := findWindow(t, u.data)
			if hwnd == 0 {
				t.Fatal("no window for serve's data dir")
			}
			var owner uint32
			_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
			if owner == serve || !slices.Contains(childrenOf(t, serve), owner) {
				t.Fatalf("the window belongs to process %d; want a child of serve (%d), not serve itself", owner, serve)
			}

			switch {
			case how == loaded:
				// A hook's library is mapped into the process that owns the
				// hooked thread when that thread next reads a window
				// message. The hook here is a function of a system library
				// serve does not have (it takes nothing and changes
				// nothing); it is set on every thread of serve that takes
				// one, a message is posted to each, and the library must
				// not turn up in serve.
				const lib = "dbghelp.dll"
				before, err := loadedModules(serve)
				if err != nil || len(before) < 5 {
					t.Fatalf("serve's libraries could not be listed: %v %v", before, err)
				}
				if slices.Contains(before, lib) {
					t.Fatalf("serve already has %s loaded: pick another library for this test", lib)
				}
				mod, err := windows.LoadLibrary(lib)
				if err != nil {
					t.Fatal(err)
				}
				hook, err := windows.GetProcAddress(mod, "SymGetOptions")
				if err != nil {
					t.Fatal(err)
				}
				runtime.LockOSThread() // a hook belongs to the thread that set it
				defer runtime.UnlockOSThread()
				threads, set := threadsOf(t, serve), 0
				for _, tid := range threads {
					hh, _, _ := user32.NewProc("SetWindowsHookExW").Call(3 /* WH_GETMESSAGE */, hook, uintptr(mod), uintptr(tid))
					if hh == 0 {
						continue // not a thread Windows keeps a message queue for
					}
					set++
					defer user32.NewProc("UnhookWindowsHookEx").Call(hh)
					_, _, _ = user32.NewProc("PostThreadMessageW").Call(uintptr(tid), 0 /* WM_NULL */, 0, 0)
				}
				time.Sleep(1500 * time.Millisecond)
				after, err := loadedModules(serve)
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(after, lib) {
					t.Errorf("a window hook loaded %s into serve", lib)
				}
				t.Logf("serve has %d threads; Windows took a hook on %d of them; none ran (user32.dll is in serve: %v)",
					len(threads), set, slices.Contains(after, "user32.dll"))
				if !askServeToStop(u.data, 60*time.Second) {
					t.Fatal("serve did not stop when asked through the window")
				}
			case strings.Contains(how, "WM_CLOSE"):
				if !askServeToStop(u.data, 60*time.Second) {
					t.Fatal("serve did not stop when asked through the window")
				}
			default:
				if r := sendWindow(hwnd, wmQueryEndSession, 0); r != 1 {
					t.Errorf("WM_QUERYENDSESSION answered %d, want 1", r)
				}
				sendWindow(hwnd, wmEndSession, 1) // answered once serve has returned, or after the wait
			}
			select {
			case <-ended:
				if exit != nil {
					t.Errorf("serve ended with %v, want a clean exit\nstdout:\n%s\nstderr:\n%s", exit, said.String(), errb.String())
				}
			case <-time.After(60 * time.Second):
				t.Fatalf("serve is still running\nstdout:\n%s\nstderr:\n%s", said.String(), errb.String())
			}
			if book := logBookText(t, filepath.Join(u.data, "logs")); strings.Contains(book, "ended unexpectedly") {
				t.Errorf("the log says a run ended unexpectedly:\n%s", book)
			}
			// The helper ends with serve.
			for deadline := time.Now().Add(10 * time.Second); findWindow(t, u.data) != 0; time.Sleep(50 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the helper's window is still there after serve ended")
				}
			}
		})
	}
}

// processTree is root and every process started by it or by one it started,
// console hosts among them, as the system lists them now.
func processTree(t *testing.T, root uint32) map[uint32]string {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
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
	each := windows.NewCallback(func(hwnd, _ uintptr) uintptr {
		if v, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd); v == 0 {
			return 1
		}
		var pid uint32
		_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
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

// A serve with no console at all (the shape of the service, and of a serve
// another program starts detached) must not put a window on the operator's
// desktop: not for its helper, not for the commands it runs as it starts and
// later. Windows gives a console program whose parent has no console a new
// one, window and all, and a window that opens takes the keyboard from
// whatever the operator is doing. This watches every process serve starts,
// from its first instant until it has stopped, and fails on any window that
// is shown.
func TestAServeWithNoConsoleShowsNoWindow(t *testing.T) {
	first, _ := forcedControls(t)
	bin := protectTestBinary(t)
	u := newTenantUnderTest(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := u.command(ctx, []string{"TENANT_TEST_TERMINAL=1", "TENANT_TEST_FORCE_FAIL=" + first + "=forced by the test"}, u.startArgs("serve")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
	var errb syncBuffer
	cmd.Stdin, cmd.Stderr = strings.NewReader("y\n"), &errb
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	serve := uint32(cmd.Process.Pid)
	ended := make(chan struct{})
	t.Cleanup(func() { // also when the test fails: serve, and with it its helper
		_ = cmd.Process.Kill()
		<-ended
	})
	// Watch from now on: a command serve runs at its start is gone in a moment.
	var mu sync.Mutex
	shown, started := map[string]bool{}, map[string]bool{}
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		for {
			tree := processTree(t, serve)
			mu.Lock()
			for _, w := range visibleWindows(tree) {
				shown[w] = true
			}
			for pid, program := range tree {
				if pid != serve {
					started[program] = true
				}
			}
			mu.Unlock()
			select {
			case <-ended:
				return
			case <-time.After(15 * time.Millisecond):
			}
		}
	}()
	up := make(chan bool, 1)
	go func() {
		defer close(ended)
		told := false
		for sc := bufio.NewScanner(pipe); sc.Scan(); {
			if !told && strings.Contains(sc.Text(), "running 24/7") {
				told = true
				up <- true
			}
		}
		if !told {
			up <- false
		}
		_ = cmd.Wait()
	}()
	if !<-up {
		t.Fatalf("serve did not come up:\n%s", errb.String())
	}
	if hosts := processTree(t, serve); hosts[serve] == "" {
		t.Fatal("serve is not in the process list")
	}
	hwnd := findWindow(t, u.data)
	if hwnd == 0 {
		t.Fatal("no window for serve's data dir")
	}
	var helper uint32
	_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&helper)))
	if !slices.Contains(childrenOf(t, serve), helper) {
		t.Fatalf("the window belongs to process %d, which is not a child of serve (%d)", helper, serve)
	}
	time.Sleep(time.Second) // what serve starts once it is up
	if !askServeToStop(u.data, 60*time.Second) {
		t.Fatal("serve did not stop when asked through the window")
	}
	select {
	case <-ended:
	case <-time.After(60 * time.Second):
		t.Fatal("serve is still running")
	}
	<-watching
	mu.Lock()
	defer mu.Unlock()
	if len(shown) != 0 {
		t.Errorf("a serve with no console showed windows on the desktop: %v", slices.Sorted(maps.Keys(shown)))
	}
	if !started[strings.ToLower(filepath.Base(bin))] {
		t.Errorf("the watch saw none of serve's own helper among what it started (%v): it proves nothing", slices.Sorted(maps.Keys(started)))
	}
	t.Logf("serve had no console and started %v; no window of theirs was shown", slices.Sorted(maps.Keys(started)))
}
