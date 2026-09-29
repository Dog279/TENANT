package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBackend returns a canned Result so Execute's refusal policy can be tested
// on any OS without a real confining backend.
type fakeBackend struct {
	res  Result
	err  error
	runs int
}

func (f *fakeBackend) Name() string                       { return "fake" }
func (f *fakeBackend) Isolation() Isolation               { return IsolationProcess }
func (f *fakeBackend) Probe(context.Context) Capabilities { return Capabilities{Backend: "fake"} }
func (f *fakeBackend) Run(context.Context, Command, Policy) (Result, error) {
	f.runs++
	return f.res, f.err
}

var refusedFS = Result{ExitCode: 1, ExitStatus: "exit status 1",
	Violations: []Violation{{Kind: "filesystem", Detail: "blocked"}}}

func mustNotAsk(t *testing.T) Escalator {
	return func(context.Context, Command, Result) bool {
		t.Fatal("escalator must not be consulted here")
		return false
	}
}

func TestExecute_NothingRefusedIsPassthrough(t *testing.T) {
	f := &fakeBackend{res: Result{Output: "ok"}}
	res, err := Execute(context.Background(), f, Command{Shell: "x"}, Policy{Enforce: true}, mustNotAsk(t))
	if err != nil || res.Output != "ok" || res.Escalated || f.runs != 1 {
		t.Fatalf("passthrough broken: %+v err=%v runs=%d", res, err, f.runs)
	}
}

func TestExecute_EnforceDeniedKeepsConfinedResult(t *testing.T) {
	for name, esc := range map[string]Escalator{
		"approver says no": func(context.Context, Command, Result) bool { return false },
		"nil escalator":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeBackend{res: refusedFS}
			res, err := Execute(context.Background(), f, Command{Shell: "x"}, Policy{Enforce: true}, esc)
			if err != nil || res.Escalated || res.ExitCode != 1 || !res.Refused() {
				t.Fatalf("a denied escalation must return the confined result: %+v err=%v", res, err)
			}
		})
	}
}

func TestExecute_EnforceApprovedRerunsOnHost(t *testing.T) {
	skipWindows(t)
	var asked Result
	f := &fakeBackend{res: refusedFS}
	res, err := Execute(context.Background(), f, Command{Shell: "echo host-ran"}, Policy{Enforce: true},
		func(_ context.Context, _ Command, refused Result) bool { asked = refused; return true })
	if err != nil {
		t.Fatal(err)
	}
	if !asked.Refused() {
		t.Fatal("the escalator must be shown the refused result")
	}
	if !res.Escalated || res.ExitCode != 0 || !strings.Contains(res.Output, "host-ran") {
		t.Fatalf("approved escalation must re-run on the host: %+v", res)
	}
	if !res.Refused() || res.Contained() {
		t.Fatalf("escalated result must keep the refusal and read as NOT contained: %+v", res)
	}
}

func TestExecute_DryRunRerunsWithoutAsking(t *testing.T) {
	skipWindows(t)
	f := &fakeBackend{res: refusedFS}
	res, err := Execute(context.Background(), f, Command{Shell: "echo dry"}, Policy{Enforce: false}, mustNotAsk(t))
	if err != nil || !res.Escalated || !strings.Contains(res.Output, "dry") || len(res.Violations) != 1 {
		t.Fatalf("dry-run must re-run on the host and report the refusal: %+v err=%v", res, err)
	}
}

// twinBackend is a backend whose commands run somewhere else (as the WSL
// backend runs Linux sh on Windows): a refused command must re-run on its
// twin, in the same language, never on the host's own shell.
type twinBackend struct {
	fakeBackend
	twin *fakeBackend
}

func (b *twinBackend) Unconfined() Backend { return b.twin }

// TEN-347 (Windows): a dry-run re-run and an approved escalation both go to
// the backend's unconfined twin; a program refusal re-runs like any other.
func TestExecute_RerunsOnTheUnconfinedTwin(t *testing.T) {
	refusedExe := Result{ExitCode: 127, Violations: []Violation{{Kind: "program", Detail: WindowsProgramRefusal}}}
	for name, tc := range map[string]struct {
		enforce bool
		esc     Escalator
	}{
		"dry run":  {false, mustNotAsk(t)},
		"approved": {true, func(context.Context, Command, Result) bool { return true }},
	} {
		b := &twinBackend{fakeBackend: fakeBackend{res: refusedExe}, twin: &fakeBackend{res: Result{Output: "ran-outside"}}}
		res, err := Execute(context.Background(), b, Command{Shell: "powershell.exe -c 1"}, Policy{Enforce: tc.enforce}, tc.esc)
		if err != nil || !res.Escalated || res.Output != "ran-outside" || b.twin.runs != 1 || !res.Refused() {
			t.Fatalf("%s: %+v err=%v twin runs=%d", name, res, err, b.twin.runs)
		}
	}
	if Outside(&fakeBackend{}) != Host() {
		t.Fatal("a backend with no twin re-runs on the host")
	}
}

