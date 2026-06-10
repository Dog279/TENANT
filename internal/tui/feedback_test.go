package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeFeedback struct {
	acks, undos int
	err         error
}

func (f *fakeFeedback) Ack() (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.acks++
	return "marked last turn as ack", nil
}
func (f *fakeFeedback) Undo() (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.undos++
	return "marked last turn as undo", nil
}

func TestAckUndoSlash(t *testing.T) {
	f := &fakeFeedback{}
	m := newModel(context.Background(), Config{Feedback: f})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	m.handleSlash("/ack")
	m.handleSlash("/undo")
	if f.acks != 1 || f.undos != 1 {
		t.Fatalf("acks=%d undos=%d want 1,1", f.acks, f.undos)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "undo") {
		t.Errorf("expected undo confirmation, got %q", last)
	}
}

func TestAckUndoUnavailable(t *testing.T) {
	m := newModel(context.Background(), Config{}) // no Feedback control
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.handleSlash("/ack") // must not panic
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "available") {
		t.Errorf("expected unavailable notice, got %q", last)
	}
}
