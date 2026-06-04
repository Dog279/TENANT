package orchestra

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"tenant/internal/model"
)

func TestBus_DirectDeliveryDrains(t *testing.T) {
	b := NewBus()
	b.Register("alice")
	b.Register("bob")

	if err := b.Send(Message{From: "alice", To: "bob", Content: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if b.Pending("bob") != 1 || b.Pending("alice") != 0 {
		t.Fatalf("pending wrong: bob=%d alice=%d", b.Pending("bob"), b.Pending("alice"))
	}
	in := b.Inbox("bob")
	if len(in) != 1 || in[0].Content != "hi" || in[0].From != "alice" {
		t.Fatalf("inbox wrong: %+v", in)
	}
	// Draining empties it.
	if b.Pending("bob") != 0 || len(b.Inbox("bob")) != 0 {
		t.Fatal("inbox should be empty after drain")
	}
}

func TestBus_DirectToUnknownErrors(t *testing.T) {
	b := NewBus()
	b.Register("alice")
	if err := b.Send(Message{From: "alice", To: "ghost", Content: "x"}); err == nil {
		t.Fatal("sending to an unregistered agent must error")
	}
}

func TestBus_BroadcastHitsEveryoneButSender(t *testing.T) {
	b := NewBus()
	for _, id := range []string{"alice", "bob", "carol"} {
		b.Register(id)
	}
	if err := b.Send(Message{From: "alice", Content: "standup"}); err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	if b.Pending("alice") != 0 {
		t.Fatal("broadcast must not echo to the sender")
	}
	if b.Pending("bob") != 1 || b.Pending("carol") != 1 {
		t.Fatalf("broadcast must reach all peers: bob=%d carol=%d", b.Pending("bob"), b.Pending("carol"))
	}
}

func TestBus_SinceIsLosslessAndCursored(t *testing.T) {
	b := NewBus()
	b.Register("alice")
	b.Register("bob")

	_ = b.Send(Message{From: "alice", To: "bob", Content: "one"})
	_ = b.Send(Message{From: "bob", Content: "two"})

	got, cur := b.Since(0)
	if len(got) != 2 || got[0].Content != "one" || got[1].Content != "two" {
		t.Fatalf("Since(0) missed traffic: %+v", got)
	}
	// Cursor advances; no repeats.
	if more, _ := b.Since(cur); len(more) != 0 {
		t.Fatalf("cursor should be caught up, got %d", len(more))
	}
	// New message after cursor is returned, nothing dropped.
	_ = b.Send(Message{From: "alice", Content: "three"})
	more, _ := b.Since(cur)
	if len(more) != 1 || more[0].Content != "three" {
		t.Fatalf("Since(cursor) lossless follow-up wrong: %+v", more)
	}
}

func TestBus_NotifyWakesAndSinceCatchesCoalesced(t *testing.T) {
	b := NewBus()
	b.Register("alice")
	b.Register("bob")
	// Two sends before anyone reads notify → coalesced to one signal, but
	// Since must still return BOTH (lossless under coalescing).
	_ = b.Send(Message{From: "alice", To: "bob", Content: "a"})
	_ = b.Send(Message{From: "alice", To: "bob", Content: "b"})
	select {
	case <-b.Notify():
	default:
		t.Fatal("expected a wake-up signal")
	}
	got, _ := b.Since(0)
	if len(got) != 2 {
		t.Fatalf("coalesced notify lost data: got %d, want 2", len(got))
	}
}

func TestBus_HistoryRetainsAcrossDrain(t *testing.T) {
	b := NewBus()
	b.Register("alice")
	b.Register("bob")
	_ = b.Send(Message{From: "alice", To: "bob", Content: "remember this"})
	_ = b.Send(Message{From: "bob", Content: "team note"})

	// Draining bob's inbox clears delivery...
	if len(b.Inbox("bob")) == 0 {
		t.Fatal("inbox should have delivered")
	}
	if b.Pending("bob") != 0 {
		t.Fatal("inbox drained")
	}
	// ...but history still recalls it (survives compaction).
	hist := b.History("bob")
	if len(hist) != 2 {
		t.Fatalf("history should retain both (direct + own broadcast), got %d: %+v", len(hist), hist)
	}
	// alice never received the direct she sent, but history shows her own send + bob's broadcast.
	ah := b.History("alice")
	if len(ah) != 2 {
		t.Fatalf("alice history should include her send + bob's broadcast, got %d", len(ah))
	}
}

func TestBus_ConcurrentSendsAreSafe(t *testing.T) {
	b := NewBus()
	b.Register("hub")
	for i := 0; i < 5; i++ {
		b.Register(senderID(i))
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = b.Send(Message{From: senderID(n), To: "hub", Content: "m"})
			}
		}(i)
	}
	wg.Wait()
	if got := b.Pending("hub"); got != 100 {
		t.Fatalf("expected 100 delivered messages, got %d", got)
	}
}

