package procprotect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// The refusal tests (TEN-535) run this test binary again as three kinds of
// process, all as the user running the tests:
//
//	victim  what Tenant is: it calls Protect (or does not: the control) and
//	        holds a marker in its memory and its environment.
//	attack  a second program of the same user: it tries each way into the
//	        victim and reports, per attempt, whether the system let it.
//	child   a process the victim starts, as Tenant starts a command: it
//	        reports what it inherited.
//
// An attempt counts as refused only when the same attempt gets into the
// victim that did not call Protect, so a pass shows the control at work and
// not an attempt that could never have worked here.

const (
	envHelper = "PROCPROTECT_HELPER" // victim | attack | child
	envKind   = "PROCPROTECT_VICTIM" // plain | protected | forced
	envMarker = "PROCPROTECT_MARKER" // the bytes an attack looks for
	envTarget = "PROCPROTECT_TARGET" // the victim, as JSON, for an attack
)

func TestMain(m *testing.M) {
	switch os.Getenv(envHelper) {
	case "victim":
		victimMain()
	case "attack":
		attackMain()
	case "child":
		emit(childState())
	default:
		code := m.Run()
		cleanupMachine()
		os.Exit(code)
	}
}

// victimInfo is what a victim says about itself once it is ready.
type victimInfo struct {
	PID    int               `json:"pid"`
	Addr   uintptr           `json:"addr"` // where the marker is in its memory
	Len    int               `json:"len"`
	Marker string            `json:"marker"`
	Report Report            `json:"report"`
	Self   map[string]string `json:"self"`  // what it can see of itself
	Child  map[string]string `json:"child"` // what a process it started inherited
}

