package tui

// The log book: /logs (TEN-381, docs/design/logging-v2.md §9.3). A light
// view over control.LogsControl in the chat pane's place: the events a
// filter matches, newest at the bottom, or tailed live; enter opens one's
// detail, c its correlated timeline, esc or q goes back. The full viewer is
// the dashboard's Logs page (decision D12).

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tenant/internal/cmdreg"
	"tenant/internal/logs/event"
)

const (
	logsLimit = 200  // the newest events /logs shows
	logsKeep  = 2000 // the newest a live tail keeps
)

// logsPane is the open /logs: a stack of views, the first its list; esc pops
// one, and popping the last closes the pane.
type logsPane struct {
	ctx    context.Context
	cancel context.CancelFunc // ends its requests and its live tail
	views  []*logsView
	live   bool   // the first view is tailed
	note   string // the last key's error or hint, until the next key
}

// logsView is one screen: a list of events, or an event's detail as text.
type logsView struct {
	title  string
	events []event.Event // a list
	text   []string      // a detail, by line; nil in a list
	ev     event.Event   // the event a detail shows
	sel    int           // the selected event, in a list
	top    int           // the first line shown
}

func (p *logsPane) view() *logsView {
	if len(p.views) == 0 {
		return nil // the first answer is still coming
	}
	return p.views[len(p.views)-1]
}

// logsMsg is a /logs answer, applied on the UI goroutine.
type logsMsg interface{ apply(m *model) tea.Cmd }

// logsLoaded is a request's answer: a view to push, or why there is none.
type logsLoaded struct {
	pane *logsPane
	view *logsView
	err  error
	live <-chan event.Event // a live tail to listen to
}

// logsLive is one event from the live tail.
type logsLive struct {
	pane *logsPane
	live <-chan event.Event
	ev   event.Event
}

func (l logsLoaded) apply(m *model) tea.Cmd {
	p := l.pane
	if m.logs != p { // closed or replaced meanwhile
		return nil
	}
	if l.err != nil {
		if len(p.views) == 0 { // a filter that doesn't parse, or no log book here
			m.closeLogs()
			m.sysChat(l.err.Error())
			return nil
		}
		p.note = l.err.Error()
		return nil
	}
	p.views = append(p.views, l.view)
	if l.live != nil {
		return p.listen(l.live)
	}
	return nil
}

func (l logsLive) apply(m *model) tea.Cmd {
	p := m.logs
	if p != l.pane { // its context is done, so the channel closes
		return nil
	}
	v := p.views[0]
	tail := v.sel == len(v.events)-1 // on the newest: keep following it
	v.events = append(v.events, l.ev)
	if n := len(v.events) - logsKeep; n > 0 {
		v.events = slices.Delete(v.events, 0, n)
		v.sel, v.top = max(v.sel-n, 0), max(v.top-n, 0)
	}
	if tail {
		v.sel = len(v.events) - 1
	}
	return p.listen(l.live)
}

func (p *logsPane) listen(ch <-chan event.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		return logsLive{pane: p, live: ch, ev: e}
	}
}

// load asks the control for a view off the UI goroutine.
func (p *logsPane) load(fn func(ctx context.Context) (*logsView, error)) tea.Cmd {
	return func() tea.Msg {
		v, err := fn(p.ctx)
		return logsLoaded{pane: p, view: v, err: err}
	}
}

// slashLogs runs /logs [follow] [filter].
func slashLogs(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	lc := m.cfg.Logs
	if lc == nil {
		m.sysChat("the log book is not available in this session")
		return nil
	}
	filter, follow := arg, false
	if f := strings.Fields(arg); len(f) > 0 && strings.EqualFold(f[0], "follow") {
		filter, follow = strings.TrimSpace(arg[len(f[0]):]), true
	}
	m.closeLogs()
	ctx, cancel := context.WithCancel(m.ctx)
	p := &logsPane{ctx: ctx, cancel: cancel, live: follow}
	m.logs = p
	title := strings.TrimSpace("/logs " + arg)
	return func() tea.Msg {
		evs, cursor, err := lc.Query(ctx, filter, logsLimit)
		l := logsLoaded{pane: p, err: err}
		if err == nil {
			l.view = &logsView{title: title, events: evs, sel: len(evs) - 1}
			if follow {
				l.live = lc.Follow(ctx, filter, cursor)
			}
		}
		return l
	}
}

func (m *model) closeLogs() {
	if m.logs != nil {
		m.logs.cancel()
		m.logs = nil
	}
}

// logsKey handles a key while the pane is open: it takes them all.
func (m *model) logsKey(k tea.KeyMsg) tea.Cmd {
	p := m.logs
	p.note = ""
	switch k.String() {
	case "esc", "q", "ctrl+c":
		if len(p.views) > 1 {
			p.views = p.views[:len(p.views)-1]
		} else {
			m.closeLogs()
		}
		return nil
	}
	v := p.view()
	if v == nil {
		return nil
	}
	page := max(m.logsRows()-1, 1)
	switch k.String() {
	case "up", "k":
		m.logsMove(v, -1)
	case "down", "j":
		m.logsMove(v, 1)
	case "pgup":
		m.logsMove(v, -page)
	case "pgdown", " ":
		m.logsMove(v, page)
	case "home", "g":
		m.logsMove(v, -1<<30)
	case "end", "G":
		m.logsMove(v, 1<<30)
	case "enter":
		if v.text != nil || len(v.events) == 0 {
			return nil
		}
		e, lc := v.events[v.sel], m.cfg.Logs
		return p.load(func(ctx context.Context) (*logsView, error) {
			text, err := lc.Event(ctx, e.Seq)
			return &logsView{title: "event " + strconv.FormatUint(e.Seq, 10), text: strings.Split(strings.TrimRight(text, "\n"), "\n"), ev: e}, err
		})
	case "c":
		e := v.ev
		if v.text == nil {
			if len(v.events) == 0 {
				return nil
			}
			e = v.events[v.sel]
		}
		id := e.Corr.TraceID
		if id == "" {
			id = e.Corr.ActivityID
		}
		if id == "" {
			p.note = "this event has no trace or activity to correlate"
			return nil
		}
		lc := m.cfg.Logs
		return p.load(func(ctx context.Context) (*logsView, error) {
			head, evs, err := lc.Timeline(ctx, id)
			sel := max(slices.IndexFunc(evs, func(x event.Event) bool { return x.Seq == e.Seq }), 0)
			return &logsView{title: "correlated · " + head, events: evs, sel: sel}, err
		})
	}
	return nil
}

