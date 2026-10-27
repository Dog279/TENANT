// Package orchestra is Tenant's multi-agent layer: a live message fabric
// that lets concurrently-running agents talk to each other (and lets an
// orchestrator watch all of it) without routing every decision through the
// user. Agents keep their own identity/soul + private memory and share a
// common team-visibility memory tier (see internal/memory visibility);
// orchestra adds the COMMUNICATION between them.
//
// The Bus is the core primitive: async per-agent mailboxes (drain-on-read,
// so reading clears an agent's context) PLUS a bounded retained log. Agents
// recall the retained tail across context compactions via team_history, and
// the orchestrator observes it via an absolute cursor (Since). Async by design:
// a sender never blocks on a recipient, so parallel agents do not deadlock.
package orchestra

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// Message is one inter-agent message. To == "" means broadcast to the
// whole team (every registered agent except the sender).
type Message struct {
	From    string
	To      string // "" = broadcast
	Content string
	At      time.Time
}

// Broadcast reports whether this message targets the whole team.
func (m Message) Broadcast() bool { return m.To == "" }

// Bus is the async message fabric. Safe for concurrent use by many agent
// goroutines plus the orchestrator.
type Bus struct {
	mu         sync.Mutex
	order      []string             // registration order (deterministic membership)
	mailboxes  map[string][]Message // agentID -> undrained messages (delivery)
	log        []Message            // bounded retained history
	logBase    int                  // absolute cursor of log[0]
	historyCap int                  // maximum retained log/mailbox entries
	notify     chan struct{}        // coalescing wake-up for observers (buffered 1)
	closed     bool
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return NewBusWithHistoryCap(DefaultBudgets().BusHistoryCap)
}

// NewBusWithHistoryCap returns an empty bus whose retained history and each
// undrained mailbox are bounded. Absolute cursors remain monotonic when old
// history is evicted; a lagged reader resumes at the oldest retained message.
func NewBusWithHistoryCap(cap int) *Bus {
	if cap <= 0 {
		cap = DefaultBudgets().BusHistoryCap
	}
	return &Bus{mailboxes: map[string][]Message{}, notify: make(chan struct{}, 1), historyCap: cap}
}

// Register creates a mailbox for an agent. Idempotent — re-registering an
// existing agent keeps its pending messages.
func (b *Bus) Register(agentID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.mailboxes[agentID]; ok {
		return
	}
	b.mailboxes[agentID] = nil
	b.order = append(b.order, agentID)
}

// Unregister drops an agent's mailbox, and anything still undrained in it,
// once the agent has left the team: a finished sub-agent never reads its
// inbox again. A long-running bus then holds only live members, instead of a
// mailbox per agent ever spawned that fills with every later broadcast. What
// it said stays in the retained log. Idempotent.
func (b *Bus) Unregister(agentID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.mailboxes[agentID]; !ok {
		return
	}
	delete(b.mailboxes, agentID)
	b.order = slices.DeleteFunc(b.order, func(id string) bool { return id == agentID })
}

// Members returns the registered agent IDs in registration order.
func (b *Bus) Members() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.order))
	copy(out, b.order)
	return out
}

// Send routes a direct or broadcast message. A direct message to an
// unregistered agent is an error (the sender learns the peer doesn't
// exist instead of the message vanishing). Stamps At if unset. Every
// routed message is appended to the retained log and wakes observers.
func (b *Bus) Send(m Message) error {
	if m.At.IsZero() {
		m.At = time.Now().UTC()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("orchestra: bus closed")
	}
	if m.Broadcast() {
		for _, id := range b.order {
			if id == m.From {
				continue // don't echo a broadcast back to its sender
			}
			b.mailboxes[id] = appendBounded(b.mailboxes[id], m, b.historyCap)
		}
	} else {
		if _, ok := b.mailboxes[m.To]; !ok {
			return fmt.Errorf("orchestra: no such agent %q", m.To)
		}
		b.mailboxes[m.To] = appendBounded(b.mailboxes[m.To], m, b.historyCap)
	}
	b.log = append(b.log, m)
	if drop := len(b.log) - b.historyCap; drop > 0 {
		copy(b.log, b.log[drop:])
		b.log = b.log[:len(b.log)-drop]
		b.logBase += drop
	}
	// Coalescing wake: at most one pending signal. The observer reads ALL
	// new messages via Since, so a coalesced signal never loses data.
	select {
	case b.notify <- struct{}{}:
	default:
	}
	return nil
}

// Inbox drains and returns an agent's pending messages (oldest first).
// Draining means each message is delivered once — the agent reads its
// mailbox on its turn.
func (b *Bus) Inbox(agentID string) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	msgs := b.mailboxes[agentID]
	b.mailboxes[agentID] = nil
	return msgs
}

// PendingWhere counts an agent's undrained messages that keep accepts (no
// drain). An orchestrator serving several conversations waits on its own
// conversation's helpers, not on everyone's.
func (b *Bus) PendingWhere(agentID string, keep func(Message) bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, m := range b.mailboxes[agentID] {
		if keep(m) {
			n++
		}
	}
	return n
}

// Take drains and returns the messages of an agent's mailbox that keep
// accepts, oldest first; the rest stay, in order, for a later Inbox or Take.
func (b *Bus) Take(agentID string, keep func(Message) bool) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	var taken, rest []Message
	for _, m := range b.mailboxes[agentID] {
		if keep(m) {
			taken = append(taken, m)
		} else {
			rest = append(rest, m)
		}
	}
	b.mailboxes[agentID] = rest
	return taken
}

// Pending reports how many undrained messages an agent has (no drain).
func (b *Bus) Pending(agentID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.mailboxes[agentID])
}

// Since returns every message logged at or after cursor, plus the new
// cursor to pass next time. It is lossless within the configured retained
// window. A lagged cursor is clamped to the oldest retained message while the
// absolute cursor remains monotonic.
func (b *Bus) Since(cursor int) ([]Message, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cursor < b.logBase {
		cursor = b.logBase
	}
	head := b.logBase + len(b.log)
	if cursor >= head {
		return nil, head
	}
	start := cursor - b.logBase
	out := make([]Message, len(b.log)-start)
	copy(out, b.log[start:])
	return out, head
}

// Notify is a coalescing wake-up: a receive means "new messages exist —
// call Since to read them." Closed by Close. Pair it with Since so the
// orchestrator reacts live without busy-polling, while Since guarantees
// completeness within the configured history window.
func (b *Bus) Notify() <-chan struct{} { return b.notify }

// History returns the retained messages relevant to an agent — everything
// it sent, was sent directly, or could see via broadcast. Does NOT drain,
// so an agent can recall the full thread even after context compaction
// dropped it from the working set (team_history).
func (b *Bus) History(agentID string) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Message
	for _, m := range b.log {
		if m.From == agentID || m.To == agentID || m.Broadcast() {
			out = append(out, m)
		}
	}
	return out
}

// Close shuts the bus: further Sends error and the notify feed is closed.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.notify)
}

func appendBounded(dst []Message, m Message, cap int) []Message {
	if len(dst) < cap {
		return append(dst, m)
	}
	copy(dst, dst[1:])
	dst[len(dst)-1] = m
	return dst
}
