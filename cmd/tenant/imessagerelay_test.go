package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/logs/event"
	"tenant/internal/plugins/imessage"
)

type fakeIMsgPoller struct {
	msgs   []imessage.InboundMessage
	polled bool
	recsT  []string // RecordSent calls as "chat|text"
}

func (f *fakeIMsgPoller) Poll(context.Context, int) ([]imessage.InboundMessage, error) {
	if f.polled {
		return nil, nil
	}
	f.polled = true
	return f.msgs, nil
}
func (f *fakeIMsgPoller) RecordSent(chatGUID, text string) {
	f.recsT = append(f.recsT, chatGUID+"|"+text)
}

type fakeIMsgSender struct {
	sent []string // "chat|text"
	err  error
}

func (f *fakeIMsgSender) SendText(_ context.Context, chatGUID, text string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.sent = append(f.sent, chatGUID+"|"+text)
	return "ok", nil
}

type fakeIMsgRunner struct {
	reply       string
	err         error
	turns       int
	lastQuery   string
	gatedDenied bool // the stamped offsite-confirm refused (Phase-1 deny-all)
}

func (f *fakeIMsgRunner) Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	f.turns++
	f.lastQuery = req.UserQuery
	if c := offsiteConfirmFrom(ctx); c != nil {
		f.gatedDenied = !c(ctx, "run", "rm -rf /") // deny-all ⇒ false ⇒ denied
	}
	if f.err != nil {
		return nil, f.err
	}
	return &agent.TurnResult{Response: f.reply}, nil
}

func inbound(chat, text string) imessage.InboundMessage {
	return imessage.InboundMessage{Message: imessage.Message{Text: text}, ChatGUID: chat}
}

func TestIMessageResponder_DrivesTurnAndReplies(t *testing.T) {
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{inbound("guid1", "what's the weather?")}}
	s := &fakeIMsgSender{}
	r := &fakeIMsgRunner{reply: "It's sunny."}
	resp := &imessageResponder{poller: p, sender: s, runner: r, confirm: denyAllConfirm}

	resp.drain(context.Background())
	resp.waitIdle() // turns now run off the poll loop (TEN-230 Phase 2)

	if r.turns != 1 || r.lastQuery != "what's the weather?" {
		t.Fatalf("turn not driven with the inbound text: turns=%d q=%q", r.turns, r.lastQuery)
	}
	if len(s.sent) != 1 || s.sent[0] != "guid1|It's sunny." {
		t.Fatalf("reply not sent to the chat: %v", s.sent)
	}
	// Anti-loop: the reply is recorded so it doesn't loop back.
	if len(p.recsT) != 1 || p.recsT[0] != "guid1|It's sunny." {
		t.Fatalf("RecordSent not called with the reply: %v", p.recsT)
	}
	// Phase-1 gating: the turn ran with a deny-all offsite-confirm.
	if !r.gatedDenied {
		t.Error("turn ctx must carry a deny-all offsite-confirm (gated tools refused)")
	}
}

// A text is recorded with its handle, chat and text masked; a failed turn fails
// its Turn activity, and the apology is a Delivery.
func TestIMessageResponder_RecordsTurnAndDelivery(t *testing.T) {
	c := captureChannelEvents(t)
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{inbound("guid1", "call mom")}}
	resp := &imessageResponder{poller: p, sender: &fakeIMsgSender{}, runner: &fakeIMsgRunner{err: errors.New("model down")}, confirm: denyAllConfirm}

	resp.drain(context.Background())
	resp.waitIdle()

	in, fail, sent := c.wait(imessage.Received, ""), c.wait(imessage.Turn, event.OpFail), c.wait(imessage.Delivery, event.OpStop)
	if in == nil || fail == nil || sent == nil {
		t.Fatalf("want the message, the failed turn and the apology's delivery; got %v %v %v", in, fail, sent)
	}
	for _, f := range []event.Field{eventField(in, "text"), eventField(in, "chat"), eventField(fail, "chat"), eventField(sent, "chat")} {
		if !f.Sensitive || f.Text() == "" {
			t.Errorf("%s must be kept, sensitive: %+v", f.Key, f)
		}
	}
	if strings.Contains(in.Msg+fail.Msg+sent.Msg, "guid1") || strings.Contains(in.Msg, "call mom") {
		t.Errorf("a message names personal content: %q / %q / %q", in.Msg, fail.Msg, sent.Msg)
	}
}

