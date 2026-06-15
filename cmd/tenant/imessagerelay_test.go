package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/agent"
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

func TestIMessageResponder_SkipsEmptyOrChatless(t *testing.T) {
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{
		inbound("guid1", "   "), // whitespace only
		inbound("", "hello"),    // no chat guid
	}}
	s := &fakeIMsgSender{}
	r := &fakeIMsgRunner{reply: "hi"}
	resp := &imessageResponder{poller: p, sender: s, runner: r, confirm: denyAllConfirm}

	resp.drain(context.Background())

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

	if r.turns != 0 {
		t.Errorf("degraded model must not drive a turn, got %d turns", r.turns)
	}
	if len(s.sent) != 1 || !strings.Contains(s.sent[0], "unavailable") {
		t.Errorf("should send an 'unavailable' notice, got %v", s.sent)
	}
}

func TestIMessageResponder_TurnErrorReplies(t *testing.T) {
	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{inbound("guid1", "boom")}}
	s := &fakeIMsgSender{}
	r := &fakeIMsgRunner{err: fmt.Errorf("model down")}
	resp := &imessageResponder{poller: p, sender: s, runner: r, confirm: denyAllConfirm}

	resp.drain(context.Background())

	if len(s.sent) != 1 || !strings.Contains(s.sent[0], "error") {
		t.Errorf("a turn error should send an apology, got %v", s.sent)
	}
	// The apology IS sent, so RecordSent fires for it (anti-loop on our own text).
	if len(p.recsT) != 1 {
		t.Errorf("the apology send should be recorded for anti-loop, got %v", p.recsT)
	}
}
