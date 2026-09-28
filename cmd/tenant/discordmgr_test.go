package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/tui"
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
	if bare.askOperator(context.Background(), "os_exec", "ls") != tui.DenyOnce {
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
	if m.askOperator(context.Background(), "os_exec", "ls") != tui.DenyOnce {
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

// TestRelayManager_ShutdownDoesNotPersistOff is the relay copy of the
// dashboard regression (found live 2026-08-12 on the server): serve teardown
// used Disable(), which persisted enabled=false — so every graceful stop
// durably switched the bot off and it never came back on the next boot. Only
// the operator's explicit Disable (/relay off) may persist "off"; process
// teardown must go through Shutdown, which persists nothing.
func TestRelayManager_ShutdownDoesNotPersistOff(t *testing.T) {
	var persisted []bool
	m := &discordRelayManager{
		base:       context.Background(),
		runner:     &fakeRunner{},
		operatorID: "op123",
		start:      func(_ context.Context, _ string) error { return nil },
		persist: func(enabled bool, op string, allowExec bool) error {
			persisted = append(persisted, enabled)
			return nil
		},
	}
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}

	m.Shutdown()
	if running, _, _ := m.Status(); running {
		t.Error("should be stopped after Shutdown")
	}
	for _, on := range persisted {
		if !on {
			t.Fatalf("Shutdown persisted enabled=false (persist log %v); teardown must not write an off choice", persisted)
		}
	}

	// The operator's explicit Disable still persists "off".
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	if len(persisted) == 0 || persisted[len(persisted)-1] {
		t.Fatalf("Disable must persist enabled=false, persist log %v", persisted)
	}
}

// SetExec switches exec mode, reports it through Status, persists it alongside
// the running/operator state, and moves the Discord broker's lock with it.
func TestRelayManager_ExecToggle(t *testing.T) {
	var lastPersist string
	br := newDiscordApprovalBroker(testLog())
	m := &discordRelayManager{
		base:       context.Background(),
		runner:     &fakeRunner{}, // non-nil so SetExec's "not configured" guard passes
		broker:     br,
		operatorID: "op123",
		persist: func(enabled bool, op string, allowExec bool) error {
			lastPersist = fmt.Sprintf("%v:%s:%v", enabled, op, allowExec)
			return nil
		},
	}
	br.SetLock(m.execLocked)
	if ok, err := br.SetPermission(catExec, "allow"); err != nil || !ok {
		t.Fatalf("SetPermission: ok=%v err=%v", ok, err)
	}
	if _, _, execOn := m.Status(); execOn {
		t.Fatal("exec mode should default off")
	}
	if br.Confirm(context.Background(), "os_exec", "ls") {
		t.Fatal("exec mode off must refuse os_exec even with relay exec=allow")
	}
	if err := m.SetExec(true); err != nil {
		t.Fatal(err)
	}
	if _, _, execOn := m.Status(); !execOn || !m.ExecOn() {
		t.Error("Status and ExecOn should report exec on")
	}
	if lastPersist != "false:op123:true" {
		t.Errorf("exec toggle should persist with allowExec=true, got %q", lastPersist)
	}
	if !br.Confirm(context.Background(), "os_exec", "ls") {
		t.Fatal("exec mode on must hand os_exec to the exec category (allow)")
	}
	if err := m.SetExec(false); err != nil {
		t.Fatal(err)
	}
	if m.ExecOn() || br.Confirm(context.Background(), "os_exec", "ls") {
		t.Error("SetExec(false) must close exec again on the next call")
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