func (m *model) logsWheel(up bool) {
	if v := m.logs.view(); v != nil {
		if up {
			m.logsMove(v, -3)
		} else {
			m.logsMove(v, 3)
		}
	}
}

// logsRows is how many lines of a view show: the pane less its title.
func (m *model) logsRows() int { return max(m.chat.Height-1, 1) }

// logsMove moves a list's selection, or scrolls a detail, by n lines.
func (m *model) logsMove(v *logsView, n int) {
	if v.text != nil {
		v.top += n
	} else {
		v.sel += n
	}
	v.fit(m.logsRows(), len(m.logsLines(v)))
}

// fit keeps the selection on an event and in sight, and the view within its
// n lines, h at a time.
func (v *logsView) fit(h, n int) {
	if v.text == nil {
		v.sel = min(max(v.sel, 0), max(n-1, 0))
		v.top = min(max(v.top, v.sel-h+1), v.sel)
	}
	v.top = min(max(v.top, 0), max(n-h, 0))
}

// logsLines is a view's lines, made safe to show: a list's one per event, a
// detail's wrapped to the pane.
func (m *model) logsLines(v *logsView) []string {
	if v.text == nil {
		out := make([]string, len(v.events))
		for i, e := range v.events {
			out[i] = logsRow(e)
		}
		return out
	}
	var out []string
	for _, l := range v.text {
		out = append(out, strings.Split(wrap(logsText(l), m.chat.Width), "\n")...)
	}
	return out
}

func logsRow(e event.Event) string {
	name := e.Provider
	if e.ID != 0 {
		name += "/" + strconv.FormatUint(uint64(e.ID), 10)
	}
	return logsText(fmt.Sprintf("%s %-8s %-14s %s", e.Time.Local().Format("01-02 15:04:05"), e.Level, name, e.Msg))
}

// escSeq is a terminal escape sequence: a CSI, a string (OSC, DCS, SOS, PM,
// APC) up to its terminator, or a two-character escape.
var escSeq = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|[\]PX^_][^\x07\x1b]*(?:\x07|\x1b\\)?|[ -~])`)

// logsText is log text made safe for the terminal (docs/design/logging-v2.md
// §11): escape sequences stripped, any other control character a space. Log
// text is untrusted; a crafted line must not recolor, retitle or clear the
// screen.
func logsText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, escSeq.ReplaceAllString(s, ""))
}

// renderLogs draws the pane in the chat pane's place, its size exactly.
func (m *model) renderLogs() string {
	w, h := m.chat.Width, m.chat.Height
	p := m.logs
	lines := []string{cHeading.Render("/logs"), cDim.Render("loading…")}
	if v := p.view(); v != nil {
		body := m.logsLines(v)
		v.fit(h-1, len(body))
		title := v.title
		if v.text == nil {
			title += fmt.Sprintf(" · %d events", len(v.events))
			if p.live && v == p.views[0] {
				title += " · live"
			}
		}
		lines = []string{cHeading.Render(logsText(title))}
		if len(body) == 0 {
			lines = append(lines, cDim.Render("no events match"))
		}
		for i := v.top; i < len(body) && i < v.top+h-1; i++ {
			switch {
			case v.text != nil:
				lines = append(lines, body[i])
			case i == v.sel:
				lines = append(lines, cOK.Bold(true).Render("▶ "+body[i]))
			default:
				lines = append(lines, logsLevelStyle(v.events[i].Level).Render("  "+body[i]))
			}
		}
	}
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i := range lines {
		lines[i] = clip.Render(lines[i])
	}
	return lipgloss.NewStyle().Width(w).Height(h).Render(strings.Join(lines, "\n"))
}

func logsLevelStyle(l event.Level) lipgloss.Style {
	switch l {
	case event.Critical, event.Error:
		return cErr
	case event.Warning:
		return cSys
	case event.Verbose:
		return cDim
	}
	return cName
}

// logsHint replaces the input area while the pane is open.
func (m *model) logsHint() string {
	hint := "↑/↓ select · enter detail · c correlated · PgUp/PgDn · g/G first/last · esc/q back"
	if v := m.logs.view(); v != nil && v.text != nil {
		hint = "↑/↓ scroll · PgUp/PgDn · c correlated · esc/q back"
	}
	out := cDim.Render(hint)
	if n := m.logs.note; n != "" {
		out = cErr.Render(logsText(n)) + "\n" + out
	}
	return out
}

// logsCommands is the log book's command.
func logsCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/logs"}, Section: "logs", Run: slashLogs,
			Summary: "the log book: the events a filter matches, or tail them live",
			Rows: []cmdreg.Row{
				{Syntax: "/logs [filter]", Desc: "the newest 200 events the filter matches, e.g. /logs level>=warning since:24h"},
				{Syntax: "/logs follow [filter]", Desc: "the same, then each new one as it is written"},
			}},
	}
}
