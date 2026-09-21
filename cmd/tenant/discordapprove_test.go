package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"tenant/internal/plugins/discord"
)

// nonceFrom extracts the token from a prompt's "…`APPROVE-<nonce>`…".
func nonceFrom(s string) string {
	i := strings.Index(s, "APPROVE-")
	if i < 0 {
		return ""
	}
	rest := s[i+len("APPROVE-"):]
	if j := strings.IndexAny(rest, "` \n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func waitBool(ch chan bool, d time.Duration) (bool, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-time.After(d):
		return false, false
	}
}

func TestApprover_NonceApproveOnce(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = 2 * time.Second
	a.setChannel("dm1")

	res := make(chan bool, 1)
	go func() { res <- a.Confirm(context.Background(), "posts a Discord message", "to #x: hi") }()

	if !waitSends(fs, 1, time.Second) {
		t.Fatal("no approval prompt posted")
	}
	nonce := approveNonce(fs, 0)
	if nonce == "" {
		t.Fatalf("no nonce on the posted buttons: %v", fs.all())
	}
	if !a.tryConsume(discord.Inbound{ChannelID: "dm1", Content: "APPROVE-" + nonce}) {
		t.Error("the exact nonce reply should be consumed")
	}
	if v, ok := waitBool(res, time.Second); !ok || !v {
		t.Errorf("Confirm should approve on the matching nonce (got v=%v ok=%v)", v, ok)
	}
}

// Two stacked approvals: one nonce reply satisfies ONLY its own Confirm (the
// debate's must-fix — a bare "y" would mis-bind).
func TestApprover_StackedCorrelation(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = 3 * time.Second
	a.setChannel("dm1")

	r1 := make(chan bool, 1)
	r2 := make(chan bool, 1)
	go func() { r1 <- a.Confirm(context.Background(), "action one", "d1") }()
	if !waitSends(fs, 1, time.Second) {
		t.Fatal("prompt 1 missing")
	}
	go func() { r2 <- a.Confirm(context.Background(), "action two", "d2") }()
	if !waitSends(fs, 2, time.Second) {
		t.Fatal("prompt 2 missing")
	}
	n1 := approveNonce(fs, 0)
	n2 := approveNonce(fs, 1)
	if n1 == "" || n2 == "" || n1 == n2 {
		t.Fatalf("expected two distinct nonces, got %q %q", n1, n2)
	}

	// Reply nonce1 → only r1 approves; r2 stays pending.
	a.tryConsume(discord.Inbound{ChannelID: "dm1", Content: "APPROVE-" + n1})
	if v, ok := waitBool(r1, time.Second); !ok || !v {
		t.Error("r1 should approve on nonce1")
	}
	if _, ok := waitBool(r2, 150*time.Millisecond); ok {
		t.Error("r2 must NOT resolve from nonce1 (mis-binding)")
	}

	// Reply nonce2 → r2 approves.
	a.tryConsume(discord.Inbound{ChannelID: "dm1", Content: "APPROVE-" + n2})
	if v, ok := waitBool(r2, time.Second); !ok || !v {
		t.Error("r2 should approve on nonce2")
	}
}

func TestApprover_TimeoutDenies(t *testing.T) {
	a := newDiscordApprover(&fakeSender{}, nil)
	a.timeout = 80 * time.Millisecond
	a.setChannel("dm1")
	if a.Confirm(context.Background(), "act", "d") {
		t.Error("an un-answered approval must DENY (fail closed)")
	}
}

func TestApprover_DenyKeyword(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = 2 * time.Second
	a.setChannel("dm1")
	res := make(chan bool, 1)
	go func() { res <- a.Confirm(context.Background(), "act", "d") }()
	if !waitSends(fs, 1, time.Second) {
		t.Fatal("no prompt")
	}
	if !a.tryConsume(discord.Inbound{ChannelID: "dm1", Content: "deny"}) {
		t.Error("'deny' should be consumed")
	}
	if v, ok := waitBool(res, time.Second); !ok || v {
		t.Error("'deny' must refuse the action")
	}
}

func TestApprover_NoChannelFailsClosed(t *testing.T) {
	a := newDiscordApprover(&fakeSender{}, nil) // no setChannel → no operator channel
	if a.Confirm(context.Background(), "act", "d") {
		t.Error("with no operator channel the approver must DENY (fail closed)")
	}
}

