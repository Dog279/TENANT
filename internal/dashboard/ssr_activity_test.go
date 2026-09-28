package dashboard

import (
	"context"
	"html"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs"
)

// logEvent files a turn event into a hub the way the serve/TUI wiring does.
func logEvent(h *logs.Hub, ev agent.Event) {
	if e, ok := agent.EntryFor(ev); ok {
		h.Log(e)
	}
}

// /activity backfills the retained log server-side; /activity/events replays
// only entries AFTER the cursor (gap-free reconnect), tagged with the Seq as
// the SSE id.
func TestActivity_BackfillAndReplay(t *testing.T) {
	l := logs.NewMemory(100)
	logEvent(l, agent.Event{Kind: agent.EventToolCall, Tool: "web_search"})
	logEvent(l, agent.Event{Kind: agent.EventError, Text: "boom-marker"})
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(l)

	// Page render backfills BOTH entries + stamps the head cursor (2, in this
	// hub's epoch) for the tail.
	body := get(t, s, "/activity").Body.String()
	if !strings.Contains(body, "web_search") || !strings.Contains(body, "boom-marker") {
		t.Errorf("/activity should backfill retained entries; got:\n%s", body)
	}
	if !strings.Contains(body, "cursor="+l.Epoch()+".2") {
		t.Errorf("/activity should stamp head cursor=<epoch>.2; got:\n%s", body)
	}
	if !strings.Contains(body, `class="src">turns</span>`) {
		t.Errorf("rows should carry the source chip; got:\n%s", body)
	}

	// SSE from cursor=1 (a bare seq, as a page from before epochs sends) replays
	// ONLY Seq 2 (id: <epoch>.2), then the tail blocks until we cancel. The
	// replay runs before the tail loop, so it's written deterministically.
	r := httptest.NewRequest("GET", "/activity/events?cursor=1", nil)
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.handleActivitySSE(rec, r); close(done) }()
	cancel()
	<-done

	out := rec.Body.String()
	if !strings.Contains(out, "id: "+l.Epoch()+".2\n") {
		t.Errorf("SSE from cursor=1 should replay Seq 2 with id: <epoch>.2; got:\n%s", out)
	}
	if strings.Contains(out, "id: "+l.Epoch()+".1\n") {
		t.Errorf("SSE from cursor=1 must NOT replay Seq 1 (already-seen); got:\n%s", out)
	}
	if !strings.Contains(out, "boom-marker") {
		t.Errorf("replayed row should carry the entry content; got:\n%s", out)
	}
}

// Last-Event-ID header wins over ?cursor= (gap-free resume on reconnect); a
// cursor carries its epoch, a bare seq (an older page) has none.
func TestActivity_LastEventIDResumes(t *testing.T) {
	r := httptest.NewRequest("GET", "/activity/events?cursor=0", nil)
	r.Header.Set("Last-Event-ID", "ab12.7")
	if ep, got := activityStartCursor(r); ep != "ab12" || got != 7 {
		t.Fatalf("Last-Event-ID should win over ?cursor; got %q.%d, want ab12.7", ep, got)
	}
	r2 := httptest.NewRequest("GET", "/activity/events?cursor=3", nil)
	if ep, got := activityStartCursor(r2); ep != "" || got != 3 {
		t.Fatalf("?cursor should be used when no Last-Event-ID; got %q.%d, want 3", ep, got)
	}
}

// A reconnect with a cursor from another epoch (the hub restarted, numbering
// from 1 again) redraws the feed from the retained ring instead of going
// quiet until the new head passes the old cursor (TEN-367).
func TestActivity_RestartRedrawsFeed(t *testing.T) {
	l := logs.NewMemory(100)
	logEvent(l, agent.Event{Kind: agent.EventToolCall, Tool: "web_search"})
	logEvent(l, agent.Event{Kind: agent.EventError, Text: "boom-marker"})
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(l)

	r := httptest.NewRequest("GET", "/activity/events", nil)
	r.Header.Set("Last-Event-ID", "0ld3p0ch.5000")
	ctx, cancel := context.WithCancel(r.Context())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.handleActivitySSE(rec, r.WithContext(ctx)); close(done) }()
	cancel()
	<-done

	out := rec.Body.String()
	for _, want := range []string{"id: " + l.Epoch() + ".2\n", "data: mode inner", "web_search", "boom-marker"} {
		if !strings.Contains(out, want) {
			t.Errorf("restart re-sync should contain %q; got:\n%s", want, out)
		}
	}
}

