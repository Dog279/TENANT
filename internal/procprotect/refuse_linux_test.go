package procprotect

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

var controlIDs = []string{"linux.dumpable", "linux.core-limit"}

// What the user's other programs may still do to a protected Tenant: recorded
// by the table, and required to keep working.
var stillAllowed = map[string]bool{
	"read /proc/<pid>/cmdline (what ps shows)": true,
	"read /proc/<pid>/status":                  true,
}

// attackerTooStrong: root reads any process, by design of the system.
func attackerTooStrong() string {
	if os.Geteuid() == 0 {
		return "running as root: root (CAP_SYS_PTRACE) reads any process, which no control here stops"
	}
	return ""
}

func cleanupMachine() {}

// victimReady: where kernel.yama.ptrace_scope is 1, only an ancestor may
// attach to a process, protected or not, so the control would be refused for
// a reason that has nothing to do with Protect. The victim lifts that for
// itself. The kernel checks the dumpable flag first, so a protected victim
// stays closed. With no Yama this fails, and nothing needed lifting.
func victimReady() {
	_ = unix.Prctl(unix.PR_SET_PTRACER, ^uintptr(0) /* PR_SET_PTRACER_ANY */, 0, 0, 0)
	if os.Getenv("PROCPROTECT_CORE") != "" && os.Getenv(envKind) == "plain" {
		// The control of the crash test must be able to dump where the
		// pattern is a file: lift its soft limit to the hard one.
		var lim unix.Rlimit
		if unix.Getrlimit(unix.RLIMIT_CORE, &lim) == nil {
			lim.Cur = lim.Max
			_ = unix.Setrlimit(unix.RLIMIT_CORE, &lim)
		}
	}
}

func selfState() map[string]string {
	out := map[string]string{}
	d, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	out["dumpable"] = fmt.Sprint(d, err)
	var lim unix.Rlimit
	err = unix.Getrlimit(unix.RLIMIT_CORE, &lim)
	out["core soft limit is 0"] = fmt.Sprint(lim.Cur == 0, err)
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/environ", &st); err == nil {
		out["/proc/self/environ owned by root"] = strconv.FormatBool(st.Uid == 0)
	}
	out["info:kernel.yama.ptrace_scope"] = strings.Join(system(), "; ")
	return out
}

func childState() map[string]string {
	return selfState()
}

func procFile(v victimInfo, name string) string { return fmt.Sprintf("/proc/%d/%s", v.PID, name) }

// found is a read's result: nil only when the marker came back.
func found(v victimInfo, b []byte) (string, error) {
	if !bytes.Contains(b, []byte(v.Marker)) {
		return "", errors.New("read succeeded but the marker is not in it")
	}
	return "read the marker", nil
}

var attempts = []attempt{
	{"ptrace attach (PTRACE_ATTACH; stands in for gdb -p)", func(v victimInfo) (string, error) {
		runtime.LockOSThread() // a tracer is one thread
		defer runtime.UnlockOSThread()
		if err := unix.PtraceAttach(v.PID); err != nil {
			return "", err
		}
		var ws unix.WaitStatus
		_, _ = unix.Wait4(v.PID, &ws, unix.WALL, nil)
		return "attached", unix.PtraceDetach(v.PID)
	}},
	{"read /proc/<pid>/mem", func(v victimInfo) (string, error) {
		f, err := os.Open(procFile(v, "mem"))
		if err != nil {
			return "", err
		}
		defer f.Close()
		b := make([]byte, v.Len)
		if _, err := f.ReadAt(b, int64(v.Addr)); err != nil {
			return "", err
		}
		return found(v, b)
	}},
	{"read /proc/<pid>/environ", func(v victimInfo) (string, error) {
		b, err := os.ReadFile(procFile(v, "environ"))
		if err != nil {
			return "", err
		}
		return found(v, b)
	}},
	{"process_vm_readv", func(v victimInfo) (string, error) {
		b := make([]byte, v.Len)
		local := []unix.Iovec{{Base: &b[0]}}
		local[0].SetLen(len(b))
		remote := []unix.RemoteIovec{{Base: v.Addr, Len: len(b)}}
		if _, err := unix.ProcessVMReadv(v.PID, local, remote, 0); err != nil {
			return "", err
		}
		return found(v, b)
	}},
	{"pidfd_getfd (take one of its open files)", func(v victimInfo) (string, error) {
		pidfd, err := unix.PidfdOpen(v.PID, 0)
		if err != nil {
			return "", fmt.Errorf("pidfd_open: %w", err)
		}
		defer unix.Close(pidfd)
		fd, err := unix.PidfdGetfd(pidfd, 0, 0)
		if err != nil {
			return "", err
		}
		unix.Close(fd)
		return "got its stdin", nil
	}},
	{"perf_event_open on the process", func(v victimInfo) (string, error) {
		attr := unix.PerfEventAttr{Type: unix.PERF_TYPE_SOFTWARE, Config: unix.PERF_COUNT_SW_TASK_CLOCK,
			Bits: unix.PerfBitExcludeKernel | unix.PerfBitExcludeHv}
		attr.Size = uint32(unsafe.Sizeof(attr))
		fd, err := unix.PerfEventOpen(&attr, v.PID, -1, -1, unix.PERF_FLAG_FD_CLOEXEC)
		if err != nil {
			return "", err
		}
		unix.Close(fd)
		return "opened a counter on it", nil
	}},
	{"read /proc/<pid>/cmdline (what ps shows)", func(v victimInfo) (string, error) {
		b, err := os.ReadFile(procFile(v, "cmdline"))
		return strings.ReplaceAll(string(b), "\x00", " "), err
	}},
	{"read /proc/<pid>/status", func(v victimInfo) (string, error) {
		b, err := os.ReadFile(procFile(v, "status"))
		return strings.SplitN(string(b), "\n", 2)[0], err
	}},
}

