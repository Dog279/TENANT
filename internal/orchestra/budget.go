package orchestra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Budgets is the operator-configurable hard resource envelope for one agent
// team run. Zero values are replaced by DefaultBudgets; this keeps old config
// files safe while allowing every limit to be overridden in config.json.
//
// Cost is estimated from backend-reported token usage and the configured
// per-million-token rates. Rates deliberately live beside the cap because
// many Tenant deployments use private endpoints whose prices are not public.
// A negative rate explicitly means free/local; zero means use the default.
type Budgets struct {
	MaxConcurrentSubagents int           `json:"max_concurrent_subagents,omitempty"`
	MaxAgentsPerRun        int           `json:"max_agents_per_run,omitempty"`
	MaxToolCallsPerTurn    int           `json:"max_tool_calls_per_turn,omitempty"`
	MaxToolCallsPerRun     int           `json:"max_tool_calls_per_run,omitempty"`
	MaxTokensPerRun        int           `json:"max_tokens_per_run,omitempty"`
	MaxCostUSD             float64       `json:"max_cost_usd,omitempty"`
	MaxWallTime            time.Duration `json:"max_wall_time,omitempty"`
	MaxBrowserInstances    int           `json:"max_browser_instances,omitempty"`
	BusHistoryCap          int           `json:"bus_history_cap,omitempty"`

	InputCostPerMillionUSD  float64 `json:"input_cost_per_million_usd,omitempty"`
	OutputCostPerMillionUSD float64 `json:"output_cost_per_million_usd,omitempty"`
}

// UnmarshalJSON reads max_wall_time as a duration ("30m", "2h") as well as
// the integer nanoseconds a time.Duration encodes to, which older files hold.
func (b *Budgets) UnmarshalJSON(data []byte) error {
	type plain Budgets
	aux := struct {
		*plain
		MaxWallTime json.RawMessage `json:"max_wall_time,omitempty"`
	}{plain: (*plain)(b)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	raw := bytes.TrimSpace(aux.MaxWallTime)
	switch {
	case len(raw) == 0 || string(raw) == "null":
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("budgets.max_wall_time: want a duration like \"30m\": %w", err)
		}
		b.MaxWallTime = d
	default:
		var ns int64
		if err := json.Unmarshal(raw, &ns); err != nil {
			return fmt.Errorf("budgets.max_wall_time: want a duration like \"30m\": %w", err)
		}
		b.MaxWallTime = time.Duration(ns)
	}
	return nil
}

// MarshalJSON writes max_wall_time as a duration a person can read and edit.
func (b Budgets) MarshalJSON() ([]byte, error) {
	type plain Budgets
	aux := struct {
		plain
		MaxWallTime string `json:"max_wall_time,omitempty"`
	}{plain: plain(b)}
	if b.MaxWallTime != 0 {
		aux.MaxWallTime = b.MaxWallTime.String()
	}
	return json.Marshal(aux)
}

// DefaultBudgets returns a conservative envelope large enough for deep
// research while preventing accidental unbounded fan-out.
func DefaultBudgets() Budgets {
	return Budgets{
		MaxConcurrentSubagents: 4,
		MaxAgentsPerRun:        16,
		MaxToolCallsPerTurn:    64,
		MaxToolCallsPerRun:     256,
		MaxTokensPerRun:        500_000,
		MaxCostUSD:             25,
		MaxWallTime:            30 * time.Minute,
		MaxBrowserInstances:    4,
		BusHistoryCap:          1_000,
		// Conservative hosted-model estimates. Operators using local/free or
		// differently-priced endpoints should override these with their rates.
		InputCostPerMillionUSD:  15,
		OutputCostPerMillionUSD: 75,
	}
}

