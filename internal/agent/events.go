package agent

import (
	"tenant/internal/memory/assemble"
)

// EventKind classifies a live turn event emitted to a Config.Observer.
// Observability is opt-in: nil Observer ⇒ zero overhead, identical
// behavior to before. The TUI subscribes to render a live feed.
type EventKind string

const (
	EventTurnStart   EventKind = "turn_start"   // a turn began (Text = user query)
	EventMemory      EventKind = "memory"       // context assembled (Budget set)
	EventSkills      EventKind = "skills"       // T4 skills retrieved (Text = names)
	EventToken       EventKind = "token"        // streaming text delta (Text)
	EventAssistant   EventKind = "assistant"    // assistant produced full text this iter (Text)
	EventToolCall    EventKind = "tool_call"    // a tool is about to run (Tool, Args)
	EventToolResult  EventKind = "tool_result"  // a tool returned (Tool, Result, IsErr)
	EventValidation  EventKind = "validation"   // a tool call failed validation (Tool, Text)
	EventFinal       EventKind = "final"        // final response ready (Text)
	EventTruncated   EventKind = "truncated"    // loop ceiling forced synthesis (Text)
	EventError       EventKind = "error"        // turn-level error (Text)
	EventCompact     EventKind = "compact"      // working set compacted (Text = "N → M messages")
	EventUsage       EventKind = "usage"        // one LLM call's token usage (PromptTokens/CompletionTokens)
	EventInterject   EventKind = "interject"    // a mid-turn user message was folded in (Text = message)
	EventRetry       EventKind = "retry"        // transient tool failure was retried by RetryDecorator (Tool, Text="attempt N/M: <err>", IsErr=true)
	EventToolCatalog EventKind = "tool_catalog" // ranked tool surface trimmed for this turn (Text="ranked: N of M tools surfaced")
	EventIngest      EventKind = "ingest"       // an inbound message arrived from an offsite channel and drove a turn; Text = "Discord: <preview>" / "iMessage: <preview>" (TEN-232)
	EventBus         EventKind = "bus"          // an inter-agent message crossed the orchestra bus; Agent = sender, Text = "→ <to>: <content>" (TEN-234)
)

// Event is one live update during a turn. Only the fields relevant to
// Kind are set.
type Event struct {
	Kind EventKind
	// Agent attributes a CROSS-AGENT event to its source sub-agent / bus sender
	// ("" = the primary agent). Set when a sub-agent's activity or an inter-agent
	// bus message is mirrored into the shared feed so the dashboard can label it;
	// the TUI skips Agent != "" on its main channel (it renders sub-agents via the
	// separate TeamEvents channel). (TEN-234)
	Agent  string
	Iter   int    // planner-loop iteration (1-based), 0 for turn-level events
	Text   string // user query / token delta / assistant text / final / error
	Tool   string // tool name (ToolCall/ToolResult/Validation)
	Args   string // tool arguments JSON (ToolCall)
	Result string // tool result text (ToolResult)
	IsErr  bool   // tool result was an error (ToolResult)

	Budget *assemble.BudgetReport // EventMemory

	// EventUsage: actual tokens for one LLM call, as reported by the
	// backend (input + output). Summed by the UI for session totals.
	PromptTokens     int
	CompletionTokens int
}

func (a *Agent) emit(e Event) {
	if a.cfg.Observer != nil {
		a.cfg.Observer(e)
	}
}
