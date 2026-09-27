//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// A server started in a Job Object serves over its pipes, and its memory is
// capped: a gigabyte allocation under a 128 MB cap does not succeed.
func TestJobObjectStartServesAndCaps(t *testing.T) {
	jb, err := For("jobobject")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Start(context.Background(), jb, helperArgv(), helperPolicy("echo"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(p.Stdin(), "in-job")
	if got := readLine(t, p); got != "echo:in-job" {
		t.Errorf("got %q", got)
	}
	_ = p.Close(5 * time.Second)

	pol := helperPolicy("alloc")
	pol.Limits.MemoryBytes = 128 << 20
	p, err = Start(context.Background(), jb, helperArgv(), pol)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(60 * time.Second):
		_ = p.Close(time.Second)
		t.Fatal("the allocating process neither finished nor died")
	}
	if p.Wait() == nil {
		t.Errorf("a 1 GiB allocation succeeded under a 128 MB job cap; stderr: %s", p.Stderr())
	}
}

// Through WSL, a Linux program serves over its pipes under bubblewrap; the
// environment allowlist crosses via WSLENV; memory and CPU are capped by the
// shell that execs it.
func TestWSLStartServesScopesAndCaps(t *testing.T) {
	wb := wslBackendOrSkip(t)
	ctx := context.Background()
	pol := Policy{Network: NetNone, Env: EnvPolicy{Isolate: true, Set: map[string]string{"FOO_318": "bar"}},
		Limits: Limits{MemoryBytes: 256 << 20, CPU: 5 * time.Second}}
	p, err := Start(ctx, wb, []string{"/bin/sh", "-c", `echo "foo=$FOO_318 secret=$SECRET_318"; ulimit -v; ulimit -t; cat`}, pol)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(5 * time.Second)
	for _, want := range []string{"foo=bar secret=", "262144", "5"} {
		if got := readLine(t, p); got != want {
			t.Errorf("got %q, want %q (stderr: %s)", got, want, p.Stderr())
		}
	}
	fmt.Fprintln(p.Stdin(), "ping")
	if got := readLine(t, p); got != "ping" {
		t.Errorf("echo through the sandbox: %q", got)
	}
}

func TestWSLEnvSharesOnlyTheAllowlist(t *testing.T) {
	parent := []string{"PATH=C:\\bin", "SECRET_318=x", "WSLENV=OLD", "KEEP=1"}
	if got := wslEnv(parent, EnvPolicy{}); !reflect.DeepEqual(got, parent) {
		t.Errorf("unscoped: %q", got)
	}
	got := wslEnv(parent, EnvPolicy{Pass: []string{"KEEP"}, Set: map[string]string{"FOO": "bar"}})
	var wslenv string
	for _, kv := range got {
		if strings.HasPrefix(kv, "WSLENV=") {
			wslenv = strings.TrimPrefix(kv, "WSLENV=")
		}
	}
	names := strings.Split(wslenv, ":")
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"FOO/u", "KEEP/u"}) {
		t.Errorf("WSLENV = %q", wslenv)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "FOO=bar") || strings.Contains(joined, "WSLENV=OLD") {
		t.Errorf("env = %q", got)
	}

	// Add crosses in either mode; unscoped, beside Tenant's own WSLENV.
	add := []string{"TENANT_TRACE_ID=abc"}
	got = wslEnv(parent, EnvPolicy{Add: add})
	if want := []string{"PATH=C:\\bin", "SECRET_318=x", "KEEP=1", "TENANT_TRACE_ID=abc", "WSLENV=OLD:TENANT_TRACE_ID/u"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unscoped + Add = %q", got)
	}
	got = wslEnv(parent, EnvPolicy{Pass: []string{"KEEP"}, Add: add})
	if !slices.Contains(got, "TENANT_TRACE_ID=abc") || !slices.Contains(got, "WSLENV=KEEP/u:TENANT_TRACE_ID/u") {
		t.Errorf("scoped + Add = %q", got)
	}
}
