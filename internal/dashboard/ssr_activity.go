package dashboard

// ssr_activity.go (TEN-238) is the Activity tab: a RETAINED, replayable event
// feed. Unlike the chat /events stream (live broker fan-out), this reads the
// agent.EventLog ring so the feed BACKFILLS the full backlog on page load and
// resumes GAP-FREE after any reconnect (fixing "no history before I opened the
// tab" + "stops updating when the macOS tab is unfocused"). The page renders the
// backlog server-side; /activity/events replays from a cursor (Last-Event-ID on
// reconnect, else ?cursor= on first load) then tails via the log's Notify/Since.

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tenant/internal/agent"
)

// SetEventLog installs the retained activity log after construction (mirrors
// SetCron). nil ⇒ the Activity tab renders empty + the stream is a no-op.
func (s *Server) SetEventLog(l *agent.EventLog) { s.evlog = l }

type activityFilterTab struct {
	Key, Label string
	On         bool
}

type activityPageData struct {
	layoutData
	Configured bool
	Backlog    template.HTML       // pre-rendered rows (each self-escaped via activityRowSeq)
	Cursor     uint64              // head Seq the live stream tails from
	Filter     string              // active kind filter ("" = all)
	Tabs       []activityFilterTab // sub-nav (Memory idiom)
	Empty      bool                // no rows match the current filter
}

// activityFilters is the troubleshooting sub-nav: each maps to a set of
// agent.EventKind (kindMatches). Order = display order.
var activityFilters = []activityFilterTab{
	{Key: "", Label: "All"},
	{Key: "tools", Label: "Tools"},
	{Key: "errors", Label: "Errors"},
	{Key: "bus", Label: "Bus"},
	{Key: "ingest", Label: "Ingest"},
	{Key: "lifecycle", Label: "Lifecycle"},
}

// handleActivityPage renders the full retained backlog server-side (filtered by
// ?kind), so history is visible instantly on load; the stream then tails from
// the head cursor with the same filter.
func (s *Server) handleActivityPage(w http.ResponseWriter, r *http.Request) {
	filter := strings.TrimSpace(r.URL.Query().Get("kind"))
	d := activityPageData{layoutData: layoutData{Title: "Activity", Page: "activity"}, Filter: filter}
	for _, t := range activityFilters {
		t.On = t.Key == filter
		d.Tabs = append(d.Tabs, t)
	}
	if s.evlog != nil {
		d.Configured = true
		events, head := s.evlog.Snapshot()
		var b strings.Builder
		n := 0
		for _, se := range events {
			if !kindMatches(se.Ev, filter) {
				continue
			}
			b.WriteString(activityRowSeq(se))
			n++
		}
		d.Backlog = template.HTML(b.String())
		d.Cursor = head
		d.Empty = n == 0
	}
	s.render(w, s.tmpl.activity, d)
}

// handleActivitySSE replays every event after the start cursor, then tails new
// ones — each patch tagged with its Seq as the SSE id so the browser's
// Last-Event-ID advances and a reconnect resumes gap-free.
func (s *Server) handleActivitySSE(w http.ResponseWriter, r *http.Request) {
	setSSEHeaders(w)
	flush(w)
	if s.evlog == nil {
		return
	}
	filter := strings.TrimSpace(r.URL.Query().Get("kind"))
	cursor := activityStartCursor(r)
	missed, head := s.evlog.Since(cursor)
	for _, se := range missed {
		if !kindMatches(se.Ev, filter) {
			continue
		}
		if _, err := w.Write(activityAppend(se.Seq, activityRowSeq(se))); err != nil {
			return
		}
	}
	flush(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-s.evlog.Notify():
			if !ok {
				return
			}
			var evs []agent.SeqEvent
			evs, head = s.evlog.Since(head)
			for _, se := range evs {
				if !kindMatches(se.Ev, filter) {
					continue
				}
				if _, err := w.Write(activityAppend(se.Seq, activityRowSeq(se))); err != nil {
					return
				}
			}
			flush(w)
		}
	}
}

// kindMatches reports whether ev belongs to the named filter bucket ("" = all).
func kindMatches(ev agent.Event, filter string) bool {
	switch filter {
	case "", "all":
		return true
	case "tools":
		switch ev.Kind {
		case agent.EventToolCall, agent.EventToolResult, agent.EventValidation, agent.EventRetry, agent.EventToolCatalog:
			return true
		}
	case "errors":
		return ev.Kind == agent.EventError || ev.IsErr
	case "bus":
		return ev.Kind == agent.EventBus
	case "ingest":
		return ev.Kind == agent.EventIngest
	case "lifecycle":
		switch ev.Kind {
		case agent.EventTurnStart, agent.EventFinal, agent.EventTruncated, agent.EventCompact, agent.EventInterject, agent.EventSkills:
			return true
		}
	}
	return false
}

// activityRowSeq renders one retained event as a themed card (TEN-238) matching
// the Memory/Skills idiom: relative timestamp, a semantic kind badge (red for
// errors → free error-highlighting, amber for validation/retry, cyan otherwise),
// an agent chip for cross-agent/bus events, the tool name in mono, and a
// snippet (errors get more room). All dynamic values are html-escaped.
func activityRowSeq(se agent.SeqEvent) string {
	ev := se.Ev
	isErr := ev.Kind == agent.EventError || ev.IsErr

	badgeClass, label := "tag", string(ev.Kind)
	switch {
	case isErr:
		badgeClass = "act-errtag"
	case ev.Kind == agent.EventValidation || ev.Kind == agent.EventRetry:
		badgeClass = "risk"
	case ev.Kind == agent.EventIngest:
		label = "📥 inbox"
	case ev.Kind == agent.EventBus:
		label = "🔀 bus"
	}

	detail := ev.Text
	if ev.Tool != "" {
		detail = strings.TrimSpace(ev.Tool + " " + ev.Result)
	}
	max := 200
	if isErr {
		max = 400 // errors carry the diagnostic — give them room
	}

	tool := ""
	if ev.Tool != "" {
		tool = `<span class="nm">` + html.EscapeString(ev.Tool) + `</span> `
		detail = strings.TrimSpace(ev.Result) // tool name shown separately
	}
	chip := ""
	if ev.Agent != "" {
		chip = ` <span class="chip">` + html.EscapeString(ev.Agent) + `</span>`
	}
	rowClass := "ep act-row"
	if isErr {
		rowClass += " act-rowerr"
	}
	return fmt.Sprintf(
		`<div class="%s"><div class="ep-when dim" title="%s">%s · <span class="%s">%s</span>%s</div><div class="ep-line">%s%s</div></div>`,
		rowClass,
		html.EscapeString(se.At.Format("2006-01-02 15:04:05")),
		html.EscapeString(relAge(se.At)),
		badgeClass, html.EscapeString(label), chip,
		tool, html.EscapeString(snippetStr(detail, max)),
	)
}

// relAge formats a compact relative age ("12s", "3m", "2h", "1d").
func relAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// activityStartCursor resolves where the stream should resume: the Last-Event-ID
// header (sent by the client on reconnect) wins; else the ?cursor= query param
// (the page's render-time head on first connect); else 0 (replay everything).
func activityStartCursor(r *http.Request) uint64 {
	if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("cursor")); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// activityAppend builds an append-to-#activity-feed patch tagged with the event
// Seq as the SSE id (so Last-Event-ID advances for gap-free reconnect).
func activityAppend(seq uint64, rowHTML string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "id: %d\n", seq)
	b.Write(patchElements("#activity-feed", "append", rowHTML))
	return b.Bytes()
}
