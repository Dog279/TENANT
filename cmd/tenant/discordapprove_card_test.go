package main

// Tests for the approval CARD overhaul: v2 layout with fallbacks, in-place
// verdicts, expiry freezing the original message, ACK-before-release, and the
// operator's timeout knob. Each is a fail-before/pass-after test against the
// pre-overhaul approver (message id discarded, bare "Approved." rewrite, silent
// text fallback, synchronous ACK-after-signal).

import (
	"context"
	"strings"
	"testing"
	"time"

	"tenant/internal/plugins/discord"
)

// approveNonce reads the nonce off the i-th posted button row — it lives only
// in the Approve button's custom_id now; the card text carries no reply
// instructions.
func approveNonce(fs *fakeSender, i int) string {
	for _, b := range fs.buttonsAt(i) {
		if n, ok := strings.CutPrefix(b.CustomID, "approve:"); ok {
			return n
		}
	}
	return ""
}

// The prompt posts as a v2 card by default: the action, no nonce clutter, and a
// live green Approve / red Deny pair with emoji on the button (not the label).
func TestApprover_CardIsV2AndClean(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = time.Second
	a.setChannel("dm1")
	go a.Confirm(context.Background(), "run a command", "echo hi")
	if !waitSends(fs, 1, time.Second) {
		t.Fatal("no card posted")
	}
	if len(fs.cardsAll()) != 1 {
		t.Fatalf("expected one v2 card, cards=%v sent=%v", fs.cardsAll(), fs.all())
	}
	if c := fs.cardsAll()[0]; strings.Contains(c, "APPROVE-") || !strings.Contains(c, "run a command") {
		t.Errorf("card must carry the action and no nonce instructions: %q", c)
	}
	bs := fs.buttonsAt(0)
	if len(bs) != 2 || bs[0].Style != 3 || bs[1].Style != 4 || bs[0].Emoji == "" || bs[0].Disabled || bs[1].Disabled {
		t.Errorf("want a live green Approve + red Deny pair with emoji, got %+v", bs)
	}
	if strings.ContainsAny(bs[0].Label, "✅⛔") {
		t.Errorf("the glyph belongs in the emoji field, not the label: %q", bs[0].Label)
	}
}

// Expiry rewrites the ORIGINAL card — buttons disabled, "Expired", action kept
// — instead of posting a second message and leaving live buttons behind.
func TestApprover_ExpiryFreezesOriginalCard(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = 80 * time.Millisecond
	a.setChannel("dm1")
	if a.Confirm(context.Background(), "act", "d") {
		t.Fatal("an un-answered approval must DENY")
	}
	edits := fs.editsAll()
	if len(edits) != 1 || !strings.Contains(edits[0], "Expired") || !strings.Contains(edits[0], "act") {
		t.Fatalf("expiry must edit the original card to Expired while keeping the action, edits=%v", edits)
	}
	if bs := fs.lastEditButtons(); len(bs) != 2 || !bs[0].Disabled || !bs[1].Disabled {
		t.Errorf("an expired card must have its buttons disabled, got %+v", bs)
	}
	for _, s := range fs.all() {
		if strings.Contains(s, "expired") {
			t.Errorf("no separate 'expired' message should post when the card itself can be edited: %q", s)
		}
	}
}

