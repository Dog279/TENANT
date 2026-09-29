package sandbox

import (
	"context"
	"os"
	"path/filepath"
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
