package improve

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

// TEN-195: the self-improvement PROPOSER/reflection calls can be pinned to a
// stronger reasoning model. These tests prove (a) the soul proposer resolves
// its LLM from the router it was handed and records that model in the proposal
// reason (provenance), and (b) the Consolidation summarizer-router selector
// prefers the pinned router while leaving the embedder on the main Router.

// routerWithSummarizerModel builds a router whose RoleSummarizer resolves to a
// profile carrying modelID, backed by a Fake that returns canned text.
func routerWithSummarizerModel(t *testing.T, modelID, genText string) *model.Router {
	t.Helper()
	reg := model.NewEmptyRegistry()
	if err := reg.Add(model.Profile{
		ID: "sum", Role: model.RoleSummarizer, Model: modelID,
		Backend: "fake", Endpoint: "http://x", ContextLength: 4000,
	}); err != nil {
		t.Fatalf("reg.Add: %v", err)
	}
	r := model.NewRouter(reg, slog.Default())
	fake := testllm.New()
	fake.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: genText, FinishReason: "stop"}, nil
	}
	r.RegisterBackend("fake", func(_ context.Context, _ model.Profile, _ *slog.Logger) (any, error) {
		return fake, nil
	})
	return r
}

func TestLLMSoulProposer_RecordsProposerModelInReason(t *testing.T) {
	r := routerWithSummarizerModel(t, "opus-improver",
		`{"change": true, "reason": "tighten tone", "instructions": ["be concise"]}`)
	p := NewLLMSoulProposer(r)

	changed, reason, instrs, err := p.Propose(context.Background(), &soul.Soul{}, SoulSignal{Acks: 3})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !changed {
		t.Fatalf("expected a proposed change")
	}
	if len(instrs) != 1 || instrs[0] != "be concise" {
		t.Fatalf("instructions = %v, want [be concise]", instrs)
	}
	if !strings.Contains(reason, "opus-improver") {
		t.Fatalf("reason missing proposer-model provenance: %q", reason)
	}
	if !strings.Contains(reason, "[proposed by ") {
		t.Fatalf("reason missing provenance marker: %q", reason)
	}
}

func TestLLMSoulProposer_NoModelID_NoProvenanceMarker(t *testing.T) {
	// A profile with no model id should not append an empty provenance marker.
	r := routerWithSummarizerModel(t, "",
		`{"change": true, "reason": "tighten tone", "instructions": ["be concise"]}`)
	_, reason, _, err := NewLLMSoulProposer(r).Propose(context.Background(), &soul.Soul{}, SoulSignal{Acks: 3})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if strings.Contains(reason, "[proposed by ") {
		t.Fatalf("should not append a provenance marker when model id is empty: %q", reason)
	}
}

func TestConsolidationJob_SummarizerRouterSelection(t *testing.T) {
	main := model.NewRouter(model.NewEmptyRegistry(), slog.Default())
	pinned := model.NewRouter(model.NewEmptyRegistry(), slog.Default())

	// No SummarizerRouter ⇒ falls back to the main Router (today's behavior).
	j := &ConsolidationJob{Router: main}
	if got := j.summarizerRouter(); got != main {
		t.Fatalf("nil SummarizerRouter should resolve to the main Router")
	}

	// SummarizerRouter set ⇒ the pinned router is used for the summarizer, but
	// Router (the embedder source) is left untouched.
	j2 := &ConsolidationJob{Router: main, SummarizerRouter: pinned}
	if got := j2.summarizerRouter(); got != pinned {
		t.Fatalf("SummarizerRouter should win for the summarizer LLM")
	}
	if j2.Router != main {
		t.Fatalf("Router (embedder source) must stay the main router")
	}
}
