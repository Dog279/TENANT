package route

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fsperm"
	"tenant/internal/runtrace"
)

const (
	DecisionLogName = "route-decisions.jsonl"
	MaxPromptTokens = 3840
	ModeShadow      = "shadow"
	ModeActive      = "active"
	ModeEverywhere  = "everywhere"
	ModeOff         = "off"

	StatusOK      = "ok"
	StatusSkipped = "skipped"
	StatusError   = "error"

	defaultHistoryLimit = 100
)

// Decision is the flattened JSONL and in-memory audit record. PromptHash is
// the only prompt identity retained; the typed prompt is never stored here.
type Decision struct {
	ID            string    `json:"id,omitempty"`
	Timestamp     time.Time `json:"ts"`
	AgentID       string    `json:"agent_id"`
	SessionID     string    `json:"session_id"`
	TraceID       string    `json:"trace_id,omitempty"` // the turn that asked (runs.db)
	PromptHash    string    `json:"prompt_hash"`
	EstTokens     int       `json:"est_tokens"`
	Domain        string    `json:"domain"`
	Complexity    int       `json:"complexity"`
	Math          bool      `json:"math"`
	Code          bool      `json:"code"`
	Label         string    `json:"label"`
	Justification string    `json:"justification"`
	Raw           string    `json:"raw"`
	LatencyMS     int64     `json:"latency_ms"`
	Endpoint      string    `json:"endpoint,omitempty"` // the classifier asked: its host (TEN-340)
	Model         string    `json:"model,omitempty"`    // and its model
	Mode          string    `json:"mode"`
	Status        string    `json:"status"`
	Error         string    `json:"error"`
	OutcomeHint   string    `json:"outcome_hint"`

	// Phase-two policy outcome. Label remains Supra's raw binary verdict;
	// SelectedTier is the code-owned result after clamps.
	SelectedTier        Tier         `json:"selected_tier,omitempty"`
	SelectedRole        string       `json:"selected_role,omitempty"`
	SelectedProfileID   string       `json:"selected_profile_id,omitempty"`
	PolicyReason        string       `json:"policy_reason,omitempty"`
	PolicyClamped       bool         `json:"policy_clamped,omitempty"`
	PrivacyPierced      bool         `json:"privacy_pierced,omitempty"`
	ClassifierAttempted bool         `json:"classifier_attempted,omitempty"`
	ShadowDisagreement  bool         `json:"shadow_disagreement,omitempty"`
	BreakerState        BreakerState `json:"breaker_state,omitempty"`
	BreakerTripped      bool         `json:"breaker_tripped,omitempty"`
	AssistedSuggestion  bool         `json:"assisted_suggestion,omitempty"`
	OperatorConfirmed   bool         `json:"operator_confirmed,omitempty"`

	// Phase-four fields (TEN-292). Surface records which caller class drove
	// the turn so the dataset export can honor the TEN-256 relay exclusion.
	// RuleRole is set when an N-way rule overrode the tier's role; EffortHint
	// is the per-request reasoning override emitted for complexity-5 prompts.
	Surface    string `json:"surface,omitempty"`
	RuleRole   string `json:"rule_role,omitempty"`
	EffortHint string `json:"effort_hint,omitempty"`
}

// Signal reconstructs the classifier result carried by a successful Decision.
func (d Decision) Signal() Signal {
	return Signal{
		Domain: d.Domain, Complexity: d.Complexity, Math: d.Math, Code: d.Code,
		Label: d.Label, Justification: d.Justification, Raw: d.Raw,
		Latency:  time.Duration(d.LatencyMS) * time.Millisecond,
		Endpoint: d.Endpoint, Model: d.Model,
	}
}

// Tags returns the stable episodic tags for this observation. Failed and
// skipped decisions still carry their shadow mode and status without inventing
// absent classifier fields.
func (d Decision) Tags() []string {
	tags := []string{"route:mode=" + d.Mode}
	if d.ID != "" {
		tags = append(tags, "route:decision="+d.ID)
	}
	if d.Label != "" {
		tags = append(tags, "route:label="+d.Label)
	}
	if d.Complexity >= 1 && d.Complexity <= 5 {
		tags = append(tags, "route:complexity="+strconv.Itoa(d.Complexity))
	}
	if d.Status != StatusOK {
		tags = append(tags, "route:status="+d.Status)
	}
	if d.SelectedTier != "" {
		tags = append(tags, "route:selected="+string(d.SelectedTier))
	}
	return tags
}

