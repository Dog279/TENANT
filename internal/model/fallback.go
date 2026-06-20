package model

import (
	"context"
	"errors"
	"sync"
	"time"
)

// fallback.go (TEN-246): an LLM decorator that, when the active provider
// rate-limits / runs out of credits / goes unreachable, transparently routes
// the call to the next configured provider so work keeps going. Composed at the
// router resolution boundary (router.LLMForRole), so every actor that resolves a
// role — agent planner, summarizer, cron, relay — gets failover uniformly.
//
// Design (TEN-246 debate winner):
//   - PER-CALL, not sticky: the preferred provider is always link[0], so when
//     its cooldown lapses the next call retries it first — automatic recovery,
//     no persisted provider mutation.
//   - In-memory per-link cooldown so a known-down provider is skipped for a
//     while (not hammered every call), then retried.
//   - STREAMING: fail over ONLY before the first content token/tool-delta is
//     emitted. Once tokens are flowing, a mid-stream failure propagates as
//     today — failing over would splice a second model onto a half-streamed
//     answer. Buffered Generate has no such constraint.
//   - Real→real failover happens here, BELOW cmd/tenant's degrade-to-echo, so
//     echo stays the last resort (only full-chain exhaustion surfaces an error).

// FailoverEvent is emitted when a link fails and the next is tried (for an
// operator-facing feed line).
type FailoverEvent struct {
	From   string // link label (profile ID) that failed
	To     string // link label being tried next
	Err    error  // the classified failure
	Reason string // short human reason ("rate limited", "out of credits", …)
}

// fbLink is one provider in the chain.
type fbLink struct {
	llm   LLM
	label string
}

// HealthConfig (TEN-282) tunes the proactive latency health-gating layer that
// demotes a link which keeps returning 200 OK but SLOWLY. It is ADDITIVE on top
// of TEN-246's reactive (error-driven) failover and shares the same per-link
// cooldown mechanism. Zero value ⇒ OFF: with SlowThreshold==0 no latency-based
// demotion ever happens, so existing callers keep today's behavior exactly.
type HealthConfig struct {
	// SlowThreshold is the per-link EWMA latency above which a link is treated
	// as "slow" and proactively cooled down. 0 disables health-gating entirely
	// (the default — non-disruptive).
	SlowThreshold time.Duration
	// MinSamples is how many successful calls a link must have served before its
	// EWMA can trip a demotion. Guards against cold-start / single-outlier trips.
	// Clamped to >=1 when health-gating is enabled.
	MinSamples int
	// Alpha is the EWMA smoothing factor in (0,1]: higher = more weight on the
	// latest sample (reacts faster, noisier); lower = smoother. Defaults to 0.3
	// when health-gating is enabled and Alpha is out of range.
	Alpha float64
	// Cooldown is how long a slow link is skipped before it is re-probed (still
	// preferred again once the cooldown lapses — never a permanent demotion).
	// Defaults to 30s when health-gating is enabled and Cooldown<=0.
	Cooldown time.Duration
}

// FallbackLLM implements model.LLM over an ordered chain (primary first).
type FallbackLLM struct {
	links    []fbLink
	observer func(FailoverEvent)
	now      func() time.Time // injectable clock (tests)

	mu            sync.Mutex
	cooldownUntil map[string]time.Time     // link label → skip-until
	health        HealthConfig             // TEN-282: latency-gating tunables (zero ⇒ off)
	latEWMA       map[string]time.Duration // link label → EWMA of successful-call latency
	latSamples    map[string]int           // link label → number of successful-call samples
}

// NewFallbackLLM builds a chain decorator. links[0] is the primary. With fewer
// than 2 links it's a transparent passthrough (callers should just use the
// single LLM, but this stays correct either way). Health-gating is OFF until
// SetHealthGating is called (default behavior unchanged for all existing callers).
func NewFallbackLLM(links []fbLink, observer func(FailoverEvent)) *FallbackLLM {
	return &FallbackLLM{
		links:         links,
		observer:      observer,
		now:           time.Now,
		cooldownUntil: make(map[string]time.Time),
		latEWMA:       make(map[string]time.Duration),
		latSamples:    make(map[string]int),
	}
}