// WithDefaults fills unset limits and rates from DefaultBudgets.
func (b Budgets) WithDefaults() Budgets {
	d := DefaultBudgets()
	if b.MaxConcurrentSubagents <= 0 {
		b.MaxConcurrentSubagents = d.MaxConcurrentSubagents
	}
	if b.MaxAgentsPerRun <= 0 {
		b.MaxAgentsPerRun = d.MaxAgentsPerRun
	}
	if b.MaxToolCallsPerTurn <= 0 {
		b.MaxToolCallsPerTurn = d.MaxToolCallsPerTurn
	}
	if b.MaxToolCallsPerRun <= 0 {
		b.MaxToolCallsPerRun = d.MaxToolCallsPerRun
	}
	if b.MaxTokensPerRun <= 0 {
		b.MaxTokensPerRun = d.MaxTokensPerRun
	}
	if b.MaxCostUSD <= 0 {
		b.MaxCostUSD = d.MaxCostUSD
	}
	if b.MaxWallTime <= 0 {
		b.MaxWallTime = d.MaxWallTime
	}
	if b.MaxBrowserInstances <= 0 {
		b.MaxBrowserInstances = d.MaxBrowserInstances
	}
	if b.BusHistoryCap <= 0 {
		b.BusHistoryCap = d.BusHistoryCap
	}
	if b.InputCostPerMillionUSD == 0 {
		b.InputCostPerMillionUSD = d.InputCostPerMillionUSD
	} else if b.InputCostPerMillionUSD < 0 {
		b.InputCostPerMillionUSD = 0
	}
	if b.OutputCostPerMillionUSD == 0 {
		b.OutputCostPerMillionUSD = d.OutputCostPerMillionUSD
	} else if b.OutputCostPerMillionUSD < 0 {
		b.OutputCostPerMillionUSD = 0
	}
	return b
}

// BudgetStatus is a point-in-time, race-free projection for the TUI,
// dashboard, event log, and tests.
type BudgetStatus struct {
	Limits            Budgets        `json:"limits"`
	RunID             uint64         `json:"run_id"`
	Active            bool           `json:"active"`
	StartedAt         time.Time      `json:"started_at,omitempty"`
	Elapsed           time.Duration  `json:"elapsed"`
	ActiveSubagents   int            `json:"active_subagents"`
	AgentsStarted     int            `json:"agents_started"`
	ToolCalls         int            `json:"tool_calls"`
	ToolCallsByTurn   map[string]int `json:"tool_calls_by_turn,omitempty"`
	PromptTokens      int            `json:"prompt_tokens"`
	CompletionTokens  int            `json:"completion_tokens"`
	Tokens            int            `json:"tokens"`
	EstimatedCostUSD  float64        `json:"estimated_cost_usd"`
	BrowserInstances  int            `json:"browser_instances"`
	TerminationReason string         `json:"termination_reason,omitempty"`
}

func (s BudgetStatus) String() string {
	state := "active"
	if !s.Active {
		state = "idle"
	}
	if s.TerminationReason != "" {
		state = "terminated: " + s.TerminationReason
	}
	return fmt.Sprintf("team budget run=%d %s; agents=%d/%d active=%d/%d tools=%d/%d tokens=%d/%d cost=$%.4f/$%.2f browsers=%d/%d wall=%s/%s",
		s.RunID, state, s.AgentsStarted, s.Limits.MaxAgentsPerRun,
		s.ActiveSubagents, s.Limits.MaxConcurrentSubagents,
		s.ToolCalls, s.Limits.MaxToolCallsPerRun, s.Tokens, s.Limits.MaxTokensPerRun,
		s.EstimatedCostUSD, s.Limits.MaxCostUSD, s.BrowserInstances,
		s.Limits.MaxBrowserInstances, s.Elapsed.Round(time.Second), s.Limits.MaxWallTime)
}

// LimitError is returned when the runtime structurally refuses work.
type LimitError struct {
	Resource string
	Used     string
	Limit    string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("orchestra: %s budget exhausted (usage %s, limit %s)", e.Resource, e.Used, e.Limit)
}

var errRunEnded = errors.New("orchestra: team run ended")

// Governor owns one team run's resource accounting and cancellation root.
// Every spawned agent context descends from runCtx, so terminating a run
// propagates cancellation through the whole team tree.
type Governor struct {
	beginMu    sync.Mutex
	mu         sync.Mutex
	limits     Budgets
	observe    func(BudgetStatus)
	runID      uint64
	open       bool
	terminated bool
	started    time.Time
	runCtx     context.Context
	cancel     context.CancelCauseFunc
	timer      *time.Timer
	stopParent func() bool

	activeAgents     int
	agentsStarted    int
	toolCalls        int
	toolCallsByTurn  map[string]int
	promptTokens     int
	completionTokens int
	estimatedCost    float64
	browsers         int
	reason           string
}

func NewGovernor(b Budgets, observe func(BudgetStatus)) *Governor {
	return &Governor{limits: b.WithDefaults(), observe: observe, toolCallsByTurn: map[string]int{}}
}

