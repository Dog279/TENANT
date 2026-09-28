package agent

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"tenant/internal/logs/event"
	"tenant/internal/orchestra"
)

func TestEntryFor_DenylistsNoise(t *testing.T) {
	for _, k := range []EventKind{EventToken, EventUsage, EventAssistant, EventMemory} {
		if _, ok := EntryFor(Event{Kind: k}); ok {
			t.Errorf("%s must not be retained", k)
		}
	}
	for _, k := range []EventKind{EventToolCall, EventFinal, EventBus, EventIngest, EventTurnStart, EventKind("future_kind")} {
		if _, ok := EntryFor(Event{Kind: k}); !ok {
			t.Errorf("%s must be retained", k)
		}
	}
}

func TestEntryFor_SourcesAndLevels(t *testing.T) {
	cases := []struct {
		name      string
		ev        Event
		src, msg  string
		level     slog.Level
		wantField map[string]string
	}{
		{"primary tool call", Event{Kind: EventToolCall, Tool: "web_search", Args: `{"q":"x"}`, Iter: 2}, "turns", "tool call web_search", slog.LevelInfo,
			map[string]string{"ev": "tool_call", "tool": "web_search", "args": `{"q":"x"}`, "iter": "2"}},
		{"failed tool result", Event{Kind: EventToolResult, Tool: "os_exec", Result: "timeout", IsErr: true}, "turns", "tool result os_exec failed", slog.LevelError,
			map[string]string{"result": "timeout"}},
		{"sub-agent final", Event{Kind: EventFinal, Agent: "Programmer", Text: "done"}, "subagents", "final response", slog.LevelInfo,
			map[string]string{"agent": "Programmer", "text": "done"}},
		{"explicit source wins", Event{Kind: EventToolCall, Agent: "tenant-cron", Source: "cron", Tool: "wiki_search"}, "cron", "tool call wiki_search", slog.LevelInfo,
			map[string]string{"agent": "tenant-cron"}},
		{"bus", Event{Kind: EventBus, Agent: "researcher", Text: "→ writer: draft"}, "subagents", "bus message", slog.LevelInfo,
			map[string]string{"text": "→ writer: draft"}},
		{"ingest by channel", Event{Kind: EventIngest, Text: "iMessage: hey"}, "imessage", "inbound message", slog.LevelInfo, nil},
		{"turn error", Event{Kind: EventError, Text: "boom"}, "turns", "turn error", slog.LevelError, map[string]string{"text": "boom"}},
		{"retry is a warning", Event{Kind: EventRetry, Tool: "web_read", Text: "attempt 1/2: eof", IsErr: true}, "turns", "tool retry web_read", slog.LevelWarn, nil},
		{"approval resolved", Event{Kind: EventApprovalResolved, Approval: &ApprovalEvent{ID: "ap-3", Category: "exec", Action: "os_exec", Origin: "discord", Outcome: "denied"}},
			"approvals", "approval denied ap-3", slog.LevelWarn, map[string]string{"category": "exec", "origin": "discord", "outcome": "denied"}},
		{"approval pending", Event{Kind: EventApprovalPending, Approval: &ApprovalEvent{ID: "ap-4", Action: "os_write"}}, "approvals", "approval pending ap-4", slog.LevelInfo, nil},
		{"add-on names its folder", Event{Kind: EventAddon, Agent: "My Watcher", Text: "disk 91%", IsErr: true}, "My Watcher", "disk 91%", slog.LevelError, nil},
	}
	for _, c := range cases {
		e, ok := EntryFor(c.ev)
		if !ok {
			t.Fatalf("%s: not retained", c.name)
		}
		if e.Source != c.src || e.Msg != c.msg || e.Level != c.level {
			t.Errorf("%s: got source=%q msg=%q level=%v, want %q %q %v", c.name, e.Source, e.Msg, e.Level, c.src, c.msg, c.level)
		}
		for k, v := range c.wantField {
			if got := e.Field(k); got != v {
				t.Errorf("%s: field %s = %q, want %q", c.name, k, got, v)
			}
		}
	}
	// The add-on's agent name is the source, not a field (it would be redundant).
	if e, _ := EntryFor(Event{Kind: EventAddon, Agent: "w", Text: "x"}); e.Field("agent") != "" {
		t.Error("add-on entries must not carry an agent field")
	}
}

