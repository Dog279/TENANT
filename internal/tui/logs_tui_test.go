package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/logs/event"
)

// fakeLogs is a stub LogsControl that records calls and serves seeded events.
type fakeLogs struct {
	evs       []event.Event
	err       error
	queries   []string
	live      chan event.Event
	followed  string // filter@cursor
	followCtx context.Context
	details   []uint64
	timelines []string
}

func (f *fakeLogs) Query(_ context.Context, filter string, limit int) ([]event.Event, string, error) {
	f.queries = append(f.queries, fmt.Sprintf("%s|%d", filter, limit))
	if f.err != nil {
		return nil, "", f.err
	}
	return append([]event.Event(nil), f.evs...), "cur", nil
}

func (f *fakeLogs) Follow(ctx context.Context, filter, cursor string) <-chan event.Event {
	f.followed, f.followCtx = filter+"@"+cursor, ctx
	return f.live
}

func (f *fakeLogs) Event(_ context.Context, seq uint64) (string, error) {
	f.details = append(f.details, seq)
	return fmt.Sprintf("Event %d · error · system/3101\n  data.key     «sensitive»\n  msg          \x1b[2Jcleared\n\n### What to do\nRestart it.\n", seq), nil
}

func (f *fakeLogs) Timeline(_ context.Context, id string) (string, []event.Event, error) {
	f.timelines = append(f.timelines, id)
	return "Run " + id + ": ok", []event.Event{
		{Seq: 1, Provider: "turns", Level: event.Info, Msg: "turn started", Corr: event.Correlation{TraceID: id}},
		{Seq: 7, Provider: "tools", Level: event.Error, Msg: "tool failed", Corr: event.Correlation{TraceID: id}},
		{Seq: 9, Provider: "turns", Level: event.Info, Msg: "turn ended", Corr: event.Correlation{TraceID: id}},
	}, nil
}

func logsEvents(n int) []event.Event {
	at := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	out := make([]event.Event, n)
	for i := range out {
		out[i] = event.Event{Seq: uint64(i + 1), Log: "cron", Provider: "cron", Time: at.Add(time.Duration(i) * time.Second),
			Level: event.Warning, Msg: fmt.Sprintf("event number %d", i+1)}
	}
	return out
}

func newLogsModel(c *fakeLogs) *model {
	m := newModel(context.Background(), Config{Logs: c})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

// step runs cmd as the program would and feeds its message back, returning
// the command that follows.
func step(t *testing.T, m *model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command to run")
	}
	msg := cmd()
	if _, ok := msg.(tea.BatchMsg); ok {
		t.Fatal("got a batch; step runs one command")
	}
	_, next := m.Update(msg)
	return next
}

func logsKeyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press sends a key and runs the request it starts, if any.
func press(t *testing.T, m *model, k string) {
	t.Helper()
	if _, cmd := m.Update(logsKeyMsg(k)); cmd != nil {
		step(t, m, cmd)
	}
}