func (g *Governor) Limits() Budgets { g.mu.Lock(); defer g.mu.Unlock(); return g.limits }

// Begin starts a fresh run, cancelling any prior open run first.
func (g *Governor) Begin(parent context.Context) context.Context {
	g.beginMu.Lock()
	defer g.beginMu.Unlock()
	return g.begin(parent)
}

// Ensure begins a run only when none is open. It serializes simultaneous
// spawn_agent calls so parallel fan-out cannot reset/cancel its siblings.
func (g *Governor) Ensure(parent context.Context) context.Context {
	g.beginMu.Lock()
	defer g.beginMu.Unlock()
	g.mu.Lock()
	if g.open {
		ctx := g.runCtx
		g.mu.Unlock()
		return ctx
	}
	g.mu.Unlock()
	return g.begin(parent)
}

// EnsureAgent is Ensure and AcquireAgent in one step, under the lock
// EndIfIdle decides under: the run a spawn opens or joins can't be ended
// between the two, as another turn's team_await did (TEN-326). A run that
// was terminated and has no agent left is over: the spawn opens a fresh one
// rather than being refused until somebody awaits.
func (g *Governor) EnsureAgent(parent context.Context) (context.Context, func(), error) {
	g.beginMu.Lock()
	g.mu.Lock()
	retire, opened := func() {}, !g.open || g.overLocked()
	var began BudgetStatus
	if opened {
		retire = g.beginLocked(parent)
		began = g.snapshotLocked()
	}
	ctx, release, err := g.acquireLocked()
	st := g.snapshotLocked()
	g.mu.Unlock()
	g.beginMu.Unlock()
	retire()
	if opened {
		g.notify(began)
	}
	if err == nil {
		g.notify(st)
	}
	return ctx, release, err
}

func (g *Governor) begin(parent context.Context) context.Context {
	g.mu.Lock()
	retire := g.beginLocked(parent)
	ctx, st := g.runCtx, g.snapshotLocked()
	g.mu.Unlock()
	retire()
	g.notify(st)
	return ctx
}

// beginLocked starts a fresh run under g.mu. retire stops the old run's
// timers and cancels its context; the caller runs it once g.mu is let go.
func (g *Governor) beginLocked(parent context.Context) (retire func()) {
	if parent == nil {
		parent = context.Background()
	}
	oldCancel, oldTimer, oldStop := g.cancel, g.timer, g.stopParent
	g.runID++
	g.open, g.terminated = true, false
	g.started = time.Now()
	g.activeAgents, g.agentsStarted, g.toolCalls = 0, 0, 0
	g.toolCallsByTurn = map[string]int{}
	g.promptTokens, g.completionTokens, g.estimatedCost = 0, 0, 0
	g.reason = ""
	g.runCtx, g.cancel = context.WithCancelCause(parent)
	runID := g.runID
	g.timer = time.AfterFunc(g.limits.MaxWallTime, func() {
		g.terminateRun(runID, fmt.Sprintf("wall time limit exceeded (%s)", g.limits.MaxWallTime))
	})
	g.stopParent = context.AfterFunc(parent, func() {
		if err := context.Cause(parent); err != nil {
			g.terminateRun(runID, "parent context canceled: "+err.Error())
		}
	})
	return func() {
		if oldTimer != nil {
			oldTimer.Stop()
		}
		if oldStop != nil {
			oldStop()
		}
		if oldCancel != nil {
			oldCancel(errRunEnded)
		}
	}
}

func (g *Governor) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

// End closes the accounting window. It is safe after termination and does not
// overwrite the authoritative termination reason.
func (g *Governor) End() {
	g.end("", false)
}

// EndWithReason closes the window and preserves an operator-facing reason for
// an early join/timeout cancellation.
func (g *Governor) EndWithReason(reason string) {
	g.end(reason, false)
}

// EndIfIdle closes the window only when no agent holds a slot, deciding under
// the lock that admits them, so an agent admitted meanwhile keeps the run
// open. team_await ends a run this way once its children have joined: a
// Status check, then End, let another turn's spawn join the run in between,
// and ended it under that spawn (TEN-326). It reports whether it closed it.
func (g *Governor) EndIfIdle() bool {
	return g.end("", true)
}