// Summary is a compact activity-feed line. Full details remain available from
// Recent or NthRecent for `/route why`.
func (d Decision) Summary() string {
	if d.Status == StatusOK {
		if d.SelectedTier != "" {
			return fmt.Sprintf("%s · %s → %s · complexity %d · %s · %dms%s",
				d.Mode, routeDash(d.Label), d.SelectedTier, d.Complexity, routeDash(d.Domain), d.LatencyMS, d.via())
		}
		return fmt.Sprintf("%s · %s · complexity %d · %s · %dms%s",
			d.Mode, d.Label, d.Complexity, d.Domain, d.LatencyMS, d.via())
	}
	if d.Status == StatusSkipped {
		if d.PolicyReason == string(ReasonBreakerOpen) {
			return fmt.Sprintf("%s · classifier skipped · breaker %s", d.Mode, d.BreakerState)
		}
		return fmt.Sprintf("%s · skipped · prompt too long (%d est. tokens)", d.Mode, d.EstTokens)
	}
	return fmt.Sprintf("%s · classifier unavailable · %dms%s", d.Mode, d.LatencyMS, d.via())
}

// via names the classifier the decision asked, for its summary: " via
// <host> (<model>)", the model without its registry path; "" when none was.
func (d Decision) via() string {
	if d.Endpoint == "" {
		return ""
	}
	return " via " + d.Endpoint + " (" + path.Base(d.Model) + ")"
}

// Engine turns classifier outcomes into best-effort durable decisions and a
// bounded newest-first diagnostic history. It is safe for concurrent turns.
type Engine struct {
	classifier Classifier
	path       string
	limit      int
	breaker    *Breaker
	now        func() time.Time

	mu      sync.Mutex
	history []Decision // chronological internally; Recent reverses a copy
	logMu   sync.Mutex // serialize whole JSONL appends without blocking history reads

	// Retention state (TEN-292), guarded by logMu with the file itself.
	retentionDays int
	lastPrune     time.Time
}

// NewEngine creates a shadow decision engine. It performs no filesystem I/O;
// a broken log path never fails or changes a decision (route 3001 reports it).
func NewEngine(classifier Classifier, dataDir string, historyLimit int) *Engine {
	if historyLimit <= 0 {
		historyLimit = defaultHistoryLimit
	}
	path := ""
	if dataDir != "" {
		path = filepath.Join(dataDir, DecisionLogName)
	}
	return &Engine{
		classifier: classifier, path: path, limit: historyLimit,
		breaker: NewBreaker(0, 0), now: time.Now,
	}
}

// Decide classifies and records one prompt. It deliberately has no error
// return: endpoint, timeout, parse, and log failures are telemetry outcomes,
// never dependencies of the agent turn.
func (e *Engine) Decide(ctx context.Context, agentID, sessionID, prompt, mode string) Decision {
	if mode == "" {
		mode = ModeShadow
	}
	d := Decision{
		Timestamp:   time.Now().UTC(),
		AgentID:     agentID,
		SessionID:   sessionID,
		TraceID:     runtrace.IDFromContext(ctx),
		PromptHash:  HashPrompt(prompt),
		EstTokens:   EstimateTokens(prompt),
		Mode:        mode,
		Status:      StatusOK,
		OutcomeHint: "",
	}
	d.ID = newDecisionID()
	if d.EstTokens > MaxPromptTokens {
		d.Status = StatusSkipped
		d.Error = fmt.Sprintf("route: prompt estimate %d exceeds classifier limit %d", d.EstTokens, MaxPromptTokens)
		if e == nil {
			return d
		}
		return e.record(ctx, d)
	}
	if e == nil || e.classifier == nil {
		d.Status = StatusError
		d.Error = "route: classifier unavailable"
		if e == nil {
			return d
		}
		return e.record(ctx, d)
	}

	start := time.Now()
	sig, err := e.classifier.Classify(ctx, prompt)
	latency := sig.Latency
	if latency <= 0 {
		latency = time.Since(start)
	}
	d.Domain = sig.Domain
	d.Complexity = sig.Complexity
	d.Math = sig.Math
	d.Code = sig.Code
	d.Label = sig.Label
	d.Justification = sig.Justification
	d.Raw = sig.Raw
	d.LatencyMS = latency.Milliseconds()
	d.Endpoint, d.Model = sig.Endpoint, sig.Model
	if err != nil {
		d.Status = StatusError
		d.Error = err.Error()
	}
	return e.record(ctx, d)
}

