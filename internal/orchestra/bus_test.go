package orchestra

import "testing"

// Take drains only what keep accepts and leaves the rest in order;
// PendingWhere counts the same way without draining.
func TestBus_TakeKeepsTheRest(t *testing.T) {
	b := NewBus()
	b.Register("orch")
	b.Register("a")
	b.Register("b")
	for _, m := range []Message{{From: "a", To: "orch", Content: "1"}, {From: "b", To: "orch", Content: "2"}, {From: "a", Content: "3"}} {
		if err := b.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	fromA := func(m Message) bool { return m.From == "a" }
	if n := b.PendingWhere("orch", fromA); n != 2 {
		t.Fatalf("pending from a = %d, want 2", n)
	}
	got := b.Take("orch", fromA)
	if len(got) != 2 || got[0].Content != "1" || got[1].Content != "3" {
		t.Fatalf("took %+v", got)
	}
	if rest := b.Inbox("orch"); len(rest) != 1 || rest[0].Content != "2" {
		t.Fatalf("the rest should stay for Inbox: %+v", rest)
	}
}