func (g *Governor) end(reason string, ifIdle bool) bool {
	g.mu.Lock()
	if !g.open || ifIdle && g.activeAgents > 0 {
		g.mu.Unlock()
		return false
	}
	g.open = false
	if reason != "" && g.reason == "" {
		g.terminated, g.reason = true, reason
	}
	cancel, timer, stop := g.cancel, g.timer, g.stopParent
	g.timer, g.stopParent = nil, nil
	st := g.snapshotLocked()
	g.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cause := error(errRunEnded)
		if reason != "" {
			cause = errors.New(reason)
		}
		cancel(cause)
	}
	g.notify(st)
	return true
}

func (g *Governor) Status() BudgetStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snapshotLocked()
}

// AcquireAgent reserves both the total-run and concurrent-agent slots and
// returns a child of the run cancellation context.
func (g *Governor) AcquireAgent() (context.Context, func(), error) {
	g.mu.Lock()
	ctx, release, err := g.acquireLocked()
	st := g.snapshotLocked()
	g.mu.Unlock()
	if err == nil {
		g.notify(st)
	}
	return ctx, release, err
}

// acquireLocked is AcquireAgent under g.mu.
func (g *Governor) acquireLocked() (context.Context, func(), error) {
	if !g.open {
		return nil, nil, fmt.Errorf("orchestra: no active team run")
	}
	if g.terminated {
		return nil, nil, fmt.Errorf("orchestra: team run terminated: %s", g.reason)
	}
	if g.activeAgents >= g.limits.MaxConcurrentSubagents {
		return nil, nil, &LimitError{Resource: "concurrent subagents", Used: fmt.Sprint(g.activeAgents), Limit: fmt.Sprint(g.limits.MaxConcurrentSubagents)}
	}
	if g.agentsStarted >= g.limits.MaxAgentsPerRun {
		return nil, nil, &LimitError{Resource: "agents per run", Used: fmt.Sprint(g.agentsStarted), Limit: fmt.Sprint(g.limits.MaxAgentsPerRun)}
	}
	g.activeAgents++
	g.agentsStarted++
	runID := g.runID
	ctx, cancel := context.WithCancel(g.runCtx)
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			g.mu.Lock()
			if g.runID == runID && g.activeAgents > 0 {
				g.activeAgents--
			}
			st := g.snapshotLocked()
			g.mu.Unlock()
			g.notify(st)
		})
	}
	return ctx, release, nil
}

// AcquireToolCall reserves one call of turn, the turn making it, before
// dispatch. A refusal happens before the underlying tool is invoked. The
// run's cap ends the run; a turn past its per-turn cap is refused alone, and
// the run, every other turn and every sub-agent in it go on (TEN-326). ctx
// is the call's: when it ends, so does the turn's count, so the counts are
// the live turns'.
func (g *Governor) AcquireToolCall(ctx context.Context, turn string) error {
	g.mu.Lock()
	if !g.open || g.overLocked() {
		g.mu.Unlock()
		return nil
	}
	if g.terminated {
		err := fmt.Errorf("orchestra: team run terminated: %s", g.reason)
		g.mu.Unlock()
		return err
	}
	if g.toolCalls >= g.limits.MaxToolCallsPerRun {
		err := &LimitError{Resource: "tool calls per run", Used: fmt.Sprint(g.toolCalls), Limit: fmt.Sprint(g.limits.MaxToolCallsPerRun)}
		g.markTerminatedLocked(err.Error())
		cancel, st := g.cancel, g.snapshotLocked()
		g.mu.Unlock()
		if cancel != nil {
			cancel(err)
		}
		g.notify(st)
		return err
	}
	perTurn, counted := g.toolCallsByTurn[turn]
	if perTurn >= g.limits.MaxToolCallsPerTurn {
		g.mu.Unlock()
		return &LimitError{Resource: "tool calls per turn", Used: fmt.Sprint(perTurn), Limit: fmt.Sprint(g.limits.MaxToolCallsPerTurn)}
	}
	g.toolCalls++
	g.toolCallsByTurn[turn] = perTurn + 1
	runID, st := g.runID, g.snapshotLocked()
	g.mu.Unlock()
	if !counted {
		context.AfterFunc(ctx, func() { g.releaseTurn(runID, turn) })
	}
	g.notify(st)
	return nil
}

// releaseTurn drops a finished turn's count, unless a later run has replaced
// the counts it was in (begin resets them).
func (g *Governor) releaseTurn(runID uint64, turn string) {
	g.mu.Lock()
	if g.runID != runID {
		g.mu.Unlock()
		return
	}
	delete(g.toolCallsByTurn, turn)
	st := g.snapshotLocked()
	g.mu.Unlock()
	g.notify(st)
}

