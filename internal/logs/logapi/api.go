// Package logapi is the log book's HTTP API, v2 (docs/design/logging-v2.md
// §9.5). The dashboard mounts it behind its bearer auth and httplimits, and
// tenant logs runs the same handlers in its own process when it can open
// logbook.db itself, so the local and the remote path have the same
// features by construction.
//
//	GET    /api/events?q=&cursor=&limit=&reverse=&count=   a page of events
//	GET    /api/events/{seq}                               one event, its catalog entry and statistics
//	GET    /api/events/stream?q=                           SSE live tail; resumes from Last-Event-ID
//	GET    /api/events/stats?q=&bucket=                    counts by span, log and level
//	GET    /api/events/top?q=&n=                           the most frequent event IDs
//	GET    /api/events/timeline/{id}?depth=                a trace's or activity's timeline, and its run
//	GET    /api/events/export?q=&format=                   every match, after a header record
//	POST   /api/events                                     one event (JSON) or a batch (NDJSON), from outside
//	GET    /api/logbook/logs[/{log}]                       the logs, their sizes and policies
//	PATCH  /api/logbook/logs/{log}                         change a log's policy, or its debug switch
//	POST   /api/logbook/logs/{log}/clear                   clear a log
//	GET    /api/logbook/views[/{name}]                     the built-in and saved views
//	POST   /api/logbook/views                              save a view
//	DELETE /api/logbook/views/{name}                       delete one
//	GET    /api/logbook/catalog[/{provider}][?id=N]        the event reference
//	GET    /api/logbook/components                         what is running
//	GET    /api/logbook/verify[?log=security]              the integrity check, and the Security log's chain
//	GET    /api/logbook/audit?since=&until=                the Security log's audit export bundle
//
// Cursors are <store ID>.<seq>; one from another store starts over and says
// so. A read with sensitive=1 returns sensitive values, and the reveal is
// recorded.
package logapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
	"tenant/internal/version"
)

// API serves one hub's log book.
type API struct {
	// Hub is the log book, with its store attached. Read per request, so a
	// hub installed after Mount is served; nil answers 503.
	Hub func() *logs.Hub
	// Runs finds a trace's run record in runs.db; nil when this process
	// can't read it.
	Runs RunLookup
	// Components lists what is running (the component registry, TEN-380);
	// nil lists nothing.
	Components func() []Component
	// BootStart is when the process boot:current names started, for the
	// Since last start view; zero means this process.
	BootStart time.Time
	// Security verifies and exports the Security log's chain, which needs
	// its key, anchor and witness: the composition root holds them.
	Security SecurityLog
}

// RunLookup finds the run record of a trace.
type RunLookup func(ctx context.Context, traceID string) (runtrace.Run, error)

// SecurityLog is the Security log's chain tools (TEN-396). A nil func is
// not available in this process.
type SecurityLog struct {
	Verify func(ctx context.Context) (store.Verification, error)
	// Export writes the audit export bundle of the records from since to
	// until (zero: unbounded).
	Export func(ctx context.Context, w io.Writer, since, until time.Time) error
}

// started is when this process started, near enough.
var started = time.Now()

const (
	pageDefault = 100
	pageMax     = 1000
	streamPage  = 500
	keepAlive   = 15 * time.Second
	maxBody     = 1 << 20 // the dashboard's own request limit
	maxDebug    = 24 * time.Hour
	maxFields   = 64
)

