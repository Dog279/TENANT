package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/plugins/discord"
)

type fakeRunner struct {
	turns chan string
	block bool // if set, Turn blocks until ctx is cancelled (for the stop test)
}

func (f *fakeRunner) Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	if f.turns != nil {
		f.turns <- req.UserQuery
	}
	if f.block {
		<-ctx.Done()
		return &agent.TurnResult{}, nil
	}
	return &agent.TurnResult{Response: "echo: " + req.UserQuery}, nil
}
func (f *fakeRunner) Interject(string) {}

type fakeSender struct {
	mu    sync.Mutex
	sent  []string
	acked []string // interaction ACK contents
}

func (s *fakeSender) Send(_ context.Context, channelID, content string) error {
	s.mu.Lock()
	s.sent = append(s.sent, channelID+"|"+content)
	s.mu.Unlock()
	return nil
}

// SendComponents + RespondInteraction make fakeSender a discordIO for the
// approver tests. SendComponents records the prompt content like Send (so the
// nonce-in-content assertions still work).
func (s *fakeSender) SendComponents(_ context.Context, channelID, content string, _ []discord.Button) (string, error) {
	s.mu.Lock()
	s.sent = append(s.sent, channelID+"|"+content)
	s.mu.Unlock()
	return "msg-1", nil
}

func (s *fakeSender) RespondInteraction(_ context.Context, _, _, content string) error {
	s.mu.Lock()
	s.acked = append(s.acked, content)
	s.mu.Unlock()
	return nil
}

func (s *fakeSender) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}
func (s *fakeSender) acks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acked...)
}

func waitSends(s *fakeSender, n int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(s.all()) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func recv(ch chan string, d time.Duration) (string, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-time.After(d):
		return "", false
	}
}

const op = "op123"

// While the model is degraded to the echo fallback, the relay must REFUSE
// rather than drive a turn — the remote operator must never get an echo stub.
func TestRelay_RefusesWhileDegraded(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 1)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, op, nil)
	r.degraded = func() bool { return true }
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "hello there"})

	if !waitSends(fs, 1, 2*time.Second) {
		t.Fatalf("expected a refusal reply, got %v", fs.all())
	}
	if !strings.Contains(strings.ToLower(strings.Join(fs.all(), "\n")), "unavailable") {
		t.Errorf("refusal should mention the model is unavailable: %v", fs.all())
	}
	select {
	case q := <-fr.turns:
		t.Errorf("degraded relay must NOT drive a turn, got %q", q)
	default:
	}
}

func TestRelay_OperatorDMDrivesTurn(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 1)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, op, nil)
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "hello there"})

	if q, ok := recv(fr.turns, 2*time.Second); !ok || q != "hello there" {
		t.Fatalf("turn not started with the DM text: %q ok=%v", q, ok)
	}
	if !waitSends(fs, 1, 2*time.Second) {
		t.Fatalf("expected the final answer, got %v", fs.all())
	}
	joined := strings.Join(fs.all(), "\n")
	if !strings.Contains(joined, "dm1|echo: hello there") {
		t.Errorf("reply not posted to the DM channel: %v", fs.all())
	}
}

func TestRelay_GateDrops(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 4)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, op, nil)
	r.Start(context.Background())

	drops := []discord.Inbound{
		{AuthorID: op, AuthorBot: true, ChannelID: "dm1", Content: "hi"},  // a bot
		{AuthorID: "intruder", ChannelID: "dm1", Content: "hi"},           // not the operator
		{AuthorID: op, GuildID: "guild1", ChannelID: "c1", Content: "hi"}, // a guild message, not a DM
		{AuthorID: op, ChannelID: "dm1", Content: "   "},                  // empty
	}
	for _, in := range drops {
		r.handleInbound(in)
	}
	if _, ok := recv(fr.turns, 200*time.Millisecond); ok {
		t.Error("a gated message should NOT start a turn")
	}
	if len(fs.all()) != 0 {
		t.Errorf("a gated message should produce no reply, got %v", fs.all())
	}
}

func TestRelay_StopKeyword(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 1), block: true}
	fs := &fakeSender{}
	r := newRelay(fr, fs, op, nil)
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "long task"})
	if _, ok := recv(fr.turns, 2*time.Second); !ok {
		t.Fatal("turn did not start")
	}
	// the turn is blocked; "stop" must cancel it and confirm.
	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "stop"})
	if !waitSends(fs, 1, 2*time.Second) {
		t.Fatal("stop produced no confirmation")
	}
	if !strings.Contains(strings.Join(fs.all(), "\n"), "stopped") {
		t.Errorf("expected a 'stopped' confirmation, got %v", fs.all())
	}
}

// ctxCapRunner captures the ctx its Turn is called with, so a test can assert
// the relay stamped the offsite approver onto it.
type ctxCapRunner struct{ ch chan context.Context }

func (c ctxCapRunner) Turn(ctx context.Context, _ agent.TurnRequest) (*agent.TurnResult, error) {
	c.ch <- ctx
	return &agent.TurnResult{Response: "ok"}, nil
}
func (c ctxCapRunner) Interject(string) {}

// With an approver set, the relay must STAMP the turn ctx so that dangerous
// tools (exec mode) route their approval to the origin-scoped button approver —
// never the local broker.
func TestRelay_StampsOffsiteApprover(t *testing.T) {
	cr := ctxCapRunner{ch: make(chan context.Context, 1)}
	fs := &fakeSender{}
	r := newRelay(cr, fs, op, nil)
	r.approver = newDiscordApprover(fs, nil)
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "do a thing"})
	select {
	case ctx := <-cr.ch:
		if offsiteConfirmFrom(ctx) == nil {
			t.Error("the relay must stamp the offsite approver onto the turn ctx")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn never ran")
	}
}

// With NO approver, the turn ctx is unstamped (a local-broker turn shape) — the
// stamp is approver-driven, not unconditional.
func TestRelay_NoApproverNoStamp(t *testing.T) {
	cr := ctxCapRunner{ch: make(chan context.Context, 1)}
	r := newRelay(cr, &fakeSender{}, op, nil)
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "hi"})
	select {
	case ctx := <-cr.ch:
		if offsiteConfirmFrom(ctx) != nil {
			t.Error("with no approver the turn ctx must not be stamped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn never ran")
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(3, time.Hour) // 3 tokens, negligible refill within the test
	allowed := 0
	for i := 0; i < 5; i++ {
		if rl.allow("u") {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("token bucket should allow exactly the capacity (3), allowed %d", allowed)
	}
}

func TestChunkMessage(t *testing.T) {
	if got := chunkMessage("short", 2000); len(got) != 1 || got[0] != "short" {
		t.Errorf("short text should be one chunk: %v", got)
	}
	big := strings.Repeat("x", 4500)
	chunks := chunkMessage(big, 2000)
	if len(chunks) != 3 {
		t.Fatalf("4500 chars / 2000 = 3 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len([]rune(c)) > 2000 {
			t.Errorf("chunk exceeds the 2000 limit: %d", len([]rune(c)))
		}
	}
	if strings.Join(chunks, "") != big {
		t.Error("chunks must reassemble to the original")
	}
}
