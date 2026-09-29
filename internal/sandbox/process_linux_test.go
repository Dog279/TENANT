//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// A server started under bubblewrap serves over its pipes, its environment is
// the allowlist, and its memory, CPU time and process count are capped by the
// shell that execs it. Skips without a working bubblewrap, unless
// TENANT_SANDBOX_LIVE=1 (redteam_linux_test.go).
func TestBwrapStartServesScopesAndCaps(t *testing.T) {
	b, err := Restricted()
	if err != nil {
		if os.Getenv("TENANT_SANDBOX_LIVE") == "1" {
			t.Fatalf("TENANT_SANDBOX_LIVE=1 but the restricted backend is unavailable: %v", err)
		}
		t.Skip(err)
	}
	t.Setenv("SECRET_318", "leak")
	pol := Policy{Network: NetNone,
		Env:    EnvPolicy{Isolate: true, Pass: []string{"PATH"}, Set: map[string]string{"FOO_318": "bar"}},
		Limits: Limits{MemoryBytes: 256 << 20, CPU: 5 * time.Second, MaxPIDs: 16}}
	p, err := Start(context.Background(), b, []string{"/bin/sh", "-c",
		`echo "foo=$FOO_318 secret=$SECRET_318"; ulimit -v; ulimit -t; ulimit -u 2>/dev/null || ulimit -p; cat`}, pol)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(5 * time.Second)
	for _, want := range []string{"foo=bar secret=", "262144", "5", "16"} {
		if got := readLine(t, p); got != want {
			t.Errorf("got %q, want %q (stderr: %s)", got, want, p.Stderr())
		}
	}
	fmt.Fprintln(p.Stdin(), "ping")
	if got := readLine(t, p); got != "ping" {
		t.Errorf("echo through the sandbox: %q", got)
	}
}
