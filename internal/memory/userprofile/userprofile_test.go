package userprofile

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

func TestProfile_LoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Missing file => empty, no error.
	p, err := Load(dir, "main")
	if err != nil || p.HasContent() {
		t.Fatalf("first load should be empty: %+v err=%v", p, err)
	}
	p.Update("## Identity\n- builds Tenant")
	if err := p.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(dir, "main")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !strings.Contains(got.Body(), "builds Tenant") {
		t.Fatalf("round-trip lost body: %q", got.Body())
	}
}

func TestProfile_RenderFencedAndNilSafe(t *testing.T) {
	var nilP *Profile
	if nilP.Render() != "" {
		t.Fatal("nil profile must render empty (no panic)")
	}
	empty := &Profile{AgentID: "main"}
	if empty.Render() != "" {
		t.Fatal("empty profile must render empty")
	}
	p := &Profile{AgentID: "main", Markdown: "## Identity\n- x"}
	out := p.Render()
	if !strings.Contains(out, "learned from past conversations") || !strings.Contains(out, "## Identity") {
		t.Fatalf("render should fence as learned/background: %q", out)
	}
}

func TestProfile_AppendRemembered(t *testing.T) {
	p := &Profile{AgentID: "main", Markdown: "## Identity\n- builds Tenant"}
	p.AppendRemembered("prefers tabs")
	p.AppendRemembered("lives in Greenland")
	p.AppendRemembered("prefers tabs") // dup — must be ignored
	body := p.Body()
	if !strings.Contains(body, rememberedHeader) {
		t.Fatalf("missing remembered section:\n%s", body)
	}
	if strings.Count(body, "prefers tabs") != 1 {
		t.Fatalf("duplicate not deduped:\n%s", body)
	}
	if !strings.Contains(body, "- lives in Greenland") || !strings.Contains(body, "## Identity") {
		t.Fatalf("append clobbered or missed content:\n%s", body)
	}
	// Render exposes it (always-on context) right away.
	if !strings.Contains(p.Render(), "lives in Greenland") {
		t.Fatal("appended fact must be in the rendered profile immediately")
	}
}

func TestProfile_AppendToEmpty(t *testing.T) {
	p := &Profile{AgentID: "main"}
	p.AppendRemembered("x")
	if !strings.Contains(p.Body(), rememberedHeader) || !strings.Contains(p.Body(), "- x") {
		t.Fatalf("append to empty profile failed: %q", p.Body())
	}
}

func TestProfile_UpdateBumpsVersion(t *testing.T) {
	p := &Profile{AgentID: "main"}
	p.Update("a")
	p.Update("b")
	if p.Version != 2 || p.Body() != "b" {
		t.Fatalf("update wrong: ver=%d body=%q", p.Version, p.Body())
	}
}

// --- synthesizer ---

type fakeLLM struct {
	out      string
	genCalls int
}

func (f *fakeLLM) Generate(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
	f.genCalls++
	return &model.GenerateResponse{Text: f.out}, nil
}
func (f *fakeLLM) GenerateStream(context.Context, model.GenerateRequest) (<-chan model.StreamChunk, error) {
	return nil, nil
}
func (f *fakeLLM) TokenCount(_ context.Context, s string) (int, error) { return len(s) / 4, nil }

type fakeSource struct{ llm *fakeLLM }

func (s fakeSource) LLMForRole(context.Context, model.Role) (model.LLM, model.Profile, error) {
	return s.llm, model.Profile{}, nil
}

func openStoreWithFacts(t *testing.T, facts ...string) *semantic.Store {
	t.Helper()
	ss, err := semantic.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	for _, f := range facts {
		if _, err := ss.Insert(context.Background(), &semantic.Fact{
			AgentID: "main", Visibility: semantic.VisibilityPrivate, Fact: f,
			Confidence: 0.9, EmbedderID: "t/2", Embedding: []float32{1, 0},
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return ss
}

func TestSynthesizer_FoldsFacts(t *testing.T) {
	llm := &fakeLLM{out: "## Identity\n- builds Tenant\n## Preferences\n- terse"}
	syn := &Synthesizer{Router: fakeSource{llm: llm}, Semantic: openStoreWithFacts(t, "user builds Tenant", "user prefers Go"), AgentID: "main"}

	md, changed, err := syn.Run(context.Background(), &Profile{AgentID: "main"})
	if err != nil || !changed {
		t.Fatalf("synth should produce a profile: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(md, "Identity") {
		t.Fatalf("synth markdown wrong: %q", md)
	}
	if llm.genCalls != 1 {
		t.Fatalf("summarizer called %d times", llm.genCalls)
	}
}

func TestSynthesizer_NoFactsNoChange(t *testing.T) {
	llm := &fakeLLM{out: "x"}
	syn := &Synthesizer{Router: fakeSource{llm: llm}, Semantic: openStoreWithFacts(t), AgentID: "main"}
	_, changed, err := syn.Run(context.Background(), &Profile{AgentID: "main"})
	if err != nil || changed {
		t.Fatalf("no facts → no change: changed=%v err=%v", changed, err)
	}
	if llm.genCalls != 0 {
		t.Fatal("summarizer must not run with no facts")
	}
}

func TestSynthesizer_EmptyOutputNoChange(t *testing.T) {
	llm := &fakeLLM{out: "   "}
	syn := &Synthesizer{Router: fakeSource{llm: llm}, Semantic: openStoreWithFacts(t, "user builds Tenant"), AgentID: "main"}
	_, changed, _ := syn.Run(context.Background(), &Profile{AgentID: "main"})
	if changed {
		t.Fatal("empty model output must not change the profile")
	}
}
