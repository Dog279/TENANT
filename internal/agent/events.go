package agent

import (
	"context"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/memory/assemble"
	"tenant/internal/orchestra"
)

// EventKind classifies a live turn event emitted to a Config.Observer.
// Observability is opt-in: nil Observer ⇒ zero overhead, identical
// behavior to before. The TUI subscribes to render a live feed.
type EventKind string

const (
	EventTurnStart   EventKind = "turn_start"   // a turn began (Text = user query, ParentTraceID = the spawning turn's)
	EventTurnEnd     EventKind = "turn_end"     // a turn ended (Duration; IsErr + Text = its error when it failed)
	EventRoute       EventKind = "route"        // shadow route observation ready (Text = prompt-free summary)
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
	EventTeamBudget  EventKind = "team_budget"  // active team-run usage or a structural termination; TeamBudget + Text
	EventApproval    EventKind = "approval"     // a dangerous action is awaiting an operator decision; Text = "<action>: <detail>" — surfaced in serve mode where there is no TUI prompt (TEN-194)
	// Approval fan-out (TEN-203): every dangerous-action approval enters/leaves an
	// id-keyed queue the broker owns. Both observers (TUI + dashboard) refresh live
	// off these — Pending appends a card, Resolved prunes it. Payload in .Approval.
	EventApprovalPending  EventKind = "approval_pending"
	EventApprovalResolved EventKind = "approval_resolved"
	// EventAddon is an activity line reported by an EXTERNAL add-on (a script or
	// sidecar automation outside tenant.exe) via the dashboard's
	// POST /api/addon/event — so custom tooling shows up in the same audited
	// feed as native activity. Agent = the add-on's source name, Text = the
	// message, IsErr = the add-on flagged it as an error.
	EventAddon EventKind = "addon"
)

// ApprovalEvent is the payload for EventApprovalPending / EventApprovalResolved
// (TEN-203). Self-documenting so observers read fields instead of parsing Text.
type ApprovalEvent struct {
	ID       string // broker-assigned id ("ap-<boot>-<n>") — the key for Resolve
	Category string // safety category (exec|write|destructive|web|send|pairing)
	Action   string // the gated action id
	Detail   string // human-readable detail
	Origin   string // "local" | "imessage" | "discord" — which surface raised it
	Outcome  string // "" while pending; approved|approved_session|approved_always|denied|denied_shutdown|expired when resolved
}

// Event is one live update during a turn. Only the fields relevant to
// Kind are set.
type Event struct {
	Kind EventKind
	// TraceID correlates turn-scoped events with the durable run record.
	TraceID string
	// ParentTraceID is the trace of the turn that spawned this one
	// (EventTurnStart of a sub-agent's turn).
	ParentTraceID string
	// Duration is how long the turn (EventTurnEnd) or the tool call
	// (EventToolResult) took.
	Duration time.Duration
	// Agent attributes a CROSS-AGENT event to its source sub-agent / bus sender
	// ("" = the primary agent). Set when a sub-agent's activity or an inter-agent
	// bus message is mirrored into the shared feed so the dashboard can label it;
	// the TUI skips Agent != "" on its main channel (it renders sub-agents via the
	// separate TeamEvents channel). (TEN-234)
	Agent string
	// Source names the log-book folder (and dashboard tab) this event is filed
	// under when it is not the primary agent's own turn: the observer wrappers
	// set it ("cron", "discord", "imessage", "eval", …). "" ⇒ derived by
	// EntryFor: "subagents" when Agent is set, else "turns".
	Source string
	Iter   int    // planner-loop iteration (1-based), 0 for turn-level events
	Text   string // user query / token delta / assistant text / final / error
	Tool   string // tool name (ToolCall/ToolResult/Validation)
	Args   string // tool arguments JSON (ToolCall)
	Result string // tool result text (ToolResult)
	IsErr  bool   // tool result was an error (ToolResult)

	Budget     *assemble.BudgetReport  // EventMemory
	TeamBudget *orchestra.BudgetStatus // EventTeamBudget
	Approval   *ApprovalEvent          // EventApprovalPending / EventApprovalResolved (TEN-203)

	// EventUsage: actual tokens for one LLM call, as reported by the
	// backend (input + output). Summed by the UI for session totals.
	PromptTokens     int
	CompletionTokens int
}

