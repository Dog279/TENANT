package web

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"tenant/internal/logs/event"
	"tenant/internal/logs/event/eventtest"
)

func TestEffectiveLaunchFlagsSandboxDefaultAndOptIn(t *testing.T) {
	tests := []struct {
		name          string
		cfg           Config
		wantNoSandbox bool
	}{
		{name: "secure default", cfg: Config{}},
		{name: "explicit opt-in", cfg: Config{NoSandbox: true}, wantNoSandbox: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := effectiveLaunchArgs(t, tt.cfg)
			got := hasLaunchFlag(args, "no-sandbox")
			if got != tt.wantNoSandbox {
				t.Fatalf("effective args contain --no-sandbox = %t, want %t; args=%q", got, tt.wantNoSandbox, args)
			}
		})
	}
}

// effectiveLaunchArgs captures the actual argv chromedp builds from Tenant's
// production allocator options, then redirects execution to a missing binary.
// This covers chromedp's root-only implicit --no-sandbox behavior as well as
// Tenant's explicit flags without requiring Chrome in the unit test.
func effectiveLaunchArgs(t *testing.T, cfg Config) []string {
	t.Helper()
	var got []string
	missing := filepath.Join(t.TempDir(), "missing-chrome")
	opts := append(execAllocatorOptions(missing, cfg, "127.0.0.1:0"), chromedp.ModifyCmdFunc(func(cmd *exec.Cmd) {
		got = append([]string(nil), cmd.Args...)
		cmd.Path = missing
	}))
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	_ = chromedp.Run(tabCtx)
	cancelTab()
	cancelAlloc()
	if len(got) == 0 {
		t.Fatal("chromedp did not construct Chrome launch arguments")
	}
	return got
}

func hasLaunchFlag(args []string, name string) bool {
	prefix := "--" + name
	for _, arg := range args {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}
	return false
}

func TestNoSandboxRequiresDetectedOuterSandbox(t *testing.T) {
	if err := validateNoSandboxMode(false, false); err != nil {
		t.Fatalf("secure default rejected: %v", err)
	}
	if err := validateNoSandboxMode(true, true); err != nil {
		t.Fatalf("detected outer sandbox rejected: %v", err)
	}
	if err := validateNoSandboxMode(true, false); err == nil {
		t.Fatal("no-sandbox opt-in without outer sandbox was accepted")
	}
}

func TestDetectOuterSandbox(t *testing.T) {
	read := func(files map[string]string) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if content, ok := files[path]; ok {
				return []byte(content), nil
			}
			return nil, errors.New("not found")
		}
	}
	neverExists := func(string) bool { return false }

	if _, ok := detectOuterSandbox("windows", neverExists, read(nil)); ok {
		t.Fatal("non-Linux host must not qualify as an outer sandbox")
	}
	if kind, ok := detectOuterSandbox("linux", func(path string) bool { return path == "/.dockerenv" }, read(nil)); !ok || kind != "Docker container" {
		t.Fatalf("Docker marker detection = %q, %t", kind, ok)
	}
	if kind, ok := detectOuterSandbox("linux", neverExists, read(map[string]string{
		"/proc/1/cgroup":     "0::/\n",
		"/proc/self/uid_map": "0 1000 1\n",
	})); !ok || kind != "user namespace" {
		t.Fatalf("user namespace detection = %q, %t", kind, ok)
	}
	if _, ok := detectOuterSandbox("linux", neverExists, read(map[string]string{
		"/proc/1/cgroup":     "0::/\n",
		"/proc/self/uid_map": "0 0 4294967295\n",
	})); ok {
		t.Fatal("initial host namespace was misidentified as an outer sandbox")
	}
}

func TestNoSandboxEmitsSecurityWarning(t *testing.T) {
	rec := eventtest.Capture(t)
	emitNoSandboxWarning("test container")
	got := rec.Events()
	if len(got) != 1 || got[0].Provider != "browser" || got[0].ID != 2001 || got[0].Level != event.Warning ||
		!slices.Contains(got[0].Keywords, "security") || !strings.Contains(got[0].Msg, "test container") {
		t.Fatalf("logged %+v, want one security warning naming the outer sandbox", got)
	}
}
