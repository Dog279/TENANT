package tui

import (
	"regexp"
	"strings"
	"testing"

	"tenant/internal/agent"
)

// Characters the cases below need, spelled as bytes so this file holds no
// invisible character itself.
const (
	bidiOverride = "\xe2\x80\xae" // U+202E
	zeroWidth    = "\xe2\x80\x8b" // U+200B
	c1CSI        = "\xc2\x9b"     // U+009B
	replacement  = "\xef\xbf\xbd" // U+FFFD
)

// anyEsc is an escape character with whatever follows it; ownStyle is the
// only sequence the app writes itself: a color or weight (SGR).
var (
	anyEsc   = regexp.MustCompile(`\x1b.?`)
	ownStyle = regexp.MustCompile(`^\x1b\[[0-9;:]*m`)
)

// drawnSafely fails if s, as the terminal would get it, holds anything but
// text, line breaks, tabs and the app's own styling.
func drawnSafely(t *testing.T, where, s string) {
	t.Helper()
	for _, at := range anyEsc.FindAllStringIndex(s, -1) {
		if !ownStyle.MatchString(s[at[0]:]) {
			end := at[0] + 12
			if end > len(s) {
				end = len(s)
			}
			t.Errorf("%s: an escape sequence the app did not write reached the screen: %q", where, s[at[0]:end])
		}
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && r != '\x1b' && drawsNothingSafe(r) {
			t.Errorf("%s: control character U+%04X reached the screen in %q", where, r, s)
			return
		}
	}
}

func TestTermText(t *testing.T) {
	for in, want := range map[string]string{
		"plain text, accents and a\ttab\nand a line":          "plain text, accents and a\ttab\nand a line",
		"\x1b[31mred\x1b[0m and \x1b[1;32mgreen\x1b[m":        "red and green",
		"first words\rsecond words":                           "first words\nsecond words",
		"dos\r\nline":                                         "dos\nline",
		"\x1b]0;a new title\x07after":                         "after",
		"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\": "link",
		// A string left open must not swallow what follows it.
		"\x1b]0;never closed, then the rest": "⟨U+001B⟩]0;never closed, then the rest",
		"clear \x1b[2J\x1b[H screen":         "clear  screen",
		"a stray \x1b escape":                "a stray ⟨U+001B⟩ escape",
		"cut off \x1b[3":                     "cut off ⟨U+001B⟩[3",
		"bell\x07 back\x08 nul\x00 del\x7f":  "bell⟨U+0007⟩ back⟨U+0008⟩ nul⟨U+0000⟩ del⟨U+007F⟩",
		"c1 " + c1CSI + "31m csi":            "c1 ⟨U+009B⟩31m csi",
		"not utf-8 \x9b31m":                  "not utf-8 " + replacement + "31m",
		// Format characters stay in ordinary text: they join and shape it.
		"zero" + zeroWidth + "width": "zero" + zeroWidth + "width",
	} {
		got := termText(in)
		if got != want {
			t.Errorf("termText(%q) = %q, want %q", in, got, want)
		}
		drawnSafely(t, "termText", got)
		if again := termText(got); again != got {
			t.Errorf("termText is not stable on its own output: %q then %q", got, again)
		}
	}
}

func TestPromptText(t *testing.T) {
	for in, want := range map[string]string{
		"go test ./...":                       "go test ./...",
		"first words\rsecond words":           "first words⟨U+000D⟩second words",
		"ls \x1b[8munseen\x1b[0m":             "ls ⟨U+001B⟩[8munseen⟨U+001B⟩[0m",
		"abc" + bidiOverride + "def":          "abc⟨U+202E⟩def",
		"a" + zeroWidth + "b":                 "a⟨U+200B⟩b",
		"top\n\n\n\n\nbottom":                 "top\n⟨4 blank lines⟩\nbottom",
		"bad byte \xff here":                  "bad byte " + replacement + " here",
		"two\r\nlines":                        "two\nlines",
		"\x1b]0;title\x07 keeps what it said": "⟨U+001B⟩]0;title⟨U+0007⟩ keeps what it said",
	} {
		got := promptText(in)
		if got != want {
			t.Errorf("promptText(%q) = %q, want %q", in, got, want)
		}
		drawnSafely(t, "promptText", got)
		if again := promptText(got); again != got {
			t.Errorf("promptText is not stable on its own output: %q then %q", got, again)
		}
	}
}