func (a *Agent) emit(e Event) {
	if a.cfg.Observer != nil {
		if t := a.turnTrace.Load(); t != nil && e.TraceID == "" {
			e.TraceID = *t
		}
		a.cfg.Observer(e)
	}
}

// ev files the turns events written under ctx in this agent's log: its
// surface's (subagents, cron, …), as its logger names it (logs.SourceOf).
func (a *Agent) ev(ctx context.Context) context.Context { return event.WithLog(ctx, Turns, a.evlog) }

// Turns is the agent loop's event provider. Its log is turns, but a turn's
// events are filed in the log of the surface that runs it, as its v1 lines
// always were: a sub-agent's in subagents, a cron job's in cron. EntryFor
// tags the live events (the rows the dashboard's Activity tab shows) with
// these IDs; the agent writes the rest itself.
var Turns = event.NewProvider("turns", "turns")

// The turn itself (lifecycle), as an activity whose correlation is its trace.

var TurnRun = Turns.Event(1, event.Info, "Turn", "Turn", "agent", "text", "duration", "error").Sensitive("text").
	Doc("A turn started (text is the message), or ended: a stop or a fail, with its duration. A sub-agent's turn names the turn that spawned it as related.",
		"A message reached the agent: typed in the TUI or the dashboard, relayed from a channel, or sent by a job or a peer. A turn fails when its model can't be reached, its context can't be assembled, or its tool calls keep failing validation.",
		"None when it stops. When it fails, read its error, then the turn's other events (filter trace:<id>).")

// Operations (1000s): what a turn does.

var RouteSeen = Turns.Event(1001, event.Info, "Routing", "route observation", "text").
	Doc("The route classifier's view of the turn's message: the role it would pick, and whether it applied.",
		"Routing is configured (route in config.json); every turn is observed, in shadow or active mode.",
		"None. The route log has the decision's detail.")

var SkillsUsed = Turns.Event(1002, event.Info, "Skills", "skills retrieved", "text").
	Doc("Saved skills matched the message and were added to the turn's prompt (text names them).",
		"The skill library holds recipes close to what was asked.",
		"None. A skill that keeps matching the wrong requests can be edited or deleted with /skills.")

var ToolsRanked = Turns.Event(1003, event.Info, "Tools", "tool surface ranked", "text").
	Doc("Which tools the turn offers the model: all of them, or the top ranked by the message, or a tool hidden by its capability gate.",
		"The tool catalog is large enough for ranking, or a profile gate dropped a tool.",
		"None. If a tool the task needs is missing, check /tools and the profile's capabilities.")

var Interjected = Turns.Event(1004, event.Info, "Turn", "operator interjected", "iter", "text").Sensitive("text").
	Doc("The operator sent a message while the turn was running; the agent folded it in.",
		"A message typed mid-turn in the TUI or the dashboard.",
		"None.")

var FinalAnswer = Turns.Event(1005, event.Info, "Turn", "final response", "agent", "iter", "text").Sensitive("text").
	Doc("The turn's answer is ready.",
		"The model answered without calling another tool.",
		"None.")

var ToolDone = Turns.Event(1101, event.Info, "Tool call", "Tool {tool} finished in {duration}", "agent", "iter", "tool", "duration").
	Doc("A tool call returned. Its arguments and result are in the diagnostic channel (turns 9001 and 9002).",
		"The model called a tool.",
		"None. A tool that is often slow shows here first.")

var Compacted = Turns.Event(1201, event.Info, "Compaction", "working set compacted", "text").
	Doc("The conversation's working set was shortened (text: messages before and after): old tool results elided and older turns summarized.",
		"The working set passed its budget's high-water mark, the context overflowed mid-turn, or the operator ran /compress.",
		"None. Compaction that repeats every turn means the context budget is small for the conversation.")

// Warnings (2000s).

