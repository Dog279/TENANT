// Package orchestra is Tenant's multi-agent layer: a live message fabric
// that lets concurrently-running agents talk to each other (and lets an
// orchestrator watch all of it) without routing every decision through the
// user. Agents keep their own identity/soul + private memory and share a
// common team-visibility memory tier (see internal/memory visibility);
// orchestra adds the COMMUNICATION between them.
//
// The Bus is the core primitive: async per-agent mailboxes (drain-on-read,
// so reading clears an agent's context) PLUS a retained append-only log of
// every message. The log is the lossless record: agents recall the full
// thread across context compactions via team_history, and the orchestrator
// observes all traffic completely via a cursor (Since) — nothing is ever
// dropped. Async by design — a sender never blocks on a recipient — so
// agents running in parallel negotiate without deadlocking on each other.
package orchestra

import (
	"fmt"
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
	mu        sync.Mutex
	order     []string             // registration order (deterministic membership)
	mailboxes map[string][]Message // agentID -> undrained messages (delivery)
	log       []Message            // retained, append-only — the lossless record
	notify    chan struct{}        // coalescing wake-up for observers (buffered 1)
	closed    bool
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{mailboxes: map[string][]Message{}, notify: make(chan struct{}, 1)}
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
			b.mailboxes[id] = append(b.mailboxes[id], m)
		}
	} else {
		if _, ok := b.mailboxes[m.To]; !ok {
			return fmt.Errorf("orchestra: no such agent %q", m.To)
		}
		b.mailboxes[m.To] = append(b.mailboxes[m.To], m)
	}
	b.log = append(b.log, m)
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

// Pending reports how many undrained messages an agent has (no drain).
func (b *Bus) Pending(agentID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.mailboxes[agentID])
}

// Since returns every message logged at or after cursor, plus the new
// cursor to pass next time. This is the orchestrator's LOSSLESS view: the
// log retains everything, so even if many sends coalesce into one Notify
// wake-up, one Since call returns them all. Nothing is dropped.
func (b *Bus) Since(cursor int) ([]Message, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(b.log) {
		return nil, len(b.log)
	}
	out := make([]Message, len(b.log)-cursor)
	copy(out, b.log[cursor:])
	return out, len(b.log)
}

// Notify is a coalescing wake-up: a receive means "new messages exist —
// call Since to read them." Closed by Close. Pair it with Since so the
// orchestrator reacts live without busy-polling, while Since guarantees
// completeness.
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
