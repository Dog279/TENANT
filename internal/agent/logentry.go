package agent

import (
	"log/slog"
	"strconv"
	"strings"

	"tenant/internal/logs"
)

// Retained reports whether a live event belongs in the retained activity
// record. It is a DENYLIST of per-token / accounting noise — token deltas (the
// flood), usage counts, per-iteration assistant prose and the memory-budget
// report — so every meaningful or future kind is kept by default. Mirrors the
// dashboard's live-feed filter (activityRelevant).
func Retained(ev Event) bool {
	switch ev.Kind {
	case EventToken, EventUsage, EventAssistant, EventMemory:
		return false
	}
	return true
}

// EntryFor renders a live turn event as one log-book line: a readable message
// ("tool call web_search", "final response", "approval denied ap-3"), the kind
// preserved in the `ev` field so the dashboard can style the row after a
// restart re-reads it from disk, and the event's payload as fields. ok is
// false for noise (see Retained). The folder is ev.Source when the emitter
// set one, else "subagents" for a sub-agent's activity, "approvals" for the
// approval kinds, the channel for an inbound message, the add-on's own name
// for an add-on line, and "turns" for the primary agent's own turn.
func EntryFor(ev Event) (logs.Entry, bool) {
	if !Retained(ev) {
		return logs.Entry{}, false
	}
	e := logs.Entry{Source: ev.Source, Level: slog.LevelInfo}
	e.Fields = append(e.Fields, logs.Field{Key: "ev", Value: string(ev.Kind)})
	if ev.Agent != "" && ev.Kind != EventAddon {
		e.Fields = append(e.Fields, logs.Field{Key: "agent", Value: ev.Agent})
	}
	if ev.Iter > 0 {
		e.Fields = append(e.Fields, logs.Field{Key: "iter", Value: strconv.Itoa(ev.Iter)})
	}
	text := func() {
		if ev.Text != "" {
			e.Fields = append(e.Fields, logs.Field{Key: "text", Value: ev.Text})
		}
	}
	tool := func() {
		if ev.Tool != "" {
			e.Fields = append(e.Fields, logs.Field{Key: "tool", Value: ev.Tool})
		}
	}
	switch ev.Kind {
	case EventTurnStart:
		e.Msg = "turn started"
		text()
	case EventRoute:
		e.Msg = "route observation"
		text()
	case EventSkills:
		e.Msg = "skills retrieved"
		text()
	case EventToolCall:
		e.Msg = "tool call " + ev.Tool
		tool()
		if ev.Args != "" {
			e.Fields = append(e.Fields, logs.Field{Key: "args", Value: ev.Args})
		}
	case EventToolResult:
		e.Msg = "tool result " + ev.Tool
		if ev.IsErr {
			e.Msg += " failed"
			e.Level = slog.LevelError
		}
		tool()
		if ev.Result != "" {
			e.Fields = append(e.Fields, logs.Field{Key: "result", Value: ev.Result})
		}
	case EventValidation:
		e.Msg = "tool call rejected " + ev.Tool
		e.Level = slog.LevelWarn
		tool()
		text()
	case EventRetry:
		e.Msg = "tool retry " + ev.Tool
		e.Level = slog.LevelWarn
		tool()
		text()
	case EventFinal:
		e.Msg = "final response"
		text()
	case EventTruncated:
		e.Msg = "loop ceiling hit — forced synthesis"
		e.Level = slog.LevelWarn
		text()
	case EventError:
		e.Msg = "turn error"
		e.Level = slog.LevelError
		text()
	case EventCompact:
		e.Msg = "working set compacted"
		text()
	case EventInterject:
		e.Msg = "operator interjected"
		text()
	case EventToolCatalog:
		e.Msg = "tool surface ranked"
		text()
	case EventIngest:
		e.Msg = "inbound message"
		text()
		if e.Source == "" {
			if ch, _, ok := strings.Cut(ev.Text, ":"); ok {
				e.Source = logs.NormalizeSource(ch)
			}
		}
	case EventBus:
		e.Msg = "bus message"
		text()
		if e.Source == "" {
			e.Source = "subagents"
		}
	case EventApproval:
		e.Msg = "approval requested"
		e.Level = slog.LevelWarn
		text()
		if e.Source == "" {
			e.Source = "approvals"
		}
	case EventApprovalPending, EventApprovalResolved:
		e.Msg = "approval pending"
		if ev.Approval != nil {
			a := ev.Approval
			if ev.Kind == EventApprovalResolved {
				e.Msg = "approval " + strings.ReplaceAll(a.Outcome, "_", " ")
				if a.Outcome == "denied" || a.Outcome == "expired" {
					e.Level = slog.LevelWarn
				}
			}
			if a.ID != "" {
				e.Msg += " " + a.ID
			}
			for _, f := range [][2]string{{"category", a.Category}, {"action", a.Action}, {"detail", a.Detail}, {"origin", a.Origin}, {"outcome", a.Outcome}} {
				if f[1] != "" {
					e.Fields = append(e.Fields, logs.Field{Key: f[0], Value: f[1]})
				}
			}
		} else if ev.Kind == EventApprovalResolved {
			e.Msg = "approval resolved"
		}
		text()
		if e.Source == "" {
			e.Source = "approvals"
		}
	case EventAddon:
		e.Msg = ev.Text
		if ev.IsErr {
			e.Level = slog.LevelError
		}
		if e.Source == "" {
			e.Source = ev.Agent
		}
	default:
		e.Msg = string(ev.Kind)
		tool()
		text()
		if ev.IsErr {
			e.Level = slog.LevelError
		}
	}
	if e.Source == "" {
		if ev.Agent != "" {
			e.Source = "subagents"
		} else {
			e.Source = "turns"
		}
	}
	return e, true
}
