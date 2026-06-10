package improve_test

import (
	"context"
	"testing"
	"time"

	"tenant/internal/improve"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/skills"
	"tenant/internal/model/testllm"
)

// seedInduction builds fresh stores with 3 recurring successful 2-tool turns
// (enough to induce one skill) and returns their episode ids.
func seedInduction(t *testing.T) (*episodic.Store, *skills.Store, []int64) {
	t.Helper()
	ctx := context.Background()
	es, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sk, err := skills.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	seq := []episodic.ToolCallRef{{Name: "sql_query"}, {Name: "os_exec"}}
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := es.Insert(ctx, &episodic.Episode{
			AgentID: "a", SessionID: "s", Timestamp: time.Now().UTC(),
			Prompt: "build and run the report", Response: "done",
			ToolCalls: seq, Outcome: episodic.OutcomeSuccess, EmbedderID: "test/2", Embedding: []float32{1, 0},
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		ids = append(ids, id)
	}
	return es, sk, ids
}

func countStatus(t *testing.T, sk *skills.Store, status string) int {
	t.Helper()
	rows, err := sk.List(context.Background(), skills.ListFilter{AgentID: "a", Status: status, IncludeDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestAutoAccept_OffProposesAsBefore(t *testing.T) {
	es, sk, _ := seedInduction(t)
	defer es.Close()
	defer sk.Close()
	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3,
		AutoAccept: func() string { return "off" }}
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countStatus(t, sk, skills.StatusProposed); got != 1 {
		t.Fatalf("off: proposed=%d want 1", got)
	}
	if got := countStatus(t, sk, skills.StatusLive); got != 0 {
		t.Fatalf("off: live=%d want 0", got)
	}
}

func TestAutoAccept_OnGoesLive(t *testing.T) {
	es, sk, _ := seedInduction(t)
	defer es.Close()
	defer sk.Close()
	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3,
		AutoAccept: func() string { return "on" }}
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countStatus(t, sk, skills.StatusLive); got != 1 {
		t.Fatalf("on: live=%d want 1", got)
	}
	if got := countStatus(t, sk, skills.StatusProposed); got != 0 {
		t.Fatalf("on: proposed=%d want 0", got)
	}
}

func TestAutoAccept_TrustedHealthyGoesLive(t *testing.T) {
	es, sk, ids := seedInduction(t)
	defer es.Close()
	defer sk.Close()
	ctx := context.Background()
	for _, id := range ids { // 3 acks, 0 undos → healthy
		if err := es.SetUserFeedback(ctx, id, episodic.FeedbackAck); err != nil {
			t.Fatal(err)
		}
	}
	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3,
		AutoAccept: func() string { return "trusted" }, TrustMinAcks: 2}
	if _, err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countStatus(t, sk, skills.StatusLive); got != 1 {
		t.Fatalf("trusted-healthy: live=%d want 1", got)
	}
}

func TestAutoAccept_TrustedSuspendsOnUndo(t *testing.T) {
	es, sk, ids := seedInduction(t)
	defer es.Close()
	defer sk.Close()
	ctx := context.Background()
	es.SetUserFeedback(ctx, ids[0], episodic.FeedbackAck)
	es.SetUserFeedback(ctx, ids[1], episodic.FeedbackAck)
	es.SetUserFeedback(ctx, ids[2], episodic.FeedbackUndo) // one undo → suspend
	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3,
		AutoAccept: func() string { return "trusted" }, TrustMinAcks: 2}
	if _, err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countStatus(t, sk, skills.StatusProposed); got != 1 {
		t.Fatalf("trusted-soured: proposed=%d want 1 (undo must suspend)", got)
	}
	if got := countStatus(t, sk, skills.StatusLive); got != 0 {
		t.Fatalf("trusted-soured: live=%d want 0", got)
	}
}

func TestAutoAccept_TrustedNeedsEnoughAcks(t *testing.T) {
	es, sk, ids := seedInduction(t)
	defer es.Close()
	defer sk.Close()
	es.SetUserFeedback(context.Background(), ids[0], episodic.FeedbackAck) // only 1 ack
	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3,
		AutoAccept: func() string { return "trusted" }, TrustMinAcks: 5}
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countStatus(t, sk, skills.StatusProposed); got != 1 {
		t.Fatalf("trusted-insufficient: proposed=%d want 1", got)
	}
}

const inducedSkillName = "auto: sql_query → os_exec"

func TestAutoAccept_GenesisAuditRow(t *testing.T) {
	es, sk, _ := seedInduction(t)
	defer es.Close()
	defer sk.Close()
	ctx := context.Background()
	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3,
		AutoAccept: func() string { return "on" }}
	if _, err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := sk.History(ctx, "a", inducedSkillName)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].ChangeSource != "auto-accept" {
		t.Fatalf("auto-accepted skill needs a durable genesis audit row (source=auto-accept); got %+v", h)
	}
}

func TestProposed_NoGenesisRow(t *testing.T) {
	es, sk, _ := seedInduction(t)
	defer es.Close()
	defer sk.Close()
	ctx := context.Background()
	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3} // off
	if _, err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	h, _ := sk.History(ctx, "a", inducedSkillName)
	if len(h) != 0 {
		t.Fatalf("a proposed (manually-reviewed) skill should keep the no-history convention; got %d entries", len(h))
	}
}

func TestAutoAccept_NilDefaultsOff(t *testing.T) {
	es, sk, _ := seedInduction(t)
	defer es.Close()
	defer sk.Close()
	job := &improve.SkillInductionJob{Episodic: es, Skills: sk, Embedder: testllm.New(), AgentID: "a", MinOccur: 3}
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countStatus(t, sk, skills.StatusProposed); got != 1 {
		t.Fatalf("nil AutoAccept must default to proposed; got proposed=%d", got)
	}
}
