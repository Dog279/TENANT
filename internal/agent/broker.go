package agent

import "sync"

// Broker fans one stream of turn Events out to many subscribers — the TUI
// feed and (TEN-76+) web dashboard clients at once. It lives here, beside
// the Event type it carries, so both the TUI wiring and the dashboard can
// share it without an import cycle.
//
// Lossy by design: each subscriber owns a buffered channel and Publish
// drops the event for any subscriber whose buffer is full rather than
// blocking. This preserves the historical single-Observer semantics — a
// slow consumer never stalls the agent turn or the other subscribers (the
// authoritative final answer is replayed from TurnResult on turn done).
type Broker struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
	buf  int
}

// defaultBrokerBuffer matches the historical evCh capacity (commands.go),
// so the TUI feed keeps the same headroom under a token flood.
const defaultBrokerBuffer = 1024

// NewBroker returns a Broker whose subscriber channels buffer `buffer`
// events (<=0 picks the default). Publish to a full subscriber drops.
func NewBroker(buffer int) *Broker {
	if buffer <= 0 {
		buffer = defaultBrokerBuffer
	}
	return &Broker{subs: map[int]chan Event{}, buf: buffer}
}

// Publish delivers ev to every current subscriber, non-blocking: a
// subscriber with a full buffer misses this event. Safe to call from the
// turn goroutine while subscribers register or cancel. Use it as the
// agent's Observer: cfg.Observer = broker.Publish.
func (b *Broker) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default: // subscriber backed up — drop rather than stall
		}
	}
}

// Subscribe registers a new consumer and returns its event channel plus a
// cancel func. cancel unregisters and closes the channel (idempotent); the
// consumer's range loop then ends. Always call cancel to avoid leaking the
// subscriber entry.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	id := b.next
	b.next++
	ch := make(chan Event, b.buf)
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
		})
	}
	return ch, cancel
}
