package route

import (
	"sync"
	"time"
)

const (
	DefaultBreakerFailures = 3
	DefaultBreakerOpenFor  = 60 * time.Second
)

// BreakerState is the classifier circuit-breaker state.
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half-open"
)

// Breaker protects turns from repeatedly waiting on a dead auxiliary runtime.
// After the open interval, exactly one caller is admitted as the half-open
// probe; concurrent callers continue to degrade immediately.
type Breaker struct {
	mu sync.Mutex

	failureThreshold int
	openFor          time.Duration
	state            BreakerState
	consecutive      int
	openedAt         time.Time
	probeInFlight    bool
	trips            uint64
	generation       uint64
}

// BreakerPermit identifies one admitted classifier call. Completion methods
// ignore permits from an older breaker generation, preventing a slow pre-trip
// call from closing (or reopening) a newer open/half-open state.
type BreakerPermit struct {
	generation uint64
	probe      bool
	admitted   bool
}

// NewBreaker constructs a breaker. Non-positive parameters select the
// production doctrine: three consecutive failures and a 60-second open state.
func NewBreaker(failureThreshold int, openFor time.Duration) *Breaker {
	if failureThreshold <= 0 {
		failureThreshold = DefaultBreakerFailures
	}
	if openFor <= 0 {
		openFor = DefaultBreakerOpenFor
	}
	return &Breaker{
		failureThreshold: failureThreshold,
		openFor:          openFor,
		state:            BreakerClosed,
	}
}

// Acquire reports whether this call may invoke the classifier and returns the
// completion permit for it. The first caller after the open interval
// atomically becomes the sole half-open probe.
func (b *Breaker) Acquire(now time.Time) (BreakerPermit, bool) {
	if b == nil {
		return BreakerPermit{admitted: true}, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerClosed:
		return BreakerPermit{generation: b.generation, admitted: true}, true
	case BreakerOpen:
		if now.Sub(b.openedAt) < b.openFor {
			return BreakerPermit{}, false
		}
		b.state = BreakerHalfOpen
		b.probeInFlight = true
		return BreakerPermit{generation: b.generation, probe: true, admitted: true}, true
	case BreakerHalfOpen:
		return BreakerPermit{}, false
	default:
		// A corrupt zero/unknown state fails safe by closing: routing remains an
		// optimization and classifier errors will reopen it normally.
		b.state = BreakerClosed
		b.consecutive = 0
		b.generation++
		return BreakerPermit{generation: b.generation, admitted: true}, true
	}
}

// RecordSuccess closes the breaker and resets the consecutive-failure count.
func (b *Breaker) RecordSuccess(permit BreakerPermit) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !permit.admitted || permit.generation != b.generation {
		return
	}
	if b.state == BreakerHalfOpen && !permit.probe {
		return
	}
	if b.state == BreakerOpen {
		return
	}
	b.state = BreakerClosed
	b.consecutive = 0
	b.openedAt = time.Time{}
	b.probeInFlight = false
	if permit.probe {
		b.generation++
	}
}

// RecordFailure advances the breaker and reports whether this failure caused
// a transition to open (including a failed half-open probe).
func (b *Breaker) RecordFailure(permit BreakerPermit, now time.Time) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !permit.admitted || permit.generation != b.generation {
		return false
	}
	if b.state == BreakerHalfOpen {
		if !permit.probe {
			return false
		}
		b.state = BreakerOpen
		b.openedAt = now
		b.probeInFlight = false
		b.consecutive = b.failureThreshold
		b.trips++
		b.generation++
		return true
	}
	if b.state == BreakerOpen {
		// A completion that was already in flight when another caller opened
		// the breaker must not extend the cooldown or count as another trip.
		return false
	}
	b.consecutive++
	if b.consecutive < b.failureThreshold {
		return false
	}
	b.state = BreakerOpen
	b.openedAt = now
	b.probeInFlight = false
	b.trips++
	b.generation++
	return true
}

// State returns the current state. An elapsed open interval is reported as
// half-open readiness, but Allow owns admission of the single probe.
func (b *Breaker) State(now time.Time) BreakerState {
	if b == nil {
		return BreakerClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerOpen && now.Sub(b.openedAt) >= b.openFor {
		return BreakerHalfOpen
	}
	return b.state
}

// Trips is the number of closed->open and half-open->open transitions.
func (b *Breaker) Trips() uint64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trips
}