// SetHealthGating enables (or reconfigures) proactive latency-based demotion.
// ADDITIVE + optional: existing call sites that never call it get zero-value
// HealthConfig ⇒ no latency tracking trips a demotion. Out-of-range Alpha /
// non-positive Cooldown / MinSamples<1 are normalized to safe conservative
// defaults so a partial config can't silently disable the guard or trip it on a
// single sample. Concurrency-safe (guarded by the existing cooldown mutex).
func (f *FallbackLLM) SetHealthGating(cfg HealthConfig) {
	if cfg.SlowThreshold > 0 { // only normalize when the feature is actually on
		if cfg.MinSamples < 1 {
			cfg.MinSamples = 5
		}
		if cfg.Alpha <= 0 || cfg.Alpha > 1 {
			cfg.Alpha = 0.3
		}
		if cfg.Cooldown <= 0 {
			cfg.Cooldown = 30 * time.Second
		}
	}
	f.mu.Lock()
	f.health = cfg
	f.mu.Unlock()
}

// FailoverAction reports whether err should trigger failover to the next
// provider. Fail-CLOSED: only the known transient/availability/quota classes
// fail over; context-overflow (compact instead), invalid-request (fails
// everywhere), cancellation (user abort), and any UNKNOWN error do NOT — a
// future backend that returns a raw error stays on the primary, the safe default.
func FailoverAction(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrRateLimited),
		errors.Is(err, ErrInsufficientBalance),
		errors.Is(err, ErrEndpointDown),
		errors.Is(err, ErrInternal):
		return true
	default:
		return false
	}
}

// cooldownFor returns how long to skip a link after a given failure. Backoff
// can't help an out-of-credits provider, so it gets a long cooldown; transient
// failures get a short one so recovery is quick.
func cooldownFor(err error) time.Duration {
	if errors.Is(err, ErrInsufficientBalance) {
		return 5 * time.Minute
	}
	return 30 * time.Second
}

func failoverReason(err error) string {
	switch {
	case errors.Is(err, ErrInsufficientBalance):
		return "out of credits"
	case errors.Is(err, ErrRateLimited):
		return "rate limited"
	case errors.Is(err, ErrEndpointDown):
		return "unreachable"
	case errors.Is(err, ErrInternal):
		return "server error"
	default:
		return "error"
	}
}

// attemptOrder returns the links to try, preferring those NOT in cooldown (in
// chain order) and appending cooling ones last — so a healthy preferred link
// always leads, but if every link is cooling we still try rather than give up.
func (f *FallbackLLM) attemptOrder() []fbLink {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	ready := make([]fbLink, 0, len(f.links))
	cooling := make([]fbLink, 0, len(f.links))
	for _, lk := range f.links {
		if t, ok := f.cooldownUntil[lk.label]; ok && now.Before(t) {
			cooling = append(cooling, lk)
		} else {
			ready = append(ready, lk)
		}
	}
	return append(ready, cooling...)
}

func (f *FallbackLLM) markCooldown(label string, err error) {
	f.mu.Lock()
	f.cooldownUntil[label] = f.now().Add(cooldownFor(err))
	f.mu.Unlock()
}

// recordLatency feeds a SUCCESSFUL call's wall-clock latency into the link's
// EWMA and, when health-gating is enabled, proactively cools down a link whose
// smoothed latency has crossed SlowThreshold. Returns the prior link label and
// EWMA when a demotion fired (for an operator feed line via the observer), else
// "" and 0. All state mutation happens under the existing cooldown mutex — no
// second lock guards the latency/cooldown maps, so there is no race between
// attemptOrder (reads cooldownUntil) and this writer.
//
// Why this never demotes a healthy fast chain:
//   - SlowThreshold==0 (the default) short-circuits before any EWMA is touched.
//   - A demotion needs MinSamples successful calls AND a smoothed (not single
//     outlier) latency above the threshold, so transient blips don't trip it.
//   - It only ever sets a SHORT cooldown via the same mechanism the chain
//     re-probes after — a recovered (fast-again) link is restored, never
//     permanently demoted.
func (f *FallbackLLM) recordLatency(label string, d time.Duration) (demotedFrom string, ewma time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg := f.health
	if cfg.SlowThreshold <= 0 {
		return "", 0 // health-gating off ⇒ do not even track (default, non-disruptive)
	}
	// EWMA update: first sample seeds the average; later samples blend in.
	prev, seen := f.latEWMA[label]
	if !seen {
		f.latEWMA[label] = d
	} else {
		a := cfg.Alpha
		f.latEWMA[label] = time.Duration(a*float64(d) + (1-a)*float64(prev))
	}
	f.latSamples[label]++
	cur := f.latEWMA[label]
	// Only demote with enough evidence, when slow, and when not already cooling
	// (so we don't keep re-stamping / re-firing while the cooldown is in effect).
	if f.latSamples[label] < cfg.MinSamples || cur <= cfg.SlowThreshold {
		return "", 0
	}
	now := f.now()
	if until, ok := f.cooldownUntil[label]; ok && now.Before(until) {
		return "", 0 // already cooling — leave the existing window in place
	}
	f.cooldownUntil[label] = now.Add(cfg.Cooldown)
	return label, cur
}

