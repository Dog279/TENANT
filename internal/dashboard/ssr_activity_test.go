package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/logs"
)

// logEvent files a turn event into a hub the way the serve/TUI wiring does.
func logEvent(h *logs.Hub, ev agent.Event) {
	if e, ok := agent.EntryFor(ev); ok {
		h.Log(e)
	}
}

// An old link to the Activity feed lands on the Logs page with the same
// filter: a trace, a source and errors only become the filter language,
// escaped; a bare /activity opens the Summary.
func TestActivity_RedirectsToLogs(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	const trace = `x'),("a b`
	for path, want := range map[string]string{
		"/activity":          "/logs",
		"/activity?trace=t2": "/logs?q=trace%3At2",
		"/activity?trace=" + url.QueryEscape(trace): "/logs?" + url.Values{"q": {`trace:"x'),(\"a b"`}}.Encode(),
		"/activity?source=discord&level=error":      "/logs?q=log%3Adiscord+level%3E%3Derror",
		"/activity?source=Cron&level=warn":          "/logs?q=log%3Acron+level%3E%3Dwarning",
		"/activity?level=bogus":                     "/logs",
	} {
		rec := get(t, s, path)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("GET %s = %d to %q, want 303 to %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	for _, gone := range []string{"/activity/events", "/activity/files/turns/2026-09-22.log"} {
		if rec := get(t, s, gone); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: the page's stream and files went with it", gone, rec.Code)
		}
	}
}

// GET /api/activity's All leaves out the dashboard's own streams opening and
// closing (UX-3); the dashboard source still has them.
func TestRESTActivity_AllWithoutStreamNoise(t *testing.T) {
	l := logs.NewMemory(100)
	l.Log(logs.Entry{Source: "system", Level: slog.LevelInfo, Msg: "a line"})
	l.Log(logs.Entry{Source: "dashboard", Level: slog.LevelInfo, Msg: "Live events stream to 127.0.0.1:1", Fields: []logs.Field{{Key: "event", Value: "dashboard/1001"}}})
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(l)
	msgs := func(q string) string {
		var resp restActivityResponse
		_ = json.Unmarshal(get(t, s, "/api/activity"+q).Body.Bytes(), &resp)
		var out []string
		for _, e := range resp.Events {
			out = append(out, e.Msg)
		}
		return strings.Join(out, "|")
	}
	if got := msgs(""); got != "a line" {
		t.Errorf("All = %q, want the line without the stream noise", got)
	}
	if got := msgs("?source=dashboard"); !strings.Contains(got, "Live events stream") {
		t.Errorf("the dashboard source = %q, want its streams", got)
	}
}