// Mount registers the routes on mux.
func (a *API) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events", a.events)
	mux.HandleFunc("POST /api/events", a.ingest)
	mux.HandleFunc("GET /api/events/{seq}", a.event)
	mux.HandleFunc("GET /api/events/stream", a.stream)
	mux.HandleFunc("GET /api/events/stats", a.stats)
	mux.HandleFunc("GET /api/events/top", a.top)
	mux.HandleFunc("GET /api/events/timeline/{id}", a.timeline)
	mux.HandleFunc("GET /api/events/export", a.export)
	mux.HandleFunc("GET /api/logbook/logs", a.logs)
	mux.HandleFunc("GET /api/logbook/logs/{log}", a.log)
	mux.HandleFunc("PATCH /api/logbook/logs/{log}", a.change)
	mux.HandleFunc("POST /api/logbook/logs/{log}/clear", a.clear)
	mux.HandleFunc("GET /api/logbook/views", a.views)
	mux.HandleFunc("POST /api/logbook/views", a.saveView)
	mux.HandleFunc("GET /api/logbook/views/{name}", a.view)
	mux.HandleFunc("DELETE /api/logbook/views/{name}", a.deleteView)
	mux.HandleFunc("GET /api/logbook/catalog", a.catalog)
	mux.HandleFunc("GET /api/logbook/catalog/{provider}", a.catalog)
	mux.HandleFunc("GET /api/logbook/components", a.components)
	mux.HandleFunc("GET /api/logbook/verify", a.verify)
	mux.HandleFunc("GET /api/logbook/audit", a.audit)
}

// Handler is the routes on a mux of their own.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.Mount(mux)
	return mux
}

// Streaming reports whether r is a long response, which the dashboard
// exempts from its write timeout.
func Streaming(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/events/stream", "/api/events/export", "/api/logbook/audit":
		return r.Method == http.MethodGet
	}
	return false
}

func (a *API) hub(w http.ResponseWriter) (*logs.Hub, bool) {
	var h *logs.Hub
	if a.Hub != nil {
		h = a.Hub()
	}
	if h == nil {
		writeError(w, http.StatusServiceUnavailable, "the log book is not open here")
	}
	return h, h != nil
}

// book is the hub and its store, or a 503.
func (a *API) book(w http.ResponseWriter) (*logs.Hub, *store.Store, bool) {
	h, ok := a.hub(w)
	if !ok {
		return nil, nil, false
	}
	st := h.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "logbook.db is not open here; only the text files are being written")
		return nil, nil, false
	}
	return h, st, true
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	v := r.URL.Query()
	f, ok := filter(w, v.Get("q"))
	if !ok {
		return
	}
	from, ok := cursor(w, v.Get("cursor"))
	if !ok {
		return
	}
	evs, next, restarted, err := st.Query(r.Context(), f, from, number(v, "limit", pageDefault, pageMax), flag(v, "reverse"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	page := EventsPage{Events: nonNil(evs), Next: next.String(), Restarted: restarted}
	if flag(v, "count") {
		n, err := st.Count(r.Context(), f)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		page.Count = &n
	}
	reveal(r, h, page.Events)
	writeJSON(w, http.StatusOK, page)
}

func (a *API) event(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "seq: a number")
		return
	}
	e, err := st.Get(r.Context(), seq)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no event %d", seq))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	evs := []event.Event{e}
	reveal(r, h, evs)
	d := EventDetail{Event: evs[0]}
	if e.ID != 0 {
		if s, ok := catalog.Lookup(e.Provider, e.ID); ok {
			v := specView(s)
			d.Spec, d.Help = &v, catalog.Help(s)
		}
		is, err := st.EventStats(r.Context(), e.Provider, e.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		d.Stats = &IDStats{Count: is.Count, First: when(is.First), Last: when(is.Last), Trend: is.Trend}
	}
	writeJSON(w, http.StatusOK, d)
}