// TestWhatAProcessSeesOfItself: after Protect the dumpable flag is off, the
// soft core limit is 0 and the process's /proc entries are root's; a process
// it starts is ordinary again (the kernel resets the flag at exec) and
// inherits the soft limit, which it may raise.
func TestWhatAProcessSeesOfItself(t *testing.T) {
	if Disabled {
		t.Skip("built with -tags debugattach: the controls are compiled out")
	}
	v := startVictim(t, "protected")
	want := map[string]string{"dumpable": "0 <nil>", "core soft limit is 0": "true <nil>"}
	if os.Geteuid() != 0 {
		want["/proc/self/environ owned by root"] = "true"
	}
	for k, w := range want {
		if v.Self[k] != w {
			t.Errorf("victim: %s = %q, want %q", k, v.Self[k], w)
		}
	}
	if v.Child["dumpable"] != "1 <nil>" || v.Child["core soft limit is 0"] != "true <nil>" {
		t.Errorf("a command the victim started: %v; want dumpable 1 (it is an ordinary process) and the inherited soft limit 0", v.Child)
	}
	t.Logf("the system's own setting: %s", v.Self["info:kernel.yama.ptrace_scope"])
}

// TestProtectThisProcess runs Protect in the test process itself: first with
// every control forced to fail (nothing changes), then for real.
func TestProtectThisProcess(t *testing.T) {
	for _, id := range controlIDs {
		ForceFail(id, "forced")
	}
	if m := Protect().Missing(); len(m) != len(controlIDs) || m[0].Reason != "forced" {
		t.Fatalf("forced: missing = %+v", m)
	}
	if !Disabled && selfState()["dumpable"] != "1 <nil>" {
		t.Fatalf("a forced failure still changed the process: %v", selfState())
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
	if s := selfState(); s["dumpable"] != "0 <nil>" || s["core soft limit is 0"] != "true <nil>" {
		t.Fatalf("after Protect: %v", s)
	}
	// The process still starts commands and reads their output.
	if out, err := Output(exec.Command("sh", "-c", "echo ok")); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("a command after Protect: %q, %v", out, err)
	}
	if err := Run(exec.Command("sh", "-c", "true")); err != nil {
		t.Fatalf("Run after Protect: %v", err)
	}
}

// TestNoDumpAfterCrash sends SIGSEGV to a victim that asked the Go runtime
// for a core dump (GOTRACEBACK=crash) and reads from its exit status whether
// the kernel wrote one. It makes a real crash dump of the control, wherever
// kernel.core_pattern sends it, so it runs only when asked:
// PROCPROTECT_CORE=1.
func TestNoDumpAfterCrash(t *testing.T) {
	if os.Getenv("PROCPROTECT_CORE") == "" {
		t.Skip("set PROCPROTECT_CORE=1: the control leaves a real core dump where kernel.core_pattern points")
	}
	if why := attackerTooStrong(); why != "" {
		t.Skip(why)
	}
	pattern, _ := os.ReadFile("/proc/sys/kernel/core_pattern")
	t.Logf("kernel.core_pattern = %q", strings.TrimSpace(string(pattern)))
	dumped := func(kind string) bool {
		c := exec.Command(os.Args[0])
		c.Dir = t.TempDir() // where a file pattern would put it
		c.Env = append(os.Environ(), envHelper+"=victim", envKind+"="+kind, envMarker+"=x", "GOTRACEBACK=crash")
		stdin, _ := c.StdinPipe()
		stdout, _ := c.StdoutPipe()
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		t.Cleanup(func() { _ = c.Process.Kill() }) // on every path, the crash that did not happen included
		buf := make([]byte, 1)
		if _, err := stdout.Read(buf); err != nil { // it is ready: Protect has run
			t.Fatal(err)
		}
		if err := c.Process.Signal(syscall.SIGSEGV); err != nil {
			t.Fatal(err)
		}
		_ = c.Wait()
		ws := c.ProcessState.Sys().(syscall.WaitStatus)
		t.Logf("%s victim: %v, core dumped = %v", kind, c.ProcessState, ws.CoreDump())
		return ws.CoreDump()
	}
	if !dumped("plain") {
		t.Skip("NOT SHOWN HERE: an unprotected process leaves no dump on this machine either")
	}
	if dumped("protected") {
		t.Error("a protected process left a core dump")
	}
}
