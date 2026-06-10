package compress

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// TestCompact_VerbatimUserRequestSurvives: even when the summarizer LLM omits
// the user's ask, the verbatim "## User Requests" block preserves it (TEN-169
// continuity — the agent must still know "what I asked").
func TestCompact_VerbatimUserRequestSurvives(t *testing.T) {
	c := &Compressor{Router: fakeSource{llm: &fakeLLM{summary: "## Resolved\n- something happened"}}, MinMessages: 4}
	msgs := []working.Message{
		{Role: "user", Content: "deploy the staging cluster, ticket TASK-9001"},
		{Role: "assistant", Content: "starting"},
		{Role: "user", Content: "and tail the logs"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "keep going"},
		{Role: "assistant", Content: "recent tail"},
	}
	out, changed, err := c.Compact(context.Background(), msgs)
	if err != nil || !changed {
		t.Fatalf("expected compaction: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(out[0].Content, "deploy the staging cluster, ticket TASK-9001") {
		t.Fatalf("user request lost across compaction (summary omitted it, verbatim block should carry it):\n%s", out[0].Content)
	}
}

// TestCompact_NoCollapseToTwo: a long, tool-heavy session must NOT collapse to
// [summary]+[1 tail] (the operator's "45 → 2 messages"). The exchange floor
// keeps several recent complete exchanges.
func TestCompact_NoCollapseToTwo(t *testing.T) {
	var msgs []working.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			working.Message{Role: "user", Content: fmt.Sprintf("ask %d", i)},
			working.Message{Role: "assistant", Content: strings.Repeat("big tool output ", 200)},
		)
	}
	c := &Compressor{Router: fakeSource{llm: &fakeLLM{summary: "## Active Task\nx"}}, MinMessages: 4}
	out, changed, err := c.Compact(context.Background(), msgs)
	if err != nil || !changed {
		t.Fatalf("expected compaction: changed=%v err=%v", changed, err)
	}
	if len(out) <= 2 {
		t.Fatalf("collapsed to %d messages (the 45→2 bug); want summary + multiple tail messages", len(out))
	}
}

// TestCompact_TailStartsOnBoundary: the kept tail must begin at a turn boundary,
// never an orphaned tool result (which would 400). Tiny TailTokens forces the
// budget clamp; the exchange floor must still keep whole exchanges.
func TestCompact_TailStartsOnBoundary(t *testing.T) {
	msgs := []working.Message{
		{Role: "user", Content: "u0"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "t1", Name: "x"}}},
		{Role: "tool", Content: "res1", ToolCallID: "t1"},
		{Role: "user", Content: "u1"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "t2", Name: "x"}}},
		{Role: "tool", Content: "res2", ToolCallID: "t2"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "final"},
	}
	c := &Compressor{Router: fakeSource{llm: &fakeLLM{summary: "## Active Task\nx"}}, TailTokens: 5, MinMessages: 4}
	out, changed, err := c.Compact(context.Background(), msgs)
	if err != nil || !changed {
		t.Fatalf("expected compaction: changed=%v err=%v", changed, err)
	}
	// out[0] is the summary (role user); out[1] is the first kept tail message —
	// it must be a turn boundary (user), not an orphaned tool result.
	if len(out) < 2 {
		t.Fatalf("unexpected output length %d", len(out))
	}
	if out[1].Role == "tool" {
		t.Fatalf("tail begins with an orphaned tool result — the request would 400")
	}
}
