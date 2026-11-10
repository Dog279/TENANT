package sandbox

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// refusingBackend refuses the first run with the given violations and succeeds on
// a within-tier re-run (when tier). It records what was asked of it so Execute's
// routing can be asserted without an engine.
type refusingBackend struct {
	refuse  []Violation
	shell   string // what CommandShell reports; "" means not a shellReporter
	tier    bool
	runs    int
	granted []Policy
}

func (f *refusingBackend) Name() string         { return "fake" }
func (f *refusingBackend) Isolation() Isolation { return IsolationContainer }
func (f *refusingBackend) Probe(context.Context) Capabilities {
	return Capabilities{Backend: "fake", Shell: f.shell}
}
func (f *refusingBackend) Run(context.Context, Command, Policy) (Result, error) {
	f.runs++
	return Result{ExitCode: 1, Violations: f.refuse, Output: "refused"}, nil
}

// tierBackend adds RunGranted; shellBackend adds CommandShell.
type tierBackend struct{ *refusingBackend }

func (t tierBackend) RunGranted(_ context.Context, _ Command, pol Policy, _ Result) (Result, error) {
	t.granted = append(t.granted, pol)
	return Result{ExitCode: 0, Output: "granted"}, nil
}

type shBackend struct{ *refusingBackend }

func (s shBackend) CommandShell() string { return "sh" }

type tierShBackend struct{ tierBackend }

func (s tierShBackend) CommandShell() string { return "sh" }

func approve(context.Context, Command, Result) bool { return true }
func deny(context.Context, Command, Result) bool    { return false }

func TestExecute_DeniedStaysConfined(t *testing.T) {
	fb := &refusingBackend{refuse: []Violation{{Kind: "filesystem", Detail: "x"}}}
	res, err := Execute(context.Background(), tierBackend{fb}, Command{Shell: "echo hi"}, Policy{Enforce: true}, deny)
	if err != nil || res.Escalated || res.ExitCode != 1 {
		t.Fatalf("denied escalation must return the confined result, got %+v %v", res, err)
	}
	if fb.runs != 1 {
		t.Fatalf("backend ran %d times, want 1", fb.runs)
	}
}

// D3: a filesystem or network refusal on a backend that can widen itself goes
// back INTO that backend with one grant, never to the host.
func TestExecute_WithinTierFirst(t *testing.T) {
	fb := &refusingBackend{refuse: []Violation{{Kind: "network", Detail: "no net"}}}
	tb := tierShBackend{tierBackend{fb}}
	res, err := Execute(context.Background(), tb, Command{Shell: "curl x"}, Policy{Enforce: true, Network: NetNone}, approve)
	if err != nil || !res.Escalated || res.Output != "granted" {
		t.Fatalf("expected the within-tier re-run, got %+v %v", res, err)
	}
	if len(res.Violations) != 1 || res.Violations[0].Kind != "network" {
		t.Fatalf("the first attempt's refusal must be kept on the result, got %v", res.Violations)
	}
}

// A refusal that names nothing grantable is not escalated anywhere: the
// confined result comes back with the reason, and the host is never touched.
func TestExecute_NothingGrantableFailsClosed(t *testing.T) {
	b := Container(ContainerOptions{CLI: "definitely-not-a-docker-binary-" + t.Name()})
	refused := Result{ExitCode: 1, Violations: []Violation{{Kind: "filesystem", Detail: "EROFS, no path"}}}
	if _, err := b.RunGranted(context.Background(), Command{Shell: "x"}, Policy{}, refused); err == nil {
		t.Fatal("RunGranted with no path must refuse")
	}
	// Through Execute, with a tier backend whose RunGranted errors.
	fb := &refusingBackend{refuse: []Violation{{Kind: "filesystem", Detail: "EROFS"}}}
	res, err := Execute(context.Background(), failingTier{fb}, Command{Shell: "x"}, Policy{Enforce: true}, approve)
	if err != nil || res.Escalated {
		t.Fatalf("expected the confined result, got %+v %v", res, err)
	}
	if !res.refusedKind("policy") || !strings.Contains(res.Violations[len(res.Violations)-1].Detail, "not escalated") {
		t.Fatalf("expected a 'not escalated' policy note, got %v", res.Violations)
	}
}

type failingTier struct{ *refusingBackend }

func (f failingTier) RunGranted(context.Context, Command, Policy, Result) (Result, error) {
	return Result{}, errNothingGrantable
}

var errNothingGrantable = &escalationError{"nothing to grant"}

type escalationError struct{ s string }

func (e *escalationError) Error() string { return e.s }

// A program refusal goes to the host verbatim, even on a tier backend: the
// container cannot run a Windows program, and only the host can.
func TestExecute_ProgramRefusalGoesToHost(t *testing.T) {
	fb := &refusingBackend{refuse: []Violation{{Kind: "program", Detail: "cmd.exe"}}}
	res, err := Execute(context.Background(), tierShBackend{tierBackend{fb}}, Command{Shell: "echo hi"}, Policy{Enforce: true}, approve)
	if err != nil || !res.Escalated {
		t.Fatalf("expected a host re-run, got %+v %v", res, err)
	}
	if !strings.Contains(res.Output, "hi") {
		t.Fatalf("host did not run the command: %q", res.Output)
	}
	if len(fb.granted) != 0 {
		t.Fatal("a program refusal must not be re-run within the tier")
	}
}

