package dashboard

// ssr_activity.go is the Activity tab: the dashboard's window onto the unified
// log book (internal/logs). The hub retains a replayable, cursor-indexed ring of
// every subsystem's lines — the agent's turns, sub-agents, approvals, the
// Discord/iMessage relays, cron, eval, plain slog records, add-ons — and files
// each under <data>/logs/<source>/. This page renders the backlog server-side,
// then tails from the head cursor over SSE (Last-Event-ID on reconnect ⇒
// gap-free, openWhenHidden ⇒ keeps updating in a background tab).
//
// One source = one folder on disk = one tab here. The tab row is read from the
// hub on every render, so a source that appears at runtime (a new subsystem,
// `tenant log <name> …`, a POST /api/logs/<name>, or a script that drops a
// folder under the log root) gets its tab without a rebuild.

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
)

// SetLogs installs the log hub after construction (mirrors SetCron). nil ⇒ the
// Activity tab renders "unavailable", the stream and the log API are no-ops.
func (s *Server) SetLogs(h *logs.Hub) { s.logs = h }

type activityTab struct {
	Key, Label string
	Count      int
	On         bool
	Title      string // hover text: native/foreign + last write + any file error
	Err        bool
}

type activityFileView struct {
	Name, Size, When, Href string
}

type activityPageData struct {
	layoutData
	Configured bool
	Root       string        // on-disk log root ("" for a memory-only hub)
	Backlog    template.HTML // pre-rendered rows (each self-escaped via logRow)
	Cursor     string        // head cursor (<epoch>.<seq>) the live stream tails from
	Source     string        // active source filter ("" = all)
	Level      string        // "error" = errors only; "" = everything
	Trace      string        // one turn's lines only ("" = every line)
	Tabs       []activityTab
	Files      []activityFileView // the active source's files on disk
	Empty      bool
}

// activityFilter is the (source, level, trace) selection shared by the page,
// the SSE tail and the REST projection. Trace keeps one turn's lines: the
// `trace` field that joins a line to its runs.db record.
type activityFilter struct {
	Source string
	Level  string
	Trace  string
}

func activityFilterFrom(r *http.Request) activityFilter {
	q := r.URL.Query()
	f := activityFilter{Source: strings.TrimSpace(q.Get("source")), Level: strings.ToLower(strings.TrimSpace(q.Get("level"))), Trace: strings.TrimSpace(q.Get("trace"))}
	if f.Source != "" && !logs.ValidSource(f.Source) {
		f.Source = logs.NormalizeSource(f.Source)
	}
	if f.Level != "" && f.Level != "error" && f.Level != "warn" {
		f.Level = ""
	}
	return f
}

func (f activityFilter) match(e logs.Entry) bool {
	if f.Source != "" && e.Source != f.Source {
		return false
	}
	if f.Trace != "" && e.Field("trace") != f.Trace {
		return false
	}
	switch f.Level {
	case "error":
		return e.Level >= slog.LevelError
	case "warn":
		return e.Level >= slog.LevelWarn
	}
	return true
}

