package dashboard

// WebSocket handler tests (TEN-78). Fakes use the unique `ws` prefix to
// avoid colliding with sibling test files (rest_test.go / auth_test.go).
//
// These exercise the two halves of the bridge against a real upgraded
// socket: broker -> client streaming, and client -> agent turn dispatch
// (plus serialization and stop). Full end-to-end turn streaming through a
// live agent is also covered by TEN-81.

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"tenant/internal/agent"
)

// wsFakeRunner is a stub AgentRunner that records Turn/Interject calls and
// lets a turn block until the test releases it (so cancellation is testable).
type wsFakeRunner struct {
	mu         sync.Mutex
	turnQuery  []string
	interjects []string

	started  chan string   // signaled with the query when Turn begins
	release  chan struct{} // Turn blocks until this is closed/sent
	canceled chan struct{} // closed when a Turn observes ctx cancellation
}

func newWSFakeRunner() *wsFakeRunner {
	return &wsFakeRunner{
		started:  make(chan string, 8),
		release:  make(chan struct{}),
		canceled: make(chan struct{}, 8),
	}
}

func (f *wsFakeRunner) Turn(ctx context.Context, r agent.TurnRequest) (*agent.TurnResult, error) {
	f.mu.Lock()
	f.turnQuery = append(f.turnQuery, r.UserQuery)
	f.mu.Unlock()

	select {
	case f.started <- r.UserQuery:
	default:
	}

	// Block until released OR the per-turn context is canceled (stop /
	// disconnect). This makes both serialization and cancellation testable.
	select {
	case <-f.release:
		return &agent.TurnResult{Response: "ok"}, nil
	case <-ctx.Done():
		select {
		case f.canceled <- struct{}{}:
		default:
		}
		return nil, ctx.Err()
	}
}

func (f *wsFakeRunner) Interject(msg string) {
	f.mu.Lock()
	f.interjects = append(f.interjects, msg)
	f.mu.Unlock()
}

func (f *wsFakeRunner) turns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.turnQuery...)
}

func (f *wsFakeRunner) interjections() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.interjects...)
}

// wsTestServer stands up the dashboard with /ws mounted and returns the
// broker (for publishing events) plus a connected client socket. The caller
// closes the returned conn; cleanup tears down the httptest server.
func wsTestServer(t *testing.T, runner AgentRunner) (*agent.Broker, *httptest.Server) {
	t.Helper()
	broker := agent.NewBroker(0)
	s := New(Config{}, runner, nil, nil, broker, nil)
	ts := httptest.NewServer(s.Handler()) // New() mounts /ws via routes()
	t.Cleanup(ts.Close)
	return broker, ts
}

// wsDial opens a client WebSocket to the test server's /ws endpoint.
func wsDial(t *testing.T, ts *httptest.Server) net.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, _, err := ws.Dial(ctx, url)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readClientEvent reads one server text frame and decodes it, under a
// deadline so a hang fails the test instead of blocking forever.
func readClientEvent(t *testing.T, conn net.Conn) wsEventOut {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, err := wsutil.ReadServerText(conn)
	if err != nil {
		t.Fatalf("read server frame: %v", err)
	}
	var ev wsEventOut
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode frame %q: %v", data, err)
	}
	return ev
}

func writeClientMsg(t *testing.T, conn net.Conn, m wsClientMsg) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal client msg: %v", err)
	}
	if err := wsutil.WriteClientText(conn, b); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
}

// TestWSStreamsBrokerEvents: an Event published to the server's broker is
// delivered to the connected client as the matching JSON frame.
func TestWSStreamsBrokerEvents(t *testing.T) {
	broker, ts := wsTestServer(t, newWSFakeRunner())
	conn := wsDial(t, ts)

	// The handler subscribes during the upgrade goroutine; publish until the
	// client actually receives it (Subscribe is lossy and racy with connect).
	want := agent.Event{
		Kind:   agent.EventToolResult,
		Iter:   2,
		Tool:   "web_read",
		Result: "page text",
		IsErr:  false,
	}
	got := make(chan wsEventOut, 1)
	go func() {
		got <- readClientEvent(t, conn)
	}()

	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		broker.Publish(want)
		select {
		case ev := <-got:
			if ev.Kind != string(want.Kind) || ev.Tool != want.Tool ||
				ev.Result != want.Result || ev.Iter != want.Iter {
				t.Fatalf("frame = %+v, want kind/tool/result/iter from %+v", ev, want)
			}
			return
		case <-tick.C:
			// republish — first publishes may have raced the subscription
		case <-deadline:
			t.Fatal("client never received the published event")
		}
	}
}