// A Windows program failing inside the WSL sandbox (hidden drives, interop's
// socket in the hidden /run) is a refusal that tells the model what to do.
func TestAttributeWindowsProgram(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"sh: 1: powershell.exe: not found", true},
		{"bash: cmd.exe: command not found", true},
		{"sh: 1: /mnt/c/Windows/System32/notepad.exe: No such file or directory", true},
		{"sh: /mnt/c/x/tool.exe: cannot execute binary file: Exec format error", true},
		{"<3>WSL (212) ERROR: UtilConnectUnix:504: connect failed 2", true},
		{"<3>WSL (9) ERROR: CreateProcessParseCommon:863: Failed to translate the current directory", true},
		{"Could not connect to the interop server", true},
		{"grep: pattern not found", false},
		{"cat: /mnt/c/x.txt: No such file or directory", false},
	} {
		v, ok := attributeWindowsProgram(tc.out)
		if ok != tc.want || (ok && (v.Kind != "program" || v.Detail != WindowsProgramRefusal)) {
			t.Errorf("%q: %+v %v, want %v", tc.out, v, ok, tc.want)
		}
	}
	if !(Result{Violations: []Violation{{Kind: "program"}}}).Refused() {
		t.Error("a program refusal must count as refused")
	}
}

// The unsandboxed twin runs the line in sh, in the work dir, with both as
// sh's own arguments: a quote in either can't break out.
func TestWSLUnsandboxedArgv(t *testing.T) {
	if got := wslArgv("Ubuntu", wslUnsandboxedArgv("", "ls; echo 'x'")...); strings.Join(got, "|") != "-d|Ubuntu|-e|sh|-c|ls; echo 'x'" {
		t.Errorf("no dir: %q", got)
	}
	got := wslUnsandboxedArgv("/mnt/c/Tenant/data/cron-work", `echo "$HOME" && dir`)
	want := []string{"sh", "-c", `cd -- "$1" && exec sh -c "$2"`, "sh", "/mnt/c/Tenant/data/cron-work", `echo "$HOME" && dir`}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("with dir: %q", got)
	}
	if runtime.GOOS != "windows" {
		out, err := exec.Command(got[0], append(got[1:4], t.TempDir(), "pwd -P; echo ran")...).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "ran") {
			t.Errorf("the wrapper must run the line in the dir: %q %v", out, err)
		}
	}
}

func TestExecute_LimitViolationIsNotEscalated(t *testing.T) {
	f := &fakeBackend{res: Result{TimedOut: true, ExitCode: -1,
		Violations: []Violation{{Kind: "limit", Detail: "wall-clock deadline exceeded"}}}}
	res, err := Execute(context.Background(), f, Command{Shell: "x"}, Policy{Enforce: false}, mustNotAsk(t))
	if err != nil || res.Escalated || !res.TimedOut || f.runs != 1 {
		t.Fatalf("a limit violation must not escalate or re-run: %+v", res)
	}
	if res.Refused() || !res.Contained() {
		t.Fatalf("limit-only result: Refused must be false, Contained true: %+v", res)
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		p, root string
		want    bool
	}{
		{"/a/b", "/a", true},
		{"/a", "/a", true},
		{"/ab", "/a", false},
		{"/a/../b", "/a", false},
		{"/", "/a", false},
		{"", "/a", false},
		{"/a/b", "", false},
	}
	for _, c := range cases {
		if got := within(c.p, c.root); got != c.want {
			t.Errorf("within(%q, %q) = %v, want %v", c.p, c.root, got, c.want)
		}
	}
}

func TestBwrapArgs_WorkDirCarveOut(t *testing.T) {
	data := t.TempDir()
	work := filepath.Join(data, "cron-work")
	secrets := filepath.Join(work, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	pol := Policy{WorkDir: work, DenyRoots: []string{data, secrets}, Network: NetFull}
	a := bwrapArgs(pol, "sh", []string{"-c", "true"})
	find := func(last bool, seq ...string) int {
		at := -1
		for i := 0; i+len(seq) <= len(a); i++ {
			match := true
			for j := range seq {
				if a[i+j] != seq[j] {
					match = false
					break
				}
			}
			if match {
				at = i
				if !last {
					return at
				}
			}
		}
		return at
	}
	maskData := find(false, "--tmpfs", data)
	bindWork := find(false, "--bind", work, work)
	remask := find(true, "--tmpfs", secrets)
	if maskData < 0 || bindWork < 0 || remask < 0 {
		t.Fatalf("missing args: %v", a)
	}
	if !(maskData < bindWork) {
		t.Fatalf("the work dir must be bound AFTER the deny mask that contains it: %v", a)
	}
	if !(bindWork < remask) {
		t.Fatalf("a deny root inside the work dir must be re-masked AFTER the work bind: %v", a)
	}
	// Read-only only once the carve-out has made its mount point inside the mask
	// (bwrap cannot create it in a read-only tmpfs), and after the re-mask too.
	if ro := find(true, "--remount-ro", data); ro < bindWork {
		t.Fatalf("the data dir mask must go read-only AFTER the work bind: %v", a)
	}
	if ro := find(true, "--remount-ro", secrets); ro < remask {
		t.Fatalf("the re-masked deny root must go read-only AFTER its re-mask: %v", a)
	}
}
