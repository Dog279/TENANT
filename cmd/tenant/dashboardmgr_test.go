package main

import (
	"context"
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

// healthzDown polls until GET /healthz stops answering (connection refused)
// or the deadline passes. Returns true once the port is no longer serving.
func healthzDown(addr string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return true
		}
		_ = resp.Body.Close()
		time.Sleep(15 * time.Millisecond)
	}
	return false
}

// TestDashboardManager_Lifecycle drives the full Enable → Disable → re-Enable
// cycle on a fixed loopback port, asserting the server actually serves, that
// Disable stops it, that a fresh server starts on re-Enable, and that the
// on/off choice is persisted and Status tracks state. (TEN-86)
func TestDashboardManager_Lifecycle(t *testing.T) {
	const addr = "127.0.0.1:8771"
	rp := &recordingPersist{}
	mgr := &dashboardManager{
		base:    context.Background(),
		cfg:     dashboard.Config{Addr: addr},
		runner:  nil, // health-only server; no agent needed
		tools:   fakeDashTools{},
		broker:  agent.NewBroker(0),
		notify:  func(string) {},
		persist: rp.persist,
	}

	// Starts stopped.
	if running, _ := mgr.Status(); running {
		t.Fatal("manager should start stopped")
	}

	// Enable → serving.
	gotAddr, err := mgr.Enable()
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if gotAddr != addr {
		t.Errorf("Enable addr = %q, want %q", gotAddr, addr)
	}
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
	if !healthzDown(addr, 2*time.Second) {
		t.Fatal("dashboard still serving after Disable")
	}
	if running, _ := mgr.Status(); running {
		t.Error("Status after Disable should be not-running")
	}

	// Re-Enable builds a fresh server on the same port (http.Server can't be
	// reused after Shutdown — this proves we construct a new one).
	if _, err := mgr.Enable(); err != nil {
		t.Fatalf("re-Enable: %v", err)
	}
	if !healthzUp(addr, 2*time.Second) {
		t.Fatal("dashboard did not come back up after re-Enable")
	}

	// Clean up the last server.
	if err := mgr.Disable(); err != nil {
		t.Fatalf("final Disable: %v", err)
	}
	healthzDown(addr, 2*time.Second)

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