// An sh line never reaches PowerShell: a non-tier backend that runs sh on a
// Windows host has its re-run refused; elsewhere the host runs it as before.
func TestExecute_ShLineNeverReachesPowerShell(t *testing.T) {
	fb := &refusingBackend{refuse: []Violation{{Kind: "filesystem", Detail: "EROFS"}}}
	res, err := Execute(context.Background(), shBackend{fb}, Command{Shell: "echo hi"}, Policy{Enforce: true}, approve)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if res.Escalated {
			t.Fatal("an sh line was re-run on the PowerShell host")
		}
		if last := res.Violations[len(res.Violations)-1]; last.Kind != "policy" || !strings.Contains(last.Detail, "powershell") {
			t.Fatalf("expected the shell-mismatch note, got %v", res.Violations)
		}
		return
	}
	if !res.Escalated || !strings.Contains(res.Output, "hi") {
		t.Fatalf("on a POSIX host the sh line runs on the host, got %+v", res)
	}
}

// boundTier is a tier backend that names its granted run with a digest from
// hashes, one per call (the last repeats): the first is taken before the
// approval is asked for, the second before the run.
type boundTier struct {
	tierBackend
	hashes []string
	errAt  int // 1-based call that fails; 0 = none
	calls  *int
}

func (b boundTier) GrantedRunHash(context.Context, Command, Policy, Result) (string, error) {
	*b.calls++
	if b.errAt == *b.calls {
		return "", errNothingGrantable
	}
	return b.hashes[min(*b.calls, len(b.hashes))-1], nil
}

// W1-6: an approval covers the exact run it was asked for. If what would run
// is different by the time the operator says yes, or cannot be pinned down,
// or the approval is too old, nothing is re-run and the refusal says so.
func TestExecute_ApprovalIsBoundToTheRunItWasAskedFor(t *testing.T) {
	cases := []struct {
		name    string
		hashes  []string
		errAt   int
		elapsed time.Duration // between the approval and the check
		granted bool
		reason  string
	}{
		{"the same run", []string{"aa"}, 0, 0, true, ""},
		{"the run changed while the approval waited", []string{"aa", "bb"}, 0, 0, false, "no longer what was approved"},
		{"it could not be pinned when asked", []string{"aa"}, 1, 0, false, "could not be pinned down"},
		{"it could not be checked before the run", []string{"aa"}, 2, 0, false, "could not be checked before it started"},
		{"the approval is too old", []string{"aa"}, 0, EscalationTTL + time.Second, false, "older than"},
	}
	for _, c := range cases {
		fb := &refusingBackend{refuse: []Violation{{Kind: "filesystem", Detail: "x", Path: "/somewhere"}}}
		calls := 0
		b := boundTier{tierBackend: tierBackend{fb}, hashes: c.hashes, errAt: c.errAt, calls: &calls}
		base := time.Unix(1_800_000_000, 0)
		ticks := 0
		escalationNow = func() time.Time {
			ticks++
			if ticks == 1 { // the ticket's issue; later reads are the check
				return base
			}
			return base.Add(c.elapsed)
		}
		res, err := Execute(context.Background(), b, Command{Shell: "echo hi"}, Policy{Enforce: true}, approve)
		escalationNow = time.Now
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.granted {
			if !res.Escalated || len(fb.granted) != 1 {
				t.Errorf("%s: want the granted re-run, got %+v (granted %d)", c.name, res, len(fb.granted))
			}
			continue
		}
		if res.Escalated || len(fb.granted) != 0 {
			t.Errorf("%s: something was re-run: %+v (granted %d)", c.name, res, len(fb.granted))
		}
		last := res.Violations[len(res.Violations)-1]
		if last.Kind != "policy" || !strings.Contains(last.Detail, "not escalated") || !strings.Contains(last.Detail, c.reason) {
			t.Errorf("%s: last violation = %+v, want a policy refusal containing %q", c.name, last, c.reason)
		}
	}
}

// A denied escalation asks the backend for its digest once and re-runs nothing.
func TestExecute_DeniedApprovalRunsNothingBound(t *testing.T) {
	fb := &refusingBackend{refuse: []Violation{{Kind: "filesystem", Detail: "x", Path: "/somewhere"}}}
	calls := 0
	b := boundTier{tierBackend: tierBackend{fb}, hashes: []string{"aa"}, calls: &calls}
	res, _ := Execute(context.Background(), b, Command{Shell: "echo hi"}, Policy{Enforce: true}, deny)
	if res.Escalated || len(fb.granted) != 0 || calls != 1 {
		t.Fatalf("denied: escalated=%v granted=%d digest calls=%d", res.Escalated, len(fb.granted), calls)
	}
}

