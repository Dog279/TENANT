package main

import (
	"context"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/tui"
)

// TestHeadlessApprovals_DrainDecide: a request posted to the broker channel
// becomes pending, surfaces a feed event, and a Decide unblocks the waiter with
// the chosen decision.
func TestHeadlessApprovals_DrainDecide(t *testing.T) {
	reqs := make(chan tui.ApprovalRequest, 4)
	var events []agent.Event
	h := newHeadlessApprovals(reqs, func(e agent.Event) { events = append(events, e) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)

	// Simulate the broker's askDecision: post a request, then block on reply.
	reply := make(chan tui.ApprovalDecision, 1)
	reqs <- tui.ApprovalRequest{Category: "exec", Action: "os_exec", Detail: "rm -rf /tmp/x", Reply: reply}

	// Wait for it to register as pending.
	var id string
	waitFor(t, func() bool {
		p := h.Pending()
		if len(p) == 1 {
			id = p[0].ID
			return true
		}
		return false
	})

	if got := h.Pending()[0]; got.Action != "os_exec" || got.Category != "exec" {
		t.Fatalf("pending = %+v", got)
	}
	if len(events) != 1 || events[0].Kind != agent.EventApproval {
		t.Fatalf("expected one EventApproval, got %+v", events)
	}

	if err := h.Decide(id, "approve"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	select {
	case d := <-reply:
		if d != tui.ApproveOnce {
			t.Fatalf("decision = %v, want ApproveOnce", d)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never received the decision")
	}
	if len(h.Pending()) != 0 {
		t.Fatalf("pending should be empty after decide, got %d", len(h.Pending()))
	}
}

// TestHeadlessApprovals_UnknownDecisionFailsClosed: an unknown/empty decision
// string is rejected (never silently approves) and leaves the request pending.
func TestHeadlessApprovals_UnknownDecision(t *testing.T) {
	reqs := make(chan tui.ApprovalRequest, 4)
	h := newHeadlessApprovals(reqs, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)

	reply := make(chan tui.ApprovalDecision, 1)
	reqs <- tui.ApprovalRequest{Category: "send", Action: "gmail_send", Detail: "to: x", Reply: reply}
	var id string
	waitFor(t, func() bool {
		p := h.Pending()
		if len(p) == 1 {
			id = p[0].ID
			return true
		}
		return false
	})

	if err := h.Decide(id, "yolo"); err == nil {
		t.Fatal("unknown decision should error")
	}
	if err := h.Decide("ap-999", "approve"); err == nil {
		t.Fatal("unknown id should error")
	}
	// Still pending (the bad decision didn't resolve it).
	if len(h.Pending()) != 1 {
		t.Fatalf("request should remain pending after a rejected decision, got %d", len(h.Pending()))
	}
}

// TestHeadlessApprovals_ShutdownDeniesAll: cancelling the drain context denies
// every pending request so blocked turn goroutines unblock (graceful shutdown).
func TestHeadlessApprovals_ShutdownDeniesAll(t *testing.T) {
	reqs := make(chan tui.ApprovalRequest, 4)
	h := newHeadlessApprovals(reqs, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go h.run(ctx)

	reply := make(chan tui.ApprovalDecision, 1)
	reqs <- tui.ApprovalRequest{Category: "exec", Action: "os_exec", Detail: "x", Reply: reply}
	waitFor(t, func() bool { return len(h.Pending()) == 1 })

	cancel() // shutdown
	select {
	case d := <-reply:
		if d != tui.DenyOnce {
			t.Fatalf("shutdown decision = %v, want DenyOnce", d)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not deny the pending request")
	}
}