// PolicyDecisionRequest is the phase-two input to DecideWithPolicy. The
// caller supplies live role/profile/locality bindings; the engine supplies the
// prompt estimate and classifier signal before applying Policy.
type PolicyDecisionRequest struct {
	AgentID   string
	SessionID string
	Prompt    string
	Mode      string
	// Surface is the caller class ("tui", "dashboard", "relay", "cron"). It is
	// audit metadata only; rollout policy already resolved it into Mode.
	Surface string
	// Assist enables operator suggestions without changing ordinary shadow
	// turns. WillRun is the tier the caller will use absent this decision; the
	// pair lets the engine audit a genuine shadow disagreement without knowing
	// about UI or model roles.
	Assist  bool
	WillRun Tier
	Input   PolicyInput
	// RefreshInput is invoked immediately before policy evaluation so callers
	// can replace role/profile/locality targets from live router state after a
	// classifier call. A panic is converted to a policy error and planner
	// fallback; routing metadata can never fail the turn.
	RefreshInput func(*PolicyInput)
	// AnnotateDecision runs on the successful path immediately before the
	// decision is recorded (TEN-292): the caller stamps N-way rule and effort
	// metadata so the durable row carries them. Panic-guarded and advisory —
	// annotation can neither change the selected target nor fail the turn.
	AnnotateDecision func(*Decision)
}

