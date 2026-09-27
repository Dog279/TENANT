package logapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// book is a hub with its store, the API over it, and a second handle on the
// store for the test to write and read through.
type book struct {
	h   *logs.Hub
	st  *store.Store
	api *API
	srv http.Handler
}

func newBook(t *testing.T) *book {
	t.Helper()
	root := t.TempDir()
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "logbook.db")
	own, err := store.Open(path, store.Options{BootID: runtrace.BootID()})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(own)
	st, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(); _ = st.Close() })
	b := &book{h: h, st: st, api: &API{Hub: func() *logs.Hub { return h }}}
	b.srv = b.api.Handler()
	return b
}

func (b *book) commit(t *testing.T, evs ...event.Event) []event.Event {
	t.Helper()
	for i := range evs {
		if evs[i].Provider == "" {
			evs[i].Provider = evs[i].Log
		}
		if evs[i].Level == 0 {
			evs[i].Level = event.Info
		}
	}
	if err := b.st.Commit(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	return evs
}

func (b *book) do(t *testing.T, method, path, ctype, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	b.srv.ServeHTTP(rec, r)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder, status int) T {
	t.Helper()
	var v T
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v: %s", v, err, rec.Body.String())
	}
	return v
}

func msgs(evs []event.Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Msg)
	}
	return strings.Join(out, ",")
}

