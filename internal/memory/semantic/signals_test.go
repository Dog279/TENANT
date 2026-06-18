package semantic_test

import (
	"context"
	"math"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
)

const day = 24 * time.Hour

// --- decay: the additive guarantee + importance-stretched longevity ---

// Default signals (importance=0.5, not pinned) must decay IDENTICALLY to
// the signal-free EffectiveConfidence at every age — the load-bearing
// additive invariant (a fact with no signals row is unchanged).
func TestEffectiveConfidenceWithSignals_DefaultMatchesLegacy(t *testing.T) {
	now := time.Now().UTC()
	def := semantic.Signals{Importance: semantic.DefaultImportance}
	for _, age := range []time.Duration{0, 10 * day, 29 * day, 30 * day, 100 * day, 200 * day, 364 * day, 365 * day, 400 * day} {
		f := &semantic.Fact{Confidence: 1.0, LastConfirmed: now.Add(-age)}
		legacy := f.EffectiveConfidence(now)
		withSig := f.EffectiveConfidenceWithSignals(now, def)
		if math.Abs(legacy-withSig) > 1e-9 {
			t.Errorf("age=%v: default-signals ec=%v != legacy ec=%v", age, withSig, legacy)
		}
	}
}

// A high-importance fact survives well past the flat 365d horizon, while
// the same fact at neutral importance has already decayed to zero.
func TestEffectiveConfidenceWithSignals_HighImportanceStretches(t *testing.T) {
	now := time.Now().UTC()
	f := &semantic.Fact{Confidence: 1.0, LastConfirmed: now.Add(-400 * day)}

	neutral := f.EffectiveConfidenceWithSignals(now, semantic.Signals{Importance: semantic.DefaultImportance})
	if neutral != 0 {
		t.Errorf("neutral fact at 400d should be fully decayed, got %v", neutral)
	}
	high := f.EffectiveConfidenceWithSignals(now, semantic.Signals{Importance: 0.9})
	if high <= 0 {
		t.Errorf("high-importance fact at 400d should still be alive, got %v", high)
	}
}

// Low-importance facts decay FASTER than neutral (junk fades sooner).
func TestEffectiveConfidenceWithSignals_LowImportanceShrinks(t *testing.T) {
	now := time.Now().UTC()
	f := &semantic.Fact{Confidence: 1.0, LastConfirmed: now.Add(-120 * day)}
	low := f.EffectiveConfidenceWithSignals(now, semantic.Signals{Importance: 0.1})
	neutral := f.EffectiveConfidenceWithSignals(now, semantic.Signals{Importance: semantic.DefaultImportance})
	if !(low < neutral) {
		t.Errorf("low importance should decay faster: low=%v neutral=%v", low, neutral)
	}
}

// Pinned facts never decay, regardless of age.
func TestEffectiveConfidenceWithSignals_PinnedNeverDecays(t *testing.T) {
	now := time.Now().UTC()
	f := &semantic.Fact{Confidence: 0.8, LastConfirmed: now.Add(-1000 * day)}
	got := f.EffectiveConfidenceWithSignals(now, semantic.Signals{Pinned: true, Importance: semantic.DefaultImportance})
	if got != 0.8 {
		t.Errorf("pinned fact should keep base confidence 0.8, got %v", got)
	}
}

// --- signals store CRUD ---

func TestSignals_GetDefaultsWhenNoRow(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, err := s.Insert(ctx, mkFact("a fact", vecA()))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	sig, err := s.GetSignals(ctx, id)
	if err != nil {
		t.Fatalf("GetSignals: %v", err)
	}
	if sig.Importance != semantic.DefaultImportance {
		t.Errorf("default importance = %v, want %v", sig.Importance, semantic.DefaultImportance)
	}
	if sig.Pinned || sig.Protected || sig.AccessCount != 0 {
		t.Errorf("unexpected non-default signals: %+v", sig)
	}
}

