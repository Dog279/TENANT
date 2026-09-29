package approval

import (
	"context"
	"testing"

	"tenant/internal/control"
)

const explicitAction = "test_explicit_merge" // files under destructive (unknown action)

func init() { RequireExplicit(explicitAction) }

// explicitBroker answers every prompt with d and counts the prompts.
func explicitBroker(t *testing.T, d control.ApprovalDecision) (*Broker, *int) {
	t.Helper()
	b := newTestBroker(testLog())
	b.SetRules(NewRules())
	asked := 0
	b.SetAsk(func(context.Context, control.ApprovalRequest) control.ApprovalDecision {
		asked++
		return d
	})
	return b, &asked
}

// A category allow was given for other actions: it must not approve an
// explicit-only one. The operator is asked instead.
func TestExplicitOnly_CategoryAllowStillAsks(t *testing.T) {
	b, asked := explicitBroker(t, control.DenyOnce)
	b.SetPermission(CatDestructive, "allow")
	if b.Confirm(context.Background(), explicitAction, "merge") {
		t.Fatal("a category allow approved an explicit-only action")
	}
	if *asked != 1 {
		t.Fatalf("asked %d times, want 1", *asked)
	}
}

func TestExplicitOnly_SessionGrantStillAsks(t *testing.T) {
	b, asked := explicitBroker(t, control.DenyOnce)
	b.mu.Lock()
	b.session[CatDestructive] = true
	b.mu.Unlock()
	if b.Confirm(context.Background(), explicitAction, "merge") {
		t.Fatal("a session grant approved an explicit-only action")
	}
	if *asked != 1 {
		t.Fatalf("asked %d times, want 1", *asked)
	}
}

// A category deny is still the operator's word: denied without a prompt.
func TestExplicitOnly_CategoryDenyHolds(t *testing.T) {
	b, asked := explicitBroker(t, control.ApproveOnce)
	b.SetPermission(CatDestructive, "deny")
	if b.Confirm(context.Background(), explicitAction, "merge") {
		t.Fatal("a category deny was overridden")
	}
	if *asked != 0 {
		t.Fatalf("asked %d times under deny, want 0", *asked)
	}
}

// An exact tool rule is the explicit answer: allow approves, deny refuses,
// neither asks.
func TestExplicitOnly_ExactRuleAnswers(t *testing.T) {
	for mode, want := range map[string]bool{"allow": true, "deny": false} {
		b, asked := explicitBroker(t, control.ApproveOnce)
		if err := b.rules.Set(explicitAction, mode); err != nil {
			t.Fatal(err)
		}
		if got := b.Confirm(context.Background(), explicitAction, "merge"); got != want {
			t.Errorf("rule %s: Confirm = %v, want %v", mode, got, want)
		}
		if *asked != 0 {
			t.Errorf("rule %s: asked %d times, want 0", mode, *asked)
		}
	}
}

// "Always" on the prompt approves this time only: it neither opens the
// category nor writes an allow rule, so the next one asks again.
func TestExplicitOnly_AlwaysCoversThisTimeOnly(t *testing.T) {
	b, asked := explicitBroker(t, control.ApproveAlways)
	for i := 0; i < 2; i++ {
		if !b.Confirm(context.Background(), explicitAction, "merge") {
			t.Fatal("a live approval was refused")
		}
	}
	if *asked != 2 {
		t.Fatalf("asked %d times, want 2 (Always must not stick)", *asked)
	}
	if b.modes[CatDestructive] != ModeAsk {
		t.Fatal("Always opened the category")
	}
	if len(b.rules.Rules()) != 0 {
		t.Fatalf("Always wrote a rule: %+v", b.rules.Rules())
	}
}

// Other actions in the category keep the normal behavior.
func TestExplicitOnly_OtherActionsUnchanged(t *testing.T) {
	b, asked := explicitBroker(t, control.DenyOnce)
	b.SetPermission(CatDestructive, "allow")
	if !b.Confirm(context.Background(), "some_destructive_thing", "x") {
		t.Fatal("category allow stopped working for ordinary actions")
	}
	if *asked != 0 {
		t.Fatal("an ordinary action under allow prompted")
	}
}

// A glob allow was written for other tools: it doesn't answer an
// explicit-only action, which asks instead. A glob deny still refuses.
func TestExplicitOnly_GlobRules(t *testing.T) {
	b, asked := explicitBroker(t, control.DenyOnce)
	if err := b.rules.Set("test_explicit_*", "allow"); err != nil {
		t.Fatal(err)
	}
	if b.Confirm(context.Background(), explicitAction, "merge") || *asked != 1 {
		t.Fatalf("a glob allow answered an explicit-only action (asked %d)", *asked)
	}
	b2, asked2 := explicitBroker(t, control.ApproveOnce)
	if err := b2.rules.Set("test_explicit_*", "deny"); err != nil {
		t.Fatal(err)
	}
	if b2.Confirm(context.Background(), explicitAction, "merge") || *asked2 != 0 {
		t.Fatalf("a glob deny didn't refuse (asked %d)", *asked2)
	}
}

// "Always" on a prompt an exact ask rule forced doesn't flip it to allow.
func TestExplicitOnly_AskRuleAlwaysDoesNotSave(t *testing.T) {
	b, asked := explicitBroker(t, control.ApproveAlways)
	if err := b.rules.Set(explicitAction, "ask"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if !b.Confirm(context.Background(), explicitAction, "merge") {
			t.Fatal("a live approval was refused")
		}
	}
	if *asked != 2 {
		t.Fatalf("asked %d times, want 2", *asked)
	}
	if mode, _, _ := b.rules.Lookup(explicitAction); mode != "ask" {
		t.Fatalf("rule became %q", mode)
	}
}
