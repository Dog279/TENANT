package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/plugins/discord"
	"tenant/internal/plugins/imessage"
)

// blockIMsgRunner holds a turn until its context is cancelled.
type blockIMsgRunner struct{}

func (blockIMsgRunner) Turn(ctx context.Context, _ agent.TurnRequest) (*agent.TurnResult, error) {
	<-ctx.Done()
	return &agent.TurnResult{}, nil
}

// A turn that Tenant stopping cuts (TEN-499) is recorded in interrupted.json
// and its channel told within 2 s, on both relays; the next start posts the
// boot notice once and removes the file; a start after that posts nothing;
// an entry whose surface has no relay is kept for the start after.
func TestInterruptedNotice(t *testing.T) {
	dir := t.TempDir()
	var stopping atomic.Bool

	fr := &fakeRunner{turns: make(chan string, 1), block: true}
	ds := &fakeSender{}
	r := newRelay(fr, ds, op)
	r.dataDir = dir
	r.stopping = stopping.Load
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	r.handleInbound(discord.Inbound{MessageID: "100", AuthorID: op, ChannelID: "dm1", Content: "a long task"})
	if _, ok := recv(fr.turns, 2*time.Second); !ok {
		t.Fatal("the Discord turn did not start")
	}
	stopping.Store(true)
	cancel()
	if !waitSends(ds, 1, 2*time.Second) || ds.all()[0] != "dm1|"+stopNotice {
		t.Fatalf("the operator was not told within 2 s: %v", ds.all())
	}

	p := &fakeIMsgPoller{msgs: []imessage.InboundMessage{{Message: imessage.Message{Text: "text me back"}, RowID: 7, ChatGUID: "chat1"}}}
	s := &fakeIMsgSender{}
	resp := &imessageResponder{poller: p, sender: s, runner: blockIMsgRunner{}, confirm: denyAllConfirm, dataDir: dir, stopping: stopping.Load}
	ictx, icancel := context.WithCancel(context.Background())
	resp.drain(ictx)
	icancel()
	resp.waitIdle()
	if len(s.sent) != 1 || s.sent[0] != "chat1|"+stopNotice {
		t.Fatalf("the texter was not told: %v", s.sent)
	}

	entries, err := readInterrupted(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("interrupted.json: %v, %+v", err, entries)
	}
	if e := entries[0]; e.Surface != "discord" || e.Channel != "dm1" || e.InboundID != "100" || e.Excerpt != "a long task" || e.ToolsRun != 0 || e.At.IsZero() {
		t.Errorf("discord entry = %+v", e)
	}
	if e := entries[1]; e.Surface != "imessage" || e.Channel != "chat1" || e.InboundID != "7" || e.Excerpt != "text me back" {
		t.Errorf("imessage entry = %+v", e)
	}

	// The next start: one notice per entry through its surface; the file
	// goes once every entry is reported.
	var posted, notes []string
	send := map[string]sendFunc{
		"discord": func(_ context.Context, ch, text string) error {
			posted = append(posted, "discord:"+ch+"|"+text)
			return nil
		},
		"imessage": func(_ context.Context, ch, text string) error {
			posted = append(posted, "imessage:"+ch+"|"+text)
			return nil
		},
	}
	note := func(s string) { notes = append(notes, s) }
	resumeInterrupted(dir, send, note)
	if len(posted) != 2 || !strings.HasPrefix(posted[0], "discord:dm1|I was stopped at ") ||
		!strings.HasSuffix(posted[0], `while "a long task" (0 tool calls had run). Tell me to continue.`) ||
		!strings.HasPrefix(posted[1], "imessage:chat1|I was stopped at ") {
		t.Fatalf("boot notices = %q", posted)
	}
	if _, err := os.Stat(interruptedPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("interrupted.json after the notices: %v, want gone", err)
	}
	resumeInterrupted(dir, send, note)
	if len(posted) != 2 {
		t.Fatalf("a second start posted again: %q", posted)
	}

	// No relay for the surface: reported, kept.
	if err := recordInterrupted(dir, newInterrupted("imessage", "chat1", "8", "later", nil)); err != nil {
		t.Fatal(err)
	}
	notes = nil
	resumeInterrupted(dir, map[string]sendFunc{"discord": send["discord"]}, note)
	kept, _ := readInterrupted(dir)
	if len(posted) != 2 || len(kept) != 1 || kept[0].InboundID != "8" || len(notes) != 1 || !strings.Contains(notes[0], "kept for the next start") {
		t.Errorf("posted = %q, kept = %+v, notes = %q", posted, kept, notes)
	}
}
