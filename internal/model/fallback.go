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

// FallbackLLM implements model.LLM over an ordered chain (primary first).
type FallbackLLM struct {
	links    []fbLink
	observer func(FailoverEvent)
	now      func() time.Time // injectable clock (tests)

	mu            sync.Mutex
	cooldownUntil map[string]time.Time // link label → skip-until
}

// NewFallbackLLM builds a chain decorator. links[0] is the primary. With fewer
// than 2 links it's a transparent passthrough (callers should just use the
// single LLM, but this stays correct either way).
func NewFallbackLLM(links []fbLink, observer func(FailoverEvent)) *FallbackLLM {
	return &FallbackLLM{
		links:         links,
		observer:      observer,
		now:           time.Now,
		cooldownUntil: make(map[string]time.Time),
	}
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
		resp, err := lk.llm.Generate(ctx, req)
		if err == nil {
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
				if chunk.Delta != "" || chunk.ToolCallDelta != nil {
					emitted = true // committed — never fail over past this point
				}
				out <- chunk
			}
			if !failedPreToken {
				return // stream completed (success, or post-token error already forwarded)
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