// handleActivityPage renders the retained backlog server-side (filtered), so
// history is visible instantly on load; the stream then tails from the head
// cursor with the same filter.
func (s *Server) handleActivityPage(w http.ResponseWriter, r *http.Request) {
	f := activityFilterFrom(r)
	d := activityPageData{layoutData: layoutData{Title: "Activity", Page: "activity"}, Source: f.Source, Level: f.Level, Trace: f.Trace}
	if s.logs == nil {
		s.render(w, s.tmpl.activity, d)
		return
	}
	d.Configured = true
	d.Root = s.logs.Root()
	for _, src := range s.logs.Sources() {
		t := activityTab{Key: src.Name, Label: src.Name, Count: src.Count, On: src.Name == f.Source}
		switch {
		case src.Err != "":
			t.Title = "file problem: " + src.Err
			t.Err = true
		case !src.Native:
			t.Title = "written by another process (tailed from disk)"
		default:
			t.Title = "written by tenant"
		}
		if !src.LastAt.IsZero() {
			t.Title += " · last " + relAge(src.LastAt)
		}
		d.Tabs = append(d.Tabs, t)
	}
	entries, head := s.logs.Snapshot()
	var b strings.Builder
	n := 0
	for _, e := range entries {
		if !f.match(e) {
			continue
		}
		b.WriteString(logRow(e))
		n++
	}
	d.Backlog = template.HTML(b.String())
	d.Cursor = s.activityCursor(head)
	d.Empty = n == 0
	if f.Source != "" {
		if files, err := s.logs.Files(f.Source); err == nil {
			for _, fi := range files {
				d.Files = append(d.Files, activityFileView{
					Name: fi.Name, Size: humanBytes(fi.Size), When: relAge(fi.ModTime),
					Href: "/activity/files/" + f.Source + "/" + fi.Name,
				})
			}
		}
	}
	s.render(w, s.tmpl.activity, d)
}

// handleActivitySSE replays every entry after the start cursor, then tails new
// ones — each patch tagged with its cursor as the SSE id so the browser's
// Last-Event-ID advances and a reconnect resumes gap-free. A cursor from
// another epoch means the hub restarted and numbers from 1 again: the feed is
// redrawn from what the hub retained, as a page load would draw it.
func (s *Server) handleActivitySSE(w http.ResponseWriter, r *http.Request) {
	setSSEHeaders(w)
	flush(w)
	if s.logs == nil {
		return
	}
	ctx, act := event.Start(r.Context(), Stream, "stream", "activity", "remote", r.RemoteAddr)
	defer act.End(nil)
	wake, unsubscribe := s.logs.Subscribe()
	defer unsubscribe()
	f := activityFilterFrom(r)
	epoch, cursor := activityStartCursor(r)
	restarted := epoch != "" && epoch != s.logs.Epoch()
	if restarted {
		cursor = 0
	}
	// lagged reports entries the ring dropped before this page read them.
	lagged := func(prev uint64, got []logs.Entry) {
		if prev > 0 && len(got) > 0 && got[0].Seq > prev+1 {
			StreamLagging.Log(ctx, "stream", "activity", "remote", r.RemoteAddr, "missed", got[0].Seq-prev-1)
		}
	}
	missed, head := s.logs.Since(cursor)
	lagged(cursor, missed) // a restart's cursor is 0: nothing to have missed
	if restarted {
		var b strings.Builder
		for _, e := range missed {
			if f.match(e) {
				b.WriteString(logRow(e))
			}
		}
		if _, err := w.Write(activityPatch(s.activityCursor(head), "inner", b.String())); err != nil {
			return
		}
		missed = nil
	}
	for _, e := range missed {
		if !f.match(e) {
			continue
		}
		if _, err := w.Write(activityPatch(s.activityCursor(e.Seq), "append", logRow(e))); err != nil {
			return
		}
	}
	flush(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			prev := head
			var entries []logs.Entry
			entries, head = s.logs.Since(head)
			lagged(prev, entries)
			for _, e := range entries {
				if !f.match(e) {
					continue
				}
				if _, err := w.Write(activityPatch(s.activityCursor(e.Seq), "append", logRow(e))); err != nil {
					return
				}
			}
			flush(w)
		}
	}
}

// handleActivityFile serves one on-disk log file as plain text (inline for a
// browser tab; ?download=1 for an attachment). Names are validated by the hub.
func (s *Server) handleActivityFile(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		http.Error(w, "log files unavailable", http.StatusNotFound)
		return
	}
	src, name := r.PathValue("source"), r.PathValue("name")
	rc, fi, err := s.logs.OpenFile(src, name)
	if err != nil {
		http.Error(w, "no such log file", http.StatusNotFound)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	disp := "inline"
	if r.URL.Query().Get("download") != "" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s-%s"`, disp, src, name))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size, 10))
	_, _ = io.Copy(w, rc)
}

