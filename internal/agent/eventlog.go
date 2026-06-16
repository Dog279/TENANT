package agent

import (
	"sync"
	"time"
)

// defaultEventLogCap is the retained-event ring size when NewEventLog(0) is used.
const defaultEventLogCap = 10000

// SeqEvent is a retained Event plus its monotonic sequence and arrival time.
type SeqEvent struct {
	Seq uint64
	Ev  Event
	At  time.Time
}

// EventLog is a bounded, cursor-indexed retention of agent Events for the
// dashboard activity feed (TEN-238): a replayable record of "what happened" so
// the feed BACKFILLS the full backlog on load and resumes GAP-FREE after any
// reconnect — fixing both the "no history before I opened the tab" and the
// "stops updating when the macOS tab is unfocused" defects. The Broker
// (chat + the TUI feed) is pure live fan-out with no history; EventLog is a
// PARALLEL sink the dashboard reads, leaving the Broker untouched.
//
// It mirrors orchestra.Bus's lossless Since(cursor) pattern, but as a BOUNDED
// ring keyed by a monotonic uint64 Seq (eviction never invalidates a cursor —
// a slice index would). Per-token/accounting noise is denylisted at write time
// so the ring holds real activity, not a token flood. Safe for concurrent use.
type EventLog struct {
	mu     sync.Mutex
	ring   []SeqEvent
	cap    int
	next   uint64        // next Seq to assign (monotonic; never reused, survives eviction)
	notify chan struct{} // coalescing wake for a live tail (pair with Since)
}

// NewEventLog returns a log retaining the most recent `capacity` events
// (<=0 ⇒ defaultEventLogCap).
func NewEventLog(capacity int) *EventLog {
	if capacity <= 0 {
		capacity = defaultEventLogCap
	}
	return &EventLog{cap: capacity, next: 1, notify: make(chan struct{}, 1)}
}

// Append records ev (unless it's denylisted noise), assigns the next Seq, evicts
// the oldest when over capacity, and wakes any live tail. Non-blocking wake.
func (l *EventLog) Append(ev Event) {
	if !eventRetained(ev) {
		return
	}
	l.mu.Lock()
	l.ring = append(l.ring, SeqEvent{Seq: l.next, Ev: ev, At: time.Now()})
	l.next++
	if len(l.ring) > l.cap {
		l.ring = l.ring[len(l.ring)-l.cap:] // ring eviction; Seq stays monotonic
	}
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// Since returns every retained event with Seq > cursor (oldest first) plus the
// new head cursor to pass next time. If cursor predates the oldest retained
// event (ring eviction outran the reader), it returns the whole ring — the
// caller backfills what survived. Lossless within the retention window.
func (l *EventLog) Since(cursor uint64) ([]SeqEvent, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	head := l.next - 1
	if len(l.ring) == 0 || cursor >= head {
		return nil, head
	}
	out := make([]SeqEvent, 0, len(l.ring))
	for _, se := range l.ring {
		if se.Seq > cursor {
			out = append(out, se)
		}
	}
	return out, head
}

// Snapshot returns the full retained backlog (oldest first) + the head cursor —
// for the initial /activity page render.
func (l *EventLog) Snapshot() ([]SeqEvent, uint64) { return l.Since(0) }

// Notify is a coalescing wake-up: a receive means "new events exist — call
// Since." Pair with Since for a gap-free live tail (mirrors orchestra.Bus).
func (l *EventLog) Notify() <-chan struct{} { return l.notify }

// eventRetained denylists per-token / accounting noise so the ring holds real
// activity. Kept in sync with dashboard.activityRelevant (same four kinds).
func eventRetained(ev Event) bool {
	switch ev.Kind {
	case EventToken, EventUsage, EventAssistant, EventMemory:
		return false
	}
	return true
}
