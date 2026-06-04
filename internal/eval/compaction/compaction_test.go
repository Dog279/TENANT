package compaction

import (
	"context"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/compress"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// --- controllable fake compactors (no model) ---

// fnCompactor replaces the head with a summary produced by summarize and keeps
// the last keepTail messages verbatim — the same shape as compress.Compressor,
// but with a fully controllable summary so we can assert the evaluator's metrics.
type fnCompactor struct {
	keepTail  int
	summarize func(head []working.Message) string
}

func (f fnCompactor) Compact(_ context.Context, msgs []working.Message) ([]working.Message, bool, error) {
	keep := f.keepTail
	if keep <= 0 {
		keep = 3
	}
	if len(msgs) < keep+2 {
		return msgs, false, nil
	}
	head := msgs[:len(msgs)-keep]
	tail := msgs[len(msgs)-keep:]
	out := make([]working.Message, 0, keep+1)
	out = append(out, working.Message{Role: "user", Content: f.summarize(head), Timestamp: time.Now().UTC()})
	out = append(out, tail...)
	return out, true, nil
}

// echoSummary preserves every head identifier (a perfect-recall compactor).
func echoSummary(head []working.Message) string {
	var b strings.Builder
	b.WriteString("## Summary (echo)\n")
	for _, m := range head {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// lossySummary is short and drops all identifiers (a lossy compactor).
func lossySummary(_ []working.Message) string {
	return "## Summary\nEarlier turns were summarized; specific details were elided."
}

type noopCompactor struct{}

func (noopCompactor) Compact(_ context.Context, msgs []working.Message) ([]working.Message, bool, error) {
	return msgs, false, nil // fail-safe / below-threshold: no change
}

func TestEvaluate_PreservingCompactor(t *testing.T) {
	rep, err := Evaluate(context.Background(), fnCompactor{keepTail: 3, summarize: echoSummary}, EstimateTokens, Options{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if rep.Recall != 1 {
		t.Errorf("recall = %.2f, want 1 (echo preserves every identifier)", rep.Recall)
	}
	if rep.Continuation != 1 {
		t.Errorf("continuation = %.2f, want 1", rep.Continuation)
	}
	if rep.DriftRate != 0 {
		t.Errorf("drift_rate = %.2f, want 0", rep.DriftRate)
	}
	for _, p := range rep.Probes {
		if !p.Compacted {
			t.Errorf("probe %q: Compacted = false, want true", p.Name)
		}
		if len(p.Lost) != 0 {
			t.Errorf("probe %q: lost markers %v, want none", p.Name, p.Lost)
		}
	}
}

func TestEvaluate_LossyCompactor(t *testing.T) {
	rep, err := Evaluate(context.Background(), fnCompactor{keepTail: 3, summarize: lossySummary}, EstimateTokens, Options{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if rep.Recall != 0 {
		t.Errorf("recall = %.2f, want 0 (head identifiers dropped)", rep.Recall)
	}
	if rep.Continuation != 0 {
		t.Errorf("continuation = %.2f, want 0", rep.Continuation)
	}
	if rep.DriftRate != 1 {
		t.Errorf("drift_rate = %.2f, want 1 (all facts lost)", rep.DriftRate)
	}
	if rep.TokensBefore <= rep.TokensAfter {
		t.Errorf("expected tokens saved: before=%d after=%d", rep.TokensBefore, rep.TokensAfter)
	}
	if rep.TokensSavedPct <= 0 {
		t.Errorf("tokens_saved_pct = %.1f, want > 0", rep.TokensSavedPct)
	}
}

func TestEvaluate_NoopCompactorFlagged(t *testing.T) {
	rep, err := Evaluate(context.Background(), noopCompactor{}, EstimateTokens, Options{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	for _, p := range rep.Probes {
		if p.Compacted {
			t.Errorf("probe %q: Compacted = true, want false (no-op compactor)", p.Name)
		}
		if !strings.Contains(p.Notes, "NO change") {
			t.Errorf("probe %q: notes should flag the no-op, got %q", p.Name, p.Notes)
		}
	}
	// Nothing changed, so markers trivially survive — the no-op flag is how an
	// operator knows the high score is meaningless.
	if rep.Recall != 1 {
		t.Errorf("no-op recall = %.2f, want 1 (trivial survival)", rep.Recall)
	}
}

func TestEvaluate_NilCompactor(t *testing.T) {
	if _, err := Evaluate(context.Background(), nil, nil, Options{}); err == nil {
		t.Fatal("nil compactor must error")
	}
}

// --- integration: the REAL compress.Compressor driven by a fixture model ---

type fakeLLM struct {
	echo  bool   // echo the rendered head transcript back as the summary
	fixed string // otherwise return this fixed summary
}

func (f fakeLLM) Generate(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
	if f.echo {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				return &model.GenerateResponse{Text: req.Messages[i].Content}, nil
			}
		}
	}
	return &model.GenerateResponse{Text: f.fixed}, nil
}
func (fakeLLM) GenerateStream(context.Context, model.GenerateRequest) (<-chan model.StreamChunk, error) {
	return nil, nil
}
func (fakeLLM) TokenCount(_ context.Context, text string) (int, error) { return len(text) / 4, nil }

type fakeSource struct{ llm model.LLM }

func (s fakeSource) LLMForRole(context.Context, model.Role) (model.LLM, model.Profile, error) {
	return s.llm, model.Profile{}, nil
}

func TestEvaluate_RealCompressor_Echo(t *testing.T) {
	c := &compress.Compressor{Router: fakeSource{llm: fakeLLM{echo: true}}, TailTokens: 800, MinMessages: 6}
	rep, err := Evaluate(context.Background(), c, EstimateTokens, Options{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !rep.Probes[0].Compacted {
		t.Fatal("needle probe: the real compressor should have compacted (session is over budget)")
	}
	// An echoing summarizer preserves the head, so the planted identifiers survive
	// through the REAL compress pipeline (summary prefix + tail).
	if rep.Recall == 0 {
		t.Errorf("recall = 0 through the real compressor with an echoing summarizer; want > 0 (lost: %v)", rep.Probes[0].Lost)
	}
}

// TEN-101: even a marker-free LLM summary now preserves identifiers, because the
// compressor appends a deterministic verbatim allowlist. This is the keystone
// behavior — recall and drift-resistance no longer depend on the summarizer prose.
func TestEvaluate_RealCompressor_AllowlistRescuesIDs(t *testing.T) {
	c := &compress.Compressor{Router: fakeSource{llm: fakeLLM{fixed: "## Active Task\nwork happened."}}, TailTokens: 800, MinMessages: 6}
	rep, err := Evaluate(context.Background(), c, EstimateTokens, Options{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !rep.Probes[0].Compacted {
		t.Fatal("needle probe: expected the real compressor to compact")
	}
	if rep.Recall != 1 {
		t.Errorf("recall = %.2f; the verbatim allowlist should preserve every ID needle even with a marker-free summary (lost: %v)", rep.Recall, rep.Probes[0].Lost)
	}
	if rep.DriftRate != 0 {
		t.Errorf("drift_rate = %.2f; the allowlist should keep facts across sequential compactions, want 0", rep.DriftRate)
	}
}

// TestCompactionBaseline is the runnable baseline scaffold: it scores the
// CURRENT compressor with a deterministic fixture model and logs the report.
// Operators point the same Evaluate() at a real model + the model's TokenCount
// for a true baseline (see cmd/tenant eval-compaction).
func TestCompactionBaseline(t *testing.T) {
	c := &compress.Compressor{Router: fakeSource{llm: fakeLLM{echo: true}}, TailTokens: 800, MinMessages: 6}
	rep, err := Evaluate(context.Background(), c, EstimateTokens, Options{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var sb strings.Builder
	WriteTerminal(&sb, rep)
	t.Logf("compaction-fidelity baseline (fixture echo summarizer):\n%s", sb.String())
	if rep.SchemaVersion != SchemaVersion {
		t.Errorf("schema version = %d, want %d", rep.SchemaVersion, SchemaVersion)
	}
}