// v2 rejected ⇒ legacy buttons. Both rejected ⇒ a text prompt that carries the
// nonce reply instructions AND surfaces on the operator feed — a degraded
// prompt is never silent.
func TestApprover_CardFallbackChain(t *testing.T) {
	t.Run("v2 to legacy", func(t *testing.T) {
		c := captureChannelEvents(t)
		fs := &fakeSender{cardFail: true}
		a := newDiscordApprover(fs, nil)
		a.timeout = time.Second
		a.setChannel("dm1")
		go a.Confirm(context.Background(), "act", "d")
		if !waitSends(fs, 1, time.Second) {
			t.Fatal("no prompt")
		}
		if len(fs.cardsAll()) != 0 {
			t.Error("a rejected v2 card must not count as posted")
		}
		if len(fs.buttonsAt(0)) != 2 {
			t.Errorf("legacy buttons must post instead, got %+v", fs.buttonsAt(0))
		}
		fb, posted := c.wait(discord.CardFallback, ""), c.wait(discord.CardPosted, "")
		if fb == nil || eventField(fb, "fallback").Text() != "legacy buttons" || posted == nil || eventField(posted, "layout").Text() != "legacy" {
			t.Errorf("want the fallback and the legacy card recorded, got %+v and %+v", fb, posted)
		}
	})
	t.Run("legacy to text", func(t *testing.T) {
		fs := &fakeSender{cardFail: true, componentsFail: true}
		a := newDiscordApprover(fs, nil)
		a.timeout = time.Second
		a.setChannel("dm1")
		notified := make(chan string, 1)
		a.notify = func(s string) {
			select {
			case notified <- s:
			default:
			}
		}
		go a.Confirm(context.Background(), "act", "d")
		if !waitSends(fs, 1, time.Second) {
			t.Fatal("no prompt")
		}
		if p := fs.all()[0]; nonceFrom(p) == "" {
			t.Errorf("the text fallback must carry APPROVE-<nonce> reply instructions: %q", p)
		}
		select {
		case n := <-notified:
			if !strings.Contains(n, "failed to post") {
				t.Errorf("the feed line must say the buttons failed to post, got %q", n)
			}
		case <-time.After(time.Second):
			t.Error("a degraded prompt must surface on the operator feed")
		}
	})
}

// A click is ACKed FIRST: by the time Confirm returns, the in-place verdict is
// already recorded, and it names the clicker.
func TestApprover_ClickAcksBeforeRelease(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = time.Second
	a.setChannel("dm1")
	res := make(chan bool, 1)
	go func() { res <- a.Confirm(context.Background(), "act", "d") }()
	if !waitSends(fs, 1, time.Second) {
		t.Fatal("no card")
	}
	a.tryInteraction(discord.Interaction{ID: "i", Token: "t", UserID: "op1", CustomID: "approve:" + approveNonce(fs, 0)})
	if v, ok := waitBool(res, time.Second); !ok || !v {
		t.Fatal("should approve")
	}
	ups := fs.updatesAll()
	if len(ups) != 1 {
		t.Fatalf("the ACK must be recorded by the time Confirm returns, updates=%v", ups)
	}
	if !strings.Contains(ups[0], "<@op1>") {
		t.Errorf("the verdict should name the clicker, got %q", ups[0])
	}
}

// A text reply also freezes the card (an out-of-band edit — there is no
// interaction token) and still confirms in-thread.
func TestApprover_TextReplyFreezesCard(t *testing.T) {
	fs := &fakeSender{}
	a := newDiscordApprover(fs, nil)
	a.timeout = time.Second
	a.setChannel("dm1")
	res := make(chan bool, 1)
	go func() { res <- a.Confirm(context.Background(), "act", "d") }()
	if !waitSends(fs, 1, time.Second) {
		t.Fatal("no card")
	}
	a.tryConsume(discord.Inbound{ChannelID: "dm1", AuthorID: "op1", Content: "approve-" + approveNonce(fs, 0)})
	if v, ok := waitBool(res, time.Second); !ok || !v {
		t.Fatal("should approve")
	}
	if e := fs.editsAll(); len(e) != 1 || !strings.Contains(e[0], "Approved") || !strings.Contains(e[0], "<@op1>") {
		t.Errorf("a text approval must freeze the card with the verdict, edits=%v", e)
	}
}

// The manager bounds the operator's timeout, pushes it to the live approver,
// and persists it.
func TestRelayManager_ApprovalTimeout(t *testing.T) {
	a := newDiscordApprover(&fakeSender{}, nil)
	m := &discordRelayManager{approver: a}
	if m.ApprovalTimeout() != approvalTimeout {
		t.Errorf("default = %s, got %s", approvalTimeout, m.ApprovalTimeout())
	}
	if err := m.SetApprovalTimeout(3 * time.Second); err == nil {
		t.Error("a 3s timeout must be rejected (below the floor)")
	}
	var persisted time.Duration
	m.persistTimeout = func(d time.Duration) error { persisted = d; return nil }
	if err := m.SetApprovalTimeout(10 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if a.timeout != 10*time.Minute || persisted != 10*time.Minute || m.ApprovalTimeout() != 10*time.Minute {
		t.Errorf("timeout must apply to the approver and persist: approver=%s persisted=%s", a.timeout, persisted)
	}
}