func TestLogs_ListShowsNewestSelected(t *testing.T) {
	fl := &fakeLogs{evs: logsEvents(3)}
	m := newLogsModel(fl)
	step(t, m, m.handleSlash("/logs level>=warning since:24h"))
	if len(fl.queries) != 1 || fl.queries[0] != "level>=warning since:24h|200" {
		t.Fatalf("queries = %v", fl.queries)
	}
	if m.logs == nil || m.logs.view() == nil {
		t.Fatal("the pane did not open")
	}
	out := stripANSI(m.View())
	for _, want := range []string{"/logs level>=warning since:24h · 3 events", "event number 1", "▶ ", "cron", "warning"} {
		if !strings.Contains(out, want) {
			t.Errorf("pane lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "▶ "+logsRow(fl.evs[2])) {
		t.Errorf("the newest event should be selected:\n%s", out)
	}
	press(t, m, "q")
	if m.logs != nil {
		t.Error("q should close the pane")
	}
}

func TestLogs_BadFilterShowsTheError(t *testing.T) {
	fl := &fakeLogs{err: errors.New(`logs: query: unknown field "bogus" (HTTP 400)`)}
	m := newLogsModel(fl)
	step(t, m, m.handleSlash("/logs bogus:1"))
	if m.logs != nil {
		t.Error("a filter that doesn't parse must not leave the pane open")
	}
	if !strings.Contains(lastMsg(m), `unknown field "bogus"`) {
		t.Errorf("the parse error should show; got %q", lastMsg(m))
	}
}

func TestLogs_ScrollKeepsTheSelectionInSight(t *testing.T) {
	m := newLogsModel(&fakeLogs{evs: logsEvents(100)})
	step(t, m, m.handleSlash("/logs"))
	v := m.logs.view()
	rows := m.logsRows()
	m.renderLogs()
	if v.sel != 99 || v.top != 100-rows {
		t.Fatalf("start: sel=%d top=%d, want 99 and %d", v.sel, v.top, 100-rows)
	}
	press(t, m, "up")
	press(t, m, "pgup")
	if want := 98 - (rows - 1); v.sel != want || v.top > v.sel {
		t.Fatalf("after up and pgup: sel=%d top=%d, want sel %d in sight", v.sel, v.top, want)
	}
	press(t, m, "g")
	if v.sel != 0 || v.top != 0 {
		t.Fatalf("g: sel=%d top=%d", v.sel, v.top)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "▶ "+logsRow(v.events[0])) || strings.Contains(out, "event number 99") {
		t.Errorf("the top of the list should show:\n%s", out)
	}
	press(t, m, "G")
	if v.sel != 99 {
		t.Fatalf("G: sel=%d", v.sel)
	}
}

func TestLogs_EnterOpensTheDetail(t *testing.T) {
	fl := &fakeLogs{evs: logsEvents(3)}
	m := newLogsModel(fl)
	step(t, m, m.handleSlash("/logs"))
	press(t, m, "up")
	press(t, m, "enter")
	if len(fl.details) != 1 || fl.details[0] != 2 {
		t.Fatalf("details = %v, want [2]", fl.details)
	}
	out := stripANSI(m.View())
	for _, want := range []string{"event 2", "data.key     «sensitive»", "What to do", "Restart it.", "cleared"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(m.View(), "\x1b[2J") {
		t.Error("an escape sequence in the detail reached the screen")
	}
	press(t, m, "esc")
	if v := m.logs.view(); v == nil || v.text != nil {
		t.Fatal("esc should go back to the list")
	}
	press(t, m, "esc")
	if m.logs != nil {
		t.Error("esc on the list should close the pane")
	}
}

func TestLogs_CorrelatedTimeline(t *testing.T) {
	evs := logsEvents(2)
	evs[1].Seq, evs[1].Corr.TraceID = 7, "3f2a9c"
	fl := &fakeLogs{evs: evs}
	m := newLogsModel(fl)
	step(t, m, m.handleSlash("/logs"))
	press(t, m, "c")
	if len(fl.timelines) != 1 || fl.timelines[0] != "3f2a9c" {
		t.Fatalf("timelines = %v", fl.timelines)
	}
	v := m.logs.view()
	if len(v.events) != 3 || v.events[v.sel].Seq != 7 {
		t.Fatalf("the timeline should open on the event it came from: %+v", v)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "correlated · Run 3f2a9c: ok") || !strings.Contains(out, "turn started") {
		t.Errorf("timeline:\n%s", out)
	}
	// From the timeline, an event's detail, and c there pivots again.
	press(t, m, "enter")
	press(t, m, "c")
	if len(fl.timelines) != 2 || len(m.logs.views) != 4 {
		t.Errorf("c from a detail: timelines=%v views=%d", fl.timelines, len(m.logs.views))
	}
	// An event with nothing to correlate says so and stays put.
	m.logs.views = m.logs.views[:1]
	press(t, m, "up")
	press(t, m, "c")
	if len(fl.timelines) != 2 || !strings.Contains(m.logsHint(), "no trace or activity") {
		t.Errorf("no pivot: timelines=%v hint=%q", fl.timelines, m.logsHint())
	}
}

func TestLogs_FollowTailsLive(t *testing.T) {
	fl := &fakeLogs{evs: logsEvents(2), live: make(chan event.Event, 4)}
	m := newLogsModel(fl)
	listen := step(t, m, m.handleSlash("/logs follow log:cron"))
	if fl.followed != "log:cron@cur" {
		t.Fatalf("follow = %q, want the filter from the query's cursor", fl.followed)
	}
	fl.live <- event.Event{Seq: 3, Provider: "cron", Level: event.Error, Msg: "live one"}
	listen = step(t, m, listen)
	v := m.logs.views[0]
	if len(v.events) != 3 || v.sel != 2 {
		t.Fatalf("live event: %d events, sel %d; want 3 with the newest selected", len(v.events), v.sel)
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "live one") || !strings.Contains(out, "· live") {
		t.Errorf("tail:\n%s", out)
	}
	// Moved off the newest, the selection stays put as more arrive.
	press(t, m, "up")
	fl.live <- event.Event{Seq: 4, Provider: "cron", Level: event.Info, Msg: "live two"}
	step(t, m, listen)
	if len(v.events) != 4 || v.sel != 1 {
		t.Errorf("after moving up: %d events, sel %d", len(v.events), v.sel)
	}
	press(t, m, "esc")
	if m.logs != nil || fl.followCtx.Err() == nil {
		t.Error("closing the pane must end the live tail")
	}
}

// Log text is untrusted: escape sequences never reach the terminal (§11).
func TestLogs_EscapeSequencesStripped(t *testing.T) {
	evs := logsEvents(1)
	evs[0].Msg = "\x1b[31mred\x1b[0m alert\x1b]0;evil title\x07 done\r\nnext\u009b2J"
	m := newLogsModel(&fakeLogs{evs: evs})
	step(t, m, m.handleSlash("/logs"))
	out := m.View()
	for _, bad := range []string{"\x1b[31m", "\x1b]0;", "evil title", "\x07", "\r", "\u009b"} {
		if strings.Contains(out, bad) {
			t.Errorf("the screen got %q", bad)
		}
	}
	if !strings.Contains(stripANSI(out), "red alert done  next 2J") {
		t.Errorf("the text itself should stay:\n%s", stripANSI(out))
	}
	if got := logsText("a\x1bPdcs\x1b\\b\x1bcc\x1b[?25ld"); got != "abcd" {
		t.Errorf("logsText = %q", got)
	}
}

func TestLogs_Unavailable(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if cmd := m.handleSlash("/logs"); cmd != nil || m.logs != nil {
		t.Fatal("no control: nothing to run")
	}
	if !strings.Contains(lastMsg(m), "not available") {
		t.Errorf("got %q", lastMsg(m))
	}
}

// The activity feed scrolls from the keyboard, so it still scrolls after
// /mouse off.
func TestFeed_KeyboardScroll(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	for i := 0; i < 200; i++ {
		m.appendFeed(fmt.Sprintf("feed line %d", i))
	}
	m.refresh()
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp, Alt: true})
	if m.feedFollow || m.feed.AtBottom() {
		t.Fatal("Alt+PgUp should scroll the feed up")
	}
	if !m.chatFollow {
		t.Error("Alt+PgUp scrolled the chat too")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlPgUp})
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	off := m.feed.YOffset
	m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
	if m.feed.YOffset != off+1 {
		t.Errorf("Alt+↓: offset %d, want %d", m.feed.YOffset, off+1)
	}
	for i := 0; i < 20 && !m.feedFollow; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyCtrlPgDown})
	}
	if !m.feedFollow {
		t.Error("paging back to the bottom should follow the feed again")
	}
}