// unruly is text that would rewrite its line, clear the screen, retitle the
// window and reorder itself if a terminal got it as it is.
const unruly = "first words\rsecond words \x1b[2J\x1b]0;a title\x07\x1b[8m abc" + bidiOverride + "def\x07\x08"

// An approval asks a person to decide on what they read, so the prompt shows
// every character that would hide or move text, from both doors a request
// comes through: the approvals channel and the event that backs it up.
func TestApproval_PromptShowsWhatTheRequestReallySays(t *testing.T) {
	for name, deliver := range map[string]func(m *model){
		"channel": func(m *model) {
			m.Update(approvalMsg(ApprovalRequest{ID: "ap-1", Category: "exec\x1b[2J", Action: "os_exec\r", Detail: unruly}))
		},
		"event": func(m *model) {
			m.applyEvent(agent.Event{Kind: agent.EventApprovalPending,
				Approval: &agent.ApprovalEvent{ID: "ap-1", Category: "exec\x1b[2J", Action: "os_exec\r", Detail: unruly}})
		},
	} {
		m := readyChatModel(Config{})
		deliver(m)
		if len(m.pending) != 1 || m.pending[0].ID != "ap-1" {
			t.Fatalf("%s: pending = %+v, want the one request under its own id", name, m.pending)
		}
		var prompt *chatMsg
		for i := range m.msgs {
			if strings.Contains(m.msgs[i].content, "approval needed") {
				prompt = &m.msgs[i]
			}
		}
		if prompt == nil {
			t.Fatalf("%s: no approval prompt in the chat", name)
		}
		for _, want := range []string{
			"first words⟨U+000D⟩second words", "⟨U+001B⟩[2J", "⟨U+001B⟩]0;a title⟨U+0007⟩", "⟨U+001B⟩[8m", "abc⟨U+202E⟩def", "⟨U+0008⟩",
			"exec⟨U+001B⟩[2J", "os_exec⟨U+000D⟩",
		} {
			if !strings.Contains(prompt.content, want) || !strings.Contains(stripANSI(prompt.rendered), want) {
				t.Errorf("%s: the prompt does not show %q:\n%s", name, want, prompt.content)
			}
		}
		drawnSafely(t, name+" prompt (plain)", prompt.content)
		drawnSafely(t, name+" prompt (styled)", prompt.rendered)
		drawnSafely(t, name+" chat", m.renderChat())
		drawnSafely(t, name+" feed", strings.Join(m.feedLines, "\n"))
	}
}

// An approval another surface owns is only mentioned here; the mention is
// still text from a request.
func TestApproval_MirroredMentionIsDrawnSafely(t *testing.T) {
	m := readyChatModel(Config{})
	m.applyEvent(agent.Event{Kind: agent.EventApprovalPending,
		Approval: &agent.ApprovalEvent{ID: "beside-discord\x1b[2J", Category: "exec\r", Action: "os_exec\x07"}})
	m.applyEvent(agent.Event{Kind: agent.EventApprovalResolved,
		Approval: &agent.ApprovalEvent{ID: "beside-discord\x1b[2J", Outcome: "approved\r"}})
	feed := strings.Join(m.feedLines, "\n")
	drawnSafely(t, "feed", feed)
	for _, want := range []string{"discord⟨U+001B⟩[2J", "exec⟨U+000D⟩", "os_exec⟨U+0007⟩", "approved⟨U+000D⟩"} {
		if !strings.Contains(stripANSI(feed), want) {
			t.Errorf("the feed does not show %q:\n%s", want, stripANSI(feed))
		}
	}
}