// noteLatency records a successful call's latency and, if a proactive demotion
// fired, emits a synthetic FailoverEvent so operators see WHY the chain moved
// off a link that returned 200 OK. Kept out of the locked section so the
// observer callback never runs while holding f.mu.
func (f *FallbackLLM) noteLatency(label string, d time.Duration) {
	from, ewma := f.recordLatency(label, d)
	if from == "" || f.observer == nil {
		return
	}
	f.observer(FailoverEvent{
		From:   from,
		To:     "",  // demotion, not an in-flight switch; next call prefers the next ready link
		Err:    nil, // succeeded — it was just slow
		Reason: "slow (" + ewma.Round(time.Millisecond).String() + " avg)",
	})
}

func (f *FallbackLLM) fire(from, to string, err error) {
	if f.observer != nil {
		f.observer(FailoverEvent{From: from, To: to, Err: err, Reason: failoverReason(err)})
	}
}

// Generate tries the chain in attempt order. A failover-class error advances to
// the next link (cooldown + event); any other error (or success) returns
// immediately. Exhaustion returns the last error.
func (f *FallbackLLM) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	attempts := f.attemptOrder()
	var lastErr error
	for i, lk := range attempts {
		start := f.now()
		resp, err := lk.llm.Generate(ctx, req)
		if err == nil {
			// Succeeded — feed latency into the health gate (no-op unless enabled).
			f.noteLatency(lk.label, f.now().Sub(start))
			return resp, nil
		}
		lastErr = err
		if !FailoverAction(err) {
			return nil, err // non-failover (4xx / context / cancel / unknown) → surface
		}
		f.markCooldown(lk.label, err)
		if i < len(attempts)-1 {
			f.fire(lk.label, attempts[i+1].label, err)
		}
	}
	return nil, lastErr
}

// GenerateStream proxies a chosen link's stream, failing over ONLY before the
// first content token/tool-delta is emitted (the streaming invariant). After
// the first emitted chunk, any error is forwarded as-is (no failover).
func (f *FallbackLLM) GenerateStream(ctx context.Context, req GenerateRequest) (<-chan StreamChunk, error) {
	attempts := f.attemptOrder()
	out := make(chan StreamChunk)
	go func() {
		defer close(out)
		var lastErr error
		for i, lk := range attempts {
			start := f.now()
			ch, err := lk.llm.GenerateStream(ctx, req)
			if err != nil {
				// Stream never opened → no tokens emitted → safe to fail over.
				lastErr = err
				if FailoverAction(err) && i < len(attempts)-1 {
					f.markCooldown(lk.label, err)
					f.fire(lk.label, attempts[i+1].label, err)
					continue
				}
				out <- StreamChunk{Error: err}
				return
			}
			emitted := false
			failedPreToken := false
			postTokenErr := false
			for chunk := range ch {
				if chunk.Error != nil && !emitted {
					// Pre-token failure → maybe fail over to the next link.
					lastErr = chunk.Error
					if FailoverAction(chunk.Error) && i < len(attempts)-1 {
						f.markCooldown(lk.label, chunk.Error)
						f.fire(lk.label, attempts[i+1].label, chunk.Error)
						failedPreToken = true
						break
					}
					out <- chunk
					return
				}
				if chunk.Error != nil {
					postTokenErr = true // committed error → not a clean latency sample
				}
				if chunk.Delta != "" || chunk.ToolCallDelta != nil {
					emitted = true // committed — never fail over past this point
				}
				out <- chunk
			}
			if !failedPreToken {
				// Stream completed. Only a stream that produced tokens AND finished
				// without a (post-token) error is a valid latency sample — a
				// 0-token completion or a faulted stream isn't a useful timing
				// signal and must not arm/fire the health gate. No-op unless
				// health-gating is enabled.
				if emitted && !postTokenErr {
					f.noteLatency(lk.label, f.now().Sub(start))
				}
				return
			}
			// pre-token failover: try the next link
		}
		if lastErr != nil {
			out <- StreamChunk{Error: lastErr}
		}
	}()
	return out, nil
}

// TokenCount delegates to the primary (link[0]); token counting is a local/
// estimate operation and the primary's tokenizer is the right reference for
// budgeting. Falls through the chain only if the primary errors.
func (f *FallbackLLM) TokenCount(ctx context.Context, text string) (int, error) {
	var lastErr error
	for _, lk := range f.links {
		n, err := lk.llm.TokenCount(ctx, text)
		if err == nil {
			return n, nil
		}
		lastErr = err
	}
	return 0, lastErr
}
