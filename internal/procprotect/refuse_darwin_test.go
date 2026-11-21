package procprotect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Run on a Mac only by CI so far (TEN-535): this file is written and vetted
// from Windows. The first run there (macOS on arm64, 2026-11-21) showed one
// thing: an attach to a process that called Protect ends the attaching
// process with SIGSEGV. What else the table shows on real hardware is to be
// recorded as it is found.

// envTracee names the process a "ptrace" helper attaches to.
const envTracee = "PROCPROTECT_TRACEE"

// The attach itself is made by a process of its own (see attempts): this
// test binary started with PROCPROTECT_HELPER=ptrace. It says "attached" or
// "refused: <why>" and ends; ended by the kernel, it says neither.
func init() {
	if os.Getenv(envHelper) != "ptrace" {
		return
	}
	pid, err := strconv.Atoi(os.Getenv(envTracee))
	if err != nil {
		fmt.Println("refused:", err)
		os.Exit(0)
	}
	runtime.LockOSThread() // a tracer is one thread
	if err := unix.PtraceAttach(pid); err != nil {
		fmt.Println("refused:", err)
		os.Exit(0)
	}
	fmt.Println("attached")
	var ws unix.WaitStatus
	_, _ = unix.Wait4(pid, &ws, 0, nil)
	_ = unix.PtraceDetach(pid)
	os.Exit(0)
}

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
		// sending the tracer SIGSEGV, and the Go runtime ends a process the
		// kernel sends one to: signal.Notify does not take it (seen on CI's
		// Mac: "SIGSEGV: segmentation violation" inside ptrace, exit status
		// 2). So the attach is made by a process of its own, and that
		// process ending on the signal is the refusal.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, os.Args[0])
		c.Env = append(os.Environ(), envHelper+"=ptrace", envTracee+"="+strconv.Itoa(v.PID))
		out, err := CombinedOutput(c)
		said := strings.TrimSpace(string(out))
		switch {
		case strings.HasPrefix(said, "attached"):
			return "attached", nil
		case err == nil && strings.HasPrefix(said, "refused: "):
			return "", errors.New(strings.TrimPrefix(said, "refused: "))
		case bytes.Contains(out, []byte("SIGSEGV")):
			return "", errors.New("the tracer was sent SIGSEGV, the kernel's answer to an attach that was denied")
		}
		if len(said) > 200 {
			said = said[:200]
		}
		return "", fmt.Errorf("the tracer ended another way: %v: %s", err, said)
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