func TestEscalationHashAndTicket(t *testing.T) {
	base := EscalationHash("sh", []string{"-c", "echo hi"}, []string{"C:\\w:/mnt/c/w:rw"}, "none", "sha256:abc")
	for name, h := range map[string]string{
		"interpreter": EscalationHash("powershell", []string{"-c", "echo hi"}, []string{"C:\\w:/mnt/c/w:rw"}, "none", "sha256:abc"),
		"argv":        EscalationHash("sh", []string{"-c", "echo hi; rm -rf /"}, []string{"C:\\w:/mnt/c/w:rw"}, "none", "sha256:abc"),
		"mount":       EscalationHash("sh", []string{"-c", "echo hi"}, []string{"C:\\:/mnt/c:rw"}, "none", "sha256:abc"),
		"network":     EscalationHash("sh", []string{"-c", "echo hi"}, []string{"C:\\w:/mnt/c/w:rw"}, "bridge", "sha256:abc"),
		"image":       EscalationHash("sh", []string{"-c", "echo hi"}, []string{"C:\\w:/mnt/c/w:rw"}, "none", "sha256:def"),
		"argv split":  EscalationHash("sh", []string{"-c echo", "hi"}, []string{"C:\\w:/mnt/c/w:rw"}, "none", "sha256:abc"),
	} {
		if h == base {
			t.Errorf("changing the %s did not change the hash", name)
		}
	}
	now := time.Now()
	tk := NewEscalationTicket(base, now)
	if !tk.Valid(base, now.Add(time.Second)) {
		t.Error("a fresh ticket for the same hash must be valid")
	}
	if tk.Valid(base, now.Add(EscalationTTL)) {
		t.Error("an expired ticket must not be valid")
	}
	if tk.Valid("other", now) || tk.Valid("", now) {
		t.Error("a ticket must not cover another hash or an empty one")
	}
}

func TestEscapeControl(t *testing.T) {
	in := "echo hi\r\x1b[2Krm -rf /\n\ttab \u202eevil ünïcode"
	got := EscapeControl(in)
	want := `echo hi\r\x1b[2Krm -rf /\n\ttab \u202eevil ünïcode`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRiskSummary(t *testing.T) {
	fb := &refusingBackend{}
	tier := tierBackend{fb}
	fs := Result{Violations: []Violation{{Kind: "filesystem", Detail: "EROFS", Path: filepath.Join("C:\\", "w", "f.txt")}}}
	if s := RiskSummary(tier, Command{}, fs); !strings.Contains(s, "write access to") || !strings.Contains(s, "inside the sandbox") {
		t.Errorf("tier filesystem: %s", s)
	}
	if s := RiskSummary(fb, Command{}, fs); !strings.Contains(s, "on the host") {
		t.Errorf("host filesystem: %s", s)
	}
	net := Result{Violations: []Violation{{Kind: "network", Detail: "no net"}}}
	if s := RiskSummary(tier, Command{}, net); !strings.Contains(s, "loopback, LAN and tailnet") {
		t.Errorf("tier network: %s", s)
	}
	prog := Result{Violations: []Violation{{Kind: "program", Detail: "cmd.exe"}}}
	if s := RiskSummary(tier, Command{}, prog); !strings.Contains(s, "HOST") {
		t.Errorf("program: %s", s)
	}
	lim := Result{Violations: []Violation{{Kind: "limit", Detail: "OOM"}}}
	if s := RiskSummary(tier, Command{}, lim); !strings.Contains(s, "nothing to approve") {
		t.Errorf("limit: %s", s)
	}
}

func TestRefusedPathIn(t *testing.T) {
	for in, want := range map[string]string{
		"sh: 1: cannot create /mnt/c/x/y: Read-only file system":                 "/mnt/c/x/y",
		"cat: can't open '/mnt/c/Users/u/secret.txt': No such file or directory": "/mnt/c/Users/u/secret.txt",
		`cat: "/home/u/a b": No such file or directory`:                          "/home/u/a",
		"touch: cannot touch '/etc/pwned': Read-only file system":                "/etc/pwned",
		"no path here":            "",
		"wrote to /dev/null fine": "",
		"grep: /proc/self/x: no":  "",
	} {
		if got := refusedPathIn(in); got != want {
			t.Errorf("refusedPathIn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostPathFor(t *testing.T) {
	for _, c := range []struct{ ctr, goos, want string }{
		{"/mnt/c/Users/u/x", "windows", `C:\Users\u\x`},
		{"/mnt/d", "windows", `D:\`},
		{"/mnt/d/", "windows", `D:\`},
		{"/etc/passwd", "windows", ""},
		{"/mnt/cx/y", "windows", ""},
		{"/home/u/x", "linux", "/home/u/x"},
		{"/Users/u/x", "darwin", "/Users/u/x"},
	} {
		if got := hostPathFor(c.ctr, c.goos); got != c.want {
			t.Errorf("hostPathFor(%q, %s) = %q, want %q", c.ctr, c.goos, got, c.want)
		}
	}
}
