package improve_test

import (
	"context"
	"testing"

	"tenant/internal/improve"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/soul"
)

type fakeProposer struct {
	changed bool
	reason  string
	instrs  []string
	err     error
	calls   int
}

func (f *fakeProposer) Propose(_ context.Context, _ *soul.Soul, _ improve.SoulSignal) (bool, string, []string, error) {
	f.calls++
	return f.changed, f.reason, f.instrs, f.err
}

type fakeScorer struct {
	regressed bool
	delta     float64
	err       error
	calls     int
}

func (f *fakeScorer) Score(_ context.Context, _ *soul.Soul) (bool, float64, error) {
	f.calls++
	return f.regressed, f.delta, f.err
}

// seedSoulJob makes a temp soul dir (with a current soul unless skipSoul) and an
// in-memory episodic store with n acked turns.
func seedSoulJob(t *testing.T, acks int, skipSoul bool) (string, *episodic.Store) {
	t.Helper()
	dir := t.TempDir()
	if !skipSoul {
		sl := &soul.Soul{
			Agent:        soul.Agent{ID: "main", Name: "T"},
			Instructions: soul.Instructions{Items: []string{"be terse"}},
		}
		if err := sl.Save(dir); err != nil {
			t.Fatal(err)
		}
	}
	es, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < acks; i++ {
		id, err := es.Insert(ctx, &episodic.Episode{
			AgentID: "main", Prompt: "do a thing", Response: "done", EmbedderID: "x", Embedding: []float32{1, 0},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := es.SetUserFeedback(ctx, id, episodic.FeedbackAck); err != nil {
			t.Fatal(err)
		}
	}
	return dir, es
}

func proposalCount(t *testing.T, dir string) int {
	t.Helper()
	ps, err := soul.ListProposals(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	return len(ps)
}

func newSoulJob(es *episodic.Store, dir string, p improve.SoulNudgeJob) *improve.SoulNudgeJob {
	p.Episodic = es
	p.AgentID = "main"
	p.BaseDir = dir
	if p.MinAcks == 0 {
		p.MinAcks = 3
	}
	return &p
}

func TestSoulNudge_InsufficientSignalSkips(t *testing.T) {
	dir, es := seedSoulJob(t, 1, false) // 1 ack < MinAcks 3
	defer es.Close()
	fp := &fakeProposer{changed: true, instrs: []string{"x"}}
	job := newSoulJob(es, dir, improve.SoulNudgeJob{Proposer: fp, Scorer: &fakeScorer{}})
	res, err := job.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fp.calls != 0 {
		t.Error("should not call proposer below the evidence bar")
	}
	if proposalCount(t, dir) != 0 || res.Changed {
		t.Error("nothing should be proposed")
	}
}

func TestSoulNudge_NoSoulSkips(t *testing.T) {
	dir, es := seedSoulJob(t, 5, true) // enough acks, but no soul file
	defer es.Close()
	fp := &fakeProposer{changed: true, instrs: []string{"x"}}
	job := newSoulJob(es, dir, improve.SoulNudgeJob{Proposer: fp, Scorer: &fakeScorer{}})
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fp.calls != 0 {
		t.Error("no soul → should not propose")
	}
}

func TestSoulNudge_NoChangeSkips(t *testing.T) {
	dir, es := seedSoulJob(t, 5, false)
	defer es.Close()
	fp := &fakeProposer{changed: false}
	fs := &fakeScorer{}
	job := newSoulJob(es, dir, improve.SoulNudgeJob{Proposer: fp, Scorer: fs})
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fs.calls != 0 || proposalCount(t, dir) != 0 {
		t.Error("no change → must not score or propose")
	}
}

func TestSoulNudge_NilScorerFailsClosed(t *testing.T) {
	dir, es := seedSoulJob(t, 5, false)
	defer es.Close()
	fp := &fakeProposer{changed: true, reason: "r", instrs: []string{"a"}}
	job := newSoulJob(es, dir, improve.SoulNudgeJob{Proposer: fp, Scorer: nil}) // no gate
	res, err := job.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if proposalCount(t, dir) != 0 || res.Changed {
		t.Error("nil scorer must fail closed — propose nothing")
	}
}

func TestSoulNudge_RegressedDiscarded(t *testing.T) {
	dir, es := seedSoulJob(t, 5, false)
	defer es.Close()
	fp := &fakeProposer{changed: true, reason: "r", instrs: []string{"a"}}
	fs := &fakeScorer{regressed: true, delta: -7}
	job := newSoulJob(es, dir, improve.SoulNudgeJob{Proposer: fp, Scorer: fs})
	res, err := job.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fs.calls != 1 {
		t.Error("scorer should be called")
	}
	if proposalCount(t, dir) != 0 || res.Changed {
		t.Error("regressed candidate must not be proposed")
	}
}

func TestSoulNudge_ScorerErrorFailsClosed(t *testing.T) {
	dir, es := seedSoulJob(t, 5, false)
	defer es.Close()
	fp := &fakeProposer{changed: true, reason: "r", instrs: []string{"a"}}
	fs := &fakeScorer{err: context.DeadlineExceeded}
	job := newSoulJob(es, dir, improve.SoulNudgeJob{Proposer: fp, Scorer: fs})
	res, err := job.Run(context.Background())
	if err != nil {
		t.Fatalf("scorer error should be non-fatal to the scheduler, got %v", err)
	}
	if proposalCount(t, dir) != 0 || res.Changed {
		t.Error("scoring error must fail closed — propose nothing")
	}
}

func TestSoulNudge_SuccessProposes(t *testing.T) {
	dir, es := seedSoulJob(t, 5, false)
	defer es.Close()
	fp := &fakeProposer{changed: true, reason: "prefer bullet points", instrs: []string{"be terse", "use bullet points"}}
	fs := &fakeScorer{regressed: false, delta: 2.5}
	job := newSoulJob(es, dir, improve.SoulNudgeJob{Proposer: fp, Scorer: fs})
	res, err := job.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("a queued proposal should report Changed=true")
	}
	if fs.calls != 1 {
		t.Errorf("exactly one fitness scoring per run, got %d (cost invariant)", fs.calls)
	}
	ps, err := soul.ListProposals(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("want 1 queued proposal, got %d", len(ps))
	}
	got := ps[0].Soul.Instructions.Items
	if len(got) != 2 || got[1] != "use bullet points" {
		t.Errorf("candidate instructions not carried into the proposal: %v", got)
	}
	if ps[0].Reason != "prefer bullet points" {
		t.Errorf("reason = %q", ps[0].Reason)
	}
}