// TestWSDispatchesTurn: a {"type":"turn","text":"hi"} frame invokes
// AgentRunner.Turn with UserQuery "hi".
func TestWSDispatchesTurn(t *testing.T) {
	runner := newWSFakeRunner()
	_, ts := wsTestServer(t, runner)
	conn := wsDial(t, ts)

	writeClientMsg(t, conn, wsClientMsg{Type: "turn", Text: "hi"})

	select {
	case q := <-runner.started:
		if q != "hi" {
			t.Fatalf("Turn query = %q, want %q", q, "hi")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Turn was not invoked")
	}
	if turns := runner.turns(); len(turns) != 1 || turns[0] != "hi" {
		t.Fatalf("recorded turns = %v, want [hi]", turns)
	}
	// Let the blocked turn finish so the handler tears down cleanly.
	close(runner.release)
}

// TestWSInterject: a {"type":"interject"} frame invokes Interject (no turn
// required to be running — it queues regardless).
func TestWSInterject(t *testing.T) {
	runner := newWSFakeRunner()
	_, ts := wsTestServer(t, runner)
	conn := wsDial(t, ts)

	writeClientMsg(t, conn, wsClientMsg{Type: "interject", Text: "also do X"})

	deadline := time.After(3 * time.Second)
	for {
		if got := runner.interjections(); len(got) == 1 && got[0] == "also do X" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("Interject not recorded, got %v", runner.interjections())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestWSSerializesTurns: while one turn runs, a second "turn" on the SAME
// connection is rejected with a notice frame and never reaches the runner.
// (Serialization is server-wide now (TEN-80); the cross-connection case is
// TestWSServerWideSerialization.)
func TestWSSerializesTurns(t *testing.T) {
	runner := newWSFakeRunner()
	_, ts := wsTestServer(t, runner)
	conn := wsDial(t, ts)

	writeClientMsg(t, conn, wsClientMsg{Type: "turn", Text: "first"})
	select {
	case q := <-runner.started:
		if q != "first" {
			t.Fatalf("first turn query = %q", q)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first turn never started")
	}

	// Second turn while the first is still blocked → notice, no new Turn.
	writeClientMsg(t, conn, wsClientMsg{Type: "turn", Text: "second"})
	ev := readClientEvent(t, conn)
	if ev.Kind != "notice" {
		t.Fatalf("expected notice frame, got %+v", ev)
	}

	// The runner must not have started a second turn.
	select {
	case q := <-runner.started:
		t.Fatalf("second turn unexpectedly started with %q", q)
	case <-time.After(100 * time.Millisecond):
	}
	if turns := runner.turns(); len(turns) != 1 {
		t.Fatalf("recorded turns = %v, want exactly one", turns)
	}
	close(runner.release)
}

// TestWSStopCancelsTurn: a {"type":"stop"} frame cancels the in-flight
// turn's context (the fake observes ctx.Done()).
func TestWSStopCancelsTurn(t *testing.T) {
	runner := newWSFakeRunner()
	_, ts := wsTestServer(t, runner)
	conn := wsDial(t, ts)

	writeClientMsg(t, conn, wsClientMsg{Type: "turn", Text: "work"})
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("turn never started")
	}

	writeClientMsg(t, conn, wsClientMsg{Type: "stop"})
	select {
	case <-runner.canceled:
		// turn observed cancellation — success
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not cancel the in-flight turn")
	}
}

// TestWSDisconnectCancelsTurn: closing the client socket cancels the
// in-flight turn (no goroutine leak; the agent ctx is torn down).
func TestWSDisconnectCancelsTurn(t *testing.T) {
	runner := newWSFakeRunner()
	_, ts := wsTestServer(t, runner)
	conn := wsDial(t, ts)

	writeClientMsg(t, conn, wsClientMsg{Type: "turn", Text: "work"})
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("turn never started")
	}

	_ = conn.Close()
	select {
	case <-runner.canceled:
		// disconnect tore down the turn context — success
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel the in-flight turn")
	}
}

// TestWSBroadcastsToAllClients: an Event published to the shared broker is
// delivered to EVERY connected client, not just one (the broker fans out;
// TEN-80 doesn't change that).
func TestWSBroadcastsToAllClients(t *testing.T) {
	broker, ts := wsTestServer(t, newWSFakeRunner())
	connA := wsDial(t, ts)
	connB := wsDial(t, ts)

	want := agent.Event{Kind: agent.EventToolResult, Iter: 1, Tool: "web_read", Result: "shared"}

	gotA := make(chan wsEventOut, 1)
	gotB := make(chan wsEventOut, 1)
	go func() { gotA <- readClientEvent(t, connA) }()
	go func() { gotB <- readClientEvent(t, connB) }()

	// Republish until BOTH clients have received — Subscribe is lossy and
	// races the upgrade goroutine, so early publishes may miss a client.
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	doneA, doneB := false, false
	for !doneA || !doneB {
		broker.Publish(want)
		select {
		case ev := <-gotA:
			if ev.Tool != want.Tool || ev.Result != want.Result {
				t.Fatalf("client A frame = %+v, want tool/result from %+v", ev, want)
			}
			doneA = true
		case ev := <-gotB:
			if ev.Tool != want.Tool || ev.Result != want.Result {
				t.Fatalf("client B frame = %+v, want tool/result from %+v", ev, want)
			}
			doneB = true
		case <-tick.C:
			// republish — first publishes may have raced a subscription
		case <-deadline:
			t.Fatalf("not all clients received the event (A=%v B=%v)", doneA, doneB)
		}
	}
}

// TestWSServerWideSerialization: two clients on ONE server share a single
// agent. While client A's turn runs, client B's "turn" is rejected with a busy
// notice and the runner's Turn is NOT called a second time. After A's turn
// completes, B can start a turn.
func TestWSServerWideSerialization(t *testing.T) {
	runner := newWSFakeRunner()
	_, ts := wsTestServer(t, runner)
	connA := wsDial(t, ts)
	connB := wsDial(t, ts)

	// A starts the (blocking) turn.
	writeClientMsg(t, connA, wsClientMsg{Type: "turn", Text: "from-A"})
	select {
	case q := <-runner.started:
		if q != "from-A" {
			t.Fatalf("first turn query = %q, want from-A", q)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client A's turn never started")
	}

	// B tries a turn while A's is active → busy notice, no second Turn call.
	writeClientMsg(t, connB, wsClientMsg{Type: "turn", Text: "from-B"})
	ev := readClientEvent(t, connB)
	if ev.Kind != "notice" {
		t.Fatalf("client B expected a busy notice, got %+v", ev)
	}
	select {
	case q := <-runner.started:
		t.Fatalf("client B's turn unexpectedly started with %q", q)
	case <-time.After(100 * time.Millisecond):
	}
	if got := runner.turns(); len(got) != 1 || got[0] != "from-A" {
		t.Fatalf("recorded turns = %v, want exactly [from-A]", got)
	}

	// A's turn completes; B can now start one.
	close(runner.release)
	deadline := time.After(3 * time.Second)
	for {
		writeClientMsg(t, connB, wsClientMsg{Type: "turn", Text: "from-B-again"})
		select {
		case q := <-runner.started:
			if q != "from-B-again" {
				t.Fatalf("client B's later turn query = %q", q)
			}
			return
		case <-time.After(50 * time.Millisecond):
			// The active-turn state clears asynchronously when A's turn
			// goroutine returns; retry B's turn until it's accepted. A
			// rejected attempt just emits a notice frame we don't read here.
		case <-deadline:
			t.Fatal("client B could not start a turn after A's completed")
		}
	}
}

// TestWSNonOwnerStopIgnored: client B cannot cancel client A's active turn
// with a "stop" — only the owner's stop (or disconnect) cancels it.
func TestWSNonOwnerStopIgnored(t *testing.T) {
	runner := newWSFakeRunner()
	_, ts := wsTestServer(t, runner)
	connA := wsDial(t, ts)
	connB := wsDial(t, ts)

	writeClientMsg(t, connA, wsClientMsg{Type: "turn", Text: "owned-by-A"})
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("client A's turn never started")
	}

	// B's stop targets a turn it doesn't own → must NOT cancel A's turn.
	writeClientMsg(t, connB, wsClientMsg{Type: "stop"})
	select {
	case <-runner.canceled:
		t.Fatal("non-owner stop canceled the active turn")
	case <-time.After(200 * time.Millisecond):
		// good — A's turn is still running
	}

	// A's own stop cancels it.
	writeClientMsg(t, connA, wsClientMsg{Type: "stop"})
	select {
	case <-runner.canceled:
		// owner stop canceled the turn — success
	case <-time.After(3 * time.Second):
		t.Fatal("owner stop did not cancel the active turn")
	}
}

// TestWSNonOwnerDisconnectKeepsTurn: a non-owner (client B) disconnecting must
// not cancel client A's active turn; then A's disconnect does cancel it.
func TestWSNonOwnerDisconnectKeepsTurn(t *testing.T) {
	runner := newWSFakeRunner()
	_, ts := wsTestServer(t, runner)
	connA := wsDial(t, ts)
	connB := wsDial(t, ts)

	writeClientMsg(t, connA, wsClientMsg{Type: "turn", Text: "owned-by-A"})
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("client A's turn never started")
	}

	// B leaves: A's turn must survive.
	_ = connB.Close()
	select {
	case <-runner.canceled:
		t.Fatal("non-owner disconnect canceled the active turn")
	case <-time.After(200 * time.Millisecond):
		// good — A's turn is still running
	}

	// A leaves: its turn is canceled.
	_ = connA.Close()
	select {
	case <-runner.canceled:
		// owner disconnect tore down the turn — success
	case <-time.After(3 * time.Second):
		t.Fatal("owner disconnect did not cancel the active turn")
	}
}
