package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs/event"
)

func lastFeed(m *model) string { return m.feedLines[len(m.feedLines)-1] }

// While the model answers, the timer says it is thinking, so a slow model
// reads apart from a hung one; then the feed says how long it thought, the
// timer follows the tool calls, and the turn's end splits its time (TEN-342).
func TestApplyEvent_ThinkingShowsLiveThenHowLong(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.busy, m.reqStart = true, time.Now().Add(-5*time.Second)

	m.applyEvent(agent.Event{Kind: agent.EventThinking, Iter: 1})
	if got := m.reqTimer(); !strings.Contains(got, "· thinking… ") {
		t.Fatalf("timer while thinking = %q", got)
	}
	m.applyEvent(agent.Event{Kind: agent.EventThought, Iter: 1, Duration: 8200 * time.Millisecond})
	if got := lastFeed(m); !strings.Contains(got, "thought for 8.2s") {
		t.Fatalf("feed after the call = %q", got)
	}
	if got := m.reqTimer(); strings.Contains(got, "thinking") {
		t.Fatalf("timer after the call = %q", got)
	}
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Tool: "web_search"})
	if got := m.reqTimer(); !strings.Contains(got, "· tools… ") {
		t.Fatalf("timer while tools run = %q", got)
	}
	m.applyEvent(agent.Event{Kind: agent.EventThought, Iter: 2, Duration: time.Second, IsErr: true})
	if got := lastFeed(m); !strings.Contains(got, "thought for 1.0s (the call failed)") {
		t.Fatalf("a failed call = %q", got)
	}
	m.applyEvent(agent.Event{Kind: agent.EventTurnEnd, Duration: 252 * time.Second, Think: 220 * time.Second, Tools: 32 * time.Second})
	if got := lastFeed(m); !strings.Contains(got, "turn 4:12 — thinking 3:40 · tools 32.0s") {
		t.Fatalf("turn summary = %q", got)
	}
	if got := m.reqTimer(); strings.Contains(got, "·") {
		t.Fatalf("timer after the turn = %q", got)
	}
}

// A scheduled job's tool calls show in the feed as cron:<tool>, apart from a
// turn's; other agents' events still stay out of the main feed.
func TestApplyEvent_CronToolsAreNamedCron(t *testing.T) {
	m := newModel(context.Background(), Config{})
	base := len(m.feedLines)
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Agent: "tenant-cron", Source: "cron", Tool: "web_search", Args: `{"q":"x"}`})
	m.applyEvent(agent.Event{Kind: agent.EventToolResult, Agent: "tenant-cron", Source: "cron", Tool: "web_search", Result: "ok"})
	m.applyEvent(agent.Event{Kind: agent.EventFinal, Agent: "tenant-cron", Source: "cron", Text: "brief"})
	if n := len(m.feedLines) - base; n != 3 {
		t.Fatalf("cron events added %d lines, want 3", n)
	}
	for _, l := range m.feedLines[base : base+2] {
		if !strings.Contains(l, "cron:web_search") {
			t.Errorf("cron tool line = %q", l)
		}
	}
	if m.lastTool != "" {
		t.Errorf("a cron tool call became the turn's last tool: %q", m.lastTool)
	}
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Agent: "writer", Tool: "os_write"}) // a sub-agent's
	if n := len(m.feedLines) - base; n != 3 {
		t.Fatalf("a sub-agent's event reached the main feed")
	}
}

func TestApplyTeamEvent_ShowsASubAgentsThinking(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.applyTeamEvent(TeamEvent{AgentID: "writer", Event: agent.Event{Kind: agent.EventThought, Duration: 2500 * time.Millisecond}})
	if got := lastFeed(m); !strings.Contains(got, "[writer] thought for 2.5s") {
		t.Fatalf("sub-agent thought = %q", got)
	}
}

// /logs names a scheduled job's tool the way the feed does.
func TestLogs_RowNamesACronJobsTool(t *testing.T) {
	cron := event.WithLog(context.Background(), agent.Turns, "cron")
	e := *agent.ToolDone.Build(cron, "agent", "tenant-cron", "tool", "web_search", "duration", time.Second)
	if got := logsRow(e); !strings.Contains(got, "Tool cron:web_search finished in 1s") {
		t.Fatalf("cron row = %q", got)
	}
}
