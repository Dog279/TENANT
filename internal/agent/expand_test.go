package agent

import (
	"context"
	"testing"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/memory/compress"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// filterGatedTools surfaces a gated tool only to a capable profile (TEN-103).
func TestFilterGatedTools(t *testing.T) {
	specs := []model.ToolSpec{{Name: "web_search"}, {Name: "memory_recall", Gate: "recall"}}
	weak := model.Profile{OperationalContextBudget: 8000} // WritableBudget < 32768
	if got := filterGatedTools(specs, weak); len(got) != 1 || got[0].Name != "web_search" {
		t.Errorf("a gated tool must be filtered for a weak profile: %+v", got)
	}
	strong := model.Profile{OperationalContextBudget: 100000}
	if got := filterGatedTools(specs, strong); len(got) != 2 {
		t.Errorf("a capable profile must keep the gated tool: %+v", got)
	}
}

// ExpandLatestCompaction rehydrates the summarized span from the archive,
// INCLUDING the event exactly at source_after (the boundary-exclusivity fix) and
// EXCLUDING events past source_before; it tolerates int64 and float64 Meta.
func TestExpandLatestCompaction(t *testing.T) {
	after := time.Unix(1000, 0)
	before := time.Unix(2000, 0)
	mkAgent := func(meta map[string]any) *Agent {
		ws := working.New()
		ws.Append(working.Message{Role: "user", Content: "earlier turn"})
		ws.Append(working.Message{Role: "user", Content: "## Active Task\nship P4.6", Kind: compress.KindCompactionSummary, Meta: meta})
		aw := archive.NewWriter(t.TempDir())
		_ = aw.Append(archive.Event{Timestamp: after, SessionID: "s1", Role: "user", Content: "boundary turn"})
		_ = aw.Append(archive.Event{Timestamp: after.Add(time.Minute), SessionID: "s1", Role: "assistant", Content: "middle turn"})
		_ = aw.Append(archive.Event{Timestamp: before.Add(time.Hour), SessionID: "s1", Role: "user", Content: "AFTER the span"})
		return &Agent{cfg: Config{Working: ws, Archive: aw, SessionID: "s1"}}
	}

	intMeta := map[string]any{
		"source_session": "s1", "source_after": after.Unix(), "source_before": before.Unix(),
		"source_msg_count": 2, "source_origin": "working",
	}
	exp, err := mkAgent(intMeta).ExpandLatestCompaction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exp == nil {
		t.Fatal("expected an expansion")
	}
	if exp.Source.SessionID != "s1" || exp.Source.MsgCount != 2 || exp.Source.Origin != "working" {
		t.Errorf("source meta wrong: %+v", exp.Source)
	}
	got := expEventContents(exp.Events)
	if !expHas(got, "boundary turn") {
		t.Error("the event exactly at source_after must be INCLUDED (boundary fix)")
	}
	if !expHas(got, "middle turn") {
		t.Error("an in-span event must be included")
	}
	if expHas(got, "AFTER the span") {
		t.Error("an event past source_before must be excluded")
	}

	// float64 Meta (a JSON-rehydrated / fake set) parses identically.
	floatMeta := map[string]any{
		"source_session": "s1", "source_after": float64(after.Unix()), "source_before": float64(before.Unix()),
		"source_msg_count": float64(2), "source_origin": "working",
	}
	expF, _ := mkAgent(floatMeta).ExpandLatestCompaction(context.Background())
	if expF == nil || expF.Source.MsgCount != 2 || !expHas(expEventContents(expF.Events), "boundary turn") {
		t.Errorf("float64 Meta must parse like int64: %+v", expF)
	}
}

func TestExpandLatestCompaction_NoSummary(t *testing.T) {
	ws := working.New()
	ws.Append(working.Message{Role: "user", Content: "just a turn, no summary"})
	a := &Agent{cfg: Config{Working: ws, Archive: archive.NewWriter(t.TempDir()), SessionID: "s1"}}
	exp, err := a.ExpandLatestCompaction(context.Background())
	if err != nil || exp != nil {
		t.Errorf("no summary → (nil,nil); got exp=%+v err=%v", exp, err)
	}
}

func expEventContents(evs []ExpandedEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Content
	}
	return out
}

func expHas(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
