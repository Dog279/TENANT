package improve

import (
	"context"
	"path/filepath"
	"testing"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

func TestFeedbackProtectionJob_PromotesAckedSources(t *testing.T) {
	ctx := context.Background()
	ss, _, _, _ := consolidateScaffold(t)
	es, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })

	// Two episodes; ack only the first.
	ep1, _ := es.Insert(ctx, &episodic.Episode{AgentID: "main", Prompt: "p1", Response: "r1", EmbedderID: "e", Embedding: []float32{1, 0}})
	ep2, _ := es.Insert(ctx, &episodic.Episode{AgentID: "main", Prompt: "p2", Response: "r2", EmbedderID: "e", Embedding: []float32{1, 0}})
	if err := es.SetUserFeedback(ctx, ep1, episodic.FeedbackAck); err != nil {
		t.Fatal(err)
	}

	// Fact A sourced from the acked ep1 → should be protected.
	fA, _ := ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "from acked turn", SourceEpisodes: []int64{ep1}, EmbedderID: "e", Embedding: []float32{1, 0}, Confidence: 0.9})
	// Fact B sourced from the un-acked ep2 → should NOT be protected.
	fB, _ := ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "from neutral turn", SourceEpisodes: []int64{ep2}, EmbedderID: "e", Embedding: []float32{1, 0}, Confidence: 0.9})

	job := &FeedbackProtectionJob{Semantic: ss, Episodic: es, AgentID: "main"}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Changed || res.Details["promoted"] != 1 {
		t.Fatalf("expected 1 promotion, got %+v", res)
	}
	if sig, _ := ss.GetSignals(ctx, fA); !sig.Protected {
		t.Error("fact from acked turn should be protected")
	}
	if sig, _ := ss.GetSignals(ctx, fB); sig.Protected {
		t.Error("fact from un-acked turn must NOT be protected")
	}

	// Idempotent: a second run promotes nothing new.
	res2, _ := job.Run(ctx)
	if res2.Changed || res2.Details["promoted"] != 0 {
		t.Errorf("second run should be a no-op, got %+v", res2)
	}
}

func TestFeedbackProtectionJob_NoAcksNoop(t *testing.T) {
	ctx := context.Background()
	ss, _, _, _ := consolidateScaffold(t)
	es, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	ep, _ := es.Insert(ctx, &episodic.Episode{AgentID: "main", Prompt: "p", Response: "r", EmbedderID: "e", Embedding: []float32{1, 0}})
	_, _ = ss.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: "f", SourceEpisodes: []int64{ep}, EmbedderID: "e", Embedding: []float32{1, 0}, Confidence: 0.9})

	job := &FeedbackProtectionJob{Semantic: ss, Episodic: es, AgentID: "main"}
	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed {
		t.Errorf("no acks ⇒ no change, got %+v", res)
	}
}