// stream is the live tail: every event matching q after the cursor in
// Last-Event-ID (or ?cursor=; neither starts at the newest event), then
// each new one as it is committed. Each message's id is the event's cursor,
// so a client that reconnects with it misses no row. A cursor from another
// store sends a restart message and starts over from the beginning.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	v := r.URL.Query()
	f, ok := filter(w, v.Get("q"))
	if !ok {
		return
	}
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = v.Get("cursor")
	}
	from, ok := cursor(w, last)
	if !ok {
		return
	}
	ctx := r.Context()
	wake, cancel := st.Subscribe() // before the first read, so no commit falls between
	defer cancel()
	if last == "" {
		head, err := st.Latest(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		from = head
	}
	sensitive := flag(v, "sensitive")
	if sensitive {
		h.Logf("logbook", slog.LevelInfo, "logbook: sensitive fields revealed in a live tail", "filter", v.Get("q"))
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	flush()
	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		head, err := st.Latest(ctx)
		if err != nil {
			return // the client reconnects from its last id
		}
		evs, next, restarted, err := st.Query(ctx, f, from, streamPage, false)
		if err != nil {
			return
		}
		if restarted {
			if _, err := fmt.Fprintf(w, "event: restart\ndata: {\"store\":%q}\n\n", hex.EncodeToString(next.StoreID[:])); err != nil {
				return
			}
		}
		if !sensitive {
			mask(evs)
		}
		for _, e := range evs {
			b, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "id: %s\ndata: %s\n\n", store.Cursor{StoreID: next.StoreID, Seq: e.Seq}, b); err != nil {
				return
			}
		}
		from = next
		if len(evs) == streamPage {
			continue
		}
		// Nothing else up to the head matched: don't read it again.
		if head.StoreID == from.StoreID && head.Seq > from.Seq {
			from.Seq = head.Seq
		}
		flush()
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-tick.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flush()
		}
	}
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	_, st, ok := a.book(w)
	if !ok {
		return
	}
	v := r.URL.Query()
	f, ok := filter(w, v.Get("q"))
	if !ok {
		return
	}
	var bucket time.Duration
	if b := v.Get("bucket"); b != "" {
		var err error
		if bucket, err = Duration(b); err != nil || bucket <= 0 {
			writeError(w, http.StatusBadRequest, "bucket: a duration such as 15m, 1h or 1d")
			return
		}
	}
	bs, err := st.Stats(r.Context(), f, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := Stats{Buckets: []Bucket{}}
	for _, b := range bs {
		out.Buckets = append(out.Buckets, Bucket{Start: when(b.Start), Log: b.Log, Level: b.Level.String(), N: b.N})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) top(w http.ResponseWriter, r *http.Request) {
	_, st, ok := a.book(w)
	if !ok {
		return
	}
	v := r.URL.Query()
	f, ok := filter(w, v.Get("q"))
	if !ok {
		return
	}
	ids, err := st.TopIDs(r.Context(), f, number(v, "n", 10, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := Top{Top: []IDCount{}}
	for _, c := range ids {
		out.Top = append(out.Top, IDCount{Provider: c.Provider, ID: c.ID, N: c.N})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) timeline(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	evs, err := st.Timeline(r.Context(), id, number(r.URL.Query(), "depth", store.MaxTimelineDepth, store.MaxTimelineDepth))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := Timeline{ID: id, Events: nonNil(evs)}
	reveal(r, h, out.Events)
	if a.Runs == nil {
		out.RunNote = "run records (runs.db) can't be read from this process"
	} else if run, err := a.Runs(r.Context(), id); err != nil {
		out.RunNote = "no run record: " + err.Error()
	} else {
		out.Run = &run
	}
	writeJSON(w, http.StatusOK, out)
}

// export writes every event matching q, oldest first, up to the newest one
// when it starts, after a header record, as NDJSON (the default), CSV or
// text. The export is recorded in the logbook log; TEN-398 makes it a
// Security record.
func (a *API) export(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	v := r.URL.Query()
	q := v.Get("q")
	f, ok := filter(w, q)
	if !ok {
		return
	}
	format := v.Get("format")
	if format == "" {
		format = NDJSON
	}
	ctx := r.Context()
	head, err := st.Latest(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ew, err := NewWriter(w, format, time.Local)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	host, _ := os.Hostname()
	sensitive := flag(v, "sensitive")
	hdr := ExportHeader{Schema: store.SchemaVersion(), Catalog: CatalogVersion(), Tenant: version.Version,
		Filter: q, Exported: time.Now().UTC(), Host: host, Sensitive: sensitive}
	w.Header().Set("Content-Type", contentTypes[format])
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=logbook-%s.%s", hdr.Exported.Format("20060102-150405"), extensions[format]))
	if err := ew.Header(hdr); err != nil {
		return
	}
	var n int64
	from := store.Cursor{StoreID: head.StoreID}
	for err == nil {
		var evs []event.Event
		if evs, from, _, err = st.Query(ctx, f, from, pageMax, false); err != nil || len(evs) == 0 {
			break
		}
		if !sensitive {
			mask(evs)
		}
		for _, e := range evs {
			if e.Seq > head.Seq {
				break
			}
			if err = ew.Write(e); err != nil {
				break
			}
			n++
		}
		if len(evs) < pageMax || evs[len(evs)-1].Seq >= head.Seq {
			break
		}
	}
	if ferr := ew.Flush(); err == nil {
		err = ferr
	}
	kv := []any{"events", n, "filter", q, "format", format, "sensitive", sensitive}
	if err != nil {
		h.Logf("logbook", slog.LevelWarn, "logbook: an export stopped before its end", append(kv, "err", err)...)
		return
	}
	h.Logf("logbook", slog.LevelInfo, "logbook: exported events", kv...)
}

// ingest is POST /api/events: an event from outside the binary, as JSON, or
// a batch, one per line, as NDJSON (Content-Type application/x-ndjson). Each
// is filed under its log forced into ext. (a log named deploy is ext.deploy),
// as logs.External keeps it, with keyword external. A batch is taken whole or
// refused whole: a log or provider that is Tenant's own is 403, a malformed
// event 400, both naming the line.
func (a *API) ingest(w http.ResponseWriter, r *http.Request) {
	h, ok := a.hub(w)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	lines := [][]byte{raw}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-ndjson") {
		lines = bytes.Split(raw, []byte("\n"))
	}
	var evs []event.Event
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		e, status, msg := decodeExternal(line)
		if status != 0 {
			if len(lines) > 1 {
				msg = fmt.Sprintf("line %d: %s", i+1, msg)
			}
			writeError(w, status, msg)
			return
		}
		evs = append(evs, e)
	}
	if len(evs) == 0 {
		writeError(w, http.StatusBadRequest, "no event in the body")
		return
	}
	for i := range evs {
		h.Emit(r.Context(), &evs[i])
	}
	writeJSON(w, http.StatusAccepted, Accepted{Accepted: len(evs)})
}

var (
	idRe  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	keyRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// attrKeys are the names the text mirror gives an event's own attributes
// (logs.EntryFromEvent); a data field under one would pass for the attribute.
var attrKeys = map[string]bool{"event": true, "severity": true, "ev": true, "task": true, "opcode": true,
	"trace": true, "activity": true, "related": true, "session": true, "approval": true, "tool_call": true,
	"actor": true, "surface": true}

// decodeExternal reads one event from outside the binary and keeps what such
// a writer may set. A non-zero status refuses it, for the reason given.
func decodeExternal(raw []byte) (event.Event, int, string) {
	var e event.Event
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, http.StatusBadRequest, "not an event: " + err.Error()
	}
	if e.Msg = strings.TrimSpace(e.Msg); e.Msg == "" {
		return e, http.StatusBadRequest, "msg required"
	}
	for _, name := range []string{e.Log, e.Provider} {
		if name != "" && Reserved(name) {
			return e, http.StatusForbidden, fmt.Sprintf("%q is one of Tenant's own logs or providers; write under a name of your own (it is filed as ext.<name>)", name)
		}
	}
	log, ok := ExternalLog(e.Log)
	if !ok {
		return e, http.StatusBadRequest, "log: a name of letters, digits, . _ or -"
	}
	e.Log = log
	for _, id := range []string{e.Corr.TraceID, e.Corr.ActivityID} {
		if id != "" && !idRe.MatchString(id) {
			return e, http.StatusBadRequest, "trace and activity: 32 lowercase hex characters (TENANT_TRACE_ID, TENANT_ACTIVITY_ID)"
		}
	}
	data := e.Data[:0]
	for _, f := range e.Data {
		if !keyRe.MatchString(f.Key) {
			return e, http.StatusBadRequest, fmt.Sprintf("data: key %q: 1-64 of letters, digits, . _ -", f.Key)
		}
		if !attrKeys[f.Key] {
			data = append(data, f)
		}
	}
	if len(data) > maxFields {
		return e, http.StatusBadRequest, fmt.Sprintf("data: at most %d fields", maxFields)
	}
	e.Data = data
	// A time from the future would dodge retention and every since: filter.
	if now := time.Now().UTC(); e.Time.IsZero() || e.Time.After(now.Add(time.Hour)) {
		e.Time = now
	}
	logs.External(&e)
	return e, 0, ""
}

// Reserved reports whether name is Tenant's own, so no outside writer may use
// it: the Security log, anything under tenant., a v1 built-in source, and
// every registered provider and its log.
func Reserved(name string) bool {
	n := logs.NormalizeSource(name)
	if n == store.SecurityLog || n == "tenant" || strings.HasPrefix(n, "tenant.") || logs.Builtin(n) {
		return true
	}
	for _, p := range event.Providers() {
		if p.Name == n || p.Log == n {
			return true
		}
	}
	return false
}

// ExternalLog is the log an outside writer's name is filed under: the name
// normalized as a log name, under ext. ("deploy" is ext.deploy).
func ExternalLog(name string) (string, bool) {
	n := logs.NormalizeSource(name)
	if n != "" && !strings.HasPrefix(n, "ext.") {
		n = logs.NormalizeSource("ext." + n)
	}
	return n, n != ""
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	ls, err := logViews(r.Context(), h, st, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, Logs{Logs: ls})
}

func (a *API) log(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	a.writeLog(w, r, h, st, r.PathValue("log"))
}

func (a *API) writeLog(w http.ResponseWriter, r *http.Request, h *logs.Hub, st *store.Store, name string) {
	ls, err := logViews(r.Context(), h, st, name)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	case len(ls) == 0:
		writeError(w, http.StatusNotFound, fmt.Sprintf("no log %q", name))
	default:
		writeJSON(w, http.StatusOK, ls[0])
	}
}

// logViews lists the store's logs, or only the one named.
func logViews(ctx context.Context, h *logs.Hub, st *store.Store, only string) ([]Log, error) {
	infos, err := st.Logs(ctx)
	if err != nil {
		return nil, err
	}
	b := h.Budget()
	pols, err := st.Policies(ctx, b)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := []Log{}
	for _, li := range infos {
		if only != "" && li.Name != only {
			continue
		}
		p, ok := pols[li.Name]
		if !ok {
			p = b.For(li.Name)
		}
		l := Log{Name: li.Name, Group: catalog.Group(li.Name), Events: li.Events, Bytes: li.Bytes, Policy: policyView(p),
			LastWrite: when(li.LastWrite), Dropped: li.Dropped, Suppressed: li.Suppressed, Cleared: when(li.Cleared)}
		if li.DebugUntil.After(now) {
			l.DebugUntil = when(li.DebugUntil)
		}
		out = append(out, l)
	}
	return out, nil
}

func policyView(p store.Policy) Policy {
	v := Policy{MaxBytes: p.MaxBytes, MaxAgeS: int64(p.MaxAge / time.Second), OnFull: p.OnFull, Enabled: !p.Disabled}
	if p.MinLevel != 0 {
		v.MinLevel = p.MinLevel.String()
	}
	return v
}

// change is PATCH /api/logbook/logs/{log}. {log} may name a provider too,
// which stands for its log. It applies at once in this process, and within
// a housekeeper tick (30 s) in the others.
func (a *API) change(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	name := logOf(r.PathValue("log"))
	if !logs.ValidSource(name) {
		writeError(w, http.StatusBadRequest, "log: [a-z0-9][a-z0-9._-]{0,39}")
		return
	}
	var c PolicyChange
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	ctx := r.Context()
	kv := []any{"log", name}
	switch {
	case c.Reset:
		if err := st.SetPolicy(ctx, name, store.Policy{}); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		kv = append(kv, "policy", "config.json's")
	case c.MaxBytes != nil || c.MaxAgeS != nil || c.OnFull != nil || c.MinLevel != nil || c.Enabled != nil:
		p, err := st.Policy(ctx, h.Budget(), name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if c.MaxBytes != nil {
			p.MaxBytes = *c.MaxBytes
		}
		if c.MaxAgeS != nil {
			p.MaxAge = time.Duration(*c.MaxAgeS) * time.Second
		}
		if c.OnFull != nil {
			p.OnFull = *c.OnFull
		}
		if c.MinLevel != nil {
			if p.MinLevel = 0; *c.MinLevel != "" && *c.MinLevel != "all" {
				lvl, ok := event.ParseLevel(*c.MinLevel)
				if !ok {
					writeError(w, http.StatusBadRequest, "min_level: critical, error, warning, info, verbose or all")
					return
				}
				p.MinLevel = lvl
			}
		}
		if c.Enabled != nil {
			p.Disabled = !*c.Enabled
		}
		if err := st.SetPolicy(ctx, name, p); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		v := policyView(p)
		kv = append(kv, "max_bytes", v.MaxBytes, "max_age_s", v.MaxAgeS, "on_full", v.OnFull, "min_level", v.MinLevel, "enabled", v.Enabled)
	}
	if c.DebugFor != nil {
		d, err := Duration(*c.DebugFor)
		if err != nil || d < 0 || d > maxDebug {
			writeError(w, http.StatusBadRequest, "debug_for: a duration up to 24h, such as 30m; 0 turns it off")
			return
		}
		var until time.Time
		if d > 0 {
			until = time.Now().Add(d)
		}
		if err := st.SetDebug(ctx, name, until); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		kv = append(kv, "debug_for", d.String())
	}
	if len(kv) > 2 {
		h.Reload(ctx)
		h.Logf("logbook", slog.LevelInfo, "logbook: log "+name+" settings changed", kv...)
	}
	a.writeLog(w, r, h, st, name)
}

// logOf is the log a name stands for: a registered provider's log, else the
// name itself.
func logOf(name string) string {
	for _, p := range event.Providers() {
		if p.Name == name {
			return p.Log
		}
	}
	return name
}

func (a *API) clear(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	name := r.PathValue("log")
	n, err := st.Clear(r.Context(), name)
	if errors.Is(err, store.ErrProtected) {
		writeError(w, http.StatusForbidden, "the Security log is never cleared")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.Logf("logbook", slog.LevelInfo, "logbook: log "+name+" cleared", "log", name, "events", n)
	writeJSON(w, http.StatusOK, Cleared{Log: name, Cleared: n})
}

// builtins are the views every log book has (§9.2).
func (a *API) builtins() []View {
	start := a.BootStart
	if start.IsZero() {
		start = started
	}
	return []View{
		{Name: "Administrative Events", Filter: "level>=warning", Builtin: true},
		{Name: "Security failures", Filter: "log:security tag:audit.failure", Builtin: true},
		{Name: "This run", Filter: "boot:current", Builtin: true},
		{Name: "Since last start", Filter: "since:" + start.UTC().Format(time.RFC3339), Builtin: true},
	}
}

func (a *API) allViews(ctx context.Context, st *store.Store) ([]View, error) {
	saved, err := st.Views(ctx)
	if err != nil {
		return nil, err
	}
	out := a.builtins()
	for _, v := range saved {
		out = append(out, View{Name: v.Name, Filter: v.Filter})
	}
	return out, nil
}

func (a *API) views(w http.ResponseWriter, r *http.Request) {
	_, st, ok := a.book(w)
	if !ok {
		return
	}
	vs, err := a.allViews(r.Context(), st)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, Views{Views: vs})
}

func (a *API) view(w http.ResponseWriter, r *http.Request) {
	_, st, ok := a.book(w)
	if !ok {
		return
	}
	vs, err := a.allViews(r.Context(), st)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, v := range vs {
		if v.Name == r.PathValue("name") {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Sprintf("no view %q", r.PathValue("name")))
}

func (a *API) builtin(name string) bool {
	for _, v := range a.builtins() {
		if strings.EqualFold(v.Name, name) {
			return true
		}
	}
	return false
}

func (a *API) saveView(w http.ResponseWriter, r *http.Request) {
	_, st, ok := a.book(w)
	if !ok {
		return
	}
	var v View
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	v.Name = strings.TrimSpace(v.Name)
	if v.Name == "" || len(v.Name) > 64 || strings.IndexFunc(v.Name, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
		writeError(w, http.StatusBadRequest, "name: 1-64 printable characters")
		return
	}
	if a.builtin(v.Name) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("%q is a built-in view", v.Name))
		return
	}
	if _, ok := filter(w, v.Filter); !ok {
		return
	}
	if err := st.SaveView(r.Context(), v.Name, v.Filter); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, View{Name: v.Name, Filter: v.Filter})
}

func (a *API) deleteView(w http.ResponseWriter, r *http.Request) {
	_, st, ok := a.book(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if a.builtin(name) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("%q is a built-in view", name))
		return
	}
	found, err := st.DeleteView(r.Context(), name)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	case !found:
		writeError(w, http.StatusNotFound, fmt.Sprintf("no view %q", name))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *API) catalog(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	specs := catalog.Specs(provider)
	if provider != "" && len(specs) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no provider %q", provider))
		return
	}
	out := Catalog{Version: CatalogVersion(), Specs: []Spec{}}
	if id := r.URL.Query().Get("id"); id != "" {
		n, err := strconv.ParseUint(id, 10, 32)
		var one []event.Spec
		for _, s := range specs {
			if err == nil && s.ID == uint32(n) {
				one = append(one, s)
			}
		}
		if len(one) == 0 {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no event %s", id))
			return
		}
		specs = one
		out.Markdown = catalog.Help(one[0])
	} else {
		out.Markdown = catalog.Markdown(specs)
	}
	for _, s := range specs {
		out.Specs = append(out.Specs, specView(s))
	}
	writeJSON(w, http.StatusOK, out)
}

