package orchestra

import (
	"context"
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
	ToolCallsByAgent  map[string]int `json:"tool_calls_by_agent,omitempty"`
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
	toolCallsByAgent map[string]int
	promptTokens     int
	completionTokens int
	estimatedCost    float64
	browsers         int
	reason           string
}

func NewGovernor(b Budgets, observe func(BudgetStatus)) *Governor {
	return &Governor{limits: b.WithDefaults(), observe: observe, toolCallsByAgent: map[string]int{}}
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

func (g *Governor) begin(parent context.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	g.mu.Lock()
	oldCancel, oldTimer, oldStop := g.cancel, g.timer, g.stopParent
	g.runID++
	g.open, g.terminated = true, false
	g.started = time.Now()
	g.activeAgents, g.agentsStarted, g.toolCalls = 0, 0, 0
	g.toolCallsByAgent = map[string]int{}
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
	ctx, st := g.runCtx, g.snapshotLocked()
	g.mu.Unlock()
	if oldTimer != nil {
		oldTimer.Stop()
	}
	if oldStop != nil {
		oldStop()
	}
	if oldCancel != nil {
		oldCancel(errRunEnded)
	}
	g.notify(st)
	return ctx
}

func (g *Governor) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

// End closes the accounting window. It is safe after termination and does not
// overwrite the authoritative termination reason.
func (g *Governor) End() {
	g.end("")
}

// EndWithReason closes the window and preserves an operator-facing reason for
// an early join/timeout cancellation.
func (g *Governor) EndWithReason(reason string) {
	g.end(reason)
}

func (g *Governor) end(reason string) {
	g.mu.Lock()
	if !g.open {
		g.mu.Unlock()
		return
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
	if !g.open {
		g.mu.Unlock()
		return nil, nil, fmt.Errorf("orchestra: no active team run")
	}
	if g.terminated {
		err := fmt.Errorf("orchestra: team run terminated: %s", g.reason)
		g.mu.Unlock()
		return nil, nil, err
	}
	if g.activeAgents >= g.limits.MaxConcurrentSubagents {
		err := &LimitError{Resource: "concurrent subagents", Used: fmt.Sprint(g.activeAgents), Limit: fmt.Sprint(g.limits.MaxConcurrentSubagents)}
		g.mu.Unlock()
		return nil, nil, err
	}
	if g.agentsStarted >= g.limits.MaxAgentsPerRun {
		err := &LimitError{Resource: "agents per run", Used: fmt.Sprint(g.agentsStarted), Limit: fmt.Sprint(g.limits.MaxAgentsPerRun)}
		g.mu.Unlock()
		return nil, nil, err
	}
	g.activeAgents++
	g.agentsStarted++
	runID := g.runID
	ctx, cancel := context.WithCancel(g.runCtx)
	st := g.snapshotLocked()
	g.mu.Unlock()
	g.notify(st)
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

// AcquireToolCall reserves one call before dispatch. A refusal happens before
// the underlying tool is invoked.
func (g *Governor) AcquireToolCall(agentID string) error {
	g.mu.Lock()
	if !g.open {
		g.mu.Unlock()
		return nil
	}
	if g.terminated {
		err := fmt.Errorf("orchestra: team run terminated: %s", g.reason)
		g.mu.Unlock()
		return err
	}
	perTurn := g.toolCallsByAgent[agentID]
	if perTurn >= g.limits.MaxToolCallsPerTurn {
		err := &LimitError{Resource: "tool calls per turn", Used: fmt.Sprint(perTurn), Limit: fmt.Sprint(g.limits.MaxToolCallsPerTurn)}
		g.markTerminatedLocked(err.Error())
		cancel, st := g.cancel, g.snapshotLocked()
		g.mu.Unlock()
		if cancel != nil {
			cancel(err)
		}
		g.notify(st)
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
	g.toolCalls++
	g.toolCallsByAgent[agentID] = perTurn + 1
	st := g.snapshotLocked()
	g.mu.Unlock()
	g.notify(st)
	return nil
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
	if g.terminated {
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

func (g *Governor) snapshotLocked() BudgetStatus {
	byAgent := make(map[string]int, len(g.toolCallsByAgent))
	for k, v := range g.toolCallsByAgent {
		byAgent[k] = v
	}
	elapsed := time.Duration(0)
	if !g.started.IsZero() {
		elapsed = time.Since(g.started)
	}
	return BudgetStatus{
		Limits: g.limits, RunID: g.runID, Active: g.open && !g.terminated,
		StartedAt: g.started, Elapsed: elapsed, ActiveSubagents: g.activeAgents,
		AgentsStarted: g.agentsStarted, ToolCalls: g.toolCalls, ToolCallsByAgent: byAgent,
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