// Whatever a model says, a tool is asked or a tool answers, the chat pane
// (rows closed and open) and the feed get text and the app's own styling,
// nothing else.
func TestEvents_NothingRawReachesTheScreen(t *testing.T) {
	m := readyChatModel(Config{Model: "glm"})
	m.busy = true
	m.applyEvent(agent.Event{Kind: agent.EventTurnStart, Text: "question " + unruly})
	m.applyEvent(agent.Event{Kind: agent.EventToken, Text: "Let me look. " + unruly + "\n\n"})
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "os_exec\x1b[2J", ToolCallID: "c1", Args: `{"command":"` + unruly + `"}`})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "os_exec\x1b[2J", ToolCallID: "c1", Result: "\x1b[32mPASS\x1b[0m " + unruly + "\nsecond line"})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Tool: "os_exec", ToolCallID: "c2", IsErr: true, Result: "failed " + unruly})
	m.applyEvent(agent.Event{Kind: agent.EventError, Text: "error " + unruly})
	m.applyEvent(agent.Event{Kind: agent.EventJudge, Text: "sent back " + unruly})
	m.applyEvent(agent.Event{Kind: agent.EventIngest, Text: "Discord: " + unruly})
	m.applyEvent(agent.Event{Kind: agent.EventCompact, Text: unruly})
	m.applyEvent(agent.Event{Kind: agent.EventToken, Text: "Done. " + unruly})
	m.applyEvent(agent.Event{Kind: agent.EventFinal, Text: "Done. " + unruly})
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Source: "cron", Agent: "cron:job\x1b[2J", Tool: "os_exec", Args: unruly})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Source: "cron", Agent: "cron:job\x1b[2J", Tool: "os_exec", Result: unruly})
	for _, k := range []agent.EventKind{agent.EventTeamBudget, agent.EventToolCall, agent.EventToolResult, agent.EventError} {
		m.applyTeamEvent(TeamEvent{AgentID: "helper\x1b[2J", Event: agent.Event{Kind: k, Tool: "web\rsearch", Args: unruly, Result: unruly, Text: unruly, IsErr: true}})
	}
	m.Update(sysMsg("improvement run " + unruly))
	m.applyResearchTimeline(ResearchTimelineUpdate{Kind: "started", Started: &ResearchStartedData{Question: unruly}})
	m.applyResearchTimeline(ResearchTimelineUpdate{Kind: "plan", Plan: &ResearchPlanData{SubQuestions: []string{unruly}}})
	m.applyResearchTimeline(ResearchTimelineUpdate{Kind: "agent_status", Agent: &ResearchAgentRow{ID: "helper\x1b[2J", SubQ: unruly, Status: "running\r"}})

	drawnSafely(t, "chat, rows closed", m.renderChat())
	m.toolsOpen = true
	open := m.renderChat()
	drawnSafely(t, "chat, rows open", open)
	drawnSafely(t, "feed", strings.Join(m.feedLines, "\n"))
	drawnSafely(t, "whole screen", m.View())
	// The tool's own colors are gone without a trace; the words meant to
	// overwrite the first ones are on their own line, both still there.
	plain := stripANSI(open)
	if !strings.Contains(plain, "PASS first words") || !strings.Contains(plain, "second words") {
		t.Errorf("the result's text did not survive:\n%s", plain)
	}
}

// The sender's own copy of a timeline update is not rewritten.
func TestSafeTimeline_CopiesWhatItChanges(t *testing.T) {
	row := &ResearchAgentRow{ID: "a", SubQ: "q\rsecond"}
	plan := &ResearchPlanData{SubQuestions: []string{"one\x07"}}
	got := safeTimeline(ResearchTimelineUpdate{Agent: row, Plan: plan})
	if row.SubQ != "q\rsecond" || plan.SubQuestions[0] != "one\x07" {
		t.Errorf("the sender's data was rewritten: %q, %q", row.SubQ, plan.SubQuestions[0])
	}
	if got.Agent.SubQ != "q\nsecond" || got.Plan.SubQuestions[0] != "one⟨U+0007⟩" {
		t.Errorf("safe copy = %q, %q", got.Agent.SubQ, got.Plan.SubQuestions[0])
	}
}
