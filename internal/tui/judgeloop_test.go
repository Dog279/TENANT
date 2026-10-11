package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/agent"
)

type fakeJudge struct {
	on       bool
	ep       string
	key      string
	hasKey   bool
	calls    []string // "set:on", "use:kind|endpoint|model", "key:<len>", "rounds:n"
	roundErr bool
}

func (f *fakeJudge) Status() string {
	if f.on {
		return "judge: on — clef-flash (clef) at " + f.ep
	}
	return "judge: off"
}
func (f *fakeJudge) On() (bool, string) { return f.on, "clef-flash (clef)" }
func (f *fakeJudge) Set(on bool) (string, error) {
	f.calls = append(f.calls, "set:"+map[bool]string{true: "on", false: "off"}[on])
	f.on = on
	return f.Status(), nil
}
func (f *fakeJudge) Use(kind, endpoint, model string) (string, error) {
	f.calls = append(f.calls, "use:"+kind+"|"+endpoint+"|"+model)
	f.ep, f.on = endpoint, true
	return f.Status(), nil
}
func (f *fakeJudge) Setup(kind, endpoint, key, model string) (string, error) {
	f.calls = append(f.calls, "setup:"+kind+"|"+endpoint+"|"+key+"|"+model)
	f.ep, f.on, f.key, f.hasKey = endpoint, true, key, key != ""
	return f.Status(), nil
}
func (f *fakeJudge) Rounds(n int) (string, error) {
	f.calls = append(f.calls, "rounds:"+strconv.Itoa(n))
	if n < 1 || n > 5 {
		return "", errors.New("rounds must be between 1 and 5")
	}
	return "judge: up to " + strconv.Itoa(n) + " rounds per answer", nil
}
func (f *fakeJudge) Saved() (string, bool) { return f.ep, f.hasKey }

// TEN-510, TEN-516: /judge shows, switches on and off, sets rounds; /model
// names the judge while it's on; a judge event shows as a ⚖ line; the old
// /judge loop spelling is usage.
func TestSlash_Judge(t *testing.T) {
	fj := &fakeJudge{ep: "http://judge:30000"}
	fm := &fakeModels{infos: []ModelInfo{{Name: "dgxt", Kind: "vllm", Model: "glm-5.3-flash", Active: true}}}
	m := newModel(context.Background(), Config{Judge: fj, Models: fm})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	last := func() string { return m.msgs[len(m.msgs)-1].content }

	m.handleSlash("/judge")
	if !strings.Contains(last(), "judge: off") {
		t.Fatalf("status: %q", last())
	}
	if plain, _ := m.renderModelList(); strings.Contains(plain, "⚖") {
		t.Fatal("/model names a judge that's off")
	}
	run := func(line string) {
		if cmd := m.handleSlash(line); cmd != nil {
			m.Update(cmd()) // the switch runs off the UI goroutine
		}
	}
	run("/judge on")
	if !strings.Contains(last(), "⚖ judge: on") {
		t.Fatalf("on: %q", last())
	}
	if plain, _ := m.renderModelList(); !strings.Contains(plain, "⚖ judge clef-flash (clef) checks the active model's task answers") {
		t.Fatalf("/model while on:\n%s", plain)
	}
	run("/judge off")
	run("/judge rounds 2")
	if !strings.Contains(last(), "up to 2 rounds") {
		t.Fatalf("rounds: %q", last())
	}
	run("/judge rounds 9")
	if !strings.Contains(last(), "between 1 and 5") {
		t.Fatalf("rounds 9: %q", last())
	}
	if got := strings.Join(fj.calls, ","); got != "set:on,set:off,rounds:2,rounds:9" {
		t.Fatalf("calls %v", fj.calls)
	}
	m.handleSlash("/judge loop on http://x")
	if !strings.Contains(last(), "usage: /judge") {
		t.Fatalf("old spelling: %q", last())
	}
	base := len(m.feedLines)
	m.applyEvent(agent.Event{Kind: agent.EventJudge, Text: "sent back (1/3): every_part 0.09"})
	if len(m.feedLines) != base+1 || !strings.Contains(m.feedLines[base], "⚖ judge sent back (1/3): every_part 0.09") {
		t.Fatalf("feed: %v", m.feedLines[base:])
	}
}

// /judge use clef prompts for the server's address, then its key (hidden),
// saves the key (blank clears it) and points the Judge at the server. Blank
// address keeps the saved one; /cancel leaves everything as it was.
func TestSlash_JudgeUsePrompts(t *testing.T) {
	fj := &fakeJudge{ep: "http://old:30000", hasKey: true}
	m := newModel(context.Background(), Config{Judge: fj})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	last := func() string { return m.msgs[len(m.msgs)-1].content }

	m.handleSlash("/judge use clef")
	if m.setupEntry == nil || !strings.Contains(last(), "server's address") || !strings.Contains(last(), "blank keeps http://old:30000") {
		t.Fatalf("address prompt: entry=%v msg=%q", m.setupEntry != nil, last())
	}
	m.input.SetValue("http://new:30000")
	if cmd := m.submit(); cmd != nil {
		t.Fatal("the address step should arm the key step, not run anything")
	}
	if m.setupEntry == nil || !m.setupEntry.masked || !strings.Contains(last(), "key") || !strings.Contains(last(), "blank removes it") {
		t.Fatalf("key prompt: %+v msg=%q", m.setupEntry, last())
	}
	m.input.SetValue("sek-1")
	cmd := m.submit()
	if cmd == nil {
		t.Fatal("the key step should run the setup")
	}
	m.Update(cmd())
	if got := strings.Join(fj.calls, ","); got != "setup:clef|http://new:30000|sek-1|" {
		t.Fatalf("calls %v", fj.calls)
	}
	if !strings.Contains(last(), "⚖ judge: on") || m.setupEntry != nil || m.input.EchoMode != textinput.EchoNormal {
		t.Fatalf("after setup: %q entry=%v echo=%v", last(), m.setupEntry != nil, m.input.EchoMode)
	}

	// Blank address keeps the saved one; blank key clears; a model argument passes through.
	fj.calls = nil
	m.handleSlash("/judge use clef clef-mini")
	m.input.SetValue("")
	m.submit()
	m.input.SetValue("")
	if cmd := m.submit(); cmd != nil {
		m.Update(cmd())
	}
	if got := strings.Join(fj.calls, ","); got != "setup:clef|http://new:30000||clef-mini" {
		t.Fatalf("defaults: %v", fj.calls)
	}

	// /cancel during the prompt changes nothing.
	fj.calls = nil
	m.handleSlash("/judge use clef")
	m.input.SetValue("http://x:1")
	m.submit() // on the masked key step now
	if m.input.EchoMode != textinput.EchoPassword {
		t.Fatal("the key step should hide what is typed")
	}
	m.handleSlash("/cancel")
	if m.setupEntry != nil || len(fj.calls) != 0 || m.input.EchoMode != textinput.EchoNormal {
		t.Fatalf("cancel: entry=%v calls=%v echo=%v", m.setupEntry != nil, fj.calls, m.input.EchoMode)
	}
}
