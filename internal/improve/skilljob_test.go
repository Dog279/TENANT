package improve_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/improve"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/skills"
	"tenant/internal/model/testllm"
)

func TestSkillInductionJob_ProposesRepeatedSequence(t *testing.T) {
	ctx := context.Background()
	es, err := episodic.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()

	seq := []episodic.ToolCallRef{{Name: "sql_query"}, {Name: "os_exec"}}
	// 3 successful turns with the same 2-tool sequence → should induce.
	for i := 0; i < 3; i++ {
		if _, err := es.Insert(ctx, &episodic.Episode{
			AgentID: "a", SessionID: "s", Timestamp: time.Now().UTC(),
			Prompt: "build and run the report", Response: "done",
			ToolCalls: seq, Outcome: episodic.OutcomeSuccess, EmbedderID: "test/2", Embedding: []float32{1, 0},
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// Noise that must NOT induce: a single-tool turn and a failed turn.
	_, _ = es.Insert(ctx, &episodic.Episode{AgentID: "a", SessionID: "s", Timestamp: time.Now().UTC(),
		Prompt: "x", ToolCalls: []episodic.ToolCallRef{{Name: "sql_query"}}, Outcome: episodic.OutcomeSuccess, EmbedderID: "test/2", Embedding: []float32{1, 0}})
	_, _ = es.Insert(ctx, &episodic.Episode{AgentID: "a", SessionID: "s", Timestamp: time.Now().UTC(),
		Prompt: "y", ToolCalls: seq, Outcome: episodic.OutcomeError, EmbedderID: "test/2", Embedding: []float32{1, 0}})

	sk, err := skills.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer sk.Close()

	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Changed {
		t.Fatalf("expected a proposal; summary=%q", res.Summary)
	}
	proposed, _ := sk.List(ctx, skills.ListFilter{AgentID: "a", Status: skills.StatusProposed, IncludeDisabled: true})
	if len(proposed) != 1 {
		t.Fatalf("expected 1 proposed skill, got %d", len(proposed))
	}
	// Proposed skills are disabled (not live-retrievable) until accepted.
	if proposed[0].Enabled {
		t.Error("proposed skill should start disabled")
	}
	// Idempotent: a second run doesn't re-propose the same sequence.
	if res2, _ := job.Run(ctx); res2.Changed {
		t.Errorf("second run should not re-propose: %q", res2.Summary)
	}
}
