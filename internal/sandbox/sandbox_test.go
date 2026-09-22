package sandbox

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based host tests are unix-only; windows-native tests live in restricted_windows_test.go")
	}
}

func TestHostRun_BasicAndExit(t *testing.T) {
	skipWindows(t)
	h := Host()
	res, err := h.Run(context.Background(), Command{Shell: "echo hello"}, Policy{})
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if strings.TrimSpace(res.Output) != "hello" {
		t.Fatalf("output = %q, want hello", res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}

	res, err = h.Run(context.Background(), Command{Shell: "exit 3"}, Policy{})
	if err != nil {
		t.Fatalf("a non-zero exit must not be an infra error, got %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3", res.ExitCode)
	}
}

func TestHostRun_WorkDir(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	res, err := Host().Run(context.Background(), Command{Shell: "pwd"}, Policy{WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// macOS /var is a symlink to /private/var; compare suffix.
	if !strings.HasSuffix(strings.TrimSpace(res.Output), strings.TrimPrefix(dir, "/private")) &&
		!strings.Contains(res.Output, dir) {
		t.Fatalf("pwd = %q, want work dir %q", strings.TrimSpace(res.Output), dir)
	}
}

func TestHostRun_Timeout(t *testing.T) {
	skipWindows(t)
	res, err := Host().Run(context.Background(), Command{Shell: "sleep 5"},
		Policy{Limits: Limits{Wall: 120 * time.Millisecond}})
	if err != nil {
		t.Fatalf("timeout must not be an infra error, got %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut")
	}
}

func TestHostRun_OutputCap(t *testing.T) {
	skipWindows(t)
	res, err := Host().Run(context.Background(),
		Command{Shell: "for i in 1 2 3 4 5 6 7 8 9 0; do echo aaaaaaaaaa; done"},
		Policy{Limits: Limits{MaxOutputBytes: 25}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Fatalf("expected truncation marker, got %q", res.Output)
	}
}

func TestEnvPolicy_InheritDefault(t *testing.T) {
	skipWindows(t)
	t.Setenv("TENANT_SBX_SECRET", "s3cr3t")
	res, err := Host().Run(context.Background(), Command{Shell: "echo $TENANT_SBX_SECRET"}, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "s3cr3t" {
		t.Fatalf("zero EnvPolicy must inherit the full env; got %q", res.Output)
	}
}

func TestEnvPolicy_Allowlist(t *testing.T) {
	skipWindows(t)
	t.Setenv("TENANT_SBX_SECRET", "s3cr3t") // a var whose NAME has no KEY/TOKEN marker
	t.Setenv("TENANT_SBX_KEEP", "keepme")
	pol := Policy{Env: EnvPolicy{Isolate: true, Pass: []string{"TENANT_SBX_KEEP"}, Set: map[string]string{"ADDED": "yes"}}}
	res, err := Host().Run(context.Background(),
		Command{Shell: "echo secret=[$TENANT_SBX_SECRET] keep=[$TENANT_SBX_KEEP] added=[$ADDED]"}, pol)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(res.Output)
	if !strings.Contains(out, "secret=[]") {
		t.Fatalf("allowlist must drop the un-listed secret var; got %q", out)
	}
	if !strings.Contains(out, "keep=[keepme]") || !strings.Contains(out, "added=[yes]") {
		t.Fatalf("allowlist must forward Pass + Set; got %q", out)
	}
}

func TestMaterialize_InheritVsScope(t *testing.T) {
	parent := []string{"PATH=/bin", "SECRET=abc", "KEEP=1"}
	if got := (EnvPolicy{}).materialize(parent); len(got) != 3 {
		t.Fatalf("zero policy should inherit all %d, got %d", len(parent), len(got))
	}
	got := (EnvPolicy{Isolate: true, Pass: []string{"KEEP"}}).materialize(parent)
	if len(got) != 1 || got[0] != "KEEP=1" {
		t.Fatalf("scoped policy = %v, want [KEEP=1]", got)
	}
}

func TestFor_Resolution(t *testing.T) {
	if b, err := For("host"); err != nil || b.Name() != "host" {
		t.Fatalf(`For("host") = %v, %v`, b, err)
	}
	if b, err := For(""); err != nil || b.Name() != "host" {
		t.Fatalf(`For("") = %v, %v`, b, err)
	}
	if _, err := For("container"); err == nil {
		t.Fatalf("container should be unimplemented")
	}
	if _, err := For("bogus"); err == nil {
		t.Fatalf("bogus backend should error")
	}
	// "restricted" is registered only by a build-tagged OS file; in the core-only
	// build it must error cleanly rather than panic.
	if restrictedFactory == nil {
		if _, err := For("restricted"); err == nil {
			t.Fatalf("restricted with no registered factory should error")
		}
	}
}

func TestStringers(t *testing.T) {
	if IsolationNone.String() != "none" || IsolationProcess.String() != "process" ||
		IsolationContainer.String() != "container" {
		t.Fatal("Isolation.String mismatch")
	}
	if NetNone.String() != "none" || NetAllowlist.String() != "allowlist" || NetFull.String() != "full" {
		t.Fatal("NetworkMode.String mismatch")
	}
	if EnforceFull.String() != "enforced" || EnforcePartial.String() != "partial" || EnforceNone.String() != "none" {
		t.Fatal("Enforcement.String mismatch")
	}
	if n, ok := ParseNetworkMode("none"); !ok || n != NetNone {
		t.Fatal("ParseNetworkMode(none)")
	}
	if _, ok := ParseNetworkMode("weird"); ok {
		t.Fatal("ParseNetworkMode(weird) should be !ok")
	}
}

func TestProbe_HostHonest(t *testing.T) {
	c := Host().Probe(context.Background())
	if c.Filesystem != EnforceNone || c.Network != EnforceNone {
		t.Fatal("host must report no fs/network enforcement")
	}
	_ = os.Environ()
}
