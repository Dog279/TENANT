package agent

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/orchestra"
)

// Retained reports whether a live event belongs in the retained activity
// record. It is a DENYLIST of per-token / accounting noise — token deltas (the
// flood), usage counts, per-iteration assistant prose, the memory-budget
// report and the thinking marks (the model log times every call; the turn's
// end carries the totals) — so every meaningful or future kind is kept by
// default.
func Retained(ev Event) bool {
	switch ev.Kind {
	case EventToken, EventUsage, EventAssistant, EventMemory, EventThinking, EventThought:
		return false
	}
	return true
}

// CronTool is how a scheduled job's tool call is named wherever it shows:
// cron:<tool>, so automated work reads apart from a turn's.
func CronTool(tool string) string { return "cron:" + tool }

// RowMsg is e's message as a log view shows it. A scheduled job's tool rows,
// the turns tool events filed in the cron log, name their tool with CronTool.
// The stored event keeps the plain name, so a tool: filter still finds them.
// Every other event shows its own message.
func RowMsg(e event.Event) string {
	if e.Log != "cron" || e.Provider != Turns.Name {
		return e.Msg
	}
	i := slices.IndexFunc(e.Data, func(f event.Field) bool { return f.Key == "tool" })
	d, ok := event.Lookup(e.Provider, e.ID)
	if i < 0 || !ok {
		return e.Msg
	}
	c := e
	c.Data = slices.Clone(e.Data)
	c.Data[i].Value = CronTool(e.Data[i].Text())
	return d.Render(&c)
}

// EntryFor renders a live turn event as one log-book line: a readable message
// ("tool call web_search", "final response", "approval denied ap-3"), the kind
// preserved in the `ev` field so the dashboard can style the row after a
// restart re-reads it from disk, the event's payload as fields, and its trace
// ID as `trace`, which joins the line to its runs.db record. ok is
// false for noise (see Retained). The folder is ev.Source when the emitter
// set one, else "subagents" for a sub-agent's activity, "approvals" for the
// approval kinds, the channel for an inbound message, the add-on's own name
// for an add-on line, and "turns" for the primary agent's own turn.
//
// The line names the catalogued event it is (event=turns/<id>, its task, and
// for a turn's start and end its opcode) so the log book reads it as one; see
// events.go. Approvals (the Security log's), inbound messages (the
// channels') and add-on lines carry none.
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
		if ev.ToolCallID != "" { // read back as the event's tool_call correlation
			e.Fields = append(e.Fields, logs.Field{Key: "tool_call", Value: ev.ToolCallID})
		}
	}
	field := func(k, v string) {
		if v != "" {
			e.Fields = append(e.Fields, logs.Field{Key: k, Value: v})
		}
	}
	duration := func() { field("duration", ev.Duration.String()) }
	var def *event.Def // the catalogued event the line is
	op := ""
	switch ev.Kind {
	case EventTurnStart:
		def, op = TurnRun, event.OpStart
		e.Msg = "turn started"
		text()
		field("related", ev.ParentTraceID)
	case EventTurnEnd:
		def, op = TurnRun, event.OpStop
		e.Msg = "turn finished"
		if ev.IsErr {
			op, e.Msg, e.Level = event.OpFail, "turn failed", slog.LevelError
			field("error", ev.Text)
		}
		duration()
		if ev.Think > 0 || ev.Tools > 0 {
			field("think", ev.Think.String())
			field("tools", ev.Tools.String())
		}
	case EventRoute:
		def = RouteSeen
		e.Msg = "route observation"
		text()
	case EventSkills:
		def = SkillsUsed
		e.Msg = "skills retrieved"
		text()
	case EventToolCall:
		def = ToolCallArgs
		e.Msg = "tool call " + ev.Tool
		tool()
		field("args", ev.Args)
	case EventToolResult:
		def = ToolResultBody
		e.Msg = "tool result " + ev.Tool
		if ev.IsErr {
			def = ToolFailedBody
			e.Msg += " failed"
			e.Level = slog.LevelError
		}
		tool()
		duration()
		field("result", ev.Result)
	case EventValidation:
		def = ToolRejected
		e.Msg = "tool call rejected " + ev.Tool
		e.Level = slog.LevelWarn
		tool()
		text()
	case EventRetry:
		def = ToolRetried
		e.Msg = "tool retry " + ev.Tool
		e.Level = slog.LevelWarn
		tool()
		text()
	case EventFinal:
		def = FinalAnswer
		e.Msg = "final response"
		text()
	case EventTruncated:
		def = Truncated
		e.Msg = "loop ceiling hit — forced synthesis"
		e.Level = slog.LevelWarn
		text()
	case EventError:
		def = TurnError
		e.Msg = "turn error"
		e.Level = slog.LevelError
		text()
	case EventCompact:
		def = Compacted
		e.Msg = "working set compacted"
		text()
	case EventInterject:
		def = Interjected
		e.Msg = "operator interjected"
		text()
	case EventToolCatalog:
		def = ToolsRanked
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
		def = orchestra.BusMessage
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
	case EventTeamBudget:
		// The team-run budget status travels as JSON so the activity API can
		// hand clients the structured value back (see dashboard restEntry).
		def = orchestra.TeamBudget
		e.Msg = "team budget"
		if st := ev.TeamBudget; st != nil && st.TerminationReason != "" {
			def = orchestra.TeamTerminated
			e.Msg = "team run terminated: " + st.TerminationReason
			e.Level = slog.LevelWarn
			field("reason", st.TerminationReason)
		}
		text()
		if st := ev.TeamBudget; st != nil {
			if b, err := json.Marshal(st); err == nil {
				e.Fields = append(e.Fields, logs.Field{Key: "team_budget", Value: string(b)})
			}
		}
		if e.Source == "" {
			e.Source = "subagents"
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
	if ev.TraceID != "" {
		e.Fields = append(e.Fields, logs.Field{Key: "trace", Value: ev.TraceID})
	}
	if e.Source == "" {
		if ev.Agent != "" {
			e.Source = "subagents"
		} else {
			e.Source = "turns"
		}
	}
	if def != nil {
		s := def.Spec()
		head := []logs.Field{{Key: "event", Value: s.Provider + "/" + strconv.FormatUint(uint64(s.ID), 10)}, {Key: "task", Value: s.Task}}
		if op != "" {
			head = append(head, logs.Field{Key: "opcode", Value: op})
		}
		e.Fields = append(head, e.Fields...)
	}
	return e, true
}
