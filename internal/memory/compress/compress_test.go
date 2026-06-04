package compress

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// fakeLLM is a minimal model.LLM for testing the compressor.
type fakeLLM struct {
	summary  string
	genErr   error
	genCalls int
}

func (f *fakeLLM) Generate(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
	f.genCalls++
	if f.genErr != nil {
		return nil, f.genErr
	}
	return &model.GenerateResponse{Text: f.summary}, nil
}
func (f *fakeLLM) GenerateStream(context.Context, model.GenerateRequest) (<-chan model.StreamChunk, error) {
	return nil, nil
}
func (f *fakeLLM) TokenCount(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil // ~4 chars/token
}

type fakeSource struct {
	llm *fakeLLM
	err error
}

func (s fakeSource) LLMForRole(context.Context, model.Role) (model.LLM, model.Profile, error) {
	if s.err != nil {
		return nil, model.Profile{}, s.err
	}
	return s.llm, model.Profile{}, nil
}

func makeMsgs(n int) []working.Message {
	out := make([]working.Message, n)
	for i := range out {
		out[i] = working.Message{
			Role:    map[bool]string{true: "user", false: "assistant"}[i%2 == 0],
			Content: fmt.Sprintf("message %d: %s", i, strings.Repeat("x", 116)), // ~30 tokens
		}
	}
	return out
}

func TestCompact_SummarizesHeadKeepsTail(t *testing.T) {
	llm := &fakeLLM{summary: "## Active Task\nfinish the port\n## Resolved\n- did X"}
	c := &Compressor{Router: fakeSource{llm: llm}, TailTokens: 40, MinMessages: 4}

	msgs := makeMsgs(8)
	out, changed, err := c.Compact(context.Background(), msgs)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !changed {
		t.Fatal("expected compaction with 8 messages over budget")
	}
	if len(out) >= len(msgs) {
		t.Fatalf("compaction should shrink the set: %d -> %d", len(msgs), len(out))
	}
	// First message is the reference-only summary; it carries the prefix
	// and the model's structured output.
	if out[0].Role != "user" || !strings.HasPrefix(out[0].Content, SummaryPrefix) {
		t.Fatalf("first message should be the fenced summary, got role=%q content=%q", out[0].Role, out[0].Content[:40])
	}
	if !strings.Contains(out[0].Content, "Active Task") {
		t.Fatal("summary body missing")
	}
	// The most recent original message survives verbatim in the tail.
	if out[len(out)-1].Content != msgs[len(msgs)-1].Content {
		t.Fatal("most-recent message must be preserved verbatim")
	}
	if llm.genCalls != 1 {
		t.Fatalf("summarizer should be called once, got %d", llm.genCalls)
	}
}

func TestCompact_SkipsShortConversations(t *testing.T) {
	llm := &fakeLLM{summary: "x"}
	c := &Compressor{Router: fakeSource{llm: llm}, MinMessages: 6}
	msgs := makeMsgs(3)
	out, changed, _ := c.Compact(context.Background(), msgs)
	if changed || len(out) != 3 {
		t.Fatal("short conversation must not be compacted")
	}
	if llm.genCalls != 0 {
		t.Fatal("summarizer must not be called for a short conversation")
	}
}

func TestCompact_EmptySummaryLeavesSetIntact(t *testing.T) {
	llm := &fakeLLM{summary: "   "} // whitespace only
	c := &Compressor{Router: fakeSource{llm: llm}, TailTokens: 40, MinMessages: 4}
	msgs := makeMsgs(8)
	out, changed, err := c.Compact(context.Background(), msgs)
	if err != nil || changed || len(out) != len(msgs) {
		t.Fatalf("empty summary must not mutate the set: changed=%v len=%d err=%v", changed, len(out), err)
	}
}

func TestCompact_ResolveErrorIsSafe(t *testing.T) {
	c := &Compressor{Router: fakeSource{err: fmt.Errorf("summarizer down")}, TailTokens: 40, MinMessages: 4}
	msgs := makeMsgs(8)
	out, changed, err := c.Compact(context.Background(), msgs)
	if err == nil || changed {
		t.Fatal("a resolve error must surface and leave the set unchanged")
	}
	if len(out) != len(msgs) {
		t.Fatal("set must be returned intact on error")
	}
}

// TestSystemPrompt_HasArtifactsSection — TEN-47.
// The compaction summarizer must produce an "## Artifacts" section so
// wiki/research/file URIs survive into the post-compaction working set.
// Drift guard: if the prompt template is rewritten, this test catches
// silent removal of the Artifacts hint.
func TestSystemPrompt_HasArtifactsSection(t *testing.T) {
	for _, want := range []string{
		"## Artifacts",
		"wiki:",
		"research:",
		"file:",
		"memory:fact-",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("systemPrompt missing %q (drift guard for TEN-47)", want)
		}
	}
}