// errPoller fails every poll with err.
type errPoller struct{ err error }

func (p *errPoller) Poll(context.Context, int) ([]imessage.InboundMessage, error) { return nil, p.err }
func (p *errPoller) RecordSent(string, string)                                    {}

// Every failed poll fails its (diagnostic) Poll activity, but the warning is
// written once per error: again only when the error changes.
func TestIMessageResponder_PollFailureRecordedOncePerError(t *testing.T) {
	c := captureChannelEvents(t)
	p := &errPoller{err: errors.New("unable to open chat.db")}
	resp := &imessageResponder{poller: p}
	resp.drain(context.Background())
	resp.drain(context.Background())
	p.err = errors.New("database is locked")
	resp.drain(context.Background())
	if n := len(c.matching(imessage.PollFailed, "")); n != 2 {
		t.Errorf("poll warnings = %d, want 2 (one per distinct error)", n)
	}
	if n := len(c.matching(imessage.Poll, event.OpFail)); n != 3 {
		t.Errorf("failed poll activities = %d, want 3", n)
	}
}

// TEN-232: an inbound that drives a turn is surfaced in the shared activity feed
// via the ingest hook, BEFORE the turn runs.
func TestIMessageResponder_IngestSurfacesInbound(t *testing.T) {
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{inbound("guid1", "what's the weather?")}}
	s := &fakeIMsgSender{}
	r := &fakeIMsgRunner{reply: "It's sunny."}
	var ingested []string
	resp := &imessageResponder{poller: p, sender: s, runner: r, confirm: denyAllConfirm,
		ingest: func(t string) { ingested = append(ingested, t) }}

	resp.drain(context.Background())
	resp.waitIdle() // turns now run off the poll loop (TEN-230 Phase 2)

	if len(ingested) != 1 || ingested[0] != "what's the weather?" {
		t.Fatalf("ingest hook should fire once with the inbound text: %v", ingested)
	}
}

// A message refused while degraded must NOT be surfaced as ingest (no turn ran).
func TestIMessageResponder_NoIngestWhenDegraded(t *testing.T) {
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{inbound("guid1", "do a thing")}}
	s := &fakeIMsgSender{}
	r := &fakeIMsgRunner{}
	var ingested []string
	resp := &imessageResponder{poller: p, sender: s, runner: r, confirm: denyAllConfirm,
		degraded: func() bool { return true },
		ingest:   func(t string) { ingested = append(ingested, t) }}

	resp.drain(context.Background())
	resp.waitIdle() // turns now run off the poll loop (TEN-230 Phase 2)

	if len(ingested) != 0 {
		t.Fatalf("degraded refusal must not surface ingest: %v", ingested)
	}
}

func TestIMessageResponder_SkipsEmptyOrChatless(t *testing.T) {
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{
		inbound("guid1", "   "), // whitespace only
		inbound("", "hello"),    // no chat guid
	}}
	s := &fakeIMsgSender{}
	r := &fakeIMsgRunner{reply: "hi"}
	resp := &imessageResponder{poller: p, sender: s, runner: r, confirm: denyAllConfirm}

	resp.drain(context.Background())
	resp.waitIdle() // turns now run off the poll loop (TEN-230 Phase 2)

	if r.turns != 0 {
		t.Errorf("no turn should run for empty/chatless messages, got %d", r.turns)
	}
	if len(s.sent) != 0 {
		t.Errorf("nothing should be sent, got %v", s.sent)
	}
}

func TestIMessageResponder_DegradedRefuses(t *testing.T) {
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{inbound("guid1", "do a thing")}}
	s := &fakeIMsgSender{}
	r := &fakeIMsgRunner{reply: "done"}
	resp := &imessageResponder{poller: p, sender: s, runner: r, confirm: denyAllConfirm,
		degraded: func() bool { return true }}

	resp.drain(context.Background())
	resp.waitIdle() // turns now run off the poll loop (TEN-230 Phase 2)

	if r.turns != 0 {
		t.Errorf("degraded model must not drive a turn, got %d turns", r.turns)
	}
	if len(s.sent) != 1 || !strings.Contains(s.sent[0], "unavailable") {
		t.Errorf("should send an 'unavailable' notice, got %v", s.sent)
	}
}

