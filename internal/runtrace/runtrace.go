// Package runtrace persists operational records for agent turns.
package runtrace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Run is the stable JSONL export shape for one turn.
type Run struct {
	TraceID          string     `json:"trace_id"`
	ParentTraceID    string     `json:"parent_trace_id,omitempty"` // the turn that spawned this one (a sub-agent's)
	AgentID          string     `json:"agent_id,omitempty"`
	SessionID        string     `json:"session_id,omitempty"`
	Source           string     `json:"source"`
	StartedAt        time.Time  `json:"started_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	Model            string     `json:"model,omitempty"`
	Provider         string     `json:"provider,omitempty"`
	LatencyMS        int64      `json:"latency_ms"`
	ThinkMS          int64      `json:"think_ms"` // of LatencyMS, the time in model calls
	ToolsMS          int64      `json:"tools_ms"` // and in tool calls (a parallel batch counts once)
	PromptTokens     int        `json:"prompt_tokens"`
	CompletionTokens int        `json:"completion_tokens"`
	CostEstimateUSD  *float64   `json:"cost_estimate_usd,omitempty"`
	Retries          int        `json:"retries"`
	Fallbacks        []Fallback `json:"fallbacks"`
	ToolCalls        []ToolCall `json:"tool_calls"`
	Approvals        []Approval `json:"approvals"`
	Outcome          string     `json:"outcome"`
	ErrorClass       string     `json:"error_class,omitempty"`
}

type Fallback struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Host      string `json:"host,omitempty"` // the fleet machine a fleet_* call worked on (TEN-531)
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Outcome   string `json:"outcome"`
}

// Approval captures both the decision and its lineage back to this run.
type Approval struct {
	ID       string `json:"id"`
	RunID    string `json:"run_id"`
	Category string `json:"category,omitempty"`
	Action   string `json:"action"`
	Detail   string `json:"detail,omitempty"`
	Origin   string `json:"origin,omitempty"`
	Outcome  string `json:"outcome"`
}

// NewID mints a random 128-bit correlation id without adding a UUID dependency.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failure is exceptionally rare. Preserve uniqueness within a
		// process rather than preventing the turn from running.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

type spanKey struct{}

// Span is the concurrency-safe, in-flight detail collector stored on a turn context.
type Span struct {
	mu                  sync.Mutex
	traceID             string
	model, provider     string
	prompt, completion  int
	retries             int
	thinkTime, toolTime time.Duration
	fallbacks           []Fallback
	tools               []ToolCall
	approvals           []Approval
}

func NewSpan(traceID string) *Span { return &Span{traceID: traceID} }

func WithSpan(ctx context.Context, span *Span) context.Context {
	return context.WithValue(ctx, spanKey{}, span)
}

func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	span, _ := ctx.Value(spanKey{}).(*Span)
	return span
}

func IDFromContext(ctx context.Context) string { return SpanFromContext(ctx).TraceID() }

// TraceID is the span's trace ID ("" for a nil span).
func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return s.traceID
}

func (s *Span) SetModel(model, provider string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.model, s.provider = model, provider
	s.mu.Unlock()
}

func (s *Span) AddUsage(prompt, completion int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.prompt += prompt
	s.completion += completion
	s.mu.Unlock()
}

func (s *Span) AddRetry() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.retries++
	s.mu.Unlock()
}

// AddThink adds one model call's time to the turn's thinking.
func (s *Span) AddThink(d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.thinkTime += d
	s.mu.Unlock()
}

// AddToolTime adds one batch of tool calls' wall time. A batch's calls run in
// parallel, so it is the batch's time, not the sum of its calls'.
func (s *Span) AddToolTime(d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.toolTime += d
	s.mu.Unlock()
}

// Phases is the turn's time so far in model calls and in tool calls.
func (s *Span) Phases() (think, tools time.Duration) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thinkTime, s.toolTime
}

func (s *Span) AddFallback(from, to, reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.fallbacks = append(s.fallbacks, Fallback{From: from, To: to, Reason: reason})
	s.mu.Unlock()
}

func (s *Span) AddTool(call ToolCall) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.tools = append(s.tools, call)
	s.mu.Unlock()
}

// AddApproval upserts by approval id so pending and resolved events form one lineage entry.
func (s *Span) AddApproval(a Approval) {
	if s == nil {
		return
	}
	a.RunID = s.traceID
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.approvals {
		if s.approvals[i].ID == a.ID {
			s.approvals[i] = a
			return
		}
	}
	s.approvals = append(s.approvals, a)
}

// Apply copies a consistent snapshot into r.
func (s *Span) Apply(r *Run) {
	if s == nil || r == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Model, r.Provider = s.model, s.provider
	r.PromptTokens, r.CompletionTokens = s.prompt, s.completion
	r.Retries = s.retries
	r.ThinkMS, r.ToolsMS = s.thinkTime.Milliseconds(), s.toolTime.Milliseconds()
	r.Fallbacks = append([]Fallback(nil), s.fallbacks...)
	r.ToolCalls = append([]ToolCall(nil), s.tools...)
	r.Approvals = append([]Approval(nil), s.approvals...)
}
