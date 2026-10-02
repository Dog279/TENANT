package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs/event"
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
	mu         sync.Mutex
	sent       []string           // Send + every card post (content), in order
	acked      []string           // RespondInteraction contents (the ext-approve path)
	cards      []string           // SendCard (v2) contents
	updates    []string           // RespondUpdate contents: in-place verdicts (button ACKs)
	ephemerals []string           // RespondEphemeral texts
	edits      []string           // EditMessage contents: expiry / text-reply verdicts
	buttons    [][]discord.Button // the button row of each card post (the nonce lives in custom_id)
	updateBtns [][]discord.Button
	editBtns   [][]discord.Button

	cardFail       bool // SendCard rejects ⇒ the approver falls back to legacy buttons
	componentsFail bool // SendComponents rejects ⇒ the approver falls back to a text prompt
}

var errFakeRejected = errors.New("fake: rejected")

func (s *fakeSender) Send(_ context.Context, channelID, content string) error {
	s.mu.Lock()
	s.sent = append(s.sent, channelID+"|"+content)
	s.mu.Unlock()
	return nil
}

// SendComponents / SendCard record the prompt content like Send (so send
// counts still line up) plus the button row, and return a message id so the
// approver can rewrite the card later. A rejecting fake records nothing.
func (s *fakeSender) SendComponents(_ context.Context, channelID, content string, buttons []discord.Button) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.componentsFail {
		return "", errFakeRejected
	}
	s.sent = append(s.sent, channelID+"|"+content)
	s.buttons = append(s.buttons, buttons)
	return "msg-1", nil
}

func (s *fakeSender) SendCard(_ context.Context, channelID, content string, buttons []discord.Button) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cardFail {
		return "", errFakeRejected
	}
	s.sent = append(s.sent, channelID+"|"+content)
	s.cards = append(s.cards, content)
	s.buttons = append(s.buttons, buttons)
	return "msg-1", nil
}

func (s *fakeSender) RespondInteraction(_ context.Context, _, _, content string) error {
	s.mu.Lock()
	s.acked = append(s.acked, content)
	s.mu.Unlock()
	return nil
}

func (s *fakeSender) RespondUpdate(_ context.Context, _, _, content string, buttons []discord.Button, _ bool) error {
	s.mu.Lock()
	s.updates = append(s.updates, content)
	s.updateBtns = append(s.updateBtns, buttons)
	s.mu.Unlock()
	return nil
}

func (s *fakeSender) RespondEphemeral(_ context.Context, _, _, text string) error {
	s.mu.Lock()
	s.ephemerals = append(s.ephemerals, text)
	s.mu.Unlock()
	return nil
}

func (s *fakeSender) EditMessage(_ context.Context, _, _, content string, buttons []discord.Button, _ bool) error {
	s.mu.Lock()
	s.edits = append(s.edits, content)
	s.editBtns = append(s.editBtns, buttons)
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
func (s *fakeSender) cardsAll() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cards...)
}
func (s *fakeSender) updatesAll() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.updates...)
}
func (s *fakeSender) ephemeralsAll() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ephemerals...)
}
func (s *fakeSender) editsAll() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.edits...)
}
func (s *fakeSender) buttonsAt(i int) []discord.Button {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.buttons) {
		return nil
	}
	return append([]discord.Button(nil), s.buttons[i]...)
}
func (s *fakeSender) lastUpdateButtons() []discord.Button {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.updateBtns) == 0 {
		return nil
	}
	return append([]discord.Button(nil), s.updateBtns[len(s.updateBtns)-1]...)
}
func (s *fakeSender) lastEditButtons() []discord.Button {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.editBtns) == 0 {
		return nil
	}
	return append([]discord.Button(nil), s.editBtns[len(s.editBtns)-1]...)
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

// channelEvents captures the catalogued events a test causes, from any
// goroutine.
type channelEvents struct {
	mu     sync.Mutex
	events []*event.Event
}

func (c *channelEvents) Emit(_ context.Context, e *event.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *channelEvents) Audit(ctx context.Context, e *event.Event) error { c.Emit(ctx, e); return nil }

// matching lists the events of d with opcode op ("" for a plain event).
func (c *channelEvents) matching(d *event.Def, op string) []*event.Event {
	s := d.Spec()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*event.Event
	for _, e := range c.events {
		if e.Provider == s.Provider && e.ID == s.ID && e.Opcode == op {
			out = append(out, e)
		}
	}
	return out
}