func TestSignals_UpsertRoundtrip(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("a fact", vecA()))
	want := semantic.Signals{FactID: id, Importance: 0.9, ConfirmCount: 3, AccessCount: 7, Pinned: true, Protected: true}
	if err := s.UpsertSignals(ctx, want); err != nil {
		t.Fatalf("UpsertSignals: %v", err)
	}
	got, err := s.GetSignals(ctx, id)
	if err != nil {
		t.Fatalf("GetSignals: %v", err)
	}
	if got.Importance != 0.9 || got.ConfirmCount != 3 || got.AccessCount != 7 || !got.Pinned || !got.Protected {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	// Upsert again (conflict path) to confirm replace semantics.
	want.Importance = 0.4
	want.Pinned = false
	if err := s.UpsertSignals(ctx, want); err != nil {
		t.Fatalf("UpsertSignals replace: %v", err)
	}
	got, _ = s.GetSignals(ctx, id)
	if got.Importance != 0.4 || got.Pinned {
		t.Errorf("replace failed: %+v", got)
	}
}

// ReinforceImportance is agreement-averaging (not a one-way max): a later
// LOWER score pulls the standing importance down (review finding 4).
func TestReinforceImportance_RunningAverage(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("a fact", vecA()))

	// Seed at 0.9, confirm_count 1 (as the distiller does on insert).
	if err := s.UpsertSignals(ctx, semantic.Signals{FactID: id, Importance: 0.9, ConfirmCount: 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A later 0.5 score → running avg (0.9*1+0.5)/2 = 0.7.
	if err := s.ReinforceImportance(ctx, id, 0.5); err != nil {
		t.Fatalf("reinforce: %v", err)
	}
	sig, _ := s.GetSignals(ctx, id)
	if math.Abs(sig.Importance-0.7) > 1e-9 || sig.ConfirmCount != 2 {
		t.Errorf("after reinforce(0.5): importance=%v count=%v, want 0.7/2", sig.Importance, sig.ConfirmCount)
	}
	// Another 0.6 → (0.7*2+0.6)/3 = 0.6667.
	_ = s.ReinforceImportance(ctx, id, 0.6)
	sig, _ = s.GetSignals(ctx, id)
	if math.Abs(sig.Importance-(2.0/3.0)) > 1e-9 || sig.ConfirmCount != 3 {
		t.Errorf("after reinforce(0.6): importance=%v count=%v, want 0.667/3", sig.Importance, sig.ConfirmCount)
	}
}

func TestReinforceImportance_FirstScoreOnFreshFact(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("a fact", vecA()))
	// No prior signals row → first reinforce sets the score directly.
	if err := s.ReinforceImportance(ctx, id, 0.8); err != nil {
		t.Fatalf("reinforce: %v", err)
	}
	sig, _ := s.GetSignals(ctx, id)
	if math.Abs(sig.Importance-0.8) > 1e-9 || sig.ConfirmCount != 1 {
		t.Errorf("first reinforce: importance=%v count=%v, want 0.8/1", sig.Importance, sig.ConfirmCount)
	}
}

func TestBumpAccess_IncrementsAndKeepsImportanceNeutral(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	id, _ := s.Insert(ctx, mkFact("a fact", vecA()))
	if err := s.BumpAccess(ctx, []int64{id}); err != nil {
		t.Fatalf("BumpAccess: %v", err)
	}
	if err := s.BumpAccess(ctx, []int64{id}); err != nil {
		t.Fatalf("BumpAccess 2: %v", err)
	}
	sig, _ := s.GetSignals(ctx, id)
	if sig.AccessCount != 2 {
		t.Errorf("access_count = %d, want 2", sig.AccessCount)
	}
	if sig.LastAccessed.IsZero() {
		t.Error("last_accessed not set")
	}
	// A bump must NOT perturb importance (stays neutral default).
	if sig.Importance != semantic.DefaultImportance {
		t.Errorf("bump changed importance to %v", sig.Importance)
	}
}

