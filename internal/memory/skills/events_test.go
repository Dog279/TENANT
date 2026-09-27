package skills

import (
	"context"
	"fmt"
	"testing"

	"tenant/internal/logs/event/eventtest"
)

// Accepting, enabling, disabling and forgetting a skill are events, and
// skill_history records each, like any other change (TEN-394).
func TestStateChangesAreEventsAndHistory(t *testing.T) {
	rec := eventtest.Capture(t)
	s := mk(t)
	ctx := context.Background()
	id, err := s.UpsertWithSource(ctx, &Skill{AgentID: "a", Name: "deploy", Description: "ship it", Status: StatusProposed}, "induction")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetEnabledByName(ctx, "a", "deploy", false); !ok || err != nil {
		t.Fatalf("disable: %v %v", ok, err)
	}
	if ok, err := s.SetTrust(ctx, "a", "deploy", TrustUntrusted); !ok || err != nil {
		t.Fatalf("distrust: %v %v", ok, err)
	}
	if err := s.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetEnabledByName(ctx, "a", "nosuch", true); ok || err != nil {
		t.Fatalf("an unknown skill: %v %v", ok, err)
	}
	if err := s.Accept(ctx, 999); err != nil {
		t.Fatalf("accepting an unknown id: %v", err)
	}

	var msgs []string
	for _, e := range rec.Events() {
		msgs = append(msgs, e.Msg)
	}
	want := "[Skill deploy saved (induction) Skill deploy accepted Skill deploy disabled Skill deploy is now untrusted (was trusted) Skill deploy removed]"
	if fmt.Sprint(msgs) != want {
		t.Fatalf("events %q\nwant %s", msgs, want)
	}
	hist, err := s.History(ctx, "a", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, h := range hist {
		sources = append(sources, fmt.Sprintf("v%d %s %s", h.Version, h.ChangeSource, h.PriorStatus))
	}
	if got := fmt.Sprint(sources); got != "[v4 tombstone live v3 distrust live v2 disable live v1 accept proposed]" {
		t.Fatalf("history %s", got)
	}
}
