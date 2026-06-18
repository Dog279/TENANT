package distill_test

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
)

// A distilled fact's importance (1-10) must land in a fact_signals row
// (mapped to 0..1, confirm_count=1).
func TestRun_ImportanceFlowsToSignals(t *testing.T) {
	s := newScaffold(t)
	ctx := context.Background()
	id1 := s.insertEpisode("the merge_pr tool is hard-gated regardless of Confirm", "noted", vecA())

	scriptFacts(s.fakeLLM, `{"facts":[
		{"fact":"merge_pr is hard-gated in Go regardless of the Confirm broker.","confidence":0.95,"importance":9,"source_episode_ids":[`+strconv.FormatInt(id1, 10)+`]}
	]}`)
	scriptEmbeddings(s.fakeEmb, [][]float32{vecA()})

	if _, err := s.distiller().Run(ctx, 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	facts, err := s.sstore.List(ctx, semantic.ListFilter{AgentIDs: []string{"test-agent"}})
	if err != nil || len(facts) != 1 {
		t.Fatalf("list: %v facts=%d", err, len(facts))
	}
	sig, err := s.sstore.GetSignals(ctx, facts[0].ID)
	if err != nil {
		t.Fatalf("GetSignals: %v", err)
	}
	if math.Abs(sig.Importance-0.9) > 1e-9 {
		t.Errorf("importance = %v, want 0.9 (from score 9)", sig.Importance)
	}
	if sig.ConfirmCount != 1 {
		t.Errorf("confirm_count = %d, want 1", sig.ConfirmCount)
	}
}

// A missing importance score must NOT skew the signal — it falls back to
// the neutral default at insert (echo-backend / older-model safety).
func TestRun_MissingImportanceIsNeutral(t *testing.T) {
	s := newScaffold(t)
	ctx := context.Background()
	id1 := s.insertEpisode("some passing remark", "ok", vecA())
	scriptFacts(s.fakeLLM, `{"facts":[
		{"fact":"A fact with no importance score.","confidence":0.7,"source_episode_ids":[`+strconv.FormatInt(id1, 10)+`]}
	]}`)
	scriptEmbeddings(s.fakeEmb, [][]float32{vecA()})

	if _, err := s.distiller().Run(ctx, 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	facts, _ := s.sstore.List(ctx, semantic.ListFilter{AgentIDs: []string{"test-agent"}})
	if len(facts) != 1 {
		t.Fatalf("facts=%d", len(facts))
	}
	sig, _ := s.sstore.GetSignals(ctx, facts[0].ID)
	if sig.Importance != semantic.DefaultImportance {
		t.Errorf("missing-importance fact = %v, want neutral %v", sig.Importance, semantic.DefaultImportance)
	}
}

// Reaffirming a fact with a LOWER score must average importance DOWN
// (agreement-averaging, not a one-way ratchet — review finding 4).
func TestRun_ReaffirmAveragesImportanceDown(t *testing.T) {
	s := newScaffold(t)
	ctx := context.Background()

	old := time.Now().UTC().Add(-24 * time.Hour)
	factID, err := s.sstore.Insert(ctx, &semantic.Fact{
		AgentID:       "test-agent",
		Visibility:    semantic.VisibilityPrivate,
		Fact:          "User prefers Go.",
		Confidence:    0.9,
		EmbedderID:    "test-embedder",
		Embedding:     vecA(),
		FirstSeen:     old,
		LastConfirmed: old,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed as if the distiller scored it 9/10 once.
	if err := s.sstore.UpsertSignals(ctx, semantic.Signals{FactID: factID, Importance: 0.9, ConfirmCount: 1}); err != nil {
		t.Fatal(err)
	}

	// A new, embedding-identical extraction scores it only 5/10.
	ep := s.insertEpisode("I use Go", "ok", vecA())
	scriptFacts(s.fakeLLM, `{"facts":[
		{"fact":"User prefers Go for backend.","confidence":0.8,"importance":5,"source_episode_ids":[`+strconv.FormatInt(ep, 10)+`]}
	]}`)
	scriptEmbeddings(s.fakeEmb, [][]float32{vecA()})

	res, err := s.distiller().Run(ctx, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FactsReaffirmed != 1 {
		t.Fatalf("expected 1 reaffirm, got %+v", res)
	}
	sig, _ := s.sstore.GetSignals(ctx, factID)
	if math.Abs(sig.Importance-0.7) > 1e-9 { // (0.9*1 + 0.5)/2
		t.Errorf("reaffirmed importance = %v, want 0.7 (averaged down)", sig.Importance)
	}
	if sig.ConfirmCount != 2 {
		t.Errorf("confirm_count = %d, want 2", sig.ConfirmCount)
	}
}