// wait returns the first event of d with opcode op, waiting up to 2 s for it.
func (c *channelEvents) wait(d *event.Def, op string) *event.Event {
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if m := c.matching(d, op); len(m) > 0 {
			return m[0]
		}
	}
	return nil
}

func captureChannelEvents(t *testing.T) *channelEvents {
	t.Helper()
	c := &channelEvents{}
	event.Install(c)
	t.Cleanup(func() { event.Install(nil) })
	return c
}

// eventField is e's field key (a zero Field when absent).
func eventField(e *event.Event, key string) event.Field {
	for _, f := range e.Data {
		if f.Key == key {
			return f
		}
	}
	return event.Field{}
}

// failRunner's turns fail with err, or, blocking, end when their ctx does.
type failRunner struct {
	err   error
	block bool
}

func (f failRunner) Turn(ctx context.Context, _ agent.TurnRequest) (*agent.TurnResult, error) {
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, f.err
}
func (failRunner) Interject(string) {}

// An accepted message is recorded with its text masked; a failed turn fails
// its RelayTurn activity; a sender over the rate limit is recorded.
func TestRelay_RecordsMessagesAndTurns(t *testing.T) {
	c := captureChannelEvents(t)
	fs := &fakeSender{}
	r := newRelay(failRunner{err: errors.New("model down")}, fs, op)
	r.rl = newRateLimiter(1, time.Hour)
	r.Start(context.Background())

	r.handleInbound(discord.Inbound{MessageID: "m1", AuthorID: op, ChannelID: "dm1", Content: "secret plans"})
	in := c.wait(discord.Received, "")
	if in == nil || eventField(in, "message").Text() != "m1" || strings.Contains(in.Msg, "secret") {
		t.Fatalf("received = %+v", in)
	}
	if f := eventField(in, "text"); !f.Sensitive || f.Text() != "secret plans" {
		t.Errorf("the text must be kept, sensitive: %+v", f)
	}
	fail := c.wait(discord.RelayTurn, event.OpFail)
	if fail == nil || fail.Level != event.Error || !strings.Contains(eventField(fail, "error").Text(), "model down") {
		t.Fatalf("want the relay turn failed with its error, got %+v", fail)
	}

	r.handleInbound(discord.Inbound{MessageID: "m2", AuthorID: op, ChannelID: "dm1", Content: "again"})
	if e := c.wait(discord.Throttled, ""); e == nil || eventField(e, "author").Text() != op {
		t.Errorf("the throttled message was not recorded: %+v", e)
	}
}

// A turn the operator stops ends its RelayTurn activity as stopped, not failed.
func TestRelay_StoppedTurnIsNotAFailure(t *testing.T) {
	c := captureChannelEvents(t)
	r := newRelay(failRunner{block: true}, &fakeSender{}, op)
	r.Start(context.Background())
	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "long job"})
	if c.wait(discord.RelayTurn, event.OpStart) == nil {
		t.Fatal("the turn never started")
	}
	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "stop"})
	if c.wait(discord.RelayTurn, event.OpStop) == nil {
		t.Fatalf("a stopped turn must end as stopped; failed: %v", c.matching(discord.RelayTurn, event.OpFail))
	}
}

// While the model is degraded to the echo fallback, the relay must REFUSE
// rather than drive a turn — the remote operator must never get an echo stub.
func TestRelay_RefusesWhileDegraded(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 1)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, op)
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
	r := newRelay(fr, fs, op)
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

// TEN-232: an inbound DM that drives a turn is surfaced in the shared activity
// feed via the ingest hook; gated/non-operator messages are not.
func TestRelay_IngestSurfacesInbound(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 1)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, op)
	r.Start(context.Background())
	var ingested []string
	r.ingest = func(t string) { ingested = append(ingested, t) }

	// A non-operator message must not surface (gated before ingest).
	r.handleInbound(discord.Inbound{AuthorID: "intruder", ChannelID: "dm1", Content: "psst"})
	// The operator's DM surfaces.
	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "hello there"})

	if _, ok := recv(fr.turns, 2*time.Second); !ok {
		t.Fatal("operator DM did not start a turn")
	}
	if len(ingested) != 1 || ingested[0] != "hello there" {
		t.Fatalf("ingest should fire once for the operator DM only: %v", ingested)
	}
}