// Two open Activity tabs each get every new line: each stream has its own
// wake-up (TEN-367).
func TestActivity_EveryTabWakes(t *testing.T) {
	l := logs.NewMemory(100)
	l.Logf("cron", slog.LevelInfo, "first-line")
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(l)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	tabs := []*syncResponse{{}, {}}
	for _, rec := range tabs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleActivitySSE(rec, httptest.NewRequest("GET", "/activity/events?cursor=0", nil).WithContext(ctx))
		}()
	}
	waitAll := func(marker string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for _, rec := range tabs {
			for !strings.Contains(rec.String(), marker) {
				if time.Now().After(deadline) {
					t.Fatalf("a tab never showed %q:\n%s", marker, rec.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	waitAll("first-line") // both streams have subscribed and replayed
	l.Logf("cron", slog.LevelInfo, "second-line")
	waitAll("second-line")
}

// Nil hub → page renders "unavailable", stream is a no-op (no panic).
func TestActivity_NilHubGraceful(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil) // no SetLogs
	if body := get(t, s, "/activity").Body.String(); !strings.Contains(body, "unavailable") {
		t.Errorf("nil hub should render an unavailable notice; got:\n%s", body)
	}
	r := httptest.NewRequest("GET", "/activity/events", nil)
	rec := httptest.NewRecorder()
	s.handleActivitySSE(rec, r) // must return immediately, no panic/block
	if rec := get(t, s, "/activity/files/turns/2026-09-22.log"); rec.Code != 404 {
		t.Errorf("file view without a hub = %d, want 404", rec.Code)
	}
}

// One source = one tab: the tab row follows the hub's live source list, so a
// source that appears later (a new subsystem, an add-on, a dropped folder)
// gets its tab on the next render — no rebuild, no code change here.
func TestActivity_TabsFollowSources(t *testing.T) {
	l := logs.NewMemory(100)
	logEvent(l, agent.Event{Kind: agent.EventToolCall, Tool: "web_search"})
	l.Logf("discord", slog.LevelInfo, "relay connected")
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(l)

	body := get(t, s, "/activity").Body.String()
	for _, want := range []string{`href="/activity?source=turns"`, `href="/activity?source=discord"`} {
		if !strings.Contains(body, want) {
			t.Errorf("tab row should contain %s; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "source=sidecar") {
		t.Fatal("no sidecar tab yet")
	}
	l.Logf("sidecar", slog.LevelWarn, "hello from a script", "k", "v")
	body = get(t, s, "/activity").Body.String()
	if !strings.Contains(body, `href="/activity?source=sidecar"`) {
		t.Errorf("a new source should grow a tab; got:\n%s", body)
	}
	// Tabs come in display order: built-ins first, custom after, and the
	// "turns" tab shows its count.
	if strings.Index(body, "source=turns") > strings.Index(body, "source=discord") || strings.Index(body, "source=discord") > strings.Index(body, "source=sidecar") {
		t.Error("tabs should be in display order (turns, discord, sidecar)")
	}
}

// ?source= narrows the backlog AND the stream; ?level=error keeps errors only;
// plain records render level badge + key=value fields.
func TestActivity_SourceAndLevelFilters(t *testing.T) {
	l := logs.NewMemory(100)
	logEvent(l, agent.Event{Kind: agent.EventToolCall, Tool: "web_search"})
	l.Logf("discord", slog.LevelInfo, "relay connected", "guild", "g1")
	l.Logf("discord", slog.LevelError, "relay died", "err", "eof")
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(l)

	body := get(t, s, "/activity?source=discord").Body.String()
	if strings.Contains(body, "web_search") || !strings.Contains(body, "relay connected") || !strings.Contains(body, "guild=g1") {
		t.Errorf("source filter wrong; got:\n%s", body)
	}
	if !strings.Contains(body, `class="tag">info</span>`) || !strings.Contains(body, `class="act-errtag">error</span>`) {
		t.Errorf("plain records should carry level badges; got:\n%s", body)
	}
	if !strings.Contains(body, "Files on disk") {
		t.Errorf("a selected source shows its files card; got:\n%s", body)
	}
	body = get(t, s, "/activity?level=error").Body.String()
	if strings.Contains(body, "relay connected") || !strings.Contains(body, "relay died") {
		t.Errorf("level=error should keep only errors; got:\n%s", body)
	}
	if !strings.Contains(body, `errtoggle on`) {
		t.Error("the errors-only toggle should render active")
	}

	r := httptest.NewRequest("GET", "/activity/events?cursor=0&source=discord&level=error", nil)
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.handleActivitySSE(rec, r); close(done) }()
	cancel()
	<-done
	out := rec.Body.String()
	if strings.Contains(out, "web_search") || strings.Contains(out, "relay connected") || !strings.Contains(out, "relay died") {
		t.Errorf("stream filter wrong; got:\n%s", out)
	}
}

// F1: the filter lands in the feed's data-init, a Datastar expression the
// browser HTML-decodes before Datastar evaluates it. html/template's escaping
// is undone there, so the values must arrive query-escaped: no quote from the
// request may close the URL string.
func TestActivity_FilterNotInDatastarExpression(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(logs.NewMemory(10))
	const payload = "x'),(window.__xss='fired'),('"
	p := url.QueryEscape(payload)
	body := get(t, s, "/activity?trace="+p+"&source="+p+"&level="+p).Body.String()
	attrs := regexp.MustCompile(`\sdata-([\w:.-]+)="([^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(attrs) == 0 {
		t.Fatalf("no Datastar attributes on /activity:\n%s", body)
	}
	for _, a := range attrs {
		if v := html.UnescapeString(a[2]); strings.Contains(v, "x'") || strings.Count(v, "'")%2 != 0 {
			t.Errorf("data-%s carries request text unescaped: %s", a[1], v)
		}
	}
	if !strings.Contains(body, "trace="+p) {
		t.Errorf("the stream URL should keep the trace filter, query-escaped; got:\n%s", body)
	}
}

// Rows are html-escaped and errors get the error styling; a sub-agent's line
// carries its agent chip.
func TestLogRow_EscapesAndStyles(t *testing.T) {
	l := logs.NewMemory(10)
	logEvent(l, agent.Event{Kind: agent.EventToolResult, Tool: "os_exec", Agent: "Programmer", Result: "<script>bad</script>", IsErr: true})
	all, _ := l.Snapshot()
	row := logRow(all[0])
	if strings.Contains(row, "<script>") || !strings.Contains(row, "&lt;script&gt;") {
		t.Errorf("row must escape: %s", row)
	}
	if !strings.Contains(row, "act-rowerr") || !strings.Contains(row, `class="chip">Programmer</span>`) || !strings.Contains(row, `class="src">subagents</span>`) {
		t.Errorf("row styling/chips wrong: %s", row)
	}
}
