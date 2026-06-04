package discord

// gateway_fuzz_test.go is TEN-124 (V2-T3): the reconnect-FSM hardening gate.
// The gateway is the only adversary-reachable surface (Discord can replay,
// truncate, or send malformed frames), so before exec lands offsite we fuzz the
// decode + dispatch path to prove it NEVER panics on garbage, and pin the FSM
// invariants (close-code decisions, idempotency eviction, backoff bounds) with
// property tests.

import (
	"encoding/json"
	"testing"
)

// fullFrame builds a complete {op,d,s,t} gateway frame as bytes (the seeds feed
// the same decode path gobwasConn.Read uses).
func fullFrame(op int, t string, d string) []byte {
	m := map[string]any{"op": op}
	if t != "" {
		m["t"] = t
	}
	if d != "" {
		m["d"] = json.RawMessage(d)
	}
	b, _ := json.Marshal(m)
	return b
}

// routeFuzzPayload mirrors session()'s op/T dispatch switch WITHOUT a socket, so
// the fuzzer drives the same decoders the live read loop does.
func routeFuzzPayload(g *Gateway, p gwPayload) {
	switch p.Op {
	case opInvalidSession:
		var resumable bool
		_ = json.Unmarshal(p.D, &resumable)
	case opDispatch:
		switch p.T {
		case "MESSAGE_CREATE":
			g.dispatchMessage(p.D)
		case "INTERACTION_CREATE":
			g.dispatchInteraction(p.D)
		}
	}
}

// FuzzGatewayPayload feeds arbitrary bytes through the full decode + dispatch
// path and asserts it never panics and never violates the seen-set bound. Run a
// bounded campaign with:
//
//	go test ./internal/plugins/discord/ -run xxx -fuzz FuzzGatewayPayload -fuzztime 30s
func FuzzGatewayPayload(f *testing.F) {
	// Seed with real frame shapes + the malformed cases that matter.
	f.Add(fullFrame(opHello, "", `{"heartbeat_interval":45000}`))
	f.Add(fullFrame(opDispatch, "READY", `{"session_id":"s1","resume_gateway_url":"wss://r.gg"}`))
	f.Add(fullFrame(opDispatch, "RESUMED", `null`))
	f.Add(fullFrame(opDispatch, "MESSAGE_CREATE", msgD("m1", "c1", "", "u1", false, "hi")))
	f.Add(fullFrame(opDispatch, "INTERACTION_CREATE", `{"id":"i","token":"t","type":3,"channel_id":"c","data":{"custom_id":"approve:Z"},"user":{"id":"op"}}`))
	f.Add(fullFrame(opInvalidSession, "", `true`))
	f.Add(fullFrame(opInvalidSession, "", `false`))
	f.Add(fullFrame(opReconnect, "", `null`))
	f.Add([]byte(`{"op":0,"t":"MESSAGE_CREATE","d":{"id":123}}`)) // wrong-typed field
	f.Add([]byte(`{"op":`))                                       // truncated
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`not json at all`))
	f.Add([]byte("\x00\x01\x02\xff"))

	f.Fuzz(func(t *testing.T, data []byte) {
		const cap = 16
		g := &Gateway{
			seen:          newSeenSet(cap),
			OnMessage:     func(Inbound) {},
			OnInteraction: func(Interaction) {},
		}
		// 1. Full-frame decode (as the live Read does) → dispatch. Never panics.
		var p gwPayload
		if json.Unmarshal(data, &p) == nil {
			routeFuzzPayload(g, p)
		}
		// 2. Hand the raw bytes straight to the sub-decoders too — they must
		//    tolerate garbage in the `d` position without panicking.
		g.dispatchMessage(json.RawMessage(data))
		g.dispatchInteraction(json.RawMessage(data))

		// 3. The idempotency set must never exceed its cap, no matter the input.
		g.seen.mu.Lock()
		n, ord := len(g.seen.set), len(g.seen.order)
		g.seen.mu.Unlock()
		if n > cap || ord > cap {
			t.Fatalf("seenSet exceeded cap %d: set=%d order=%d", cap, n, ord)
		}
	})
}

// decideClose must give up on EXACTLY the fatal auth/intent/version codes and
// resume on everything else across the whole plausible close-code space.
func TestDecideClose_Exhaustive(t *testing.T) {
	fatal := map[int]bool{4004: true, 4010: true, 4011: true, 4012: true, 4013: true, 4014: true}
	for code := 1000; code <= 4999; code++ {
		o := decideClose(code)
		if fatal[code] {
			if o.action != actGiveup {
				t.Errorf("close %d must give up", code)
			}
			if o.err == nil {
				t.Errorf("a fatal close (%d) must carry an error for OnFatal", code)
			}
		} else if o.action != actResume {
			t.Errorf("close %d must resume, got action %d", code, o.action)
		}
	}
}

// seenSet stays bounded and FIFO-evicts under far more inserts than its cap.
func TestSeenSet_BoundedUnderLoad(t *testing.T) {
	const cap = 8
	s := newSeenSet(cap)
	ids := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		id := "id-" + itoa(i)
		ids = append(ids, id)
		if !s.add(id) {
			t.Fatalf("%s should be new", id)
		}
		s.mu.Lock()
		over := len(s.set) > cap || len(s.order) > cap
		s.mu.Unlock()
		if over {
			t.Fatalf("seenSet exceeded cap %d after %d inserts", cap, i+1)
		}
	}
	// The last `cap` ids are still present; everything older was evicted.
	for _, id := range ids[len(ids)-cap:] {
		if s.add(id) {
			t.Errorf("recent id %s should still be remembered (not re-addable)", id)
		}
	}
	for _, id := range ids[:len(ids)-cap] {
		if !s.add(id) {
			t.Errorf("evicted id %s should be addable again", id)
		}
	}
}

// backoff jitter stays in [cur/2, cur], cur grows ×2 capped at max and never
// shrinks, and reset returns to base.
func TestBackoff_BoundsGrowthReset(t *testing.T) {
	b := newBackoff()
	for i := 0; i < 100; i++ {
		cur := b.cur
		d := b.nextDelay()
		if d < cur/2 || d > cur {
			t.Fatalf("delay %v out of [cur/2,cur]=[%v,%v]", d, cur/2, cur)
		}
		if b.cur < cur || b.cur > b.max {
			t.Fatalf("cur moved out of bounds: %v → %v (max %v)", cur, b.cur, b.max)
		}
	}
	if b.cur != b.max {
		t.Errorf("cur should saturate at max under repeated waits, got %v (max %v)", b.cur, b.max)
	}
	b.reset()
	if b.cur != b.base {
		t.Errorf("reset must return cur to base, got %v (base %v)", b.cur, b.base)
	}
}

// itoa is a tiny dependency-free int→string (avoid importing strconv just for
// the test ids).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