var ToolRejected = Turns.Event(2001, event.Warning, "Validation", "tool call rejected {tool}", "agent", "iter", "tool", "text").
	Doc("A tool call failed validation before it ran: an unknown tool, arguments that aren't JSON, or a missing or mistyped argument (text). The model gets the error and may retry.",
		"The model called the tool wrongly: common with small local models, or after a tool's schema changed.",
		"None when it recovers. If the turn stops with too many validation failures, try a stronger model for the planner role.")

var ToolRetried = Turns.Event(2002, event.Warning, "Retry", "tool retry {tool}", "agent", "iter", "tool", "text").
	Doc("A tool call failed with a transient error and was retried (text: the attempt and the error).",
		"The tool's service was briefly unreachable, timed out, or rate-limited the request.",
		"None when a retry succeeds. Frequent retries point at the tool's service or the network.")

var Truncated = Turns.Event(2003, event.Warning, "Turn", "loop ceiling hit — forced synthesis", "agent", "text").Sensitive("text").
	Doc("The turn used all its planner iterations, or kept repeating one tool call, so the agent wrote an answer from what it had gathered (text).",
		"The task needed more steps than plan_loop_ceiling allows, or the model was stuck.",
		"Raise plan_loop_ceiling in config.json for long tasks, or use /goal; a model stuck in a loop may need a stronger planner.")

var RunRecordFailed = Turns.Event(2101, event.Warning, "Run record", "Could not {op} the run record of turn {trace_id}: {err}", "op", "trace_id", "err").
	Doc("The turn ran, but its record in runs.db was not written, so /runs and the dashboard won't show it (or show it unfinished).",
		"runs.db could not be written: the disk is full, the file is locked or damaged.",
		"Check free space and the data directory's permissions; tenant doctor checks runs.db.")

var EmbedFailed = Turns.Event(2102, event.Warning, "Retrieval", "Query embedding failed; the turn goes on without retrieval: {err}", "agent", "err").
	Doc("The message could not be embedded, so the turn runs without recalled memories, episodes or skills.",
		"The embedder role's model (often Ollama) is down, unreachable or not loaded.",
		"Check the embedder's backend (tenant doctor, the model log).")

var RoutedRoleUnavailable = Turns.Event(2103, event.Warning, "Routing", "Routed role {role} unavailable; using the planner: {err}", "agent", "role", "err").
	Doc("Active routing picked a role for the turn, but that role's model could not be resolved in time, so the turn runs on the planner.",
		"The role's backend is down or slow, or the route timeout (route.timeout_ms) is shorter than its warm-up.",
		"Check the role's backend in the model log, or raise route.timeout_ms.")

var EmptyFinal = Turns.Event(2104, event.Warning, "Turn", "The model gave an empty answer after {tools_called} tool calls; forcing a synthesis", "agent", "iter", "tools_called").
	Doc("The model stopped calling tools but wrote nothing, so the agent asked it again, with tools off, to answer from what it gathered.",
		"A thinking model's whole reply was a reasoning block, or the model gave up.",
		"None when the synthesis answers. If it happens often, try another planner model.")

var Oscillating = Turns.Event(2105, event.Warning, "Turn", "The same tool calls repeated {identical_runs} times; forcing a synthesis", "agent", "iter", "identical_runs").
	Doc("The model kept making the same tool calls, so the turn stopped looping and answered from what it had.",
		"A weak model locked onto one call, or a tool keeps returning something the model doesn't accept.",
		"Look at the repeated call's result in the turn's tool events; a stronger planner usually avoids it.")

var CompactFailed = Turns.Event(2106, event.Warning, "Compaction", "Working-set compaction failed at {stage}: {err}", "agent", "stage", "err").
	Doc("The working set could not be compacted, so the conversation stays long and the next turns may overflow the context.",
		"The compressor's model failed or timed out (stage summary), or eliding old tool results failed (stage microcompaction).",
		"Check the compressor's backend in the model log; /clear starts a fresh working set.")

var ToolHidden = Turns.Event(2107, event.Warning, "Tools", "Tool {tool} hidden: the active profile has no capability entry for gate {gate}", "tool", "gate", "profile").
	Doc("A tool was not offered to the model because its capability gate is unknown to the active profile. Written once per gate.",
		"The profile has no capabilities entry for the gate, and no built-in rule covers it.",
		"Add the gate to the profile's capabilities (allow or deny) so the choice is deliberate.")