// RecordUsage accounts one completed model call. Token and cost exhaustion
// cancel the run before another model/tool step can begin.
func (g *Governor) RecordUsage(prompt, completion int) error {
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	g.mu.Lock()
	if !g.open || g.terminated {
		g.mu.Unlock()
		return nil
	}
	g.promptTokens += prompt
	g.completionTokens += completion
	g.estimatedCost += float64(prompt)*g.limits.InputCostPerMillionUSD/1_000_000 +
		float64(completion)*g.limits.OutputCostPerMillionUSD/1_000_000
	total := g.promptTokens + g.completionTokens
	var err error
	if total > g.limits.MaxTokensPerRun {
		err = &LimitError{Resource: "tokens per run", Used: fmt.Sprint(total), Limit: fmt.Sprint(g.limits.MaxTokensPerRun)}
	} else if g.estimatedCost > g.limits.MaxCostUSD {
		err = &LimitError{Resource: "estimated cost per run", Used: fmt.Sprintf("$%.4f", g.estimatedCost), Limit: fmt.Sprintf("$%.2f", g.limits.MaxCostUSD)}
	}
	if err != nil {
		g.markTerminatedLocked(err.Error())
	}
	cancel, st := g.cancel, g.snapshotLocked()
	g.mu.Unlock()
	if err != nil && cancel != nil {
		cancel(err)
	}
	g.notify(st)
	return err
}

// AcquireBrowser reserves a browser process slot before Chrome is launched.
func (g *Governor) AcquireBrowser() (func(), error) {
	g.mu.Lock()
	if g.terminated && !g.overLocked() {
		err := fmt.Errorf("orchestra: team run terminated: %s", g.reason)
		g.mu.Unlock()
		return nil, err
	}
	if g.browsers >= g.limits.MaxBrowserInstances {
		err := &LimitError{Resource: "browser instances", Used: fmt.Sprint(g.browsers), Limit: fmt.Sprint(g.limits.MaxBrowserInstances)}
		g.mu.Unlock()
		return nil, err
	}
	g.browsers++
	st := g.snapshotLocked()
	g.mu.Unlock()
	g.notify(st)
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.browsers > 0 {
				g.browsers--
			}
			st := g.snapshotLocked()
			g.mu.Unlock()
			g.notify(st)
		})
	}, nil
}

func (g *Governor) terminateRun(runID uint64, reason string) {
	g.mu.Lock()
	if g.runID != runID || !g.open || g.terminated {
		g.mu.Unlock()
		return
	}
	g.markTerminatedLocked(reason)
	cancel, st := g.cancel, g.snapshotLocked()
	g.mu.Unlock()
	if cancel != nil {
		cancel(errors.New(reason))
	}
	g.notify(st)
}

func (g *Governor) markTerminatedLocked(reason string) { g.terminated, g.reason = true, reason }

// overLocked reports a run that was terminated and has no agent left in it:
// it refuses nothing more (tool calls, browsers), and the next spawn opens a
// fresh run in its place. A terminated run still winding its agents down
// refuses (TEN-326).
func (g *Governor) overLocked() bool { return g.terminated && g.activeAgents == 0 }

func (g *Governor) snapshotLocked() BudgetStatus {
	byTurn := make(map[string]int, len(g.toolCallsByTurn))
	for k, v := range g.toolCallsByTurn {
		byTurn[k] = v
	}
	elapsed := time.Duration(0)
	if !g.started.IsZero() {
		elapsed = time.Since(g.started)
	}
	return BudgetStatus{
		Limits: g.limits, RunID: g.runID, Active: g.open && !g.terminated,
		StartedAt: g.started, Elapsed: elapsed, ActiveSubagents: g.activeAgents,
		AgentsStarted: g.agentsStarted, ToolCalls: g.toolCalls, ToolCallsByTurn: byTurn,
		PromptTokens: g.promptTokens, CompletionTokens: g.completionTokens,
		Tokens: g.promptTokens + g.completionTokens, EstimatedCostUSD: g.estimatedCost,
		BrowserInstances: g.browsers, TerminationReason: g.reason,
	}
}

func (g *Governor) notify(st BudgetStatus) {
	if g.observe != nil {
		g.observe(st)
	}
}
