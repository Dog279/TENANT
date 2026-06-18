package improve

import (
	"context"
	"testing"
	"time"

	"tenant/internal/memory/semantic"
	"tenant/internal/memory/sme"
	"tenant/internal/model"
)

func mkSME(t *testing.T, ss *semantic.Store) *sme.Store {
	t.Helper()
	st, err := sme.New(ss.DB())
	if err != nil {
		t.Fatalf("sme.New: %v", err)
	}
	return st
}

func TestReflectionJob_SynthesizesAndReaffirms(t *testing.T) {
	ctx := context.Background()
	ss, router, fakeLLM, _ := consolidateScaffold(t)
	smeStore := mkSME(t, ss)
	live := sme.NewLive()

	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	mk := func(text string, imp float64) int64 {
		id, err := ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: text, EmbedderID: "e", Embedding: []float32{1, 0}, Confidence: 0.9, FirstSeen: old, LastConfirmed: old})
		if err != nil {
			t.Fatal(err)
		}
		if err := ss.UpsertSignals(ctx, semantic.Signals{FactID: id, Importance: imp, ConfirmCount: 1}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	f1 := mk("Tenant is a Go MCP framework over SQLite", 0.9)
	f2 := mk("Additive-only: the Windows build must stay green", 0.95)

	fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: `{"sections":[
			{"section":"Architecture & Decisions","body":"Tenant is a Go MCP framework over SQLite.","source_fact_ids":[1]},
			{"section":"Conventions & Gotchas","body":"Additive-only — keep the Windows build green.","source_fact_ids":[2]}
		]}`, FinishReason: "stop"}, nil
	}

	job := &ReflectionJob{Semantic: ss, SME: smeStore, Live: live, Router: router, AgentID: "main"}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Changed || res.Details["sections_written"] != 2 {
		t.Fatalf("expected 2 sections written + Changed; got %+v", res)
	}

	// Active doc has both sections.
	secs, _ := smeStore.ActiveSections(ctx, "", "main")
	if len(secs) != 2 {
		t.Fatalf("active sections = %d, want 2", len(secs))
	}
	// Live cache refreshed with the rendered doc.
	if live.String() == "" || !containsAll(live.String(), "Architecture & Decisions", "Windows build green") {
		t.Errorf("live render missing content:\n%s", live.String())
	}
	// Cited source facts were reaffirmed (decay clock reset past the 90d-old seed).
	for _, id := range []int64{f1, f2} {
		got, _ := ss.Get(ctx, id)
		if !got.LastConfirmed.After(old) {
			t.Errorf("cited fact %d not reaffirmed: last_confirmed=%v old=%v", id, got.LastConfirmed, old)
		}
	}
}

func TestReflectionJob_BelowMinFactsNoop(t *testing.T) {
	ctx := context.Background()
	ss, router, _, _ := consolidateScaffold(t)
	smeStore := mkSME(t, ss)
	_, _ = ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "only one fact", EmbedderID: "e", Embedding: []float32{1, 0}, Confidence: 0.9})

	job := &ReflectionJob{Semantic: ss, SME: smeStore, Router: router, AgentID: "main"}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed {
		t.Errorf("single-fact store should not synthesize; got %+v", res)
	}
	if secs, _ := smeStore.ActiveSections(ctx, "", "main"); len(secs) != 0 {
		t.Errorf("no sections should be written, got %d", len(secs))
	}
}

func TestReflectionJob_FailsClosedOnBadJSON(t *testing.T) {
	ctx := context.Background()
	ss, router, fakeLLM, _ := consolidateScaffold(t)
	smeStore := mkSME(t, ss)
	for i := 0; i < 3; i++ {
		_, _ = ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "a load-bearing fact", EmbedderID: "e", Embedding: []float32{1, 0}, Confidence: 0.9})
	}
	fakeLLM.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "I cannot produce JSON today, sorry!", FinishReason: "stop"}, nil
	}
	job := &ReflectionJob{Semantic: ss, SME: smeStore, Router: router, AgentID: "main"}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run should fail closed, not error: %v", err)
	}
	if res.Changed {
		t.Errorf("bad JSON should write nothing; got %+v", res)
	}
	if secs, _ := smeStore.ActiveSections(ctx, "", "main"); len(secs) != 0 {
		t.Errorf("no sections on bad JSON, got %d", len(secs))
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
