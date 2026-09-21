package discord

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A button click's handler must not run on the gateway read loop: its HTTP ACK
// is bounded by Discord's 3-second window, and a slow one would stall
// heartbeats. Script an INTERACTION_CREATE whose handler blocks, followed by a
// MESSAGE_CREATE: the message must be delivered while the handler is still
// blocked. Fails on synchronous dispatch (the message waits behind the click).
func TestGateway_InteractionHandlerOffReadLoop(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	gotMsg := make(chan Inbound, 1)
	g := &Gateway{
		Token:         "tok",
		OnInteraction: func(Interaction) { entered <- struct{}{}; <-block },
		OnMessage:     func(in Inbound) { gotMsg <- in },
		seen:          newSeenSet(64),
	}
	click, _ := json.Marshal(map[string]any{
		"id": "i1", "token": "t", "type": 3, "channel_id": "dm1",
		"data": map[string]any{"custom_id": "approve:X"}, "message": map[string]any{"id": "m1"}, "user": map[string]any{"id": "op"},
	})
	fake := newFakeConn(
		srv(opHello, `{"heartbeat_interval":60000}`, nil, ""),
		srv(opDispatch, `{"session_id":"s","resume_gateway_url":"wss://r"}`, intp(1), "READY"),
		srv(opDispatch, string(click), intp(2), "INTERACTION_CREATE"),
		srv(opDispatch, msgD("m2", "dm1", "", "op", false, "after the click"), intp(3), "MESSAGE_CREATE"),
	)
	var st sessionState
	done := make(chan struct{})
	go func() { g.session(context.Background(), fake, &st, false); close(done) }()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("interaction handler never ran")
	}
	select {
	case in := <-gotMsg:
		if in.MessageID != "m2" {
			t.Errorf("wrong message delivered: %+v", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MESSAGE_CREATE was blocked behind the interaction handler — dispatch is on the read loop")
	}
	close(block)
	_ = fake.Close()
	<-done
}
