package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/agent"
	"tenant/internal/memory/assemble"
)

// readyChatModel is a model with a 120x30 terminal, so the panes have a width.
func readyChatModel(cfg Config) *model {
	m := newModel(context.Background(), cfg)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

var at = time.Date(2026, 10, 11, 12, 4, 0, 0, time.Local)

// A turn is a block: who, the time and the model on the header, the answer
// rendered from Markdown (no literal markup), a rule between turns.
func TestRenderChat_TurnBlocksRenderMarkdown(t *testing.T) {
	m := readyChatModel(Config{Model: "glm-5.3-flash"})
	m.msgs = append(m.msgs,
		chatMsg{role: "user", content: "plan it", at: at},
		chatMsg{role: "assistant", content: "# Plan\n\n- one\n- two\n\nUse `tenant backup create`.", at: at, model: "glm-5.3-flash", final: true},
		chatMsg{role: "user", content: "thanks", at: at},
	)
	out := stripANSI(m.renderChat())
	for _, want := range []string{"● you · 12:04", "  plan it", "● tenant · 12:04 · glm-5.3-flash", "  Plan", "  • one", "  • two", "tenant backup create", "────"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"# Plan", "- one", "`tenant", "you plan it", "tenant # Plan"} {
		if strings.Contains(out, bad) {
			t.Errorf("markup or the old layout leaked, %q in:\n%s", bad, out)
		}
	}
}

// A tool call is one closed row inside the answer where it happened; Ctrl+O
// opens the rows to their arguments and results. The feed keeps its lines,
// indented under the turn's header.
func TestApplyEvent_ToolRowsCollapsedThenOpen(t *testing.T) {
	m := readyChatModel(Config{Model: "glm"})
	m.busy = true
	base := len(m.feedLines)
	m.applyEvent(agent.Event{Kind: agent.EventTurnStart, Text: "find the docs for me please"})
	m.applyEvent(agent.Event{Kind: agent.EventToken, Text: "Let me look.\n\n"})
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "web_search", ToolCallID: "c1", Args: `{"q":"tenant docs"}`})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "web_search", ToolCallID: "c1", Result: "three results about tenant\nsecond line of the result", Duration: 1200 * time.Millisecond})
	m.applyEvent(agent.Event{Kind: agent.EventToken, Text: "Found it."})
	m.applyEvent(agent.Event{Kind: agent.EventFinal, Text: "Found it."})

	out := stripANSI(m.renderChat())
	for _, want := range []string{"● tenant · ", "· glm", "  Let me look.", `⏺ web_search({"q":"tenant docs"}) ✓ 1.2s · three results about tenant`, "  Found it."} {
		if !strings.Contains(out, want) {
			t.Errorf("closed rows: missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "second line") {
		t.Errorf("a closed row showed its result body:\n%s", out)
	}
	if i, j := strings.Index(out, "Let me look."), strings.Index(out, "⏺ web_search"); i > j {
		t.Errorf("the row is not where the call happened (text at %d, row at %d)", i, j)
	}

	m.toolsOpen = true
	out = stripANSI(m.renderChat())
	for _, want := range []string{`⎿ {"q":"tenant docs"}`, "⎿ three results about tenant", "second line of the result"} {
		if !strings.Contains(out, want) {
			t.Errorf("open rows: missing %q in:\n%s", want, out)
		}
	}

	feed := m.feedLines[base:]
	if got := stripANSI(feed[0]); !strings.HasPrefix(got, "▸ ") || !strings.Contains(got, "find the docs for me please") {
		t.Errorf("turn header = %q", got)
	}
	if got := stripANSI(feed[1]); !strings.HasPrefix(got, "  → web_search") {
		t.Errorf("tool line not indented under the header: %q", got)
	}
}

// The final answer from the turn result is the tail of what streamed when the
// turn called tools, so the streamed text and its rows stay; a plain answer
// is replaced by the authoritative text as before.
func TestFinalizeAssistant_KeepsStreamedTextAroundToolRows(t *testing.T) {
	m := readyChatModel(Config{})
	m.busy = true
	m.applyEvent(agent.Event{Kind: agent.EventToken, Text: "Checking.\n\n"})
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "os_read", Args: `{"p":"x"}`})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "os_read", Result: "ok"})
	m.applyEvent(agent.Event{Kind: agent.EventToken, Text: "Done: it works."})
	m.finalizeAssistant("Done: it works.")
	last := m.msgs[len(m.msgs)-1]
	if last.content != "Checking.\n\nDone: it works." || !last.final || len(last.tools) != 1 || last.tools[0].pos == 0 {
		t.Fatalf("streamed text not kept: content=%q final=%v tools=%+v", last.content, last.final, last.tools)
	}

	m2 := readyChatModel(Config{})
	m2.busy = true
	m2.applyEvent(agent.Event{Kind: agent.EventToken, Text: "partial"})
	m2.finalizeAssistant("the whole answer")
	if got := m2.msgs[len(m2.msgs)-1]; got.content != "the whole answer" || !got.final {
		t.Fatalf("plain answer not replaced: %+v", got)
	}
}