// logRow renders one retained entry as a themed card matching the Memory/Skills
// idiom: relative timestamp, the source chip, a semantic badge (the event kind
// for turn events, the level for plain records; red for errors, amber for
// warnings/validation/retry, cyan otherwise), an agent chip for cross-agent
// events, the tool name in mono, and a snippet (errors get more room; plain
// records show their key=value fields in dim mono). Everything is html-escaped.
func logRow(e logs.Entry) string {
	kind := e.Kind()
	isErr := e.IsErr()
	badgeClass, label := "tag", kind
	if kind == "" {
		label = strings.ToLower(logs.LevelName(e.Level))
	}
	switch {
	case isErr:
		badgeClass = "act-errtag"
	case e.Level >= slog.LevelWarn, kind == "validation", kind == "retry":
		badgeClass = "risk"
	case kind == "ingest":
		label = "📥 inbox"
	case kind == "bus":
		label = "🔀 bus"
	case kind == "addon":
		label = "🧩 add-on"
	}

	tool := e.Field("tool")
	detail := e.Msg
	var kv string
	switch kind {
	case "tool_call":
		detail = e.Field("args")
	case "tool_result":
		detail = e.Field("result")
	case "":
		var parts []string
		for _, f := range e.Fields {
			parts = append(parts, f.Key+"="+f.Value)
		}
		kv = strings.Join(parts, " ")
	default:
		if t := e.Field("text"); t != "" {
			detail = t
		}
	}
	max := 200
	if isErr {
		max = 400 // errors carry the diagnostic — give them room
	}
	toolHTML := ""
	if tool != "" {
		toolHTML = `<span class="nm">` + html.EscapeString(tool) + `</span> `
	}
	chip := ""
	if a := e.Field("agent"); a != "" {
		chip = ` <span class="chip">` + html.EscapeString(a) + `</span>`
	}
	kvHTML := ""
	if kv != "" {
		kvHTML = ` <span class="kv">` + html.EscapeString(snippetStr(kv, 240)) + `</span>`
	}
	rowClass := "ep act-row"
	if isErr {
		rowClass += " act-rowerr"
	}
	return fmt.Sprintf(
		`<div class="%s"><div class="ep-when dim" title="%s">%s · <span class="src">%s</span> <span class="%s">%s</span>%s</div><div class="ep-line">%s%s%s</div></div>`,
		rowClass,
		html.EscapeString(e.At.Format("2006-01-02 15:04:05")),
		html.EscapeString(relAge(e.At)),
		html.EscapeString(e.Source),
		badgeClass, html.EscapeString(label), chip,
		toolHTML, html.EscapeString(snippetStr(detail, max)), kvHTML,
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

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// activityStartCursor resolves where the stream should resume: the Last-Event-ID
// header (sent by the client on reconnect) wins; else the ?cursor= query param
// (the page's render-time head on first connect); else 0 (replay everything).
// A cursor is "<epoch>.<seq>"; a bare "<seq>" (from a page served before
// cursors carried an epoch) keeps its old meaning, with epoch "".
func activityStartCursor(r *http.Request) (epoch string, seq uint64) {
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("cursor")} {
		ep, num, found := strings.Cut(strings.TrimSpace(v), ".")
		if !found {
			ep, num = "", ep
		}
		if n, err := strconv.ParseUint(num, 10, 64); err == nil {
			return ep, n
		}
	}
	return "", 0
}

// activityCursor is the feed cursor for seq: "<epoch>.<seq>".
func (s *Server) activityCursor(seq uint64) string {
	return s.logs.Epoch() + "." + strconv.FormatUint(seq, 10)
}

// activityPatch builds a #activity-feed patch tagged with a feed cursor as the
// SSE id (so Last-Event-ID advances for gap-free reconnect).
func activityPatch(cursor, mode, rowsHTML string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "id: %s\n", cursor)
	b.Write(patchElements("#activity-feed", mode, rowsHTML))
	return b.Bytes()
}
