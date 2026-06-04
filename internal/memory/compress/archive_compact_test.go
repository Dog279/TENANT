package compress

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// echoLLM echoes the last user message (the rendered source) back as the summary,
// so a test can assert WHICH source the compressor summarized from.
type echoLLM struct{}

func (echoLLM) Generate(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return &model.GenerateResponse{Text: req.Messages[i].Content}, nil
		}
	}
	return &model.GenerateResponse{Text: "summary"}, nil
}
func (echoLLM) GenerateStream(context.Context, model.GenerateRequest) (<-chan model.StreamChunk, error) {
	return nil, nil
}
func (echoLLM) TokenCount(_ context.Context, s string) (int, error) { return len(s) / 4, nil }

type srcLLM struct{ llm model.LLM }

func (s srcLLM) LLMForRole(context.Context, model.Role) (model.LLM, model.Profile, error) {
	return s.llm, model.Profile{}, nil
}

func TestCompactWithArchive_AllowlistPreservation(t *testing.T) {
	t0 := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	msgs := []working.Message{
		{Role: "user", Content: "start the task", Timestamp: at(0)},
		{Role: "assistant", Content: "Working on JIRA-4242, editing src/main.go", Timestamp: at(1)},
		{Role: "assistant", Content: "Hit error: connection refused to db; notes in wiki:notes/incident.md", Timestamp: at(2)},
		{Role: "user", Content: "ok keep going", Timestamp: at(3)},
		{Role: "assistant", Content: "recent tail one", Timestamp: at(4)},
		{Role: "assistant", Content: "recent tail two", Timestamp: at(5)},
	}
	// Fixed, marker-free summary: any identifier in the output can ONLY have come
	// from the deterministic verbatim allowlist.
	c := &Compressor{Router: fakeSource{llm: &fakeLLM{summary: "## Active Task\ndid stuff"}}, TailTokens: 8, MinMessages: 4}
	out, changed, err := c.Compact(context.Background(), msgs)
	if err != nil || !changed {
		t.Fatalf("expected compaction, changed=%v err=%v", changed, err)
	}
	sm := out[0]
	if sm.Kind != KindCompactionSummary {
		t.Errorf("summary Kind = %q, want %q", sm.Kind, KindCompactionSummary)
	}
	if !strings.Contains(sm.Content, "## Verbatim") {
		t.Fatalf("summary missing the verbatim allowlist:\n%s", sm.Content)
	}
	for _, want := range []string{"JIRA-4242", "src/main.go", "wiki:notes/incident.md", "refused"} {
		if !strings.Contains(sm.Content, want) {
			t.Errorf("verbatim allowlist dropped %q\n%s", want, sm.Content)
		}
	}
	if sm.Meta["source_origin"] != "working" {
		t.Errorf("source_origin = %v, want working", sm.Meta["source_origin"])
	}
}

func TestCompactWithArchive_FromArchiveAndTagResolution(t *testing.T) {
	dir := t.TempDir()
	w := archive.NewWriter(dir)
	base := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		content := fmt.Sprintf("archived turn %d", i)
		if i == 0 {
			content = "the deploy key is ARCHIVED-0007, see file src/app.go"
		}
		if err := w.Append(archive.Event{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			SessionID: "s1", Role: "assistant", Content: content,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Working set: head placeholders that do NOT contain the archived token, plus a
	// recent tail, timestamped well after the archived span. (In production the head
	// turns are themselves archived; here we omit them to prove the summary is built
	// from the ARCHIVE, not the in-memory head.)
	tb := base.Add(time.Hour)
	at := func(n int) time.Time { return tb.Add(time.Duration(n) * time.Second) }
	msgs := []working.Message{
		{Role: "user", Content: "placeholder head A", Timestamp: at(0)},
		{Role: "assistant", Content: "placeholder head B", Timestamp: at(1)},
		{Role: "user", Content: "placeholder head C", Timestamp: at(2)},
		{Role: "assistant", Content: "recent tail X", Timestamp: at(3)},
		{Role: "assistant", Content: "recent tail Y", Timestamp: at(4)},
		{Role: "assistant", Content: "recent tail Z", Timestamp: at(5)},
	}
	c := &Compressor{Router: srcLLM{llm: echoLLM{}}, TailTokens: 8, MinMessages: 4}
	out, changed, err := c.CompactWithArchive(context.Background(), msgs, w.Reader(), "s1")
	if err != nil || !changed {
		t.Fatalf("expected compaction, changed=%v err=%v", changed, err)
	}
	sm := out[0]
	if !strings.Contains(sm.Content, "ARCHIVED-0007") {
		t.Fatalf("summary not sourced from the archive (missing ARCHIVED-0007):\n%s", sm.Content)
	}
	if sm.Meta["source_origin"] != "archive" {
		t.Errorf("source_origin = %v, want archive", sm.Meta["source_origin"])
	}
	if sm.Meta["source_session"] != "s1" {
		t.Errorf("source_session = %v, want s1", sm.Meta["source_session"])
	}
	if sm.Meta["source_msg_count"] != 5 {
		t.Errorf("source_msg_count = %v, want 5", sm.Meta["source_msg_count"])
	}
	// Tag -> archive resolution: the source_before tag re-fetches exactly the span.
	before, ok := sm.Meta["source_before"].(int64)
	if !ok {
		t.Fatalf("source_before not int64: %T", sm.Meta["source_before"])
	}
	got := 0
	for _, err := range w.Reader().Stream(archive.Filter{SessionID: "s1", Before: time.Unix(before, 0)}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		got++
	}
	if got != 5 {
		t.Errorf("tag->archive resolution returned %d events, want 5", got)
	}
}