func TestApprover_ButtonApprove(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = 2 * time.Second
	a.setChannel("dm1")

	res := make(chan bool, 1)
	go func() { res <- a.Confirm(context.Background(), "run a command", "echo hi") }()
	if !waitSends(fs, 1, time.Second) {
		t.Fatal("no button prompt posted")
	}
	nonce := approveNonce(fs, 0)
	if nonce == "" {
		t.Fatalf("no nonce on the posted buttons: %v", fs.all())
	}
	if !a.tryInteraction(discord.Interaction{ID: "i1", Token: "tok", CustomID: "approve:" + nonce}) {
		t.Error("the Approve button should be consumed")
	}
	if v, ok := waitBool(res, time.Second); !ok || !v {
		t.Error("Confirm should approve on the Approve button")
	}
	// The click is ACKed by rewriting the card IN PLACE: the verdict, the
	// original action kept as the record, and the buttons frozen.
	ups := fs.updatesAll()
	if len(ups) == 0 || !strings.Contains(ups[0], "Approved") || !strings.Contains(ups[0], "run a command") {
		t.Errorf("the interaction must be ACKed by editing the card to its verdict (keeping the action), updates=%v", ups)
	}
	if bs := fs.lastUpdateButtons(); len(bs) != 2 || !bs[0].Disabled || !bs[1].Disabled {
		t.Errorf("a resolved card must freeze its buttons (disabled), got %+v", bs)
	}
}

func TestApprover_ButtonDeny(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = 2 * time.Second
	a.setChannel("dm1")
	res := make(chan bool, 1)
	go func() { res <- a.Confirm(context.Background(), "act", "d") }()
	waitSends(fs, 1, time.Second)
	a.tryInteraction(discord.Interaction{ID: "i", Token: "t", CustomID: "deny:" + approveNonce(fs, 0)})
	if v, ok := waitBool(res, time.Second); !ok || v {
		t.Error("the Deny button should refuse the action")
	}
}

func TestApprover_StaleButtonAcks(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	// No pending approval: a click on a stale button must still ACK (so Discord
	// doesn't show "interaction failed") and report consumed.
	if !a.tryInteraction(discord.Interaction{ID: "i", Token: "t", CustomID: "approve:GONE"}) {
		t.Error("a stale button should be ACKed/consumed")
	}
	if len(fs.ephemeralsAll()) == 0 {
		t.Error("a stale button must be ACKed (privately, to the clicker)")
	}
	if len(fs.updatesAll()) != 0 {
		t.Error("a stale click must not rewrite the card")
	}
}

func TestParseButtonID(t *testing.T) {
	if ap, n, ok := parseButtonID("approve:7Q2X"); !ok || !ap || n != "7Q2X" {
		t.Errorf("approve parse wrong: %v %q %v", ap, n, ok)
	}
	if ap, n, ok := parseButtonID("deny:ZZ"); !ok || ap || n != "ZZ" {
		t.Errorf("deny parse wrong: %v %q %v", ap, n, ok)
	}
	if _, _, ok := parseButtonID("garbage"); ok {
		t.Error("a garbage custom_id must not parse")
	}
}

// The relay gates interactions to the operator before the approver sees them.
func TestRelay_InteractionOperatorGate(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.setChannel("dm1")
	r := newRelay(&fakeRunner{}, fs, op, nil)
	r.approver = a

	// A non-operator click never acts — but it IS answered privately, so the
	// intruder's client doesn't show "This interaction failed" (and the card
	// is untouched).
	r.handleInteraction(discord.Interaction{UserID: "intruder", ID: "x", Token: "x", CustomID: "approve:X"})
	if eph := fs.ephemeralsAll(); len(eph) != 1 || !strings.Contains(eph[0], "operator") {
		t.Errorf("a non-operator click must get an ephemeral 'operator only' ACK and nothing else, got %v", eph)
	}
	if len(fs.updatesAll()) != 0 {
		t.Error("a non-operator click must never touch the card")
	}
	// The operator's click does reach the approver (a stale button → a private ACK).
	r.handleInteraction(discord.Interaction{UserID: op, ID: "i", Token: "t", CustomID: "approve:X"})
	if len(fs.ephemeralsAll()) != 2 {
		t.Error("the operator's click should reach the approver")
	}
	// An unknown button (neither approve/deny nor ext-) is still answered.
	r.handleInteraction(discord.Interaction{UserID: op, ID: "j", Token: "u", CustomID: "poll:vote"})
	if len(fs.ephemeralsAll()) != 3 {
		t.Error("an unrecognized button must still be ACKed rather than left failing")
	}
}

// Approve-once: granting one action does NOT auto-approve the next — there is no
// session/always memory, so a stale grant can't leak.
func TestApprover_ApproveOnceNoSession(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = 2 * time.Second
	a.setChannel("dm1")

	r1 := make(chan bool, 1)
	go func() { r1 <- a.Confirm(context.Background(), "act", "d") }()
	waitSends(fs, 1, time.Second)
	a.tryConsume(discord.Inbound{ChannelID: "dm1", Content: "APPROVE-" + approveNonce(fs, 0)})
	if v, _ := waitBool(r1, time.Second); !v {
		t.Fatal("first action should approve")
	}

	// A second action must re-prompt, not auto-approve.
	r2 := make(chan bool, 1)
	go func() { r2 <- a.Confirm(context.Background(), "act2", "d2") }()
	if _, ok := waitBool(r2, 200*time.Millisecond); ok {
		t.Error("second action auto-approved — there must be NO session/always grant over Discord")
	}
	if !waitSends(fs, 3, time.Second) { // prompt1, approved1, prompt2
		t.Fatal("second action did not re-prompt")
	}
}
