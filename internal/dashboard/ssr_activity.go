package dashboard

// ssr_activity.go is what is left of the Activity feed page: the Logs page
// replaced it. GET /activity sends an old link or bookmark to the Logs page
// with the same filter, and activityFilter stays for GET /api/activity, the
// JSON feed `tenant attach` and outside pollers read.

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"tenant/internal/logs"
)

// SetLogs installs the log hub after construction (mirrors SetCron). nil ⇒ the
// Logs page says the log book isn't open, and the log API is a no-op.
func (s *Server) SetLogs(h *logs.Hub) { s.logs = h }

// activityFilter is GET /api/activity's (source, level, trace) selection.
// Trace keeps one turn's lines: the `trace` field that joins a line to its
// runs.db record.
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
	// The dashboard's own live streams opening and closing (Stream, 1001)
	// were a third of All: they stay on the dashboard source (UX-3).
	if f.Source == "" && e.Field("event") == "dashboard/1001" {
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

// logsQuery is the filter in the Logs page's filter language: the log, the
// level and the trace; "" for none.
func (f activityFilter) logsQuery() string {
	var terms []string
	if f.Source != "" {
		terms = append(terms, "log:"+f.Source) // a valid source is a bare word
	}
	switch f.Level {
	case "error":
		terms = append(terms, "level>=error")
	case "warn":
		terms = append(terms, "level>=warning")
	}
	if f.Trace != "" {
		terms = append(terms, filterTerm("trace", f.Trace))
	}
	return strings.Join(terms, " ")
}

// handleActivityMoved is GET /activity: the Activity feed became the Logs
// page, so an old link lands there with the same filter
// (/activity?trace=X → /logs?q=trace:X).
func (s *Server) handleActivityMoved(w http.ResponseWriter, r *http.Request) {
	to := "/logs"
	if q := activityFilterFrom(r).logsQuery(); q != "" {
		to = logsHref(q)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
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
