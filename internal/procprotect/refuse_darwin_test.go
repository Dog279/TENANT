package procprotect

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// NOT RUN ON A MAC YET (TEN-535): this file is compiled and vetted from
// Windows, and nothing in it has been observed. What the table shows on real
// hardware is to be recorded as it is found.

var controlIDs = []string{"darwin.deny-attach", "darwin.core-limit"}

var stillAllowed = map[string]bool{}

func attackerTooStrong() string {
	if os.Geteuid() == 0 {
		return "running as root: root attaches to any process, which no control here stops"
	}
	return ""
}

func cleanupMachine() {}
func victimReady()    {}

func selfState() map[string]string {
	var lim unix.Rlimit
	err := unix.Getrlimit(unix.RLIMIT_CORE, &lim)
	return map[string]string{"core soft limit is 0": fmt.Sprint(lim.Cur == 0, err)}
}

func childState() map[string]string { return selfState() }

var attempts = []attempt{
	// On a Mac with System Integrity Protection on, this is expected to be
	// refused for an unprotected process too, unless the user has turned
	// Developer mode on: the table then says it shows nothing here.
	{"ptrace attach (PT_ATTACH; stands in for lldb -p)", func(v victimInfo) (string, error) {
		// The kernel answers an attach to a process that denied it by
		// sending the tracer SIGSEGV. Taken here, it does not end this one.
		signal.Notify(make(chan os.Signal, 1), syscall.SIGSEGV)
		runtime.LockOSThread() // a tracer is one thread
		defer runtime.UnlockOSThread()
		if err := unix.PtraceAttach(v.PID); err != nil {
			return "", err
		}
		var ws unix.WaitStatus
		_, _ = unix.Wait4(v.PID, &ws, 0, nil)
		return "attached", unix.PtraceDetach(v.PID)
	}},
}

// TestWhatAProcessSeesOfItself: after Protect the soft core limit is 0, and a
// command the process starts inherits it.
func TestWhatAProcessSeesOfItself(t *testing.T) {
	if Disabled {
		t.Skip("built with -tags debugattach: the controls are compiled out")
	}
	v := startVictim(t, "protected")
	if got := v.Self["core soft limit is 0"]; got != "true <nil>" {
		t.Errorf("victim: core soft limit is 0 = %q", got)
	}
	if got := v.Child["core soft limit is 0"]; got != "true <nil>" {
		t.Errorf("a command the victim started: core soft limit is 0 = %q (%v)", got, v.Child)
	}
}

// TestProtectThisProcess runs Protect in the test process itself. Under a
// debugger the kernel ends the process here: PT_DENY_ATTACH does that to a
// process that is already traced.
func TestProtectThisProcess(t *testing.T) {
	for _, id := range controlIDs {
		ForceFail(id, "forced")
	}
	if m := Protect().Missing(); !Disabled && (len(m) != len(controlIDs) || m[0].Reason != "forced") {
		t.Fatalf("forced: missing = %+v", m)
	}
	for _, id := range controlIDs {
		ForceFail(id, "")
	}
	if Disabled {
		t.Skip("built with -tags debugattach: nothing to apply")
	}
	r := Protect()
	if len(r.Missing()) != 0 || len(r.Controls) != len(controlIDs) || r.Controls[0].Note != "attach denied, not verified" {
		t.Fatalf("Protect() = %+v", r)
	}
	if got := selfState()["core soft limit is 0"]; got != "true <nil>" {
		t.Fatalf("after Protect: core soft limit is 0 = %q", got)
	}
}
