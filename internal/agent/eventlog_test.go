package agent

import "testing"

func TestEventLog_BackfillAndCursor(t *testing.T) {
	l := NewEventLog(100)
	for i := 0; i < 5; i++ {
		l.Append(Event{Kind: EventToolCall, Tool: "t"})
	}
	// Snapshot returns the full backlog + head.
	all, head := l.Snapshot()
	if len(all) != 5 || head != 5 {
		t.Fatalf("snapshot: got %d events, head %d; want 5/5", len(all), head)
	}
	if all[0].Seq != 1 || all[4].Seq != 5 {
		t.Fatalf("seqs not monotonic-from-1: %d..%d", all[0].Seq, all[4].Seq)
	}
	// Since(cursor) is gap-free: only events after the cursor.
	rest, head2 := l.Since(3)
	if len(rest) != 2 || rest[0].Seq != 4 || head2 != 5 {
		t.Fatalf("Since(3): got %d (first seq %d) head %d; want 2/4/5", len(rest), seq0(rest), head2)
	}
	// Caught-up cursor → nothing new.
	if got, _ := l.Since(5); len(got) != 0 {
		t.Fatalf("Since(head) should be empty, got %d", len(got))
	}
}

func TestEventLog_RingEvictionKeepsCursorsMonotonic(t *testing.T) {
	l := NewEventLog(3) // tiny ring
	for i := 0; i < 10; i++ {
		l.Append(Event{Kind: EventToolCall})
	}
	all, head := l.Snapshot()
	if len(all) != 3 || head != 10 {
		t.Fatalf("ring should retain last 3 with head 10; got %d/%d", len(all), head)
	}
	if all[0].Seq != 8 || all[2].Seq != 10 {
		t.Fatalf("evicted ring seqs wrong: %d..%d (want 8..10)", all[0].Seq, all[2].Seq)
	}
	// A cursor older than the oldest retained event returns the whole surviving
	// ring (lossy beyond the window, but never corrupt).
	got, _ := l.Since(2)
	if len(got) != 3 || got[0].Seq != 8 {
		t.Fatalf("stale cursor should return the whole ring (3, first seq 8); got %d/%d", len(got), seq0(got))
	}
}

func TestEventLog_DenylistsNoiseAtWrite(t *testing.T) {
	l := NewEventLog(100)
	for _, k := range []EventKind{EventToken, EventUsage, EventAssistant, EventMemory} {
		l.Append(Event{Kind: k})
	}
	l.Append(Event{Kind: EventToolCall})
	l.Append(Event{Kind: EventBus})
	all, _ := l.Snapshot()
	if len(all) != 2 {
		t.Fatalf("noise kinds must be denylisted at write; retained %d, want 2", len(all))
	}
}

func TestEventLog_NotifyWakes(t *testing.T) {
	l := NewEventLog(10)
	l.Append(Event{Kind: EventToolCall})
	select {
	case <-l.Notify():
	default:
		t.Fatal("Append should wake Notify")
	}
}

func seq0(s []SeqEvent) uint64 {
	if len(s) == 0 {
		return 0
	}
	return s[0].Seq
}
