package distill_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

// A borderline-similar NEW fact judged "supersedes" must: insert the new fact
// (with an event-time start), close the old fact's validity (valid_to=now),
// and soft-supersede it — a recorded transition, not an orphan (Phase 2).
func TestRun_SupersedesRecordsTransition(t *testing.T) {
	s := newScaffold(t)
	ctx := context.Background()

	// Pre-seed the old fact with an OLD last_confirmed + an importance signal.
	old := time.Now().UTC().Add(-48 * time.Hour)
	oldID, err := s.sstore.Insert(ctx, &semantic.Fact{
		AgentID: "test-agent", Visibility: semantic.VisibilityPrivate,
		Fact: "User works at Acme.", Confidence: 0.9, EmbedderID: "test-embedder",
		Embedding: vecA(), FirstSeen: old, LastConfirmed: old,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.sstore.UpsertSignals(ctx, semantic.Signals{FactID: oldID, Importance: 0.7, ConfirmCount: 1}); err != nil {
		t.Fatal(err)
	}

	ep := s.insertEpisode("I just switched jobs to Globex", "noted", vecA())
	// extractBatch → facts JSON; classifyBorderline → "supersedes". Route by prompt.
	s.fakeLLM.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "verdict") {
			return &model.GenerateResponse{Text: `{"verdict":"supersedes"}`, FinishReason: "stop"}, nil
		}
		return &model.GenerateResponse{Text: `{"facts":[{"fact":"User works at Globex.","confidence":0.9,"importance":8,"source_episode_ids":[` + strconv.FormatInt(ep, 10) + `]}]}`, FinishReason: "stop"}, nil
	}
	// New fact embedding ~0.82 cosine to old [1,0,0,0] → borderline band [0.80,0.85).
	scriptEmbeddings(s.fakeEmb, [][]float32{{0.82, 0.5724, 0, 0}})

	res, err := s.distiller().Run(ctx, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FactsSuperseded != 1 || res.FactsInserted != 1 {
		t.Fatalf("want 1 superseded + 1 inserted, got %+v", res)
	}

	// Old fact: soft-superseded + event-time closed.
	oldFact, err := s.sstore.Get(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	if oldFact.SupersededBy == 0 {
		t.Error("old fact should be superseded")
	}
	oldSig, _ := s.sstore.GetSignals(ctx, oldID)
	if oldSig.ValidTo.IsZero() {
		t.Error("old fact's valid_to should be set (event-time closed)")
	}

	// New fact: live, with an event-time start.
	live, _ := s.sstore.List(ctx, semantic.ListFilter{AgentIDs: []string{"test-agent"}})
	if len(live) != 1 || live[0].Fact != "User works at Globex." {
		t.Fatalf("expected exactly the new fact live, got %d: %+v", len(live), live)
	}
	newSig, _ := s.sstore.GetSignals(ctx, live[0].ID)
	if newSig.ValidFrom.IsZero() {
		t.Error("new fact's valid_from should be set")
	}
}
