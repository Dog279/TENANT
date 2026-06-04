package compress

import (
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/working"
	"tenant/internal/model"
)

func toolResult(callID, content string, ts time.Time) working.Message {
	return working.Message{Role: "tool", ToolCallID: callID, Content: content, Timestamp: ts}
}
func asstCall(callID, name string) working.Message {
	return working.Message{Role: "assistant", ToolCalls: []model.ToolCall{{ID: callID, Name: name}}}
}

func TestMicrocompact_ElidesOldLargeKeepsRecentAndSmall(t *testing.T) {
	big := strings.Repeat("x", 3000)
	const small = "ok"
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	msgs := []working.Message{
		{Role: "user", Content: "do stuff"},
		asstCall("c1", "web_read"),
		toolResult("c1", big, base),                    // old + large -> elide
		asstCall("c2", "sql_query"),
		toolResult("c2", small, base.Add(time.Minute)), // old + small -> keep
		asstCall("c3", "web_read"),
		toolResult("c3", big, base.Add(2*time.Minute)), // most-recent + large -> protected
	}
	c := &Compressor{MicrocompactProtectRecent: 1, MicrocompactMinBytes: 2000}
	out, changed, err := c.Microcompact(msgs)
	if err != nil || !changed {
		t.Fatalf("expected change, got changed=%v err=%v", changed, err)
	}
	if out[2].Kind != KindToolElided {
		t.Errorf("old large tool result should be elided, kind=%q", out[2].Kind)
	}
	if !strings.HasPrefix(out[2].Content, "[tool result elided") {
		t.Errorf("elided content should be a stub, got %q", out[2].Content)
	}
	if !strings.Contains(out[2].Content, "web_read") || !strings.Contains(out[2].Content, "recall:c1") {
		t.Errorf("stub must name the tool + call id, got %q", out[2].Content)
	}
	if out[2].Meta["call_id"] != "c1" || out[2].Meta["elided"] != true || out[2].Meta["orig_bytes"] != 3000 {
		t.Errorf("meta should record provenance, got %v", out[2].Meta)
	}
	if out[4].Kind != "" || out[4].Content != small {
		t.Errorf("small tool result must be kept verbatim, got kind=%q content=%q", out[4].Kind, out[4].Content)
	}
	if out[6].Kind != "" || out[6].Content != big {
		t.Error("most-recent tool result must be protected verbatim")
	}
	if msgs[2].Kind == KindToolElided || msgs[2].Content != big {
		t.Error("Microcompact must not mutate the input slice")
	}
}

func TestMicrocompact_NoToolResultsNoChange(t *testing.T) {
	msgs := []working.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: strings.Repeat("y", 5000)}, // big, but not a tool result
	}
	if _, changed, _ := (&Compressor{}).Microcompact(msgs); changed {
		t.Fatal("no tool results -> no change")
	}
}

func TestMicrocompact_SupersededDuplicate(t *testing.T) {
	dup := strings.Repeat("z", 500) // >= redundantElideFloor, < default minBytes
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msgs := []working.Message{
		asstCall("a", "ls"),
		toolResult("a", dup, base), // earlier identical -> superseded -> elide
		{Role: "assistant", Content: "thinking"},
		asstCall("b", "ls"),
		toolResult("b", dup, base.Add(time.Minute)), // latest identical + recent -> protected
	}
	c := &Compressor{MicrocompactProtectRecent: 1}
	out, changed, _ := c.Microcompact(msgs)
	if !changed {
		t.Fatal("expected superseded elision")
	}
	if out[1].Kind != KindToolElided {
		t.Errorf("earlier identical result should be elided as superseded, kind=%q", out[1].Kind)
	}
	if out[1].Meta["superseded"] != true {
		t.Errorf("meta should mark superseded, got %v", out[1].Meta)
	}
	if out[4].Kind != "" {
		t.Error("latest identical result is recent+protected, must be kept")
	}
}

func TestMicrocompact_AllProtected(t *testing.T) {
	big := strings.Repeat("x", 3000)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msgs := []working.Message{
		asstCall("c1", "web_read"), toolResult("c1", big, base),
		asstCall("c2", "web_read"), toolResult("c2", big, base.Add(time.Minute)),
	}
	c := &Compressor{MicrocompactProtectRecent: 5} // more than present -> all protected
	if _, changed, _ := c.Microcompact(msgs); changed {
		t.Fatal("all tool results protected -> no change")
	}
}