func TestRelay_GateDrops(t *testing.T) {
	fr := &fakeRunner{turns: make(chan string, 4)}
	fs := &fakeSender{}
	r := newRelay(fr, fs, op)
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
	r := newRelay(fr, fs, op)
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
	r := newRelay(cr, fs, op)
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
	r := newRelay(cr, &fakeSender{}, op)
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

// REVSEC-2: the relay stamps its broker's lock beside the approver, so a
// connector tool that never asks the broker (trusted service, allow rule)
// still refuses what Discord's exec mode locks; a sub-agent the turn spawns
// on the team's run window keeps both stamps.
func TestRelay_StampsTheLockForSubAgentsToo(t *testing.T) {
	cr := ctxCapRunner{ch: make(chan context.Context, 1)}
	fs := &fakeSender{}
	r := newRelay(cr, fs, op)
	r.approver = newDiscordApprover(fs, nil)
	b := newDiscordApprovalBroker(nil)
	b.SetLock(func(action, cat string) bool { return action == "mcp:x:delete" })
	r.confirm, r.locked = b.Confirm, b.Locked
	r.Start(context.Background())
	r.handleInbound(discord.Inbound{AuthorID: op, ChannelID: "dm1", Content: "do a thing"})
	var turn context.Context
	select {
	case turn = <-cr.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never ran")
	}
	sub := inheritExecSurface(context.Background(), turn) // a run window opened by a local turn
	for name, ctx := range map[string]context.Context{"turn": turn, "sub-agent": sub} {
		if !offsiteLocked(ctx, "mcp:x:delete") || offsiteLocked(ctx, "mcp:x:search") {
			t.Errorf("%s: the Discord lock isn't stamped", name)
		}
		if offsiteConfirmFrom(ctx) == nil || execSurfaceFrom(ctx) != surfaceDiscord {
			t.Errorf("%s: lost the Discord approver or surface", name)
		}
	}
	if offsiteLocked(context.Background(), "mcp:x:delete") {
		t.Error("a local turn has no lock")
	}
}

// fakeReader is the catch-up's ReadChannel: the same messages on every READY,
// newest first, as Discord returns them.
type fakeReader struct{ msgs []discord.Message }

func (f *fakeReader) ReadChannel(context.Context, string, int) ([]discord.Message, error) {
	return f.msgs, nil
}

// The DM cursor (TEN-499) is saved after each accepted message; a READY
// routes the messages after it, oldest first, a second READY routes none, a
// live message at or before the cursor is dropped, and a relay started fresh
// from the saved cursor catches up the same way.
func TestDiscordCatchUp(t *testing.T) {
	dir := t.TempDir()
	var got []string
	r := newRelay(&fakeRunner{}, &fakeSender{}, op)
	r.dataDir = dir
	r.ingest = func(text string) { got = append(got, text) }
	r.Start(context.Background())
	cursor := func() map[string]string {
		b, _ := os.ReadFile(filepath.Join(dir, "discord-cursor.json"))
		var m map[string]string
		_ = json.Unmarshal(b, &m)
		return m
	}
	r.handleInbound(discord.Inbound{MessageID: "100", AuthorID: op, ChannelID: "dm1", Content: "first"})
	if c := cursor(); c["dm1"] != "100" {
		t.Fatalf("cursor after the first message = %v", c)
	}
	reader := &fakeReader{msgs: []discord.Message{
		{ID: "103", ChannelID: "dm1", Content: "the bot's own reply", Author: discord.Author{ID: "bot", Bot: true}},
		{ID: "102", ChannelID: "dm1", Content: "third", Author: discord.Author{ID: op}},
		{ID: "101", ChannelID: "dm1", Content: "second", Author: discord.Author{ID: op}},
		{ID: "99", ChannelID: "dm1", Content: "before the cursor", Author: discord.Author{ID: op}},
	}}
	r.reader = reader
	r.ready("bot")
	want := []string{"first", "second", "third"}
	if !slices.Equal(got, want) || cursor()["dm1"] != "102" {
		t.Fatalf("routed %v (cursor %v), want %v with the cursor at 102", got, cursor(), want)
	}
	r.ready("bot")
	r.handleInbound(discord.Inbound{MessageID: "101", AuthorID: op, ChannelID: "dm1", Content: "second, replayed"})
	if !slices.Equal(got, want) {
		t.Fatalf("a second READY or a replayed message routed: %v", got)
	}

	r2 := newRelay(&fakeRunner{}, &fakeSender{}, op)
	r2.dataDir, r2.reader = dir, reader
	r2.ingest = func(text string) { got = append(got, text) }
	r2.Start(context.Background())
	r2.ready("bot")
	if !slices.Equal(got, want) {
		t.Fatalf("a fresh relay did not load the cursor: routed %v", got)
	}
}
