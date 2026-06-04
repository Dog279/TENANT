package main

import (
	"context"
	"testing"
)

// originConfirm must route by ORIGIN: a local (unstamped) turn uses the broker;
// an offsite (stamped) turn uses the ctx approver and NEVER consults the broker
// — that's the "no leak" property (a local exec=allow can't reach an offsite
// turn).
func TestOriginConfirm_RoutesByOrigin(t *testing.T) {
	var brokerCalls, approverCalls int
	broker := func(context.Context, string, string) bool { brokerCalls++; return true }
	approver := func(context.Context, string, string) bool { approverCalls++; return false }

	oc := originConfirm(broker)

	// Unstamped (local) turn → broker decides.
	if !oc(context.Background(), "os_exec", "rm -rf x") {
		t.Error("a local turn should use the broker (which returned true)")
	}
	if brokerCalls != 1 || approverCalls != 0 {
		t.Fatalf("local turn must hit ONLY the broker: broker=%d approver=%d", brokerCalls, approverCalls)
	}

	// Stamped (offsite) turn → ctx approver decides; broker untouched.
	ctx := withOffsiteConfirm(context.Background(), approver)
	if oc(ctx, "os_exec", "rm -rf x") {
		t.Error("an offsite turn must use the approver (which returned false)")
	}
	if approverCalls != 1 {
		t.Errorf("offsite turn must hit the approver once, got %d", approverCalls)
	}
	if brokerCalls != 1 {
		t.Error("offsite turn must NOT consult the local broker — that would be a leak")
	}
}

// With no broker and no offsite approver, originConfirm fails closed (deny).
func TestOriginConfirm_NoBrokerFailsClosed(t *testing.T) {
	if originConfirm(nil)(context.Background(), "os_exec", "x") {
		t.Error("no broker + no offsite approver must fail closed (deny)")
	}
}

// withOffsiteConfirm(nil) is a no-op: the turn stays local-broker-gated rather
// than silently becoming unstamped-but-special.
func TestWithOffsiteConfirm_NilIsNoop(t *testing.T) {
	ctx := withOffsiteConfirm(context.Background(), nil)
	if offsiteConfirmFrom(ctx) != nil {
		t.Error("withOffsiteConfirm(nil) must not stamp the ctx")
	}
}