// outcome is one attempt against a victim.
type outcome struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`  // the system let it through
	Err  string `json:"err"` // how it was refused
	Got  string `json:"got"` // what it got
}

// attempt is one way into another process. do returns what it got.
type attempt struct {
	name string
	do   func(v victimInfo) (string, error)
}

func emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(b))
}

func victimMain() {
	kind := os.Getenv(envKind)
	var rep Report
	switch kind {
	case "protected":
		rep = Protect()
	case "forced":
		for _, id := range controlIDs {
			ForceFail(id, "forced by the test")
		}
		rep = Protect()
	}
	victimReady()
	marker := []byte(os.Getenv(envMarker)) // a copy on the heap, away from the environment block
	info := victimInfo{PID: os.Getpid(), Addr: uintptr(unsafe.Pointer(&marker[0])), Len: len(marker),
		Marker: string(marker), Report: rep, Self: selfState()}
	// A process the victim starts, the way Tenant starts every command.
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), envHelper+"=child")
	if out, err := Output(c); err != nil {
		info.Child = map[string]string{"error": err.Error()}
	} else if err := json.Unmarshal(out, &info.Child); err != nil {
		info.Child = map[string]string{"error": err.Error() + ": " + string(out)}
	}
	emit(info)
	_, _ = io.Copy(io.Discard, os.Stdin) // until the test closes it
	runtime.KeepAlive(marker)
}

func attackMain() {
	var v victimInfo
	if err := json.Unmarshal([]byte(os.Getenv(envTarget)), &v); err != nil {
		panic(err)
	}
	var out []outcome
	for _, a := range attempts {
		o := outcome{Name: a.name}
		got, err := a.do(v)
		o.Got, o.OK = got, err == nil
		if err != nil {
			o.Err = err.Error()
		}
		out = append(out, o)
	}
	emit(out)
}

// startVictim starts a victim of kind and waits until it is ready. It ends
// with the test.
func startVictim(t *testing.T, kind string, env ...string) victimInfo {
	t.Helper()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), envHelper+"=victim", envKind+"="+kind, envMarker+"=ten535-"+kind+"-"+t.Name())
	c.Env = append(c.Env, env...)
	c.Stderr = os.Stderr
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	var err error
	Ordinary(func() { // the test process may be protected itself by now
		if stdin, err = c.StdinPipe(); err == nil {
			stdout, err = c.StdoutPipe()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = c.Process.Kill()
		_ = c.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("the %s victim said nothing: %v", kind, err)
	}
	var v victimInfo
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("the %s victim said %q: %v", kind, line, err)
	}
	return v
}

// attack runs every attempt against v from a second process.
func attack(t *testing.T, v victimInfo) map[string]outcome {
	t.Helper()
	target, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // an attacker that hangs is killed
	defer cancel()
	c := exec.CommandContext(ctx, os.Args[0])
	c.Env = append(os.Environ(), envHelper+"=attack", envTarget+"="+string(target))
	c.Stderr = os.Stderr
	b, err := Output(c)
	if err != nil {
		t.Fatalf("the attacker failed: %v\n%s", err, b)
	}
	var list []outcome
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("the attacker said %q: %v", b, err)
	}
	out := map[string]outcome{}
	for _, o := range list {
		out[o.Name] = o
	}
	return out
}

// TestRefusals is the table: every attempt gets into a process that did not
// call Protect, and is refused by one that did.
func TestRefusals(t *testing.T) {
	if len(attempts) == 0 {
		t.Skip("no controls on this system")
	}
	if Disabled {
		t.Skip("built with -tags debugattach: the controls are compiled out")
	}
	if why := attackerTooStrong(); why != "" {
		t.Skip(why)
	}
	plain, protected := startVictim(t, "plain"), startVictim(t, "protected")
	if m := protected.Report.Missing(); len(m) > 0 {
		t.Fatalf("the protected victim reports controls not in force: %+v", m)
	}
	control, got := attack(t, plain), attack(t, protected)
	t.Logf("%-52s | %-28s | %s", "attempt", "no Protect (control)", "after Protect")
	for _, a := range attempts {
		c, g := control[a.name], got[a.name]
		t.Logf("%-52s | %-28s | %s", a.name, verdict(c), verdict(g))
		wantOpen := stillAllowed[a.name]
		switch {
		case !c.OK:
			t.Logf("  NOT SHOWN HERE: %q fails against an unprotected process too (%s), so its refusal proves nothing on this machine", a.name, c.Err)
		case wantOpen && !g.OK:
			t.Errorf("%q must still work after Protect, and was refused: %s", a.name, g.Err)
		case !wantOpen && g.OK:
			t.Errorf("%q was NOT refused after Protect (got %q)", a.name, g.Got)
		}
	}
}

func verdict(o outcome) string {
	if o.OK {
		return "let through"
	}
	return "refused: " + o.Err
}

// TestForcedFailureIsReported: a control that could not be applied is in the
// report with its reason, and is really not in force.
func TestForcedFailureIsReported(t *testing.T) {
	if len(controlIDs) == 0 {
		t.Skip("no controls on this system")
	}
	v := startVictim(t, "forced")
	missing := v.Report.Missing()
	if len(missing) != len(controlIDs) {
		t.Fatalf("missing = %+v, want every one of %v", missing, controlIDs)
	}
	for i, c := range missing {
		if c.ID != controlIDs[i] || c.Reason != "forced by the test" || c.Leaves == "" || c.Name == "" {
			t.Errorf("control %d = %+v, want id %s with the forced reason, a name and what it leaves open", i, c, controlIDs[i])
		}
	}
	plain := startVictim(t, "plain")
	for k, want := range plain.Self {
		if strings.HasPrefix(k, "info:") {
			continue
		}
		if v.Self[k] != want {
			t.Errorf("with every control forced to fail, %s = %q; an unprotected process has %q", k, v.Self[k], want)
		}
	}
}

func TestReportMissingAndStillOpen(t *testing.T) {
	r := Report{Controls: []Control{{ID: "a", OK: true}, {ID: "b", Reason: "no"}}}
	if m := r.Missing(); len(m) != 1 || m[0].ID != "b" {
		t.Fatalf("Missing() = %+v", m)
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if StillOpen(goos) == "" {
			t.Errorf("StillOpen(%s) is empty", goos)
		}
	}
	if StillOpen("plan9") != "" {
		t.Error("a system with no controls has nothing to say")
	}
	for _, tc := range []struct {
		what, out string
		err       error
		want      string
	}{
		{"System Integrity Protection", "System Integrity Protection status: enabled.\n", nil, "System Integrity Protection status: enabled."},
		{"FileVault", "FileVault is On.\nmore\n", nil, "FileVault is On."},
		{"FileVault", "Encryption in progress\n", nil, "FileVault: Encryption in progress"},
		{"FileVault", "", nil, "FileVault: could not be read"},
		{"FileVault", "FileVault is On.", errors.New("exit status 1"), "FileVault: could not be read"},
	} {
		if got := statusLine(tc.what, []byte(tc.out), tc.err); got != tc.want {
			t.Errorf("statusLine(%q, %q, %v) = %q, want %q", tc.what, tc.out, tc.err, got, tc.want)
		}
	}
	if Disabled {
		if r := Protect(); !r.Disabled || len(r.Controls) != 0 {
			t.Fatalf("a debugattach build's Protect() = %+v", r)
		}
	}
}
