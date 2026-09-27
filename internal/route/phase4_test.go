package route

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/runtrace"
)

type phase4Classifier struct {
	sig Signal
	err error
}

func (c phase4Classifier) Classify(context.Context, string) (Signal, error) { return c.sig, c.err }

func phase4Input() PolicyInput {
	return PolicyInput{
		Small: Target{Role: "planner", ProfileID: "small-p", Local: true},
		Big:   Target{Role: "planner-big", ProfileID: "big-p", Local: true},
	}
}

func TestAnnotateDecisionStampsMetadataOnDurableRow(t *testing.T) {
	dir := t.TempDir()
	sig := Signal{Domain: "software", Complexity: 5, Code: true, Label: "big", Justification: "j", Raw: "raw"}
	e := NewEngine(phase4Classifier{sig: sig}, dir, 0)
	d, err := e.DecideWithPolicy(context.Background(), PolicyDecisionRequest{
		AgentID: "a", SessionID: "s", Prompt: "write a parser", Mode: ModeShadow, Surface: "tui",
		Input: phase4Input(),
		AnnotateDecision: func(d *Decision) {
			d.RuleRole = "coder"
			d.EffortHint = EffortHintHigh
		},
	})
	if err != nil {
		t.Fatalf("DecideWithPolicy: %v", err)
	}
	if d.RuleRole != "coder" || d.EffortHint != EffortHintHigh || d.Surface != "tui" {
		t.Fatalf("annotation missing on returned decision: %+v", d)
	}
	recent := e.Recent(1)
	if len(recent) != 1 || recent[0].RuleRole != "coder" || recent[0].EffortHint != EffortHintHigh {
		t.Fatalf("annotation missing on recorded history: %+v", recent)
	}
}

func TestAnnotateDecisionCannotRerouteOrFailTheTurn(t *testing.T) {
	dir := t.TempDir()
	sig := Signal{Domain: "software", Complexity: 5, Code: true, Label: "big", Justification: "j", Raw: "raw"}
	e := NewEngine(phase4Classifier{sig: sig}, dir, 0)
	d, err := e.DecideWithPolicy(context.Background(), PolicyDecisionRequest{
		AgentID: "a", Prompt: "p", Mode: ModeShadow,
		Input: phase4Input(),
		AnnotateDecision: func(d *Decision) {
			// A buggy annotator tries to re-route through the back door and
			// then panics. Neither may have any effect on the selection.
			d.SelectedTier = TierSmall
			d.SelectedRole = "attacker"
			d.SelectedProfileID = "cloud-p"
			d.PolicyReason = "hacked"
			d.PrivacyPierced = true
			panic("annotator bug")
		},
	})
	if err != nil {
		t.Fatalf("DecideWithPolicy: %v", err)
	}
	if d.Status != StatusOK {
		t.Fatalf("annotation affected decision status: %+v", d)
	}
	if d.SelectedTier != TierBig || d.SelectedRole != "planner-big" || d.SelectedProfileID != "big-p" {
		t.Fatalf("annotator re-routed the selection: %+v", d)
	}
	if d.PolicyReason != string(ReasonClassifierLabel) || d.PrivacyPierced {
		t.Fatalf("annotator altered policy provenance: %+v", d)
	}
}

func TestDecisionSurfaceIsRecorded(t *testing.T) {
	dir := t.TempDir()
	sig := Signal{Domain: "chat", Complexity: 1, Label: "small", Justification: "j", Raw: "raw"}
	e := NewEngine(phase4Classifier{sig: sig}, dir, 0)
	for _, surface := range []string{"relay", "spawn", ""} {
		d, err := e.DecideWithPolicy(context.Background(), PolicyDecisionRequest{
			AgentID: "a", Prompt: "hi", Mode: ModeShadow, Surface: surface,
			Input: phase4Input(),
		})
		if err != nil {
			t.Fatalf("DecideWithPolicy(%q): %v", surface, err)
		}
		if d.Surface != surface {
			t.Fatalf("Surface = %q, want %q", d.Surface, surface)
		}
	}
}

// The turn's trace rides the ctx onto the decision and its durable row, so a
// decision joins its runs.db record (TEN-368).
func TestDecisionCarriesTheTurnTrace(t *testing.T) {
	dir := t.TempDir()
	sig := Signal{Domain: "chat", Complexity: 1, Label: "small", Justification: "j", Raw: "raw"}
	e := NewEngine(phase4Classifier{sig: sig}, dir, 0)
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan("trace-7"))
	d, err := e.DecideWithPolicy(ctx, PolicyDecisionRequest{AgentID: "a", Prompt: "hi", Mode: ModeShadow, Input: phase4Input()})
	if err != nil || d.TraceID != "trace-7" {
		t.Fatalf("DecideWithPolicy: trace = %q, err = %v; want trace-7", d.TraceID, err)
	}
	if d := e.Decide(ctx, "a", "s", "hi", ModeShadow); d.TraceID != "trace-7" {
		t.Fatalf("Decide: trace = %q, want trace-7", d.TraceID)
	}
	raw, err := os.ReadFile(filepath.Join(dir, DecisionLogName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), `"trace_id":"trace-7"`); got != 2 {
		t.Fatalf("durable rows with the trace = %d, want 2:\n%s", got, raw)
	}
}

// TestRetentionSurvivesConcurrentRecords guards the logMu contract: the
// append-path prune shares the mutex with appends, so concurrent records
// during a prune must neither race nor drop fresh rows.
func TestRetentionSurvivesConcurrentRecords(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(nil, dir, 0)
	e.SetRetention(90)
	now := time.Now().UTC()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 25; i++ {
			e.record(Decision{ID: "a", Timestamp: now})
		}
	}()
	for i := 0; i < 25; i++ {
		e.record(Decision{ID: "b", Timestamp: now})
	}
	<-done
	if got := len(e.decisionsForStats()); got != 50 {
		t.Fatalf("decisions after concurrent record+prune = %d, want 50", got)
	}
}