// CatalogVersion names the catalog as this process has it: a hash of its
// reference, so it changes whenever any entry does.
func CatalogVersion() string {
	sum := sha256.Sum256([]byte(catalog.Markdown(catalog.Specs(""))))
	return hex.EncodeToString(sum[:6])
}

func specView(s event.Spec) Spec {
	ch := "operational"
	if s.Channel == event.Diagnostic {
		ch = "diagnostic"
	}
	return Spec{Provider: s.Provider, Log: s.Log, ID: s.ID, Level: s.Level.String(), Channel: ch, Task: s.Task,
		Template: s.Template, Fields: s.Fields, Sensitive: s.Sensitive, Keywords: s.Keywords,
		Meaning: s.Meaning, Cause: s.Cause, Action: s.Action, Retired: s.Retired, Filter: catalog.Filter(s)}
}

func (a *API) components(w http.ResponseWriter, _ *http.Request) {
	out := Components{Components: []Component{}}
	if a.Components != nil {
		out.Components = append(out.Components, a.Components()...)
	}
	writeJSON(w, http.StatusOK, out)
}

// verify checks the database's integrity and, for log=security, walks the
// Security log's chain against its key, anchor and witness. The check is
// recorded in the logbook log; TEN-398 makes it a Security record.
func (a *API) verify(w http.ResponseWriter, r *http.Request) {
	h, st, ok := a.book(w)
	if !ok {
		return
	}
	log := r.URL.Query().Get("log")
	if log != "" && log != store.SecurityLog {
		writeError(w, http.StatusBadRequest, "log: only security has a check of its own")
		return
	}
	problems, err := st.IntegrityCheck(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := Verify{Log: log, OK: len(problems) == 0, Problems: problems}
	switch {
	case log == "":
	case a.Security.Verify == nil:
		out.Note = "the Security log's chain can't be checked from this process"
	default:
		v, err := a.Security.Verify(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		c := &Chain{Records: v.Records, Head: v.Head.Rec, Archives: v.Archives, Tombstones: v.Tombstones, Restores: len(v.Restores), Notes: v.Notes}
		if v.Break != nil {
			c.Break, out.OK = v.Break.Error(), false
		}
		out.Chain = c
	}
	kv := []any{"log", log}
	if out.OK {
		h.Logf("logbook", slog.LevelInfo, "logbook: the integrity check passed", kv...)
	} else {
		if out.Chain != nil && out.Chain.Break != "" {
			problems = append(problems, out.Chain.Break)
		}
		h.Logf("logbook", slog.LevelError, "logbook: the integrity check failed", append(kv, "problems", strings.Join(problems, "; "))...)
	}
	writeJSON(w, http.StatusOK, out)
}

// audit writes the Security log's audit export bundle: its records from
// since to until (a duration back from now, or a time, as in a filter),
// with their chain, verifiable offline with the key. The export is recorded.
func (a *API) audit(w http.ResponseWriter, r *http.Request) {
	h, ok := a.hub(w)
	if !ok {
		return
	}
	if a.Security.Export == nil {
		writeError(w, http.StatusServiceUnavailable, "the Security log can't be exported from this process")
		return
	}
	v := r.URL.Query()
	var bounds [2]time.Time
	for i, k := range []string{"since", "until"} {
		if s := v.Get(k); s != "" {
			f, err := query.Parse(k + ":" + s)
			if err != nil || len(f.Terms) != 1 {
				writeError(w, http.StatusBadRequest, k+": a duration (7d) or a time (RFC 3339, 2026-09-27)")
				return
			}
			bounds[i] = f.Terms[0].Values[0].Time
		}
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", "attachment; filename=security-"+time.Now().UTC().Format("20060102-150405")+".ndjson")
	kv := []any{"since", v.Get("since"), "until", v.Get("until")}
	cw := &counter{w: w}
	if err := a.Security.Export(r.Context(), cw, bounds[0], bounds[1]); err != nil {
		if cw.n == 0 { // nothing is out yet: say why
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		h.Logf("logbook", slog.LevelWarn, "logbook: an audit export stopped before its end", append(kv, "err", err)...)
		return
	}
	h.Logf("logbook", slog.LevelInfo, "logbook: exported the Security log's audit bundle", kv...)
}

// reveal masks the sensitive values of evs, unless the request asks for
// them with sensitive=1. A reveal is recorded in the logbook log; TEN-398
// makes it a Security record.
func reveal(r *http.Request, h *logs.Hub, evs []event.Event) {
	if !flag(r.URL.Query(), "sensitive") {
		mask(evs)
		return
	}
	n := 0
	for _, e := range evs {
		for _, f := range e.Data {
			if f.Sensitive {
				n++
			}
		}
	}
	if n > 0 {
		h.Logf("logbook", slog.LevelInfo, "logbook: sensitive fields revealed", "fields", n, "path", r.URL.Path, "filter", r.URL.Query().Get("q"))
	}
}

func mask(evs []event.Event) {
	for i := range evs {
		for j := range evs[i].Data {
			if f := &evs[i].Data[j]; f.Sensitive {
				f.Value = event.Masked
			}
		}
	}
}

func filter(w http.ResponseWriter, q string) (query.Filter, bool) {
	f, err := query.Parse(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return f, false
	}
	return f, true
}

func cursor(w http.ResponseWriter, s string) (store.Cursor, bool) {
	if s == "" {
		return store.Cursor{}, true
	}
	c, err := store.ParseCursor(s)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cursor: <store ID>.<seq>, as a response's next or an SSE id")
		return c, false
	}
	return c, true
}

// Duration reads a duration as Go writes one (90s, 15m, 2h) or in days (7d).
func Duration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(strings.TrimSpace(s), "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(strings.TrimSpace(s))
}

func flag(v url.Values, key string) bool {
	b, _ := strconv.ParseBool(v.Get(key))
	return b
}

// number is the query parameter key, def when absent or not positive, at
// most max.
func number(v url.Values, key string, def, max int) int {
	n, err := strconv.Atoi(v.Get(key))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, max)
}

func when(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nonNil(evs []event.Event) []event.Event {
	if evs == nil {
		return []event.Event{}
	}
	return evs
}

// counter counts what goes through to w.
type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
