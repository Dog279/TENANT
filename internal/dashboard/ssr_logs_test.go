package dashboard

import (
	"bufio"
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/lifecycle"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// logsBook is a dashboard over a log book whose store files Security records
// (it has a chain key) and knows its boot, as serve's does; and a second
// handle on the store for the test.
func logsBook(t *testing.T) (*Server, *logs.Hub, *store.Store) {
	t.Helper()
	root := t.TempDir()
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "logbook.db")
	key := []byte("0123456789abcdef0123456789abcdef")
	own, err := store.Open(path, store.Options{BootID: runtrace.BootID(), ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(own)
	st, err := store.Open(path, store.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(); _ = st.Close() })
	s := withSession(New(Config{Addr: "example.com", Auth: "tok"}, nil, &restFakeTools{}, nil, agent.NewBroker(0), nil))
	s.SetLogs(h)
	return s, h, st
}

// commit files evs, with a provider, a level and a time where they have none.
func commit(t *testing.T, st *store.Store, evs ...event.Event) []event.Event {
	t.Helper()
	for i := range evs {
		if evs[i].Provider == "" {
			evs[i].Provider = evs[i].Log
		}
		if evs[i].Level == 0 {
			evs[i].Level = event.Info
		}
		if evs[i].Time.IsZero() {
			evs[i].Time = time.Now().UTC()
		}
	}
	if err := st.Commit(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	return evs
}

// security is the Security log's records of def id.
func security(t *testing.T, st *store.Store, id int) []event.Event {
	t.Helper()
	f, err := query.Parse("log:security id:" + itoa(id))
	if err != nil {
		t.Fatal(err)
	}
	evs, _, _, err := st.Query(context.Background(), f, store.Cursor{}, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// browse is one action of the drill: a request for a page, counted.
type browse struct {
	t *testing.T
	s *Server
	n int
}

func (b *browse) get(path string) string {
	b.t.Helper()
	b.n++
	rec := get(b.t, b.s, path)
	if rec.Code != http.StatusOK {
		b.t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// link is the href of the first link on page whose text starts with text.
func link(t *testing.T, page, text string) string {
	t.Helper()
	m := regexp.MustCompile(`href="([^"]*)"[^>]*>` + regexp.QuoteMeta(text)).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no link %q on the page:\n%s", text, page)
	}
	return html.UnescapeString(m[1])
}

// The discovery drill (docs/design/logging-v2.md §9.1), questions 1-4: each
// answered in at most three requests, following only links the pages show.
func TestLogsDiscoveryDrill(t *testing.T) {
	s, _, st := logsBook(t)
	now := time.Now()

	t.Run("1 what is running", func(t *testing.T) {
		act := runtrace.NewID()
		s.logbook.Components = func(context.Context) ([]logapi.Component, error) {
			return []logapi.Component{
				{Name: "discord relay", Kind: lifecycle.Connector, Log: "discord", State: lifecycle.Running, Since: now.Add(-3 * time.Hour), Started: now.Add(-3 * time.Hour), Activity: act},
				{Name: "cron", Kind: lifecycle.Subsystem, Log: "cron", State: lifecycle.Failed, Since: now.Add(-time.Minute), Error: "database is locked"},
				{Name: "mcp github", Kind: lifecycle.Child, Log: "mcp.github", State: lifecycle.Degraded, Started: now.Add(-time.Hour), Error: "slow to answer"},
			}, nil
		}
		defer func() { s.logbook.Components = nil }()
		b := &browse{t: t, s: s}
		page := b.get(link(t, b.get("/logs"), "Components"))
		for _, want := range []string{"discord relay", "connector", `class="lg-ok">running`, "3h 0m", "cron", `class="act-errtag">failed`, "database is locked",
			`class="risk">degraded`, "slow to answer", "1 running · 1 degraded · 1 failed",
			`href="/logs?q=log%3Acron&#43;level%3E%3Dwarning"`, `href="/logs?q=activity%3A` + act + `"`} {
			if !strings.Contains(page, want) {
				t.Errorf("Components lacks %q:\n%s", want, page)
			}
		}
		if b.n > 3 {
			t.Errorf("%d requests", b.n)
		}
	})

	t.Run("2 what went wrong recently", func(t *testing.T) {
		commit(t, st, event.Event{Log: "cron", Level: event.Error, Msg: "job nightly failed: exit status 1"},
			event.Event{Log: "discord", Level: event.Warning, Msg: "relay disconnected: gateway reset"},
			event.Event{Log: "discord", Msg: "relay connected"})
		b := &browse{t: t, s: s}
		summary := b.get("/logs")
		if !strings.Contains(summary, "job nightly failed: exit status 1") {
			t.Errorf("the Summary doesn't list the error:\n%s", summary)
		}
		admin := b.get(link(t, summary, "Administrative Events"))
		for _, want := range []string{"job nightly failed", "relay disconnected"} {
			if !strings.Contains(admin, want) {
				t.Errorf("Administrative Events lacks %q", want)
			}
		}
		if strings.Contains(admin, "relay connected") {
			t.Error("Administrative Events holds an info event")
		}
		if b.n > 3 {
			t.Errorf("%d requests", b.n)
		}
	})

	t.Run("3 what happened at 14:03", func(t *testing.T) {
		y, m, d := now.Date()
		at := func(h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, time.Local).UTC() }
		commit(t, st, event.Event{Log: "discord", Level: event.Warning, Time: at(14, 3), Msg: "slice: discord reset at 14:03"},
			event.Event{Log: "turns", Time: at(14, 3), Msg: "slice: a turn at 14:03"},
			event.Event{Log: "cron", Time: at(13, 30), Msg: "slice: before the window"},
			event.Event{Log: "cron", Time: at(14, 30), Msg: "slice: after the window"})
		b := &browse{t: t, s: s}
		page := b.get("/logs?" + url.Values{"q": {"since:14:00 until:14:10"}}.Encode())
		for _, want := range []string{"slice: discord reset at 14:03", "slice: a turn at 14:03"} {
			if !strings.Contains(page, want) {
				t.Errorf("the slice lacks %q", want)
			}
		}
		for _, not := range []string{"before the window", "after the window"} {
			if strings.Contains(page, not) {
				t.Errorf("the slice holds %q", not)
			}
		}
		if b.n > 3 {
			t.Errorf("%d requests", b.n)
		}
	})

	t.Run("4 why did this fail and what else did that run do", func(t *testing.T) {
		trace, sandbox, approval, call := runtrace.NewID(), runtrace.NewID(), runtrace.NewID(), "call_7"
		s.SetRunRecords(func(_ context.Context, id string) (runtrace.Run, error) {
			return runtrace.Run{TraceID: id, Source: "discord", Outcome: "error", ErrorClass: "tool_failed", Model: "qwen3",
				StartedAt: now.Add(-time.Minute), ToolCalls: []runtrace.ToolCall{{ID: call, Name: "os_exec", Outcome: "error"}},
				Approvals: []runtrace.Approval{{ID: approval, RunID: id, Action: "os_exec", Category: "exec", Outcome: "approved"}}}, nil
		})
		defer s.SetRunRecords(nil)
		commit(t, st,
			event.Event{Log: "turns", Msg: "turn started", Opcode: event.OpStart, Corr: event.Correlation{TraceID: trace}},
			event.Event{Log: "security", Msg: "approval granted: os_exec make build", Corr: event.Correlation{TraceID: trace, ApprovalID: approval}},
			event.Event{Log: "turns", Msg: "tool call os_exec", Corr: event.Correlation{TraceID: trace, ToolCallID: call}},
			event.Event{Log: "sandbox", Msg: "sandbox run started", Opcode: event.OpStart, Corr: event.Correlation{ActivityID: sandbox, RelatedID: trace}},
			event.Event{Log: "sandbox", Msg: "child stderr: permission denied", Corr: event.Correlation{ActivityID: sandbox}},
			event.Event{Log: "turns", Level: event.Error, Msg: "turn failed: os_exec exited 1", Corr: event.Correlation{TraceID: trace}})
		b := &browse{t: t, s: s}
		event := b.get(link(t, b.get("/logs"), `<span class="lg-lv">ERR</span><span class="lg-t"`))
		if !strings.Contains(event, "turn failed: os_exec exited 1") {
			t.Fatalf("the latest error isn't the failed turn:\n%s", event)
		}
		corr := b.get(link(t, event, "Correlated"))
		for _, want := range []string{"Timeline of <code>trace " + trace, "<b>error</b> · tool_failed", `<span class="nm">os_exec</span> · error`,
			"os_exec (exec) · approved", "turn started", "approval granted: os_exec make build", "tool call os_exec",
			"sandbox run started", "child stderr: permission denied", "turn failed"} {
			if !strings.Contains(corr, want) {
				t.Errorf("the Correlated tab lacks %q", want)
			}
		}
		if b.n > 3 {
			t.Errorf("%d requests", b.n)
		}
	})
}

// The tree: Custom Views first, then Tenant's logs, the subsystems grouped as
// catalog.Group puts them, and External. A log appears once written to.
func TestLogsTree(t *testing.T) {
	s, _, st := logsBook(t)
	commit(t, st, event.Event{Log: "turns"}, event.Event{Log: "system"}, event.Event{Log: "subagents"}, event.Event{Log: "security"},
		event.Event{Log: "discord", Level: event.Warning}, event.Event{Log: "imessage"}, event.Event{Log: "cron", Level: event.Error},
		event.Event{Log: "mcp.github"}, event.Event{Log: "plugin.weather"}, event.Event{Log: "sandbox"}, event.Event{Log: "ext.deploy"})
	if _, err := s.logbook.SaveView(context.Background(), st, logapi.View{Name: "Mine", Filter: "log:cron"}); err != nil {
		t.Fatal(err)
	}
	page := get(t, s, "/logs").Body.String()
	tree := page[strings.Index(page, `<nav class="lg-tree"`):strings.Index(page, `href="/logs/components"`)]
	order := []string{"Custom Views", "Administrative Events", "Security failures", "This run", "Since last start", "Mine",
		"<summary>Tenant", ">System<", ">Security<", ">Turns<", ">Sub-agents<",
		"<summary>Subsystems", "<summary>Channels", ">Discord ", ">iMessage<", "<summary>Scheduler", ">Cron ",
		"<summary>Integrations", "<summary>MCP", ">github<", "<summary>Plugins", ">weather<", ">Sandbox<", "<summary>External", ">deploy<"}
	at := 0
	for _, want := range order {
		i := strings.Index(tree[at:], want)
		if i < 0 {
			t.Fatalf("the tree lacks %q after position %d:\n%s", want, at, tree)
		}
		at += i
	}
	if strings.Contains(tree, "<summary>Sandbox") {
		t.Error("Sandbox › Sandbox should fold into one entry")
	}
	if !strings.Contains(tree, `>Cron <span class="lg-n bad">1</span>`) || !strings.Contains(tree, `>Discord <span class="lg-n">1</span>`) {
		t.Errorf("the counts of warnings and worse are missing:\n%s", tree)
	}
	if strings.Contains(tree, "Peering") {
		t.Error("a log nobody wrote to is in the tree")
	}
	commit(t, st, event.Event{Log: "peering"})
	if !strings.Contains(get(t, s, "/logs").Body.String(), ">Peering<") {
		t.Error("a log doesn't appear the first time it is written to")
	}
}

var rowRe = regexp.MustCompile(`<a class="lg-row lv-`)

// The grid pages by cursor, newest first, a page at a time: the page renders
// the first, and the sentinel under it fetches each next one.
func TestLogsGridPagesAndCounts(t *testing.T) {
	s, _, st := logsBook(t)
	var evs []event.Event
	for i := range 250 {
		evs = append(evs, event.Event{Log: "bulk", Msg: "bulk " + itoa(i)})
	}
	commit(t, st, evs...)
	commit(t, st, event.Event{Log: "other", Msg: "x"}, event.Event{Log: "other", Msg: "y"})

	page := get(t, s, "/logs?q=log:bulk").Body.String()
	if n := len(rowRe.FindAllString(page, -1)); n != logsPageSize {
		t.Fatalf("the first page has %d rows", n)
	}
	if strings.Index(page, "bulk 249") > strings.Index(page, "bulk 150") || strings.Contains(page, "bulk 149<") {
		t.Error("the first page isn't the newest 100, newest first")
	}
	next := func(page string) string {
		u, err := url.Parse(link(t, page, "Load older events"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(page, `data-on-intersect="@get(`) {
			t.Error("the sentinel doesn't fetch the next page as it scrolls into view")
		}
		rec := get(t, s, "/logs/rows?"+u.RawQuery)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
			t.Fatalf("rows = %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	p2 := next(page)
	if n := len(rowRe.FindAllString(p2, -1)); n != 100 || !strings.Contains(p2, "data: mode append\ndata: selector #lg-rows") ||
		!strings.Contains(p2, "bulk 149<") || !strings.Contains(p2, "bulk 50<") || strings.Contains(p2, "bulk 49<") {
		t.Fatalf("page 2 has %d rows:\n%s", n, p2)
	}
	p3 := next(strings.ReplaceAll(p2, "data: elements ", ""))
	if n := len(rowRe.FindAllString(p3, -1)); n != 50 || !strings.Contains(p3, "bulk 0<") || strings.Contains(p3, "data-on-intersect") || !strings.Contains(p3, "The oldest match.") {
		t.Fatalf("the last page has %d rows:\n%s", n, p3)
	}

	if c := get(t, s, "/logs/count?q=log:bulk").Body.String(); !strings.Contains(c, "252 events; 250 match") {
		t.Errorf("count = %s", c)
	}
	if c := get(t, s, "/logs/count?q=").Body.String(); !strings.Contains(c, "252 events; 252 match") {
		t.Errorf("count of everything = %s", c)
	}
}

// A filter's parse error shows inline; Find adds full text to the filter.
func TestLogsFilterAndFind(t *testing.T) {
	s, _, st := logsBook(t)
	commit(t, st, event.Event{Log: "cron", Msg: "the needle is here"}, event.Event{Log: "cron", Msg: "only hay"})
	page := get(t, s, "/logs?q="+url.QueryEscape("level>>warning")).Body.String()
	if !regexp.MustCompile(`class="banner err lg-ferr">filter: column \d+: `).MatchString(page) || rowRe.MatchString(page) {
		t.Errorf("a bad filter should show its error and no rows:\n%s", page)
	}
	page = get(t, s, "/logs?"+url.Values{"q": {"log:cron"}, "find": {"needle"}}.Encode()).Body.String()
	if !strings.Contains(page, "the needle is here") || strings.Contains(page, "only hay") || !strings.Contains(page, `value="log:cron &#34;needle&#34;"`) {
		t.Errorf("find:\n%s", page)
	}
}

// The detail tabs: General's times and pivots, Details' masked fields and raw
// JSON with an audited reveal, and Help's catalog entry and statistics.
func TestLogsDetailTabsAndReveal(t *testing.T) {
	s, _, st := logsBook(t)
	trace := runtrace.NewID()
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	evs := commit(t, st,
		event.Event{Log: "dashboard", Provider: "dashboard", ID: 2001, Level: event.Warning, Time: at.Add(-48 * time.Hour), Msg: "HTTP server: first"},
		event.Event{Log: "dashboard", Provider: "dashboard", ID: 2001, Level: event.Warning, Time: at, Msg: "HTTP server: tls handshake error",
			Task: "Server", Actor: "operator", Surface: "dashboard", Corr: event.Correlation{TraceID: trace},
			Data: []event.Field{{Key: "err", Value: "tls: bad record"}, {Key: "stack", Value: "the secret payload", Sensitive: true}}})
	seq := itoa(int(evs[1].Seq))
	detail := func(tab string) string {
		rec := get(t, s, "/logs/detail/"+seq+"?tab="+tab)
		if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "event: datastar-patch-elements\ndata: elements <section id=\"lg-detail\"") {
			t.Fatalf("detail %s = %d %s", tab, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	g := detail("general")
	for _, want := range []string{at.In(time.Local).Format("2006-01-02 15:04:05.000"), at.Format("2006-01-02 15:04:05.000") + ` <span class="dim">UTC`, "dashboard 2001 (version 0)",
		`href="/logs?q=trace%3A` + trace + `"`, ">operator<", ">dashboard<", `href="/logs/event/` + seq + `"`, "navigator.clipboard.writeText"} {
		if !strings.Contains(g, want) {
			t.Errorf("General lacks %q:\n%s", want, g)
		}
	}
	d := detail("details")
	if strings.Contains(d, "the secret payload") || !strings.Contains(d, event.Masked) || !strings.Contains(d, "tls: bad record") ||
		!strings.Contains(d, "Raw JSON") || !strings.Contains(d, `@post('\/logs\/detail\/`+seq+`\/reveal')`) {
		t.Errorf("Details should mask the sensitive value and offer a reveal:\n%s", d)
	}
	if n := len(security(t, st, 4904)); n != 0 {
		t.Fatalf("%d reveal records before any reveal", n)
	}
	rec := do(t, s, "POST", "/logs/detail/"+seq+"/reveal", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "the secret payload") || strings.Contains(rec.Body.String(), "Reveal sensitive values") {
		t.Fatalf("reveal = %d %s", rec.Code, rec.Body.String())
	}
	recs := security(t, st, 4904)
	if len(recs) != 1 || !strings.Contains(recs[0].Msg, "event "+seq+", fields stack") || recs[0].Surface != "dashboard" || recs[0].Actor != "operator" {
		t.Fatalf("the reveal's Security record = %+v", recs)
	}
	h := detail("help")
	for _, want := range []string{"<b>What it means.</b> Go&#39;s HTTP server", "Seen once in the last 24 hours, 2 times in all", `<svg class="lg-trend"`,
		`href="/logs?q=provider%3Adashboard&#43;id%3A2001"`} {
		if !strings.Contains(h, want) {
			t.Errorf("Help lacks %q:\n%s", want, h)
		}
	}
}

// Log text is untrusted: every value is HTML-escaped wherever it is shown.
func TestLogsEscapesLogText(t *testing.T) {
	s, _, st := logsBook(t)
	evs := commit(t, st, event.Event{Log: "ext.evil", Level: event.Error, Msg: `<script>alert(1)</script>`, Task: `"><b>task`,
		Data: []event.Field{{Key: "x", Value: `<img src=x onerror=alert(2)>`}}})
	seq := itoa(int(evs[0].Seq))
	for _, p := range []string{"/logs", "/logs?q=", "/logs/event/" + seq, "/logs/detail/" + seq + "?tab=details", "/logs/rows?q=&cursor=" + url.QueryEscape(store.Cursor{StoreID: st.StoreID(), Seq: evs[0].Seq + 1}.String())} {
		body := get(t, s, p).Body.String()
		if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") || strings.Contains(body, `"><b>task`) {
			t.Errorf("%s renders log text raw:\n%s", p, body)
		}
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") && !strings.Contains(body, "&lt;img src=x onerror=alert(2)&gt;") {
			t.Errorf("%s doesn't show the message escaped:\n%s", p, body)
		}
	}
}

// /logs/event/{seq} opens the page with that event selected, in its log.
func TestLogsPermalink(t *testing.T) {
	s, _, st := logsBook(t)
	evs := commit(t, st, event.Event{Log: "cron", Msg: "older"}, event.Event{Log: "cron", Msg: "the one"}, event.Event{Log: "cron", Msg: "newer"})
	seq := itoa(int(evs[1].Seq))
	page := get(t, s, "/logs/event/"+seq).Body.String()
	if !strings.Contains(page, `<code class="lg-q">log:cron</code>`) || !strings.Contains(page, `data-signals:sel="`+seq+`"`) ||
		!strings.Contains(page, `<div class="lg-msgfull">the one</div>`) || strings.Index(page, "the one") > strings.Index(page, ">older<") {
		t.Errorf("the permalink should open the event's log, selected, with it at the top:\n%s", page)
	}
	if page := get(t, s, "/logs/event/99999").Body.String(); !strings.Contains(page, "There is no event 99999") {
		t.Errorf("a missing event:\n%s", page)
	}
}

// The actions: save and delete a view, a log's properties and its debug
// switch, clear (never the Security log), export, each through what the API
// calls, which files each Security record.
func TestLogsActions(t *testing.T) {
	s, h, st := logsBook(t)
	ctx := context.Background()
	commit(t, st, event.Event{Log: "cron", Level: event.Error, Msg: "job failed"}, event.Event{Log: "cron", Msg: "job ran"})
	form := func(path string, v url.Values) *url.URL {
		t.Helper()
		rec := do(t, s, "POST", path, "application/x-www-form-urlencoded", v.Encode())
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d %s", path, rec.Code, rec.Body.String())
		}
		u, _ := url.Parse(rec.Header().Get("Location"))
		return u
	}

	u := form("/logs/views", url.Values{"name": {"Cron errors"}, "filter": {"log:cron level>=error"}})
	if u.Query().Get("view") != "Cron errors" || u.Query().Get("err") != "" {
		t.Fatalf("save view → %s", u)
	}
	page := get(t, s, u.String()).Body.String()
	if !strings.Contains(page, `class="lg-leaf on" title="log:cron level&gt;=error">Cron errors`) || !strings.Contains(page, "job failed") ||
		strings.Contains(page, "job ran") || !strings.Contains(page, "Delete this view") {
		t.Errorf("the saved view:\n%s", page)
	}
	if u := form("/logs/views", url.Values{"name": {"This run"}, "filter": {"log:cron"}}); !strings.Contains(u.Query().Get("err"), "built-in") {
		t.Errorf("saving over a built-in view → %s", u)
	}
	if u := form("/logs/views", url.Values{"name": {"bad"}, "filter": {"level>>"}}); !strings.Contains(u.Query().Get("err"), "filter") {
		t.Errorf("saving a bad filter → %s", u)
	}
	form("/logs/views/delete", url.Values{"name": {"Cron errors"}})
	if strings.Contains(get(t, s, "/logs").Body.String(), "Cron errors") {
		t.Error("the deleted view is still in the tree")
	}

	page = get(t, s, "/logs?q=log:cron").Body.String()
	for _, want := range []string{"Properties · cron", `action="/logs/log/cron/properties"`, `action="/logs/log/cron/clear"`, "Clear 2 events",
		`href="/api/events/export?format=ndjson&amp;q=log%3Acron"`, `href="/api/events/export?format=csv&amp;q=log%3Acron"`, "Debug for 30 min"} {
		if !strings.Contains(page, want) {
			t.Errorf("the cron grid lacks %q", want)
		}
	}
	if rec := get(t, s, "/api/events/export?format=csv&q=log%3Acron"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "job failed") {
		t.Errorf("the export link = %d %s", rec.Code, rec.Body.String())
	}

	u = form("/logs/log/cron/properties", url.Values{"max_mb": {"5"}, "max_days": {"7"}, "on_full": {"archive"}, "min_level": {"warning"}, "enabled": {"on"}})
	if u.Query().Get("err") != "" {
		t.Fatalf("properties → %s", u)
	}
	p, err := st.Policy(ctx, h.Budget(), "cron")
	if err != nil || p.MaxBytes != 5<<20 || p.MaxAge != 7*24*time.Hour || p.OnFull != store.Archive || p.MinLevel != event.Warning || p.Disabled {
		t.Errorf("cron's policy = %+v %v", p, err)
	}
	if u := form("/logs/log/cron/properties", url.Values{"max_mb": {"lots"}, "max_days": {"7"}}); u.Query().Get("err") == "" {
		t.Error("a size that isn't a number was taken")
	}
	form("/logs/log/cron/properties", url.Values{"action": {"debug"}})
	if dbg, _ := st.Debugging(ctx); time.Until(dbg["cron"]) < 29*time.Minute {
		t.Errorf("debug for 30 min: %v", dbg)
	}
	if len(security(t, st, 4902)) != 2 {
		t.Errorf("reconfiguring should file a Security record each time: %+v", security(t, st, 4902))
	}

	u = form("/logs/log/cron/clear", url.Values{"confirm": {"yes"}})
	if !strings.Contains(u.Query().Get("msg"), "Cleared 2 events") {
		t.Errorf("clear → %s", u)
	}
	if n, _ := st.Count(ctx, query.Filter{Terms: []query.Term{{Field: "log", Op: query.Match, Values: []query.Value{{Text: "cron"}}}}}); n != 0 {
		t.Errorf("cron still holds %d events", n)
	}
	if len(security(t, st, 4901)) != 1 {
		t.Error("clearing should file a Security record")
	}
	if u := form("/logs/log/security/clear", url.Values{"confirm": {"yes"}}); !strings.Contains(u.Query().Get("err"), "never cleared") {
		t.Errorf("clearing the Security log → %s", u)
	}
	if page := get(t, s, "/logs?q=log:security").Body.String(); !strings.Contains(page, "never cleared, only archived") || strings.Contains(page, `action="/logs/log/security/clear"`) {
		t.Error("the Security log offers a clear")
	}
}

// The live tail patches each new matching event onto the top of the grid,
// from this page's own subscription to the store.
func TestLogsLiveTail(t *testing.T) {
	s, _, st := logsBook(t)
	commit(t, st, event.Event{Log: "cron", Msg: "before"})
	page := get(t, s, "/logs?q=log:cron&live=1").Body.String()
	m := regexp.MustCompile(`data-init="@get\('(/logs/tail\?[^']*)'`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("live mode doesn't open the tail:\n%s", page)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+html.UnescapeString(m[1]), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	commit(t, st, event.Event{Log: "other", Msg: "not this log"}, event.Event{Log: "cron", Msg: "tailed <now>"})
	sc := bufio.NewScanner(resp.Body)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
		if strings.Contains(sc.Text(), "tailed &lt;now&gt;") {
			break
		}
	}
	out := strings.Join(got, "\n")
	if !strings.Contains(out, "tailed &lt;now&gt;") || !strings.Contains(out, "data: mode prepend") || !strings.Contains(out, "id: ") ||
		strings.Contains(out, "before") || strings.Contains(out, "not this log") {
		t.Errorf("the tail:\n%s", out)
	}
}

// The Summary page: counts per log over three windows with sparklines, sizes
// against limits, the top IDs, the latest errors and unexpected exits.
func TestLogsSummary(t *testing.T) {
	s, _, st := logsBook(t)
	ctx := context.Background()
	if err := st.SetPolicy(ctx, "cron", store.Policy{MaxBytes: 1 << 20, MaxAge: 7 * 24 * time.Hour, OnFull: store.Overwrite}); err != nil {
		t.Fatal(err)
	}
	commit(t, st, event.Event{Log: "cron", Provider: "cron", ID: 3001, Level: event.Error, Msg: "job x failed"},
		event.Event{Log: "cron", Provider: "cron", ID: 3001, Level: event.Error, Msg: "job y failed"},
		event.Event{Log: "system", Provider: "system", Level: event.Critical, Msg: "tenant serve exited unexpectedly"},
		event.Event{Log: "discord", Msg: "fine"})
	page := get(t, s, "/logs").Body.String()
	for _, want := range []string{`<div class="stat-label">Errors · 24 h</div><div class="stat-value lg-bad">3</div>`, "Unexpected exits · 7 d",
		"tenant serve exited unexpectedly", "job y failed", `<svg class="lg-spark bad"`, " of 1.0 MB", "7d · overwrite when full",
		`href="/logs?q=provider%3Acron&#43;id%3A3001&#43;since%3A24h">cron 3001</a></td><td>2</td>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the Summary lacks %q", want)
		}
	}
}
