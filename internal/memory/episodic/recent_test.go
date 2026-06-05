package episodic_test

import (
	"context"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
)

func recInsert(t *testing.T, s *episodic.Store, agent, prompt string, ts time.Time, tombstone bool) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), &episodic.Episode{
		AgentID: agent, Prompt: prompt, Response: "resp:" + prompt,
		EmbedderID: "fake", Embedding: []float32{1, 0}, Timestamp: ts,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if tombstone {
		if err := s.Tombstone(context.Background(), id); err != nil {
			t.Fatalf("tombstone: %v", err)
		}
	}
	return id
}

// Recent: newest-by-id, chronological order, EXACT agent (no sub-agent glob),
// non-tombstoned only, capped by n.
func TestRecent_OrderScopeTombstone(t *testing.T) {
	s, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	recInsert(t, s, "main", "first", base, false)                         // id1
	recInsert(t, s, "main", "second", base.Add(1*time.Minute), false)     // id2
	recInsert(t, s, "main", "deleted", base.Add(2*time.Minute), true)     // id3 tombstoned
	recInsert(t, s, "main", "third", base.Add(3*time.Minute), false)      // id4
	recInsert(t, s, "main-researcher-1", "sub", base.Add(4*time.Minute), false) // id5 — must NOT leak
	recInsert(t, s, "other", "stranger", base.Add(5*time.Minute), false)  // id6 — must NOT leak

	got, err := s.Recent(context.Background(), "main", 2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 2 || got[0].Prompt != "second" || got[1].Prompt != "third" {
		t.Fatalf("want [second third] chronological, got %v", promptsOf(got))
	}

	all, _ := s.Recent(context.Background(), "main", 10)
	if len(all) != 3 {
		t.Fatalf("want 3 live main episodes, got %d (%v)", len(all), promptsOf(all))
	}
	for _, e := range all {
		if e.Prompt == "deleted" {
			t.Error("tombstoned episode resurfaced in Recent")
		}
		if e.AgentID != "main" {
			t.Errorf("non-main agent leaked into recap: %s", e.AgentID)
		}
	}

	// Exact-agent scoping: sub-agent query returns only its own row.
	sub, _ := s.Recent(context.Background(), "main-researcher-1", 10)
	if len(sub) != 1 || sub[0].Prompt != "sub" {
		t.Errorf("sub-agent scope wrong: %v", promptsOf(sub))
	}

	// Guards.
	if r, _ := s.Recent(context.Background(), "", 5); r != nil {
		t.Error("empty agentID should return nil")
	}
	if r, _ := s.Recent(context.Background(), "main", 0); r != nil {
		t.Error("n<=0 should return nil")
	}
}

func promptsOf(eps []*episodic.Episode) []string {
	out := make([]string, len(eps))
	for i, e := range eps {
		out[i] = e.Prompt
	}
	return out
}
