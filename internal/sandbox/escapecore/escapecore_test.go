package escapecore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tenant/internal/sandbox"
)

// fakeBackend answers Run from a function so the suite's bookkeeping can be
// tested without spawning anything.
type fakeBackend struct {
	run  func(cmd sandbox.Command, pol sandbox.Policy) (sandbox.Result, error)
	pols []sandbox.Policy
}

func (*fakeBackend) Name() string                 { return "fake" }
func (*fakeBackend) Isolation() sandbox.Isolation { return sandbox.IsolationProcess }
func (*fakeBackend) Probe(context.Context) sandbox.Capabilities {
	return sandbox.Capabilities{Backend: "fake"}
}
func (f *fakeBackend) Run(_ context.Context, cmd sandbox.Command, pol sandbox.Policy) (sandbox.Result, error) {
	f.pols = append(f.pols, pol)
	return f.run(cmd, pol)
}

// denyAdversaries fails every adversarial command (by its Origin) and runs every
// legitimate twin cleanly — an ideal confining backend.
func denyAdversaries(cmd sandbox.Command, _ sandbox.Policy) (sandbox.Result, error) {
	switch cmd.Origin {
	case "read-secret", "write-home", "dotdot", "net":
		return sandbox.Result{ExitCode: 1, Output: "Operation not permitted"}, nil
	}
	return sandbox.Result{Output: "ok"}, nil
}

func pinNetwork(t *testing.T, up bool) {
	t.Helper()
	orig := hostHasNetwork
	hostHasNetwork = func(context.Context) bool { return up }
	t.Cleanup(func() { hostHasNetwork = orig })
}

func TestEscapeCore_CaseTable(t *testing.T) {
	seen := map[string]bool{}
	netCases := 0
	for _, c := range cases {
		if seen[c.name] {
			t.Errorf("duplicate case name %q", c.name)
		}
		seen[c.name] = true
		switch c.kind {
		case KindFilesystem:
			if c.needsNet {
				t.Errorf("%s: a filesystem case must never be skippable for lack of network", c.name)
			}
		case KindNetwork:
			netCases++
		default:
			t.Errorf("%s: unknown kind %q", c.name, c.kind)
		}
	}
	if netCases != 1 {
		t.Errorf("want exactly one network case, got %d", netCases)
	}
}

func TestEscapeCore_ContainingBackend(t *testing.T) {
	pinNetwork(t, true)
	f := &fakeBackend{run: denyAdversaries}
	results, err := Evaluate(context.Background(), f)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(results) != len(cases) {
		t.Fatalf("got %d results, want %d", len(results), len(cases))
	}
	for i, r := range results {
		if r.Name != cases[i].name || r.Kind != cases[i].kind {
			t.Errorf("result %d = %s/%s, want %s/%s", i, r.Name, r.Kind, cases[i].name, cases[i].kind)
		}
		if r.Skipped || !r.AdversarialContained || !r.LegitOK {
			t.Errorf("%s: skipped=%v contained=%v legitOK=%v, want false/true/true",
				r.Name, r.Skipped, r.AdversarialContained, r.LegitOK)
		}
		if r.AdversarialCommand == "" || r.LegitCommand == "" || r.AdversarialExit != 1 {
			t.Errorf("%s: diagnostics not recorded: %+v", r.Name, r)
		}
	}
	// Every run used the standard escape policy: the work dir is the only
	// read/write root, the secret dir is denied and sits outside it, no network.
	for _, pol := range f.pols {
		if len(pol.ReadRoots) != 1 || pol.ReadRoots[0] != pol.WorkDir ||
			len(pol.WriteRoots) != 1 || pol.WriteRoots[0] != pol.WorkDir {
			t.Fatalf("roots not pinned to the work dir: %+v", pol)
		}
		if len(pol.DenyRoots) != 1 || strings.HasPrefix(pol.DenyRoots[0], pol.WorkDir) {
			t.Fatalf("secret dir must be denied and outside the work dir: %+v", pol)
		}
		if pol.Network != sandbox.NetNone || !pol.Enforce || pol.Limits.Wall <= 0 {
			t.Fatalf("policy must be network=none, enforcing, wall-capped: %+v", pol)
		}
	}
}

// A backend that exits non-zero but still prints the secret did NOT contain it.
func TestEscapeCore_LeakIsNotContainment(t *testing.T) {
	pinNetwork(t, false)
	f := &fakeBackend{run: func(cmd sandbox.Command, pol sandbox.Policy) (sandbox.Result, error) {
		if cmd.Origin == "read-secret" {
			return sandbox.Result{ExitCode: 1, Output: SecretSentinel + "\n"}, nil
		}
		return denyAdversaries(cmd, pol)
	}}
	results, err := Evaluate(context.Background(), f)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if results[0].Name != "read-secret-outside-roots" || results[0].AdversarialContained {
		t.Fatalf("a leaked secret must not count as contained: %+v", results[0])
	}
	if !results[2].AdversarialContained {
		t.Fatalf("an unrelated case must stay contained: %+v", results[2])
	}
}

