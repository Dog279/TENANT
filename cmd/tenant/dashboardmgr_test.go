package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/dashboard"
)

// fakeDashTools is a no-op dashboard.ToolControl for the manager lifecycle
// test — the manager only needs SOMETHING that satisfies the interface; the
// REST surface isn't exercised here.
type fakeDashTools struct{}

func (fakeDashTools) ToolList() []dashboard.ToolInfo                     { return nil }
func (fakeDashTools) SetEnabled(string, bool) (int, string, error)       { return 0, "", nil }
func (fakeDashTools) SetPluginEnabled(string, bool) (int, string, error) { return 0, "", nil }
func (fakeDashTools) Plugins() []string                                  { return nil }

// recordingPersist captures the on/off sequence the manager persists.
type recordingPersist struct {
	mu    sync.Mutex
	calls []bool
}

func (r *recordingPersist) persist(enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, enabled)
	return nil
}

func (r *recordingPersist) snapshot() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.calls...)
}

// healthzUp polls GET /healthz on addr until it answers 200 or the deadline
// passes. Returns true once the server is serving.
func healthzUp(addr string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	return false
}

// TestDashboardManager_ShutdownDoesNotPersist: process teardown (serve SIGTERM
// / exit) must NOT persist an "off" choice, while Disable (operator /dashboard
// off) must. Regression for the bug where every serve shutdown wrote
// dashboard.enabled=false, so the panel came back off on the next launch (and
// corrupted the choice the TUI reads).
func TestDashboardManager_ShutdownDoesNotPersist(t *testing.T) {
	rp := &recordingPersist{}
	mgr := &dashboardManager{base: context.Background(), persist: rp.persist}

	// Operator turning it off persists exactly one false.
	if err := mgr.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if got := rp.snapshot(); len(got) != 1 || got[0] {
		t.Fatalf("Disable should persist one false, got %v", got)
	}

	// Process teardown persists nothing.
	if err := mgr.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := rp.snapshot(); len(got) != 1 {
		t.Errorf("Shutdown must not persist; calls now %v", got)
	}
}

// TestDashboardManager_Lifecycle drives the full Enable → Disable → re-Enable
// cycle on an OS-assigned loopback port, asserting the server actually serves, that
// Disable stops it, that a fresh server starts on re-Enable, and that the
// on/off choice is persisted and Status tracks state. (TEN-86)
func TestDashboardManager_Lifecycle(t *testing.T) {
	rp := &recordingPersist{}
	mgr := &dashboardManager{
		base:    context.Background(),
		cfg:     dashboard.Config{Addr: "127.0.0.1:0", Auth: "test-token"},
		runner:  nil, // health-only server; no agent needed
		tools:   fakeDashTools{},
		broker:  agent.NewBroker(0),
		notify:  func(string) {},
		persist: rp.persist,
	}
	t.Cleanup(func() { _ = mgr.Shutdown() })

	// Starts stopped.
	if running, _ := mgr.Status(); running {
		t.Fatal("manager should start stopped")
	}

	// Enable → serving.
	gotAddr, err := mgr.Enable()
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, port, err := net.SplitHostPort(gotAddr); err != nil || port == "0" {
		t.Fatalf("Enable addr = %q, want a bound ephemeral port (err=%v)", gotAddr, err)
	}
	addr := gotAddr
	if !healthzUp(addr, 2*time.Second) {
		t.Fatal("dashboard did not come up after Enable")
	}
	if running, ra := mgr.Status(); !running || ra != addr {
		t.Errorf("Status after Enable = (%v, %q), want (true, %q)", running, ra, addr)
	}

	// Enable again while running is a no-op (same addr, no extra persist).
	if _, err := mgr.Enable(); err != nil {
		t.Fatalf("second Enable: %v", err)
	}

	// Disable → port stops serving.
	if err := mgr.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("dashboard still serving after synchronous Disable")
	}
	if running, _ := mgr.Status(); running {
		t.Error("Status after Disable should be not-running")
	}

	// Re-Enable builds a fresh server on another reserved ephemeral port
	// (http.Server can't be reused after Shutdown).
	restartedAddr, err := mgr.Enable()
	if err != nil {
		t.Fatalf("re-Enable: %v", err)
	}
	if !healthzUp(restartedAddr, 2*time.Second) {
		t.Fatal("dashboard did not come back up after re-Enable")
	}

	// Clean up the last server.
	if err := mgr.Disable(); err != nil {
		t.Fatalf("final Disable: %v", err)
	}

	// Persistence: the first Enable wrote true, the first Disable wrote
	// false. (The no-op second Enable wrote nothing.) We assert the leading
	// true,false then accept the trailing re-Enable/Disable pair.
	calls := rp.snapshot()
	want := []bool{true, false, true, false}
	if len(calls) != len(want) {
		t.Fatalf("persist calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("persist calls = %v, want %v", calls, want)
		}
	}
}