func TestPinnedFacts_OrderLimitAndExclusions(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	idLow, _ := s.Insert(ctx, mkFact("low pinned", vecA()))
	idHigh, _ := s.Insert(ctx, mkFact("high pinned", vecB()))
	idUnpinned, _ := s.Insert(ctx, mkFact("unpinned", vecC()))
	idTomb, _ := s.Insert(ctx, mkFact("tombstoned pinned", vecA()))

	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idLow, Importance: 0.6, Pinned: true})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idHigh, Importance: 0.95, Pinned: true})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idUnpinned, Importance: 0.99})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idTomb, Importance: 0.99, Pinned: true})
	if err := s.Tombstone(ctx, idTomb); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}

	got, err := s.PinnedFacts(ctx, "main", 5)
	if err != nil {
		t.Fatalf("PinnedFacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pinned, want 2 (tombstoned + unpinned excluded)", len(got))
	}
	if got[0].ID != idHigh || got[1].ID != idLow {
		t.Errorf("pinned order wrong: got %d,%d want %d,%d", got[0].ID, got[1].ID, idHigh, idLow)
	}
	// Limit honored.
	one, _ := s.PinnedFacts(ctx, "main", 1)
	if len(one) != 1 || one[0].ID != idHigh {
		t.Errorf("limit=1 should yield the highest-importance pin")
	}
}

func TestMergeProtected_Predicate(t *testing.T) {
	cases := []struct {
		name string
		sig  semantic.Signals
		want bool
	}{
		{"neutral", semantic.Signals{Importance: 0.5}, false},
		{"pinned", semantic.Signals{Importance: 0.5, Pinned: true}, true},
		{"protected flag", semantic.Signals{Importance: 0.5, Protected: true}, true},
		{"high but unused", semantic.Signals{Importance: 0.95, AccessCount: 0}, false},
		{"high and used", semantic.Signals{Importance: 0.95, AccessCount: 1}, true},
		{"just-below-threshold and used", semantic.Signals{Importance: 0.85, AccessCount: 5}, false},
	}
	for _, c := range cases {
		if got := semantic.MergeProtected(c.sig); got != c.want {
			t.Errorf("%s: MergeProtected=%v want %v", c.name, got, c.want)
		}
	}
}

func TestMergeProtectedStats(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	idNeutral, _ := s.Insert(ctx, mkFact("neutral", vecA()))
	idPinned, _ := s.Insert(ctx, mkFact("pinned", vecB()))
	idHighUsed, _ := s.Insert(ctx, mkFact("high used", vecC()))
	idHighUnused, _ := s.Insert(ctx, mkFact("high unused", vecA()))

	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idNeutral, Importance: 0.5})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idPinned, Importance: 0.5, Pinned: true})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idHighUsed, Importance: 0.95, AccessCount: 3})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idHighUnused, Importance: 0.95, AccessCount: 0})

	prot, live, err := s.MergeProtectedStats(ctx, "main")
	if err != nil {
		t.Fatalf("MergeProtectedStats: %v", err)
	}
	if live != 4 {
		t.Errorf("live = %d, want 4", live)
	}
	if prot != 2 { // pinned + high-used; high-unused & neutral are not protected
		t.Errorf("protected = %d, want 2", prot)
	}
}

// foreign_keys must be enforced even on the :memory: connection (review
// finding 2/3), so fact_signals' ON DELETE CASCADE holds everywhere.
func TestOpen_ForeignKeysEnforcedInMemory(t *testing.T) {
	s, err := semantic.Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var fk int
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("pragma read: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1 (enforced)", fk)
	}
}

func TestDampenHeat_HalvesHottest(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	idHot, _ := s.Insert(ctx, mkFact("hot", vecA()))
	idWarm, _ := s.Insert(ctx, mkFact("warm", vecB()))
	idOne, _ := s.Insert(ctx, mkFact("single hit", vecC()))

	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idHot, Importance: 0.5, AccessCount: 10})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idWarm, Importance: 0.5, AccessCount: 4})
	_ = s.UpsertSignals(ctx, semantic.Signals{FactID: idOne, Importance: 0.5, AccessCount: 1})

	if err := s.DampenHeat(ctx, "main", 5); err != nil {
		t.Fatalf("DampenHeat: %v", err)
	}
	hot, _ := s.GetSignals(ctx, idHot)
	warm, _ := s.GetSignals(ctx, idWarm)
	one, _ := s.GetSignals(ctx, idOne)
	if hot.AccessCount != 5 {
		t.Errorf("hot access_count = %d, want 5", hot.AccessCount)
	}
	if warm.AccessCount != 2 {
		t.Errorf("warm access_count = %d, want 2", warm.AccessCount)
	}
	if one.AccessCount != 1 { // access_count<=1 is never dampened (would erase the signal)
		t.Errorf("single-hit access_count = %d, want 1 (untouched)", one.AccessCount)
	}
}