// waitFor polls the store until pred holds for the events of log.
func (b *book) waitFor(t *testing.T, log string, pred func([]event.Event) bool) []event.Event {
	t.Helper()
	f, _ := query.Parse("log:" + log)
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, _, _, err := b.st.Query(context.Background(), f, store.Cursor{}, 1000, false)
		if err != nil {
			t.Fatal(err)
		}
		if pred(evs) {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("log %s never held what the test waits for: %+v", log, evs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Filters, paging forward and back, counts; a cursor from another store
// starts over and says so; bad filters and cursors are 400s.
func TestEventsPagesAndFilters(t *testing.T) {
	b := newBook(t)
	b.commit(t, event.Event{Log: "a", Msg: "one"}, event.Event{Log: "b", Msg: "other"},
		event.Event{Log: "a", Msg: "two", Level: event.Error}, event.Event{Log: "a", Msg: "three"})
	p := decode[EventsPage](t, b.do(t, "GET", "/api/events?q=log:a&limit=2&count=1", "", ""), 200)
	if msgs(p.Events) != "one,two" || p.Count == nil || *p.Count != 3 || p.Restarted {
		t.Fatalf("page 1 = %s count=%v", msgs(p.Events), p.Count)
	}
	p = decode[EventsPage](t, b.do(t, "GET", "/api/events?q=log:a&limit=2&cursor="+p.Next, "", ""), 200)
	if msgs(p.Events) != "three" || p.Count != nil {
		t.Fatalf("page 2 = %s", msgs(p.Events))
	}
	p = decode[EventsPage](t, b.do(t, "GET", "/api/events?q=log:a&reverse=1&limit=2", "", ""), 200)
	if msgs(p.Events) != "three,two" {
		t.Fatalf("newest first = %s", msgs(p.Events))
	}
	p = decode[EventsPage](t, b.do(t, "GET", "/api/events?q=level>=error", "", ""), 200)
	if msgs(p.Events) != "two" {
		t.Fatalf("level>=error = %s", msgs(p.Events))
	}
	p = decode[EventsPage](t, b.do(t, "GET", "/api/events?q=log:a&cursor=00000000000000000000000000000000.3", "", ""), 200)
	if !p.Restarted || msgs(p.Events) != "one,two,three" {
		t.Fatalf("a foreign cursor: restarted=%v %s", p.Restarted, msgs(p.Events))
	}
	if rec := b.do(t, "GET", "/api/events?q=level>=loud", "", ""); rec.Code != 400 || !strings.Contains(rec.Body.String(), "level") {
		t.Errorf("bad filter = %d %s", rec.Code, rec.Body.String())
	}
	if rec := b.do(t, "GET", "/api/events?cursor=12", "", ""); rec.Code != 400 {
		t.Errorf("bad cursor = %d", rec.Code)
	}
}

// One event with its catalog entry, Help and statistics; its sensitive
// values masked unless asked for, and a reveal recorded.
func TestEventDetail(t *testing.T) {
	b := newBook(t)
	evs := b.commit(t,
		event.Event{Log: "system", Provider: "system", ID: 3101, Level: event.Error, Msg: "discord failed: gone",
			Data: []event.Field{{Key: "component", Value: "discord"}, {Key: "token", Value: "hunter2", Sensitive: true}}},
		event.Event{Log: "system", Provider: "system", ID: 3101, Level: event.Error, Msg: "cron failed"})
	d := decode[EventDetail](t, b.do(t, "GET", "/api/events/"+strconv.FormatUint(evs[0].Seq, 10), "", ""), 200)
	if d.Spec == nil || d.Spec.Task != "Component" || !strings.Contains(d.Help, "What to do") || d.Stats == nil || d.Stats.Count != 2 || d.Stats.Trend[6] != 2 {
		t.Fatalf("detail = %+v", d)
	}
	if d.Event.Data[1].Value != event.Masked || d.Event.Data[0].Value != "discord" {
		t.Errorf("data = %+v", d.Event.Data)
	}
	d = decode[EventDetail](t, b.do(t, "GET", "/api/events/"+strconv.FormatUint(evs[0].Seq, 10)+"?sensitive=1", "", ""), 200)
	if d.Event.Data[1].Value != "hunter2" {
		t.Errorf("revealed data = %+v", d.Event.Data)
	}
	b.waitFor(t, "logbook", func(evs []event.Event) bool {
		return slices.ContainsFunc(evs, func(e event.Event) bool { return strings.Contains(e.Msg, "revealed") })
	})
	if rec := b.do(t, "GET", "/api/events/999", "", ""); rec.Code != 404 {
		t.Errorf("missing event = %d", rec.Code)
	}
}

// sse reads one SSE response's messages as they come.
type sse struct {
	body io.Closer
	sc   *bufio.Scanner
}

type message struct{ id, kind, data string }

func openStream(t *testing.T, url, last string) *sse {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if last != "" {
		req.Header.Set("Last-Event-ID", last)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("stream: HTTP %d", resp.StatusCode)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return &sse{body: resp.Body, sc: bufio.NewScanner(resp.Body)}
}

func (s *sse) next(t *testing.T) message {
	t.Helper()
	var m message
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case line == "" && (m.data != "" || m.kind != ""):
			return m
		case strings.HasPrefix(line, "id: "):
			m.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			m.kind = line[7:]
		case strings.HasPrefix(line, "data: "):
			m.data = line[6:]
		}
	}
	t.Fatalf("stream ended: %v", s.sc.Err())
	return m
}

func (m message) event(t *testing.T) event.Event {
	t.Helper()
	var e event.Event
	if err := json.Unmarshal([]byte(m.data), &e); err != nil {
		t.Fatalf("data %q: %v", m.data, err)
	}
	return e
}

// The live tail sends what matches after Last-Event-ID, then each new event;
// reconnecting with the last id it got misses no row and repeats none. A
// cursor from another store sends restart and starts over.
func TestStreamResumesWithNoMissedRows(t *testing.T) {
	b := newBook(t)
	srv := httptest.NewServer(b.srv)
	t.Cleanup(srv.Close)
	first := b.commit(t, event.Event{Log: "a", Msg: "0"}, event.Event{Log: "b", Msg: "skip"})
	head := store.Cursor{StoreID: b.st.StoreID(), Seq: first[0].Seq}
	s := openStream(t, srv.URL+"/api/events/stream?q=log:a", head.String())
	var got []string
	var last string
	go func() { // commits while the client reads, drops and resumes
		for i := 1; i <= 20; i++ {
			pair := []event.Event{{Log: "a", Provider: "a", Level: event.Info, Msg: strconv.Itoa(i)}, {Log: "b", Provider: "b", Level: event.Info, Msg: "skip"}}
			if err := b.st.Commit(context.Background(), pair); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for len(got) < 10 {
		m := s.next(t)
		got, last = append(got, m.event(t).Msg), m.id
	}
	s.body.Close()
	s = openStream(t, srv.URL+"/api/events/stream?q=log:a", last)
	for len(got) < 20 {
		m := s.next(t)
		if c, err := store.ParseCursor(m.id); err != nil || c.StoreID != b.st.StoreID() {
			t.Fatalf("id %q: %v", m.id, err)
		}
		got = append(got, m.event(t).Msg)
	}
	for i, m := range got {
		if m != strconv.Itoa(i+1) {
			t.Fatalf("stream = %v: missed or repeated a row", got)
		}
	}

	s = openStream(t, srv.URL+"/api/events/stream?q=log:a", "00000000000000000000000000000000.5")
	if m := s.next(t); m.kind != "restart" {
		t.Fatalf("a foreign cursor: first message %+v", m)
	}
	if m := s.next(t); m.event(t).Msg != "0" {
		t.Errorf("after restart: %+v", m)
	}
}

// POST /api/events files outside events under ext., marked external, with
// only what an outside writer may set; Tenant's own logs and providers are
// refused, and a batch is taken or refused whole.
func TestIngest(t *testing.T) {
	b := newBook(t)
	trace := "0123456789abcdef0123456789abcdef"
	body := `{"log":"deploy","msg":"build started","level":"warning","trace":"` + trace + `","session":"s1","actor":"operator",
		"id":4103,"provider":"deploy","keywords":["audit.failure"],"data":{"commit":"abc","event":"security/4103","n":3}}`
	a := decode[Accepted](t, b.do(t, "POST", "/api/events", "application/json", body), 202)
	if a.Accepted != 1 {
		t.Fatalf("accepted = %d", a.Accepted)
	}
	batch := `{"log":"ext.deploy","msg":"one"}` + "\n\n" + `{"log":"deploy","msg":"two"}` + "\n"
	if a := decode[Accepted](t, b.do(t, "POST", "/api/events", "application/x-ndjson", batch), 202); a.Accepted != 2 {
		t.Fatalf("batch accepted = %d", a.Accepted)
	}
	evs := b.waitFor(t, "ext.deploy", func(evs []event.Event) bool { return len(evs) == 3 })
	if since := time.Since(evs[1].Time); since < 0 || since > time.Minute {
		t.Errorf("an event with no time is stamped when it arrives: %v", evs[1].Time)
	}
	e := evs[0]
	if e.Provider != "ext.deploy" || e.ID != 0 || e.Level != event.Warning || e.Corr.TraceID != trace || e.Corr.SessionID != "" ||
		e.Actor != "" || !slices.Equal(e.Keywords, []string{"external"}) || len(e.Data) != 2 || e.Data[1].Value != int64(3) {
		t.Errorf("filed as %+v", e)
	}
	for _, c := range []struct {
		body   string
		status int
		want   string
	}{
		{`{"log":"security","msg":"forged"}`, 403, "Tenant's own"},
		{`{"log":"Discord","msg":"forged"}`, 403, "Tenant's own"},
		{`{"log":"x","provider":"tenant.approval","msg":"forged"}`, 403, "Tenant's own"},
		{`{"log":"x","provider":"system","msg":"forged"}`, 403, "Tenant's own"},
		{`{"log":"x","msg":" "}`, 400, "msg"},
		{`{"log":"x","msg":"m","trace":"nothex"}`, 400, "trace"},
		{`{"log":"x","msg":"m","data":{"bad key":1}}`, 400, "key"},
		{`{"msg":"m"}`, 400, "log"},
		{`not json`, 400, "not an event"},
	} {
		rec := b.do(t, "POST", "/api/events", "application/json", c.body)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s → %d %s", c.body, rec.Code, rec.Body.String())
		}
	}
	rec := b.do(t, "POST", "/api/events", "application/x-ndjson", `{"log":"x","msg":"fine"}`+"\n"+`{"log":"security","msg":"no"}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "line 2") {
		t.Errorf("a batch with a reserved line = %d %s", rec.Code, rec.Body.String())
	}
	time.Sleep(200 * time.Millisecond) // two pipeline batches
	if evs := b.waitFor(t, "ext.x", func([]event.Event) bool { return true }); len(evs) != 0 {
		t.Errorf("a refused batch was partly filed: %+v", evs)
	}
}

// The logs, their policies and debug switch; a change applies and is
// recorded; the Security log is never disabled or cleared; clear empties a
// log.
func TestLogsChangeAndClear(t *testing.T) {
	b := newBook(t)
	b.commit(t, event.Event{Log: "discord", Msg: "one"}, event.Event{Log: "discord", Msg: "two"}, event.Event{Log: "security", Msg: "audit"})
	ls := decode[Logs](t, b.do(t, "GET", "/api/logbook/logs", "", ""), 200)
	if len(ls.Logs) != 2 || ls.Logs[0].Name != "discord" || ls.Logs[0].Events != 2 || ls.Logs[0].Policy.MaxAgeS != 30*86400 ||
		!slices.Equal(ls.Logs[0].Group, []string{"Subsystems", "Channels"}) || ls.Logs[0].LastWrite == nil || !ls.Logs[0].Policy.Enabled {
		t.Fatalf("logs = %+v", ls)
	}
	l := decode[Log](t, b.do(t, "PATCH", "/api/logbook/logs/discord", "application/json",
		`{"max_bytes":1048576,"min_level":"warning","on_full":"keep","debug_for":"30m"}`), 200)
	if l.Policy.MaxBytes != 1<<20 || l.Policy.MinLevel != "warning" || l.Policy.OnFull != "keep" || l.Policy.MaxAgeS != 30*86400 || l.DebugUntil == nil {
		t.Fatalf("after PATCH = %+v", l)
	}
	l = decode[Log](t, b.do(t, "PATCH", "/api/logbook/logs/discord", "application/json", `{"reset":true,"debug_for":"0"}`), 200)
	if l.Policy.MaxBytes != 64<<20 || l.Policy.MinLevel != "" || l.DebugUntil != nil {
		t.Fatalf("after reset = %+v", l)
	}
	for body, want := range map[string]string{`{"debug_for":"48h"}`: "debug_for", `{"min_level":"loud"}`: "min_level", `{"on_full":"shred"}`: "when full", `{"nope":1}`: "unknown field"} {
		if rec := b.do(t, "PATCH", "/api/logbook/logs/discord", "application/json", body); rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s → %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := b.do(t, "PATCH", "/api/logbook/logs/security", "application/json", `{"enabled":false}`); rec.Code != 400 {
		t.Errorf("disabling security = %d", rec.Code)
	}
	if rec := b.do(t, "POST", "/api/logbook/logs/security/clear", "", ""); rec.Code != 403 {
		t.Errorf("clearing security = %d", rec.Code)
	}
	if c := decode[Cleared](t, b.do(t, "POST", "/api/logbook/logs/discord/clear", "", ""), 200); c.Cleared != 2 {
		t.Errorf("cleared = %+v", c)
	}
	if l := decode[Log](t, b.do(t, "GET", "/api/logbook/logs/discord", "", ""), 200); l.Events != 0 || l.Cleared == nil {
		t.Errorf("after clear = %+v", l)
	}
	if rec := b.do(t, "GET", "/api/logbook/logs/nosuch", "", ""); rec.Code != 404 {
		t.Errorf("unknown log = %d", rec.Code)
	}
	b.waitFor(t, "logbook", func(evs []event.Event) bool {
		return slices.ContainsFunc(evs, func(e event.Event) bool { return strings.Contains(e.Msg, "discord cleared") }) &&
			slices.ContainsFunc(evs, func(e event.Event) bool { return strings.Contains(e.Msg, "discord settings changed") })
	})
}

func TestViews(t *testing.T) {
	b := newBook(t)
	b.api.BootStart = time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	vs := decode[Views](t, b.do(t, "GET", "/api/logbook/views", "", ""), 200)
	var names []string
	for _, v := range vs.Views {
		names = append(names, v.Name+"="+v.Filter)
	}
	if want := []string{"Administrative Events=level>=warning", "Security failures=log:security tag:audit.failure", "This run=boot:current",
		"Since last start=since:2026-09-27T08:00:00Z"}; !slices.Equal(names, want) {
		t.Fatalf("views = %q", names)
	}
	for _, v := range vs.Views { // every built-in filter parses, and runs
		if rec := b.do(t, "GET", "/api/events?q="+strings.ReplaceAll(v.Filter, " ", "+"), "", ""); rec.Code != 200 {
			t.Errorf("view %s: %d %s", v.Name, rec.Code, rec.Body.String())
		}
	}
	decode[View](t, b.do(t, "POST", "/api/logbook/views", "application/json", `{"name":"Discord errors","filter":"log:discord level>=error"}`), 201)
	if v := decode[View](t, b.do(t, "GET", "/api/logbook/views/Discord%20errors", "", ""), 200); v.Filter != "log:discord level>=error" || v.Builtin {
		t.Errorf("saved view = %+v", v)
	}
	for body, status := range map[string]int{`{"name":"x","filter":"level>=loud"}`: 400, `{"name":"this run","filter":"log:a"}`: 403, `{"name":"","filter":"log:a"}`: 400} {
		if rec := b.do(t, "POST", "/api/logbook/views", "application/json", body); rec.Code != status {
			t.Errorf("%s → %d, want %d", body, rec.Code, status)
		}
	}
	if rec := b.do(t, "DELETE", "/api/logbook/views/This%20run", "", ""); rec.Code != 403 {
		t.Errorf("deleting a built-in = %d", rec.Code)
	}
	if rec := b.do(t, "DELETE", "/api/logbook/views/Discord%20errors", "", ""); rec.Code != 204 {
		t.Errorf("delete = %d", rec.Code)
	}
	if rec := b.do(t, "DELETE", "/api/logbook/views/Discord%20errors", "", ""); rec.Code != 404 {
		t.Errorf("delete again = %d", rec.Code)
	}
}

// Stats, top IDs, the timeline with and without a run record, the catalog,
// components and verify.
func TestAggregatesTimelineCatalogComponentsVerify(t *testing.T) {
	b := newBook(t)
	trace, act := runtrace.NewID(), runtrace.NewID()
	b.commit(t,
		event.Event{Log: "turns", Msg: "turn", Corr: event.Correlation{TraceID: trace}},
		event.Event{Log: "sandbox", Msg: "run", Opcode: event.OpStart, Corr: event.Correlation{ActivityID: act, RelatedID: trace}},
		event.Event{Log: "sandbox", Msg: "child output", Corr: event.Correlation{ActivityID: act}},
		event.Event{Log: "system", Provider: "system", ID: 3101, Level: event.Error, Msg: "x failed"},
		event.Event{Log: "other", Msg: "unrelated"})
	st := decode[Stats](t, b.do(t, "GET", "/api/events/stats?bucket=1h", "", ""), 200)
	if len(st.Buckets) != 4 || st.Buckets[0].Start == nil || st.Buckets[0].Log != "other" {
		t.Errorf("stats = %+v", st)
	}
	if rec := b.do(t, "GET", "/api/events/stats?bucket=soon", "", ""); rec.Code != 400 {
		t.Errorf("bad bucket = %d", rec.Code)
	}
	top := decode[Top](t, b.do(t, "GET", "/api/events/top?q=level>=error", "", ""), 200)
	if len(top.Top) != 1 || top.Top[0] != (IDCount{Provider: "system", ID: 3101, N: 1}) {
		t.Errorf("top = %+v", top)
	}
	tl := decode[Timeline](t, b.do(t, "GET", "/api/events/timeline/"+trace, "", ""), 200)
	if msgs(tl.Events) != "turn,run,child output" || tl.Run != nil || !strings.Contains(tl.RunNote, "can't be read") {
		t.Errorf("timeline = %+v", tl)
	}
	b.api.Runs = func(_ context.Context, id string) (runtrace.Run, error) {
		if id == trace {
			return runtrace.Run{TraceID: id, Outcome: "ok"}, nil
		}
		return runtrace.Run{}, errors.New("runtrace: trace " + id + " not found")
	}
	if tl := decode[Timeline](t, b.do(t, "GET", "/api/events/timeline/"+trace, "", ""), 200); tl.Run == nil || tl.Run.Outcome != "ok" {
		t.Errorf("timeline with a run = %+v", tl)
	}
	if tl := decode[Timeline](t, b.do(t, "GET", "/api/events/timeline/"+act, "", ""), 200); msgs(tl.Events) != "run,child output" || !strings.Contains(tl.RunNote, "not found") {
		t.Errorf("activity timeline = %+v", tl)
	}

	c := decode[Catalog](t, b.do(t, "GET", "/api/logbook/catalog/system?id=3101", "", ""), 200)
	if len(c.Specs) != 1 || c.Specs[0].Filter != "provider:system id:3101" || !strings.HasPrefix(c.Markdown, "### system 3101") || c.Version != CatalogVersion() {
		t.Errorf("catalog entry = %+v", c)
	}
	if c := decode[Catalog](t, b.do(t, "GET", "/api/logbook/catalog", "", ""), 200); len(c.Specs) != len(catalog.Specs("")) || !strings.Contains(c.Markdown, "## system") {
		t.Errorf("catalog has %d specs", len(c.Specs))
	}
	for _, p := range []string{"/api/logbook/catalog/nosuch", "/api/logbook/catalog/system?id=1234"} {
		if rec := b.do(t, "GET", p, "", ""); rec.Code != 404 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}

	if cs := decode[Components](t, b.do(t, "GET", "/api/logbook/components", "", ""), 200); cs.Components == nil || len(cs.Components) != 0 {
		t.Errorf("components before the registry = %+v", cs)
	}
	b.api.Components = func() []Component {
		return []Component{{Name: "discord", Kind: "connector", Log: "discord", State: "running"}}
	}
	if cs := decode[Components](t, b.do(t, "GET", "/api/logbook/components", "", ""), 200); len(cs.Components) != 1 {
		t.Errorf("components = %+v", cs)
	}

	v := decode[Verify](t, b.do(t, "GET", "/api/logbook/verify?log=security", "", ""), 200)
	if !v.OK || !strings.Contains(v.Note, "TEN-396") {
		t.Errorf("verify = %+v", v)
	}
	if rec := b.do(t, "GET", "/api/logbook/verify?log=discord", "", ""); rec.Code != 400 {
		t.Errorf("verify discord = %d", rec.Code)
	}
}

// An export is a header record, then every match in order, sensitive
// values masked unless asked for; it is recorded.
func TestExport(t *testing.T) {
	b := newBook(t)
	b.commit(t, event.Event{Log: "a", Msg: "one", Data: []event.Field{{Key: "k", Value: "secret", Sensitive: true}}},
		event.Event{Log: "b", Msg: "skip"}, event.Event{Log: "a", Msg: "two"})
	rec := b.do(t, "GET", "/api/events/export?q=log:a", "", "")
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if rec.Code != 200 || len(lines) != 3 || rec.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("export = %d %q", rec.Code, lines)
	}
	var hdr struct{ Export ExportHeader }
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil || hdr.Export.Schema != store.SchemaVersion() ||
		hdr.Export.Filter != "log:a" || hdr.Export.Catalog == "" || hdr.Export.Host == "" || hdr.Export.Exported.IsZero() {
		t.Fatalf("header = %s (%v)", lines[0], err)
	}
	var e event.Event
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil || e.Msg != "one" || e.Data[0].Value != event.Masked {
		t.Errorf("first event = %s", lines[1])
	}
	rec = b.do(t, "GET", "/api/events/export?q=log:a&format=csv&sensitive=1", "", "")
	if body := rec.Body.String(); !strings.HasPrefix(body, "# tenant logbook export: schema=") || !strings.Contains(body, "\nseq,time,level,") ||
		!strings.Contains(body, `secret`) || strings.Count(body, "\n") != 4 {
		t.Errorf("csv export:\n%s", body)
	}
	if rec := b.do(t, "GET", "/api/events/export?format=xml", "", ""); rec.Code != 400 {
		t.Errorf("format xml = %d", rec.Code)
	}
	b.waitFor(t, "logbook", func(evs []event.Event) bool {
		return slices.ContainsFunc(evs, func(e event.Event) bool { return e.Msg == "logbook: exported events" })
	})
}

// With no store, reads are 503 and an outside event still reaches the text
// files.
func TestWithoutAStore(t *testing.T) {
	root := t.TempDir()
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	srv := (&API{Hub: func() *logs.Hub { return h }}).Handler()
	for _, p := range []string{"/api/events", "/api/logbook/logs", "/api/events/stream"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 503 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("POST", "/api/events", strings.NewReader(`{"log":"deploy","msg":"hi"}`)))
	if all, _ := h.Snapshot(); rec.Code != 202 || len(all) != 1 || all[0].Source != "ext.deploy" {
		t.Errorf("ingest without a store = %d %+v", rec.Code, all)
	}
	rec = httptest.NewRecorder()
	(&API{Hub: func() *logs.Hub { return nil }}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/events", nil))
	if rec.Code != 503 {
		t.Errorf("no hub = %d", rec.Code)
	}
}

func TestExternalLogAndDuration(t *testing.T) {
	for in, want := range map[string]string{"deploy": "ext.deploy", "ext.deploy": "ext.deploy", "My Script!": "ext.my-script",
		strings.Repeat("x", 50): "ext." + strings.Repeat("x", 36), "..": ""} {
		if got, _ := ExternalLog(in); got != want {
			t.Errorf("ExternalLog(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "7d": 7 * 24 * time.Hour, "1.5d": 36 * time.Hour} {
		if got, err := Duration(in); err != nil || got != want {
			t.Errorf("Duration(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := Duration("xd"); err == nil {
		t.Error("Duration(xd) should fail")
	}
}
