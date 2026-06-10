package assemble

import (
	"testing"

	"tenant/internal/model"
)

// validateToolPairing is the structural-validity predicate the backends enforce:
// every tool result must follow an earlier assistant tool_use with the same id,
// and every assistant tool_use must have a matching tool result. Returns "" when
// valid, else a description of the first violation.
func validateToolPairing(msgs []model.Message) string {
	issued := map[string]bool{}
	resulted := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "tool" {
			if m.ToolCallID == "" || !issued[m.ToolCallID] {
				return "orphaned tool result: " + m.ToolCallID
			}
			resulted[m.ToolCallID] = true
			continue
		}
		for _, tc := range m.ToolCalls {
			issued[tc.ID] = true
		}
	}
	for id := range issued {
		if !resulted[id] {
			return "childless tool_use: " + id
		}
	}
	return ""
}

func asst(content string, callIDs ...string) model.Message {
	m := model.Message{Role: "assistant", Content: content}
	for _, id := range callIDs {
		m.ToolCalls = append(m.ToolCalls, model.ToolCall{ID: id, Name: "x"})
	}
	return m
}
func toolRes(id string) model.Message {
	return model.Message{Role: "tool", ToolCallID: id, Content: "result"}
}

// TestSanitizePairs_DropsOrphanedResult is the 400 regression: a truncated
// working set whose oldest tool_use was dropped leaves an orphaned tool result —
// the exact shape that makes the backend reject the request.
func TestSanitizePairs_DropsOrphanedResult(t *testing.T) {
	// Simulates truncateWorking having dropped the assistant that issued t1,
	// leaving its result orphaned at the head, followed by a clean t2 pair.
	in := []model.Message{
		{Role: "system", Content: "sys"},
		toolRes("t1"), // orphan — its tool_use was trimmed
		asst("calling", "t2"),
		toolRes("t2"),
		{Role: "user", Content: "next"},
	}
	out := sanitizePairs(in)
	if v := validateToolPairing(out); v != "" {
		t.Fatalf("sanitized output still invalid: %s", v)
	}
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "t1" {
			t.Fatal("orphaned t1 result was not dropped")
		}
	}
	// The valid t2 pair + surrounding messages must survive.
	if len(out) != 4 {
		t.Errorf("expected 4 messages after dropping the orphan, got %d", len(out))
	}
}

func TestSanitizePairs_StripsChildlessToolUse(t *testing.T) {
	in := []model.Message{
		asst("I'll look it up", "t9"), // tool_use whose result was trimmed
		{Role: "user", Content: "hi"},
	}
	out := sanitizePairs(in)
	if v := validateToolPairing(out); v != "" {
		t.Fatalf("still invalid: %s", v)
	}
	if len(out[0].ToolCalls) != 0 {
		t.Error("childless tool_use t9 was not stripped")
	}
	if out[0].Content != "I'll look it up" {
		t.Error("assistant content should be preserved when stripping calls")
	}
}

func TestSanitizePairs_DropsEmptyAssistantTurn(t *testing.T) {
	in := []model.Message{
		asst("", "t1"), // no content, childless call → whole turn is empty
		{Role: "user", Content: "hi"},
	}
	out := sanitizePairs(in)
	for _, m := range out {
		if m.Role == "assistant" {
			t.Fatal("empty assistant turn should be dropped")
		}
	}
}

func TestSanitizePairs_ValidSequenceUnchanged(t *testing.T) {
	in := []model.Message{
		{Role: "user", Content: "do x"},
		asst("ok", "t1"),
		toolRes("t1"),
		asst("done"),
	}
	out := sanitizePairs(in)
	if len(out) != len(in) {
		t.Errorf("valid sequence was modified: %d → %d", len(in), len(out))
	}
	if v := validateToolPairing(out); v != "" {
		t.Fatalf("valid sequence flagged invalid: %s", v)
	}
}