// The trace ID is written as `trace`, joining the line to its runs.db record;
// an event from outside a turn has none.
func TestEntryFor_WritesTrace(t *testing.T) {
	e, _ := EntryFor(Event{Kind: EventToolResult, Tool: "web_read", Result: "ok", TraceID: "4f2a"})
	if got := e.Field("trace"); got != "4f2a" {
		t.Errorf("trace field = %q, want 4f2a", got)
	}
	if e, _ := EntryFor(Event{Kind: EventBus, Agent: "a", Text: "x"}); e.Field("trace") != "" {
		t.Errorf("an event with no trace must not carry one: %+v", e.Fields)
	}
}

// Each line EntryFor tags names a registered definition and its task. A
// plain event's line has the definition's level, and its message is the
// template rendered from the line's fields; a turn's start and end carry
// their opcode, the fail at error level. Lines of other owners carry none.
func TestEntryFor_NamesItsCatalogueEntry(t *testing.T) {
	hole := regexp.MustCompile(`\{([A-Za-z0-9_.]+)\}`)
	stopped := &orchestra.BudgetStatus{TerminationReason: "token budget"}
	for _, ev := range []Event{
		{Kind: EventTurnStart, Text: "q"}, {Kind: EventTurnEnd}, {Kind: EventTurnEnd, IsErr: true, Text: "boom"},
		{Kind: EventRoute, Text: "r"}, {Kind: EventSkills}, {Kind: EventToolCall, Tool: "t", Args: "{}"},
		{Kind: EventToolResult, Tool: "t"}, {Kind: EventToolResult, Tool: "t", IsErr: true},
		{Kind: EventValidation, Tool: "t"}, {Kind: EventRetry, Tool: "t"}, {Kind: EventFinal}, {Kind: EventTruncated},
		{Kind: EventError}, {Kind: EventCompact}, {Kind: EventInterject}, {Kind: EventToolCatalog},
		{Kind: EventBus, Agent: "a"}, {Kind: EventTeamBudget}, {Kind: EventTeamBudget, TeamBudget: stopped},
	} {
		e, _ := EntryFor(ev)
		p, id, _ := strings.Cut(e.Field("event"), "/")
		n, _ := strconv.ParseUint(id, 10, 32)
		d, ok := event.Lookup(p, uint32(n))
		if !ok {
			t.Errorf("%s: event=%q names no definition", ev.Kind, e.Field("event"))
			continue
		}
		s := d.Spec()
		if e.Field("task") != s.Task {
			t.Errorf("%s: task %q, want %q", ev.Kind, e.Field("task"), s.Task)
		}
		switch op := e.Field("opcode"); op {
		case "":
			msg := hole.ReplaceAllStringFunc(s.Template, func(h string) string { return e.Field(h[1 : len(h)-1]) })
			if e.Level != s.Level.Slog() || e.Msg != msg {
				t.Errorf("%s: %v %q, want %v %q", ev.Kind, e.Level, e.Msg, s.Level.Slog(), msg)
			}
		case event.OpStart, event.OpStop, event.OpFail:
			if (op == event.OpFail) != (e.Level == slog.LevelError) {
				t.Errorf("%s: opcode %s at %v", ev.Kind, op, e.Level)
			}
		default:
			t.Errorf("%s: opcode %q", ev.Kind, op)
		}
	}
	for _, k := range []EventKind{EventIngest, EventApproval, EventApprovalPending, EventApprovalResolved, EventAddon, "future_kind"} {
		if e, _ := EntryFor(Event{Kind: k, Agent: "x", Text: "discord: hi"}); e.Field("event") != "" {
			t.Errorf("%s is not the turns' to catalogue: %s", k, e.Line())
		}
	}
}