type fakeRunnable struct{ started chan struct{} }

func (f *fakeRunnable) Run(ctx context.Context) {
	if f.started != nil {
		close(f.started)
	}
	<-ctx.Done() // run until the manager cancels
}

// TEN-230 Phase 1c: the responder manager starts/stops live, reads the allowlist
// fresh at each Start, persists the enabled intent, and cleans up on Stop.
func TestIMessageResponderManager_StartStopLifecycle(t *testing.T) {
	var persisted []bool
	var lastAllow []string
	builds, cleanups := 0, 0
	started := make(chan struct{})
	mgr := &imessageResponderManager{
		base:      context.Background(),
		allowFrom: func() []string { return []string{"+15551234567", "a@b.com"} },
		persist:   func(on bool) error { persisted = append(persisted, on); return nil },
		buildFn: func(allow []string) (responderRunnable, func(), error) {
			builds++
			lastAllow = allow
			return &fakeRunnable{started: started}, func() { cleanups++ }, nil
		},
	}

	if mgr.On() {
		t.Fatal("manager should start OFF")
	}
	status, err := mgr.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !mgr.On() || !strings.Contains(status, "ON") {
		t.Fatalf("should be ON after Start (status=%q)", status)
	}
	<-started // the runnable goroutine actually launched
	if builds != 1 || len(lastAllow) != 2 {
		t.Fatalf("buildFn should run once with the fresh allowlist: builds=%d allow=%v", builds, lastAllow)
	}

	// Idempotent: a second Start doesn't rebuild.
	if s, _ := mgr.Start(); !strings.Contains(s, "already on") || builds != 1 {
		t.Fatalf("second Start should be a no-op: status=%q builds=%d", s, builds)
	}

	if _, err := mgr.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if mgr.On() {
		t.Fatal("should be OFF after Stop")
	}
	if cleanups != 1 {
		t.Errorf("cleanup should run exactly once on Stop, got %d", cleanups)
	}
	// Idempotent Stop.
	if s, _ := mgr.Stop(); !strings.Contains(s, "already off") {
		t.Errorf("second Stop should be a no-op: %q", s)
	}
	if len(persisted) != 2 || persisted[0] != true || persisted[1] != false {
		t.Errorf("enabled intent should persist true then false, got %v", persisted)
	}
}

func TestIMessageResponderManager_StartBuildErrorStaysOff(t *testing.T) {
	mgr := &imessageResponderManager{
		base:      context.Background(),
		allowFrom: func() []string { return nil },
		persist:   func(bool) error { t.Fatal("persist must not be called on a failed Start"); return nil },
		buildFn: func([]string) (responderRunnable, func(), error) {
			return nil, nil, fmt.Errorf("no Full Disk Access")
		},
	}
	if _, err := mgr.Start(); err == nil {
		t.Fatal("Start should surface the build error")
	}
	if mgr.On() {
		t.Fatal("a failed Start must leave the responder OFF")
	}
}

func TestIMessageResponder_TurnErrorReplies(t *testing.T) {
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{inbound("guid1", "boom")}}
	s := &fakeIMsgSender{}
	r := &fakeIMsgRunner{err: fmt.Errorf("model down")}
	resp := &imessageResponder{poller: p, sender: s, runner: r, confirm: denyAllConfirm}

	resp.drain(context.Background())
	resp.waitIdle() // turns now run off the poll loop (TEN-230 Phase 2)

	if len(s.sent) != 1 || !strings.Contains(s.sent[0], "error") {
		t.Errorf("a turn error should send an apology, got %v", s.sent)
	}
	// The apology IS sent, so RecordSent fires for it (anti-loop on our own text).
	if len(p.recsT) != 1 {
		t.Errorf("the apology send should be recorded for anti-loop, got %v", p.recsT)
	}
}

// denyAllConfirm refuses every gated tool.
func denyAllConfirm(context.Context, string, string) bool { return false }

// waitIdle blocks until no turn is in flight. Stop needs no such wait:
// cancelling the run ctx unwinds the turn.
func (r *imessageResponder) waitIdle() { r.wg.Wait() }