var StreamRetry = Turns.Event(2108, event.Warning, "Retry", "Planner stream dropped; retrying on a fresh connection (attempt {attempt}): {err}", "agent", "iter", "attempt", "err").
	Doc("The planner's streamed reply broke off mid-way, so the call was sent again on a new connection.",
		"The backend recycled the connection (a GOAWAY or a reset), restarted, or the network dropped.",
		"None when the retry succeeds. Repeated drops point at the backend or a proxy in front of it.")

var RecordNotSaved = Turns.Event(2109, event.Warning, "Memory", "The turn's {record} was not saved: {err}", "agent", "record", "episode", "err").
	Doc("Part of the turn's memory was not written: an archive event (the transcript), its episode, or the episode's route tags.",
		"The memory store's database could not be written: the disk is full, or the file is locked or damaged.",
		"Check free space and the data directory; tenant doctor checks the stores.")

var TurnRefused = Turns.Event(2201, event.Warning, "Turn gate", "Turn from {source} refused: {err}", "source", "err").
	Doc("A message was not run because the shared agent's turn queue had no room for it.",
		"The queue is full, or this surface has used its share of it: turns are arriving faster than the agent finishes them.",
		"Wait for the running turns to finish. A queue that stays full means a stuck turn (see the dashboard's status) or too many jobs at once.")

// Errors (3000s).

var TurnError = Turns.Event(3001, event.Error, "Turn", "turn error", "agent", "iter", "text").
	Doc("The turn hit an error (text): a planner call failed, the context overflowed, or its forced synthesis failed. Some are recovered from: an overflow is compacted and retried.",
		"The model's backend failed or refused the request, or the conversation outgrew the model's context.",
		"Read the error; the model log usually has the backend's side. A turn that ends on it also has a turn fail (turns 1).")

var ToolFailed = Turns.Event(3101, event.Error, "Tool call", "Tool {tool} failed after {duration}", "agent", "iter", "tool", "duration", "error").
	Doc("A tool call returned an error. The model gets the error and usually tries something else; the result is in the diagnostic channel (turns 9003), and error holds a failure outside the tool itself.",
		"The tool refused its arguments, its service failed, or the tool is broken.",
		"None when the turn recovers. A tool that fails every time needs its configuration or its service checked.")

var RoutePanicked = Turns.Event(3102, event.Error, "Routing", "Route {hook} panicked; {fallback}", "agent", "hook", "fallback", "panic", "stack").
	Doc("The route classifier's code panicked. The turn went on without it.",
		"A bug in the route classifier or its policy.",
		"Report it with the stack; turn routing off (route in config.json) until it is fixed.")

var ToolPanicked = Turns.Event(3103, event.Critical, "Tool call", "Tool {tool} panicked: {panic}", "tool", "panic", "stack").
	Doc("A tool's code panicked. Only that call failed: the model got an error result and the turn went on.",
		"A bug in the tool or its plugin.",
		"Report it with the stack, and disable the tool (/disable) if it keeps happening.")

// Diagnostics (9000s): the tool payloads.

var ToolCallArgs = Turns.Event(9001, event.Info, "Tool call", "tool call {tool}", "agent", "iter", "tool", "args").Sensitive("args").Diagnostic().
	Doc("A tool call about to run, with its arguments.",
		"The model called a tool.",
		"None.")

var ToolResultBody = Turns.Event(9002, event.Info, "Tool call", "tool result {tool}", "agent", "iter", "tool", "duration", "result").Sensitive("result").Diagnostic().
	Doc("A tool call's result, as the model got it (before the result cap).",
		"A tool call returned.",
		"None.")

var ToolFailedBody = Turns.Event(9003, event.Error, "Tool call", "tool result {tool} failed", "agent", "iter", "tool", "duration", "result").Sensitive("result").Diagnostic().
	Doc("A failed tool call's result: the error the model got.",
		"A tool call returned an error.",
		"See turns 3101.")
