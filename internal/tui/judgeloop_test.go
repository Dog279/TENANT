package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/agent"
)

type fakeJudgeLoop struct {
	on   bool
	sets []string // "on|endpoint|model"
}

func (f *fakeJudgeLoop) Status() string {
	if f.on {
		return "judge loop: on — clef-flash"
	}
	return "judge loop: off"
}
func (f *fakeJudgeLoop) On() (bool, string) { return f.on, "clef-flash" }
func (f *fakeJudgeLoop) Set(on bool, endpoint, model string) (string, error) {
	f.sets = append(f.sets, map[bool]string{true: "on", false: "off"}[on]+"|"+endpoint+"|"+model)
	f.on = on
	return f.Status(), nil
}

// TEN-510: /judge loop shows, switches on (with a server and model) and off;
// /model names the judge while it's on; a judge event shows as a ⚖ line.
func TestSlash_JudgeLoop(t *testing.T) {
	fj := &fakeJudgeLoop{}
	fm := &fakeModels{infos: []ModelInfo{{Name: "dgxt", Kind: "vllm", Model: "glm-5.3-flash", Active: true}}}
	m := newModel(context.Background(), Config{JudgeLoop: fj, Models: fm})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	last := func() string { return m.msgs[len(m.msgs)-1].content }

	m.handleSlash("/judge loop")
	if !strings.Contains(last(), "judge loop: off") {
		t.Fatalf("status: %q", last())
	}
	if plain, _ := m.renderModelList(); strings.Contains(plain, "⚖") {
		t.Fatal("/model names a judge that's off")
	}
	m.handleSlash("/judge loop on http://judge:30000 clef-flash")
	m.handleSlash("/judge loop off")
	m.handleSlash("/judge loop on")
	if strings.Join(fj.sets, ",") != "on|http://judge:30000|clef-flash,off||,on||" {
		t.Fatalf("Set calls %v", fj.sets)
	}
	if plain, _ := m.renderModelList(); !strings.Contains(plain, "⚖ judge clef-flash checks the active model's task answers") {
		t.Fatalf("/model while on:\n%s", plain)
	}
	m.handleSlash("/judge loop maybe")
	if !strings.Contains(last(), "usage: /judge loop") {
		t.Fatalf("bad arg: %q", last())
	}
	base := len(m.feedLines)
	m.applyEvent(agent.Event{Kind: agent.EventJudge, Text: "sent back (1/3): every_part 0.09"})
	if len(m.feedLines) != base+1 || !strings.Contains(m.feedLines[base], "⚖ judge sent back (1/3): every_part 0.09") {
		t.Fatalf("feed: %v", m.feedLines[base:])
	}
}