// The network case is Skipped only when the host has no outbound network, and
// its legitimate twin still runs; filesystem cases never skip.
func TestEscapeCore_NetworkSkipOnlyWithoutHostNetwork(t *testing.T) {
	for _, up := range []bool{false, true} {
		pinNetwork(t, up)
		results, err := Evaluate(context.Background(), &fakeBackend{run: denyAdversaries})
		if err != nil {
			t.Fatalf("network=%v: Evaluate: %v", up, err)
		}
		for _, r := range results {
			wantSkip := r.Kind == KindNetwork && !up
			if r.Skipped != wantSkip {
				t.Errorf("network=%v %s: Skipped=%v, want %v", up, r.Name, r.Skipped, wantSkip)
			}
			if r.Skipped && (r.SkipReason == "" || r.AdversarialCommand != "" || !r.LegitOK) {
				t.Errorf("network=%v %s: a skip needs a reason, no adversary run, and a legit run: %+v", up, r.Name, r)
			}
		}
	}
}

// A backend that cannot start a command is an infrastructure failure, reported
// as an error alongside the results completed before it.
func TestEscapeCore_InfraErrorStops(t *testing.T) {
	pinNetwork(t, false)
	boom := errors.New("sandbox-exec missing")
	f := &fakeBackend{run: func(cmd sandbox.Command, pol sandbox.Policy) (sandbox.Result, error) {
		if cmd.Origin == "write-home" {
			return sandbox.Result{}, boom
		}
		return denyAdversaries(cmd, pol)
	}}
	results, err := Evaluate(context.Background(), f)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "write-outside-roots") {
		t.Fatalf("err = %v, want the backend error naming the case", err)
	}
	if len(results) != 1 || results[0].Name != "read-secret-outside-roots" {
		t.Fatalf("want only the completed first case, got %+v", results)
	}
}

// A cancelled run is an error, never a column of "contained" results.
func TestEscapeCore_CancelledIsNotContainment(t *testing.T) {
	pinNetwork(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := Evaluate(ctx, &fakeBackend{run: denyAdversaries})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(results) != 0 {
		t.Fatalf("a cancelled run must not report results, got %+v", results)
	}
	if _, err := Evaluate(context.Background(), nil); err == nil {
		t.Fatal("nil backend must error")
	}
}

// Against the real unconfined host backend every filesystem adversary escapes
// and every twin succeeds; temp dirs and the $HOME artifact are cleaned up.
func TestEscapeCore_HostBaseline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("escape cases are POSIX sh")
	}
	pinNetwork(t, false) // no network assumptions: the network case skips
	results, err := Evaluate(context.Background(), sandbox.Host())
	if err != nil {
		t.Fatalf("Evaluate(host): %v", err)
	}
	for _, r := range results {
		if !r.LegitOK {
			t.Errorf("%s: legit twin failed on host: exit=%d output=%q", r.Name, r.LegitExit, r.LegitOutput)
		}
		if r.Kind == KindFilesystem && r.AdversarialContained {
			t.Errorf("%s: host must not contain a filesystem escape: exit=%d output=%q", r.Name, r.AdversarialExit, r.AdversarialOutput)
		}
	}
	if !strings.Contains(results[0].AdversarialOutput, SecretSentinel) {
		t.Errorf("host read of the secret should return the sentinel, got %q", results[0].AdversarialOutput)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, homeArtifact)); !os.IsNotExist(err) {
			t.Errorf("$HOME/%s was not cleaned up (stat err=%v)", homeArtifact, err)
		}
	}
}

// The POSIX network probe must work under dash (Debian/Ubuntu/WSL /bin/sh), not
// just bash: the old /dev/tcp one-liner fails unconfined under dash, which would
// leave the network case unprovable on Linux. macOS ships /bin/dash, so run it.
func TestPosixNetProbe_WorksUnderDash(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash not installed")
	}
	if !hostHasNetwork(context.Background()) {
		t.Skip("host has no outbound network")
	}
	out, err := exec.Command(dash, "-c", posixNetProbe()).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "connected") {
		t.Fatalf("probe under dash: err=%v out=%q", err, out)
	}
}

// Windows backends run PowerShell; every case must render a PowerShell form that
// fails closed (try/catch → exit 1) and never uses POSIX-only syntax.
func TestCases_RenderPowerShellForms(t *testing.T) {
	prev := usePowerShell
	usePowerShell = true
	defer func() { usePowerShell = prev }()
	e := env{workDir: `C:\w`, secretDir: `C:\s`, secretFile: `C:\s\secret.txt`, legitFile: `C:\w\legit.txt`}
	for _, c := range cases {
		adv, legit := c.commands(e)
		for _, cmd := range []string{adv, legit} {
			for _, posix := range []string{"/dev/tcp", "cat ", "echo pwned >", "2>/dev/null", "exec "} {
				if strings.Contains(cmd, posix) {
					t.Errorf("%s: PowerShell form contains POSIX syntax %q: %s", c.name, posix, cmd)
				}
			}
		}
		if !strings.HasPrefix(adv, "try {") || !strings.Contains(adv, "catch { exit 1 }") {
			t.Errorf("%s: adversarial PowerShell form must fail closed via try/catch: %s", c.name, adv)
		}
	}
}
