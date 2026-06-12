package tui

// eval_tui_test.go covers the /eval slash command's dispatch (TEN-196):
// argument parsing routes to the right EvalControl method, errors surface in
// chat, and a session without the control degrades to a clear notice.

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeEval struct {
	calls  []string
	setErr error
}

func (f *fakeEval) Status() string { f.calls = append(f.calls, "status"); return "nightly eval: off" }
func (f *fakeEval) SetEvery(spec string) (string, error) {
	f.calls = append(f.calls, "every|"+spec)
	if f.setErr != nil {
		return "", f.setErr
	}
	return "nightly eval: every " + spec, nil
}
func (f *fakeEval) SetAt(spec string) (string, error) {
	f.calls = append(f.calls, "at|"+spec)
	return "nightly eval: daily at " + spec, nil
}
func (f *fakeEval) Off() (string, error) {
	f.calls = append(f.calls, "off")
	return "nightly eval: off", nil
}
func (f *fakeEval) RunNow() (string, error) {
	f.calls = append(f.calls, "now")
	return "eval queued", nil
}
func (f *fakeEval) Trend(n int) string {
	f.calls = append(f.calls, "trend")
	return "trend table"
}

func newEvalModel(c EvalControl) *model {
	m := newModel(context.Background(), Config{Eval: c})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

func TestSlash_EvalDispatch(t *testing.T) {
	f := &fakeEval{}
	m := newEvalModel(f)

	m.handleSlash("/eval")
	m.handleSlash("/eval every 24h")
	m.handleSlash("/eval at 03:15")
	m.handleSlash("/eval off")
	m.handleSlash("/eval now")
	m.handleSlash("/eval trend 5")

	want := []string{"status", "every|24h", "at|03:15", "off", "now", "trend"}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, f.calls[i], want[i])
		}
	}
	if got := m.msgs[len(m.msgs)-1].content; got != "trend table" {
		t.Errorf("trend output = %q", got)
	}
}

func TestSlash_EvalErrorsAndUsage(t *testing.T) {
	f := &fakeEval{setErr: errors.New("bad interval")}
	m := newEvalModel(f)

	m.handleSlash("/eval every nonsense")
	if got := m.msgs[len(m.msgs)-1].content; !strings.Contains(got, "bad interval") {
		t.Errorf("control error should surface in chat, got %q", got)
	}

	m.handleSlash("/eval frobnicate")
	if got := m.msgs[len(m.msgs)-1].content; !strings.Contains(got, "usage: /eval") {
		t.Errorf("unknown subcommand should print usage, got %q", got)
	}
}

func TestSlash_EvalUnavailable(t *testing.T) {
	m := newModel(context.Background(), Config{}) // no Eval control
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/eval")
	if got := m.msgs[len(m.msgs)-1].content; !strings.Contains(got, "not available") {
		t.Errorf("missing control should say not available, got %q", got)
	}
}
