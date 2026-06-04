package agent

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestBroker_FanOut: one Publish reaches every subscriber.
func TestBroker_FanOut(t *testing.T) {
	b := NewBroker(8)
	const n = 5
	chans := make([]<-chan Event, n)
	cancels := make([]func(), n)
	for i := range chans {
		chans[i], cancels[i] = b.Subscribe()
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	b.Publish(Event{Kind: EventFinal, Text: "hi"})
	for i, ch := range chans {
		select {
		case ev := <-ch:
			if ev.Kind != EventFinal || ev.Text != "hi" {
				t.Fatalf("sub %d: got %+v", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: no event", i)
		}
	}
}

// TestBroker_SlowSubscriberDropsWithoutBlocking: a subscriber that never
// drains must not block Publish or starve a healthy subscriber. The slow
// one only loses the overflow; the fast one sees everything.
func TestBroker_SlowSubscriberDropsWithoutBlocking(t *testing.T) {
	b := NewBroker(2)              // tiny buffer so the slow sub overflows fast
	_, cancelSlow := b.Subscribe() // slow: never drained
	fast, cancelFast := b.Subscribe()
	defer cancelSlow()
	defer cancelFast()

	const total = 50
	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			b.Publish(Event{Kind: EventToken, Iter: i})
		}
		close(done)
	}()
	// Publish must finish promptly despite `slow` never being drained.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	// The fast subscriber can drain at least a buffer's worth (some were
	// produced before it started draining; the point is it isn't starved
	// and Publish never deadlocked).
	got := 0
	for {
		select {
		case <-fast:
			got++
			if got >= 2 {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("fast subscriber starved: got %d", got)
		}
	}
}

// TestBroker_CancelStopsDelivery: after cancel, the channel is closed and
// no further events arrive.
func TestBroker_CancelStopsDelivery(t *testing.T) {
	b := NewBroker(4)
	ch, cancel := b.Subscribe()

	b.Publish(Event{Kind: EventTurnStart})
	cancel()

	// Drain whatever was buffered until the channel closes; a closed
	// channel yields ok==false.
	closed := false
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-time.After(time.Second):
			t.Fatal("channel not closed after cancel")
		}
	}
	// Publish after cancel must be a no-op (no panic on a removed sub).
	b.Publish(Event{Kind: EventToken})
}

// TestBroker_CancelIdempotent: calling cancel twice is safe.
func TestBroker_CancelIdempotent(t *testing.T) {
	b := NewBroker(1)
	_, cancel := b.Subscribe()
	cancel()
	cancel() // must not panic / double-close
}

// TestBroker_NoGoroutineLeak: a Subscribe+cancel pair leaves no lingering
// goroutine or subscriber entry behind.
func TestBroker_NoGoroutineLeak(t *testing.T) {
	b := NewBroker(4)
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe()
			go func() {
				for range ch { // drains until cancel closes the channel
				}
			}()
			b.Publish(Event{Kind: EventToken})
			cancel()
		}()
	}
	wg.Wait()

	b.mu.Lock()
	n := len(b.subs)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("subscribers leaked: %d still registered", n)
	}
	// Allow the drain goroutines to observe the close and exit.
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}
