package semantic_test

import (
	"context"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
)

func TestSetValidTo_RoundtripAndClear(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("user works at Acme", vecA()))
	at := time.Unix(1_700_000_000, 0).UTC()

	if err := s.SetValidTo(ctx, id, at); err != nil {
		t.Fatalf("SetValidTo: %v", err)
	}
	sig, _ := s.GetSignals(ctx, id)
	if !sig.ValidTo.Equal(at) {
		t.Errorf("valid_to = %v, want %v", sig.ValidTo, at)
	}
	// Clearing (zero) makes it currently-true again.
	if err := s.SetValidTo(ctx, id, time.Time{}); err != nil {
		t.Fatalf("SetValidTo clear: %v", err)
	}
	sig, _ = s.GetSignals(ctx, id)
	if !sig.ValidTo.IsZero() {
		t.Errorf("valid_to should be cleared, got %v", sig.ValidTo)
	}
}

// FactsAsOf returns the world-state at a timestamp by event-time window,
// INCLUDING superseded facts (historical recall) and timeless facts.
func TestFactsAsOf_WindowsAndSuperseded(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	T := time.Unix(1_700_000_000, 0).UTC()

	oldID, _ := s.Insert(ctx, mkFact("user works at Acme", vecA()))
	newID, _ := s.Insert(ctx, mkFact("user works at Globex", vecB()))
	timelessID, _ := s.Insert(ctx, mkFact("user prefers Go", vecC()))

	// Transition at T: Acme valid until T, Globex valid from T. Acme is also
	// soft-superseded by Globex (as the distiller would do).
	if err := s.SetValidTo(ctx, oldID, T); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSignals(ctx, semantic.Signals{FactID: newID, Importance: 0.5, ValidFrom: T}); err != nil {
		t.Fatal(err)
	}
	if err := s.Supersede(ctx, oldID, newID); err != nil {
		t.Fatal(err)
	}

	ids := func(facts []*semantic.Fact) map[int64]bool {
		m := map[int64]bool{}
		for _, f := range facts {
			m[f.ID] = true
		}
		return m
	}

	// Before the transition: Acme (superseded but historically true) + timeless.
	before, err := s.FactsAsOf(ctx, "main", T.Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("FactsAsOf before: %v", err)
	}
	gotBefore := ids(before)
	if !gotBefore[oldID] {
		t.Error("as-of before transition should include the (superseded) Acme fact")
	}
	if gotBefore[newID] {
		t.Error("as-of before transition must NOT include the not-yet-valid Globex fact")
	}
	if !gotBefore[timelessID] {
		t.Error("timeless fact should always be included")
	}

	// After the transition: Globex + timeless, NOT Acme.
	after, err := s.FactsAsOf(ctx, "main", T.Add(time.Hour), 50)
	if err != nil {
		t.Fatalf("FactsAsOf after: %v", err)
	}
	gotAfter := ids(after)
	if !gotAfter[newID] {
		t.Error("as-of after transition should include Globex")
	}
	if gotAfter[oldID] {
		t.Error("as-of after transition must NOT include the ended Acme fact")
	}
	if !gotAfter[timelessID] {
		t.Error("timeless fact should always be included")
	}
}

func TestFactsAsOf_ExcludesTombstoned(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("forget me", vecA()))
	if err := s.Tombstone(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := s.FactsAsOf(ctx, "main", time.Now().UTC(), 50)
	if err != nil {
		t.Fatalf("FactsAsOf: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tombstoned fact should be excluded from as-of, got %d", len(got))
	}
}