func senderID(n int) string { return "agent" + string(rune('0'+n)) }

// --- CommsTool ---

func dispatch(t *testing.T, tool CommsTool, name string, args map[string]string) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, isErr, err := tool.Dispatch(context.Background(), model.ToolCall{Name: name, Arguments: raw})
	if err != nil {
		t.Fatalf("dispatch %s: %v", name, err)
	}
	return out, isErr
}

func TestCommsTool_SendBroadcastCheck(t *testing.T) {
	b := NewBus()
	b.Register("alice")
	b.Register("bob")
	alice := CommsTool{Bus: b, Self: "alice"}
	bob := CommsTool{Bus: b, Self: "bob"}

	// alice -> bob direct
	if out, isErr := dispatch(t, alice, "team_send", map[string]string{"to": "bob", "message": "can you take the API?"}); isErr {
		t.Fatalf("team_send failed: %q", out)
	}
	// alice broadcasts
	if _, isErr := dispatch(t, alice, "team_broadcast", map[string]string{"message": "starting now"}); isErr {
		t.Fatal("broadcast failed")
	}
	// bob checks — sees the direct + the broadcast, oldest first
	out, isErr := dispatch(t, bob, "team_check", nil)
	if isErr {
		t.Fatalf("team_check failed: %q", out)
	}
	if !strings.Contains(out, "can you take the API?") || !strings.Contains(out, "starting now") {
		t.Fatalf("bob missed messages:\n%s", out)
	}
	if !strings.Contains(out, "(broadcast)") {
		t.Fatalf("broadcast not labeled:\n%s", out)
	}
	// Inbox cleared after check.
	if out, _ := dispatch(t, bob, "team_check", nil); !strings.Contains(out, "no new messages") {
		t.Fatalf("inbox should be empty: %q", out)
	}
}

func TestCommsTool_SendToUnknownIsToolError(t *testing.T) {
	b := NewBus()
	b.Register("alice")
	alice := CommsTool{Bus: b, Self: "alice"}
	out, isErr := dispatch(t, alice, "team_send", map[string]string{"to": "ghost", "message": "x"})
	if !isErr || !strings.Contains(out, "no such agent") {
		t.Fatalf("unknown recipient should be a clean tool error: %q isErr=%v", out, isErr)
	}
}

func TestCommsTool_HistoryAndRoster(t *testing.T) {
	b := NewBus()
	b.Register("alice")
	b.Register("bob")
	alice := CommsTool{Bus: b, Self: "alice"}
	bob := CommsTool{Bus: b, Self: "bob"}

	dispatch(t, alice, "team_send", map[string]string{"to": "bob", "message": "ping"})
	dispatch(t, bob, "team_check", nil) // drains bob's inbox

	// history recalls even after the drain (cross-compaction recovery).
	if out, _ := dispatch(t, bob, "team_history", nil); !strings.Contains(out, "ping") {
		t.Fatalf("team_history should retain after drain: %q", out)
	}

	// roster shows peers; a late-spawned agent appears for those who check after.
	if out, _ := dispatch(t, alice, "team_roster", nil); !strings.Contains(out, "bob") {
		t.Fatalf("roster should list peers: %q", out)
	}
	b.Register("carol") // spawned in a later wave
	if out, _ := dispatch(t, alice, "team_roster", nil); !strings.Contains(out, "carol") {
		t.Fatalf("roster should reflect newly-spawned agents: %q", out)
	}
}
