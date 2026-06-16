package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestRelayManager_PermsAndAskOperator covers the TEN-231 wiring: Perms()
// exposes the broker for /relay permissions, and askOperator (the broker's "ask"
// backend) fails closed when no approver is built (operator unreachable → deny).
func TestRelayManager_PermsAndAskOperator(t *testing.T) {
	// Unconfigured manager: no broker → Perms() is nil (the TUI reports unavailable).
	bare := &discordRelayManager{base: context.Background()}
	if bare.Perms() != nil {
		t.Fatal("Perms() must be nil with no broker (discord unconfigured)")
	}
	if bare.askOperator(context.Background(), "os_exec", "ls") {
		t.Fatal("askOperator must deny when no approver is built (fail closed)")
	}

	// Configured broker but still no live approver (relay not built) → ask denies,
	// but Perms() drives the per-category modes regardless.
	br := newDiscordApprovalBroker(testLog())
	m := &discordRelayManager{base: context.Background(), broker: br}
	if m.Perms() == nil {
		t.Fatal("Perms() must expose the broker once wired")
	}
	if ok, err := m.Perms().SetPermission(catExec, "deny"); err != nil || !ok {
		t.Fatalf("SetPermission via Perms() failed: ok=%v err=%v", ok, err)
	}
	if m.askOperator(context.Background(), "os_exec", "ls") {
		t.Fatal("askOperator must deny with a nil approver")
	}
}

func TestRelayManager_Lifecycle(t *testing.T) {
	started := 0
	var persisted []string
	m := &discordRelayManager{
		base:   context.Background(),
		runner: &fakeRunner{}, // non-nil so the "not configured" guard passes
		start:  func(_ context.Context, _ string) error { started++; return nil },
		persist: func(enabled bool, op string, allowExec bool) error {
			persisted = append(persisted, fmt.Sprintf("%v:%s:%v", enabled, op, allowExec))
			return nil
		},
	}

	// Enable with no operator → loud fail; nothing started.
	if err := m.Enable(); err == nil || !strings.Contains(err.Error(), "no operator") {
		t.Fatalf("Enable with no operator must fail loudly, got %v", err)
	}
	if running, opset, _ := m.Status(); running || opset {
		t.Error("must not be running and must have no operator yet")
	}
	if started != 0 {
		t.Error("start must not run without an operator")
	}

	// Set the operator, then enable.
	if err := m.SetOperator("op123"); err != nil {
		t.Fatal(err)
	}
	if _, opset, _ := m.Status(); !opset {
		t.Error("operator should be set")
	}
	if err := m.Enable(); err != nil {
		t.Fatalf("Enable after SetOperator: %v", err)
	}
	if running, _, _ := m.Status(); !running {
		t.Error("should be running")
	}
	if started != 1 {
		t.Errorf("start should run once, got %d", started)
	}

	// Enable again is a no-op.
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Error("a second Enable must be a no-op")
	}

	// Disable stops it.
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	if running, _, _ := m.Status(); running {
		t.Error("should be stopped after Disable")
	}
}

// SetExec flips the live gate + reports through Status, and persists alongside
// the running/operator state.
func TestRelayManager_ExecToggle(t *testing.T) {
	gate := &execGate{}
	var lastPersist string
	m := &discordRelayManager{
		base:       context.Background(),
		runner:     &fakeRunner{}, // non-nil so SetExec's "not configured" guard passes
		gate:       gate,
		operatorID: "op123",
		persist: func(enabled bool, op string, allowExec bool) error {
			lastPersist = fmt.Sprintf("%v:%s:%v", enabled, op, allowExec)
			return nil
		},
	}
	if _, _, execOn := m.Status(); execOn {
		t.Fatal("exec mode should default off")
	}
	if err := m.SetExec(true); err != nil {
		t.Fatal(err)
	}
	if !gate.enabled() {
		t.Error("SetExec(true) must flip the shared gate on")
	}
	if _, _, execOn := m.Status(); !execOn {
		t.Error("Status should report exec on")
	}
	if lastPersist != "false:op123:true" {
		t.Errorf("exec toggle should persist with allowExec=true, got %q", lastPersist)
	}
	if err := m.SetExec(false); err != nil {
		t.Fatal(err)
	}
	if gate.enabled() {
		t.Error("SetExec(false) must flip the gate off")
	}
}

// SetExec on an unconfigured relay (nil runner) fails loudly instead of
// silently persisting a mode that can never take effect.
func TestRelayManager_ExecNotConfigured(t *testing.T) {
	m := &discordRelayManager{base: context.Background()}
	if err := m.SetExec(true); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("SetExec on a nil runner should fail 'not configured', got %v", err)
	}
}

func TestRelayManager_NotConfigured(t *testing.T) {
	m := &discordRelayManager{base: context.Background(), operatorID: "op", start: func(context.Context, string) error { return nil }}
	if err := m.Enable(); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("Enable with a nil runner should fail 'not configured', got %v", err)
	}
}