// DecideWithPolicy classifies, applies the ordered policy clamps, updates the
// circuit breaker, and records the complete decision. Its error is advisory to
// the caller: on every error the returned Decision already selects Small, so
// the turn can take today's planner path.
func (e *Engine) DecideWithPolicy(ctx context.Context, req PolicyDecisionRequest) (Decision, error) {
	if req.Mode == "" {
		req.Mode = ModeActive
	}
	in := req.Input
	if in.EstTokens <= 0 {
		in.EstTokens = EstimateTokens(req.Prompt)
	}
	d := Decision{
		Timestamp:   time.Now().UTC(),
		AgentID:     req.AgentID,
		SessionID:   req.SessionID,
		TraceID:     runtrace.IDFromContext(ctx),
		PromptHash:  HashPrompt(req.Prompt),
		EstTokens:   in.EstTokens,
		Mode:        req.Mode,
		Surface:     req.Surface,
		Status:      StatusOK,
		OutcomeHint: "",
	}
	d.ID = newDecisionID()
	if e != nil && e.now != nil {
		d.Timestamp = e.now().UTC()
	}
	if err := refreshPolicyInput(ctx, req.RefreshInput, &in); err != nil {
		d.Status = StatusError
		d.Error = err.Error()
		applyPolicyResult(&d, fallbackPolicyResult(in, ReasonClassifierFailure))
		if e != nil {
			d.BreakerState = e.breaker.State(d.Timestamp)
			d = e.record(ctx, d)
		}
		return d, err
	}

	policy := Policy{}
	// Code-owned choices do not need the auxiliary runtime and therefore
	// neither consume nor heal breaker state. In particular, operator
	// overrides and a sticky tool-bearing session must still work while the
	// classifier is down or the breaker is open.
	sticky := in.StickySession && (in.StickyTier == TierSmall || in.StickyTier == TierBig)
	if in.Override != OverrideNone || in.Force != nil || sticky || in.EstTokens > LongPromptTokens {
		result, err := policy.Apply(in)
		if err != nil {
			d.Status = StatusError
			d.Error = err.Error()
			applyPolicyResult(&d, fallbackPolicyResult(in, ReasonClassifierFailure))
			if e != nil {
				d = e.record(ctx, d)
			}
			return d, fmt.Errorf("route: policy decision: %w", err)
		}
		applyPolicyResult(&d, result)
		if e != nil {
			d.BreakerState = e.breaker.State(d.Timestamp)
			d = e.record(ctx, d)
		}
		return d, nil
	}

	if e == nil {
		err := errors.New("route: classifier unavailable")
		d.Status = StatusError
		d.Error = err.Error()
		applyPolicyResult(&d, fallbackPolicyResult(in, ReasonClassifierFailure))
		return d, err
	}
	permit, allowed := e.breaker.Acquire(d.Timestamp)
	if !allowed {
		d.Status = StatusSkipped
		d.Error = "route: classifier circuit breaker open; classification skipped"
		d.BreakerState = e.breaker.State(d.Timestamp)
		applyPolicyResult(&d, fallbackPolicyResult(in, ReasonBreakerOpen))
		return e.record(ctx, d), nil
	}
	if e.classifier == nil {
		err := errors.New("route: classifier unavailable")
		d.Status = StatusError
		d.Error = err.Error()
		d.BreakerTripped = e.breaker.RecordFailure(permit, d.Timestamp)
		d.BreakerState = e.breaker.State(d.Timestamp)
		applyPolicyResult(&d, fallbackPolicyResult(in, ReasonClassifierFailure))
		return e.record(ctx, d), err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	d.ClassifierAttempted = true
	start := e.now()
	sig, classifyErr := classifySafely(e.classifier, ctx, req.Prompt)
	finished := e.now()
	if classifyErr == nil && ctx.Err() != nil {
		classifyErr = fmt.Errorf("route: classifier deadline: %w", ctx.Err())
	}
	latency := sig.Latency
	if latency <= 0 {
		latency = finished.Sub(start)
	}
	d.Domain = sig.Domain
	d.Complexity = sig.Complexity
	d.Math = sig.Math
	d.Code = sig.Code
	d.Label = sig.Label
	d.Justification = sig.Justification
	d.Raw = sig.Raw
	d.LatencyMS = latency.Milliseconds()
	d.Endpoint, d.Model = sig.Endpoint, sig.Model
	in.Signal = sig
	if classifyErr != nil {
		d.Status = StatusError
		d.Error = classifyErr.Error()
		d.BreakerTripped = e.breaker.RecordFailure(permit, finished)
		d.BreakerState = e.breaker.State(finished)
		applyPolicyResult(&d, fallbackPolicyResult(in, ReasonClassifierFailure))
		d = e.record(ctx, d)
		return d, fmt.Errorf("route: classify prompt: %w", classifyErr)
	}
	if err := refreshPolicyInput(ctx, req.RefreshInput, &in); err != nil {
		d.Status = StatusError
		d.Error = err.Error()
		e.breaker.RecordSuccess(permit) // classifier succeeded; caller metadata failed
		d.BreakerState = e.breaker.State(e.now())
		applyPolicyResult(&d, fallbackPolicyResult(in, ReasonClassifierFailure))
		d = e.record(ctx, d)
		return d, err
	}

	result, policyErr := policy.Apply(in)
	if policyErr != nil {
		d.Status = StatusError
		d.Error = policyErr.Error()
		if errors.Is(policyErr, ErrInvalidClassifierLabel) {
			// A malformed short-prompt label is a classifier failure even if a
			// permissive fake returned nil error.
			d.BreakerTripped = e.breaker.RecordFailure(permit, e.now())
		} else {
			// Caller-owned metadata was bad, not the auxiliary runtime.
			e.breaker.RecordSuccess(permit)
		}
		d.BreakerState = e.breaker.State(e.now())
		applyPolicyResult(&d, fallbackPolicyResult(in, ReasonClassifierFailure))
		d = e.record(ctx, d)
		return d, fmt.Errorf("route: apply policy: %w", policyErr)
	}
	e.breaker.RecordSuccess(permit)
	d.BreakerState = e.breaker.State(e.now())
	applyPolicyResult(&d, result)
	markAssistedSuggestion(&d, req)
	annotateSafely(ctx, req.AnnotateDecision, &d)
	return e.record(ctx, d), nil
}

// annotateSafely lets the caller stamp advisory metadata (rule role, effort
// hint) onto the row about to be recorded. The selected target is restored
// afterwards so a buggy annotator cannot re-route the turn through the back
// door, and a panic degrades to "no annotation".
func annotateSafely(ctx context.Context, annotate func(*Decision), d *Decision) {
	if annotate == nil || d == nil {
		return
	}
	tier, role, profileID := d.SelectedTier, d.SelectedRole, d.SelectedProfileID
	reason, clamped, pierced := d.PolicyReason, d.PolicyClamped, d.PrivacyPierced
	defer func() {
		if v := recover(); v != nil {
			Panicked.Log(ctx, "stage", "decision annotator", "panic", v, "stack", string(debug.Stack()))
		}
		d.SelectedTier, d.SelectedRole, d.SelectedProfileID = tier, role, profileID
		d.PolicyReason, d.PolicyClamped, d.PrivacyPierced = reason, clamped, pierced
	}()
	annotate(d)
}

func markAssistedSuggestion(d *Decision, req PolicyDecisionRequest) {
	if d == nil || !req.Assist || req.Mode != ModeShadow ||
		d.Status != StatusOK || !d.ClassifierAttempted ||
		(req.WillRun != TierSmall && req.WillRun != TierBig) {
		return
	}
	d.AssistedSuggestion = d.SelectedTier != "" && d.SelectedTier != req.WillRun
}

func classifySafely(classifier Classifier, ctx context.Context, prompt string) (sig Signal, err error) {
	defer func() {
		if v := recover(); v != nil {
			Panicked.Log(ctx, "stage", "classifier", "panic", v, "stack", string(debug.Stack()))
			err = fmt.Errorf("route: classifier panic: %v", v)
		}
	}()
	return classifier.Classify(ctx, prompt)
}

func refreshPolicyInput(ctx context.Context, refresh func(*PolicyInput), in *PolicyInput) (err error) {
	if refresh == nil {
		return nil
	}
	defer func() {
		if v := recover(); v != nil {
			Panicked.Log(ctx, "stage", "policy input refresh", "panic", v, "stack", string(debug.Stack()))
			err = fmt.Errorf("route: refresh policy input: %v", v)
		}
	}()
	refresh(in)
	return nil
}

func applyPolicyResult(d *Decision, result PolicyResult) {
	if d == nil {
		return
	}
	d.SelectedTier = result.Tier
	d.SelectedRole = string(result.Target.Role)
	d.SelectedProfileID = result.Target.ProfileID
	d.PolicyReason = result.Reason
	d.PolicyClamped = result.Clamped
	d.PrivacyPierced = result.PrivacyPierced
	d.ShadowDisagreement = d.Label != "" && d.Label != string(result.Tier)
	d.OutcomeHint = fmt.Sprintf("%s via role %s/profile %s (%s)",
		result.Tier, routeDash(result.Target.Role), routeDash(result.Target.ProfileID), result.Reason)
}

func fallbackPolicyResult(in PolicyInput, reason PolicyReason) PolicyResult {
	target := withTier(in.Small, TierSmall)
	return makePolicyResult(target, in.Signal.Label, reason, true, false)
}

func routeDash[T ~string](s T) string {
	if strings.TrimSpace(string(s)) == "" {
		return "-"
	}
	return string(s)
}

// Probe exercises the classifier with a known prompt and its real parser. It
// intentionally bypasses both the JSONL sink and in-memory decision history.
func (e *Engine) Probe(ctx context.Context) (Signal, error) {
	if e == nil || e.classifier == nil {
		return Signal{}, errors.New("route: classifier unavailable")
	}
	return e.classifier.Classify(ctx, "Write a Go function that adds two integers.")
}

// Recent returns up to n decisions, newest first. n <= 0 returns the engine's
// complete bounded history.
func (e *Engine) Recent(n int) []Decision {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if n <= 0 || n > len(e.history) {
		n = len(e.history)
	}
	out := make([]Decision, n)
	for i := 0; i < n; i++ {
		out[i] = e.history[len(e.history)-1-i]
	}
	return out
}

// NthRecent returns a one-based decision for `/route why [n]` style callers.
func (e *Engine) NthRecent(n int) (Decision, bool) {
	if n < 1 {
		return Decision{}, false
	}
	recent := e.Recent(n)
	if len(recent) < n {
		return Decision{}, false
	}
	return recent[n-1], true
}

// Path is the configured JSONL location.
func (e *Engine) Path() string {
	if e == nil {
		return ""
	}
	return e.path
}

// BreakerState reports classifier availability without running a health probe.
func (e *Engine) BreakerState() BreakerState {
	if e == nil || e.breaker == nil {
		return BreakerClosed
	}
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	return e.breaker.State(now)
}

// Stats is the aggregate used by /route stats. Rates are fractions in [0,1].
type Stats struct {
	TotalDecisions int

	RoutedDecisions int
	BigDecisions    int
	BigRate         float64

	ShadowCompared         int
	ShadowDisagreements    int
	ShadowDisagreementRate float64

	ClassifierAttempts int
	ParseFailures      int
	ParseFailureRate   float64
	P50ClassifyLatency time.Duration
	P95ClassifyLatency time.Duration

	BreakerTrips int

	// Graduation is populated by StatsWithOutcomes. Stats leaves it at its
	// zero value so existing diagnostics retain their historical behavior.
	Graduation Graduation
}

// Stats computes aggregates from the complete durable JSONL log. If the log
// is unavailable it falls back to the engine's bounded in-memory history. A
// partial/corrupt tail retains the valid durable prefix; telemetry failure
// never reaches a turn.
func (e *Engine) Stats() Stats {
	decisions := e.decisionsForStats()
	return statsFromDecisions(decisions)
}

func statsFromDecisions(decisions []Decision) Stats {
	stats := Stats{TotalDecisions: len(decisions)}
	latencies := make([]int64, 0, len(decisions))
	for _, d := range decisions {
		if d.SelectedTier != "" {
			stats.RoutedDecisions++
			if d.SelectedTier == TierBig {
				stats.BigDecisions++
			}
		}
		if d.Mode == ModeShadow && d.Label != "" && d.SelectedTier != "" {
			stats.ShadowCompared++
			if d.ShadowDisagreement || d.Label != string(d.SelectedTier) {
				stats.ShadowDisagreements++
			}
		}
		if classifierWasAttempted(d) {
			stats.ClassifierAttempts++
			latencies = append(latencies, d.LatencyMS)
			if strings.Contains(d.Error, "route: parse:") {
				stats.ParseFailures++
			}
		}
		if d.BreakerTripped {
			stats.BreakerTrips++
		}
	}
	if stats.RoutedDecisions > 0 {
		stats.BigRate = float64(stats.BigDecisions) / float64(stats.RoutedDecisions)
	}
	if stats.ShadowCompared > 0 {
		stats.ShadowDisagreementRate = float64(stats.ShadowDisagreements) / float64(stats.ShadowCompared)
	}
	if stats.ClassifierAttempts > 0 {
		stats.ParseFailureRate = float64(stats.ParseFailures) / float64(stats.ClassifierAttempts)
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		stats.P50ClassifyLatency = time.Duration(nearestRank(latencies, 0.50)) * time.Millisecond
		stats.P95ClassifyLatency = time.Duration(nearestRank(latencies, 0.95)) * time.Millisecond
	}
	return stats
}

// Confirm marks one previously recorded assisted suggestion as operator gold.
// The durable row is replaced in place rather than appended, so confirmation
// cannot inflate decision counts. Repeating a confirmation is idempotent.
func (e *Engine) Confirm(id string) error {
	if e == nil {
		return errors.New("route: confirm: engine unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("route: confirm: decision id required")
	}
	if e.path == "" {
		return errors.New("route: confirm: decision log unavailable")
	}

	e.logMu.Lock()
	defer e.logMu.Unlock()
	// Exclusive across processes: a row another process appends while this
	// rewrite runs would land in the copy the rename replaces.
	lock, err := datalock.File(e.path, true)
	if err != nil {
		return fmt.Errorf("route: confirm: lock decision log: %w", err)
	}
	defer lock.Release()
	raw, err := os.ReadFile(e.path)
	if err != nil {
		return fmt.Errorf("route: confirm: read decision log: %w", err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	decisions := make([]Decision, 0, len(lines))
	found := false
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d Decision
		if err := json.Unmarshal(line, &d); err != nil {
			return fmt.Errorf("route: confirm: decode decision row %d: %w", i+1, err)
		}
		if d.ID == id {
			if !d.AssistedSuggestion {
				return fmt.Errorf("route: confirm: decision %q is not an assisted suggestion", id)
			}
			d.OperatorConfirmed = true
			found = true
		}
		decisions = append(decisions, d)
	}
	if !found {
		return fmt.Errorf("route: confirm: decision %q not found", id)
	}
	encoded := make([]byte, 0, len(raw)+32)
	for _, d := range decisions {
		line, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("route: confirm: encode decision: %w", err)
		}
		encoded = append(encoded, line...)
		encoded = append(encoded, '\n')
	}
	if err := replaceDecisionLog(e.path, encoded); err != nil {
		return fmt.Errorf("route: confirm: replace decision log: %w", err)
	}

	e.mu.Lock()
	for i := range e.history {
		if e.history[i].ID == id {
			e.history[i].OperatorConfirmed = true
		}
	}
	e.mu.Unlock()
	return nil
}

func replaceDecisionLog(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return fsperm.ReplaceWithMode(path, data, info.Mode().Perm())
}

func (e *Engine) decisionsForStats() []Decision {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	path := e.path
	history := append([]Decision(nil), e.history...)
	e.mu.Unlock()
	if path != "" {
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			var durable []Decision
			decoder := json.NewDecoder(f)
			for {
				var d Decision
				err = decoder.Decode(&d)
				if errors.Is(err, io.EOF) {
					if len(durable) > 0 || len(history) == 0 {
						return durable
					}
					break
				}
				if err != nil {
					if len(durable) > 0 {
						return durable
					}
					break
				}
				durable = append(durable, d)
			}
		}
	}
	return history
}

func classifierWasAttempted(d Decision) bool {
	if d.ClassifierAttempted {
		return true
	}
	// Phase-one records predate the explicit field. They attempted unless the
	// overlength guard marked them skipped. A phase-two policy reason with the
	// field unset is a deliberate no-classifier path (force/long/breaker).
	return d.PolicyReason == "" && d.Status != StatusSkipped
}

func nearestRank(sorted []int64, percentile float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// record keeps d in the history and the durable log, and writes its event.
func (e *Engine) record(ctx context.Context, d Decision) Decision {
	logDecision(ctx, d)
	e.mu.Lock()
	e.history = append(e.history, d)
	if len(e.history) > e.limit {
		copy(e.history, e.history[len(e.history)-e.limit:])
		e.history = e.history[:e.limit]
	}
	e.mu.Unlock()
	e.logMu.Lock()
	err := appendDecision(e.path, d) // telemetry must never fail or alter a decision
	pruneDue := e.pruneDueLocked()
	e.logMu.Unlock()
	if err != nil {
		RecordFailed.Log(ctx, "decision_id", d.ID, "err", err)
	}
	if pruneDue {
		// The whole-file retention rewrite runs OFF the record path: a caller
		// tripping the daily gate must not pay the rewrite inside its turn or
		// spawn. Prune re-takes logMu itself; errors are logged, never returned.
		go func() {
			if _, _, err := e.Prune(); err != nil {
				PruneFailed.Log(context.Background(), "err", err)
			}
		}()
	}
	return d
}

// logDecision writes d's event: the decision, or its fallback to the planner,
// and a tripped breaker.
func logDecision(ctx context.Context, d Decision) {
	if d.Status == StatusError {
		DecideFailed.Log(ctx, "decision_id", d.ID, "summary", d.Summary(), "mode", d.Mode,
			"agent", d.AgentID, "session", d.SessionID, "err", d.Error,
			"endpoint", d.Endpoint, "model", d.Model, "latency_ms", d.LatencyMS)
	} else {
		Decided.Log(ctx, "decision_id", d.ID, "summary", d.Summary(), "mode", d.Mode, "status", d.Status,
			"tier", string(d.SelectedTier), "role", d.SelectedRole, "profile", d.SelectedProfileID,
			"reason", d.PolicyReason, "agent", d.AgentID, "session", d.SessionID,
			"endpoint", d.Endpoint, "model", d.Model, "latency_ms", d.LatencyMS)
	}
	if d.BreakerTripped {
		BreakerOpened.Log(ctx, "decision_id", d.ID)
	}
}

// appendDecision adds one line under the log's shared lock, so a compaction in
// any process (which holds it exclusively) never loses the row.
func appendDecision(path string, d Decision) error {
	if path == "" {
		return nil // no data dir: the history stays in memory only
	}
	if err := fsperm.MkdirAll(filepath.Dir(path)); err != nil {
		return fmt.Errorf("route: create decision log dir: %w", err)
	}
	b, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("route: marshal decision: %w", err)
	}
	lock, err := datalock.File(path, false)
	if err != nil {
		return fmt.Errorf("route: lock decision log: %w", err)
	}
	defer lock.Release()
	f, err := fsperm.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return fmt.Errorf("route: open decision log: %w", err)
	}
	_, writeErr := f.Write(append(b, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("route: append decision log: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("route: close decision log: %w", closeErr)
	}
	return nil
}

// HashPrompt returns the lowercase SHA-256 identity used by decision logs.
func HashPrompt(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// EstimateTokens is Tenant's dependency-free four-bytes-per-token estimate.
func EstimateTokens(prompt string) int {
	if prompt == "" {
		return 0
	}
	return (len(prompt) + 3) / 4
}

func newDecisionID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	// crypto/rand failure must not make routing a turn dependency. The fallback
	// remains unique enough for a local append-only observation log.
	seed := fmt.Sprintf("%d:%p", time.Now().UnixNano(), &id)
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}