// A failed call is open without Ctrl+O, with its error.
func TestRenderChat_FailedToolRowIsOpen(t *testing.T) {
	m := readyChatModel(Config{})
	m.busy = true
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "os_exec", Args: `{"cmd":"rm -rf /"}`})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "os_exec", IsErr: true, Result: "blocked: needs your approval"})
	out := stripANSI(m.renderChat())
	if !strings.Contains(out, "⏺ os_exec(") || !strings.Contains(out, "✗") || !strings.Contains(out, "blocked: needs your approval") {
		t.Fatalf("failed row not open with its error:\n%s", out)
	}
}

// While streaming, the closed paragraphs render as Markdown and the open tail
// stays plain; the line under the answer says what the agent is doing.
func TestRenderChat_StreamingTailAndLiveLine(t *testing.T) {
	m := readyChatModel(Config{})
	m.busy = true
	m.applyEvent(agent.Event{Kind: agent.EventThinking, Iter: 1})
	m.applyEvent(agent.Event{Kind: agent.EventToken, Text: "## Done so far\n\n- step one\n\nNow the *open"})
	out := stripANSI(m.renderChat())
	for _, want := range []string{"Done so far", "• step one", "Now the *open", "thinking…"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "## Done") || strings.Contains(out, "- step one") {
		t.Errorf("closed paragraphs not rendered:\n%s", out)
	}
}

// The context line is one line: the total against the window, then the
// static breakdown and the working set.
func TestApplyEvent_ContextBudgetIsOneLine(t *testing.T) {
	m := readyChatModel(Config{})
	base := len(m.feedLines)
	m.applyEvent(agent.Event{Kind: agent.EventMemory, Budget: &assemble.BudgetReport{
		Total: 12300, ContextWindow: 30000, SoulTokens: 1200, SystemTokens: 2000, ToolTokens: 4900, WorkingTokens: 3900,
	}})
	if n := len(m.feedLines) - base; n != 1 {
		t.Fatalf("context budget took %d lines, want 1", n)
	}
	got := stripANSI(lastFeed(m))
	for _, want := range []string{"ctx 12.3k tok (41%)", "soul 1.2k", "sys 2.0k", "tools 4.9k", "working 3.9k"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// wrap breaks lines at spaces, never inside a word; a word wider than the
// pane is still broken at the column.
func TestWrap_BreaksAtWords(t *testing.T) {
	out := wrap("aaa bbb ccc dddd", 7)
	for _, line := range strings.Split(out, "\n") {
		for _, w := range strings.Fields(line) {
			switch w {
			case "aaa", "bbb", "ccc", "dddd":
			default:
				t.Errorf("word split by the wrap: %q in %q", w, out)
			}
		}
		if len(line) > 7 {
			t.Errorf("line too wide: %q", line)
		}
	}
	if strings.ReplaceAll(out, "\n", "") != "aaa bbb ccc dddd" {
		t.Errorf("content changed: %q", out)
	}
}

// /feed off, Alt+F and the saved choice: the feed pane goes, the chat takes
// the width, the hook records on/off, and FeedOff starts hidden.
func TestFeedToggle_CommandKeyAndSavedChoice(t *testing.T) {
	var saved []bool
	m := readyChatModel(Config{SaveFeed: func(on bool) error { saved = append(saved, on); return nil }})
	wide := m.chat.Width
	if !strings.Contains(stripANSI(m.View()), "activity") {
		t.Fatal("the feed should start shown")
	}
	m.handleSlash("/feed off")
	if !m.feedOff || strings.Contains(stripANSI(m.View()), "activity") || m.chat.Width <= wide {
		t.Fatalf("after /feed off: off=%v width %d→%d", m.feedOff, wide, m.chat.Width)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: true})
	if m.feedOff || m.chat.Width != wide {
		t.Fatalf("Alt+F did not bring the feed back: off=%v width=%d", m.feedOff, m.chat.Width)
	}
	if len(saved) != 2 || saved[0] || !saved[1] {
		t.Fatalf("saved choices = %v, want [false true]", saved)
	}
	if h := readyChatModel(Config{FeedOff: true}); !h.feedOff || strings.Contains(stripANSI(h.View()), "activity") {
		t.Fatal("FeedOff did not start the app with the feed hidden")
	}
}
