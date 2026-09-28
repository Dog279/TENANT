package dashboard

// ssr_logs.go is the Logs page (TEN-379; docs/design/logging-v2.md §9.2): the
// log book in Event Viewer's layout. A tree of views and logs on the left, a
// grid paged by cursor, an event's detail tabs below it, and the actions on
// a log on the right; the Summary page at the tree's root and the Components
// view (TEN-380). Pages render on the server like every other; the grid's
// older pages, the detail pane, the match count and the live tail arrive as
// Datastar SSE patches. It calls what the log book's API handlers call
// (logapi and the store), never the API over HTTP. Log text is untrusted:
// html/template escapes every value, and the Activity feed stays the live,
// friendly view.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
)

const (
	logsPageSize    = 100 // rows per page of the grid: the first, and each scroll
	logsTimelineMax = 500 // rows of the Correlated tab; the rest are a filter away
)

// logsTmpl is parsed here rather than in parseSSR, so the page stays in its
// own files. It takes the ui.html partials like every other set (TEN-211).
var logsTmpl = template.Must(template.New("layout.html").Funcs(uiFuncs).Funcs(assetFuncs).ParseFS(ssrFS, "templates/layout.html", "templates/logs.html", "templates/ui.html"))

func (s *Server) mountLogsSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /logs", s.handleLogsPage)
	mux.HandleFunc("GET /logs/components", s.handleLogsComponents)
	mux.HandleFunc("GET /logs/event/{seq}", s.handleLogsPermalink)
	mux.HandleFunc("GET /logs/rows", s.handleLogsRows)
	mux.HandleFunc("GET /logs/count", s.handleLogsCount)
	mux.HandleFunc("GET /logs/tail", s.handleLogsTail)
	mux.HandleFunc("GET /logs/detail/{seq}", s.handleLogsDetail)
	mux.HandleFunc("POST /logs/detail/{seq}/reveal", s.handleLogsDetail)
	mux.HandleFunc("POST /logs/views", s.handleLogsSaveView)
	mux.HandleFunc("POST /logs/views/delete", s.handleLogsDeleteView)
	mux.HandleFunc("POST /logs/log/{log}/properties", s.handleLogsProperties)
	mux.HandleFunc("POST /logs/log/{log}/clear", needConfirm(nil, s.handleLogsClear)) // the events are gone for good (TEN-211)
}

// logbookStore is the hub and its store; the store is nil when logbook.db
// isn't open in this process.
func (s *Server) logbookStore() (*logs.Hub, *store.Store) {
	if s.logs == nil {
		return nil, nil
	}
	return s.logs, s.logs.Store()
}

type logsData struct {
	layoutData
	Open     bool
	Mode     string // summary, grid or components
	Tree     []*logsNode
	Sel      uint64 // the event the detail pane shows
	Msg, Err string

	// The grid.
	Heading      string
	Q            string // the filter in force
	View         string // the view it came from
	Saved        bool   // that view is the operator's own
	FilterErr    string
	Grid         logsGrid
	CountURL     string
	Live         bool
	LiveHref     string
	TailURL      string
	Exports      []logsLink
	Props        *logsProps // the one log the filter names: its Properties and Clear
	ClearExports []logsLink
	Detail       *logsDetail

	Summary    *logsSummary
	Components *logsComps
}

type logsLink struct {
	Label, Href, Get string
	On               bool
}

func logsQS(q string) string { return url.Values{"q": {q}}.Encode() }

// handleLogsPage is GET /logs: the Summary page, or with q= (or view=) the
// grid of the events the filter matches, newest first.
func (s *Server) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	if !v.Has("q") && !v.Has("view") && !v.Has("find") {
		s.renderLogs(w, r, "summary", v, 0)
		return
	}
	s.renderLogs(w, r, "grid", v, 0)
}

// handleLogsPermalink is GET /logs/event/{seq}: the page with that event
// selected, in the grid of its log unless q= says otherwise.
func (s *Server) handleLogsPermalink(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		http.Error(w, "seq: a number", http.StatusBadRequest)
		return
	}
	v := r.URL.Query()
	if !v.Has("q") && !v.Has("view") {
		if _, st := s.logbookStore(); st != nil {
			if e, err := st.Get(r.Context(), seq); err == nil {
				v.Set("q", "log:"+e.Log)
			}
		}
	}
	s.renderLogs(w, r, "grid", v, seq)
}

func (s *Server) handleLogsComponents(w http.ResponseWriter, r *http.Request) {
	s.renderLogs(w, r, "components", r.URL.Query(), 0)
}

func (s *Server) renderLogs(w http.ResponseWriter, r *http.Request, mode string, v url.Values, sel uint64) {
	d := logsData{layoutData: layoutData{Title: "Logs", Page: "logs"}, Mode: mode, Sel: sel, Msg: v.Get("msg"), Err: v.Get("err")}
	h, st := s.logbookStore()
	if st == nil {
		s.render(w, logsTmpl, d)
		return
	}
	d.Open = true
	ctx := r.Context()
	sum, err := h.Summary(ctx, time.Now())
	if err != nil {
		d.Err = s.flashFor(ctx, "logs summary", err, "The summary could not be read. The dashboard log has the reason.")
	}
	views, err := s.logbook.Views(ctx, st)
	if err != nil {
		d.Err = s.flashFor(ctx, "logs views", err, "The views could not be read. The dashboard log has the reason.")
	}
	d.View = v.Get("view")
	d.Q = strings.TrimSpace(v.Get("q"))
	if d.View != "" {
		d.Q = ""
		i := slices.IndexFunc(views, func(x logapi.View) bool { return x.Name == d.View })
		if i < 0 {
			d.FilterErr = fmt.Sprintf("There is no view %q.", d.View)
		} else {
			d.Q, d.Saved = views[i].Filter, !views[i].Builtin
		}
	}
	if find := strings.TrimSpace(v.Get("find")); find != "" {
		d.Q = strings.TrimSpace(d.Q + " " + quoteFilter(find))
		d.View, d.Saved = "", false
	}
	d.Tree = logsTree(views, sum, d.View, d.Q, mode)

	switch mode {
	case "summary":
		d.Summary = summaryView(sum)
	case "components":
		d.Components = s.componentsView(ctx)
	default:
		s.gridView(ctx, h, st, &d, v)
	}
	s.render(w, logsTmpl, d)
}

// gridView fills in the grid, the actions and the detail pane.
func (s *Server) gridView(ctx context.Context, h *logs.Hub, st *store.Store, d *logsData, v url.Values) {
	d.Heading = "Events"
	if d.View != "" {
		d.Heading = d.View
	}
	f, err := query.Parse(d.Q)
	if d.FilterErr == "" && err != nil {
		d.FilterErr = err.Error()
	}
	if d.FilterErr != "" {
		return
	}
	from := store.Cursor{}
	if c := v.Get("cursor"); c != "" {
		if from, err = store.ParseCursor(c); err != nil {
			d.Err = "That link to more events is out of date. Showing the newest."
		}
	} else if d.Sel != 0 {
		from = store.Cursor{StoreID: st.StoreID(), Seq: d.Sel + 1} // the selected event heads the page
	}
	if d.Grid, err = logsGridPage(ctx, st, f, d.Q, from); err != nil {
		d.FilterErr = err.Error() // the store refuses what it can't plan, such as boot:current without a boot ID
		return
	}
	d.CountURL = "/logs/count?" + logsQS(d.Q)
	d.Live = v.Get("live") == "1"
	live := url.Values{"q": {d.Q}} // the filter, or the view it came from
	if d.View != "" {
		live = url.Values{"view": {d.View}}
	}
	if d.Live {
		if head, err := st.Latest(ctx); err == nil {
			d.TailURL = "/logs/tail?" + url.Values{"q": {d.Q}, "cursor": {head.String()}}.Encode()
		}
	} else {
		live.Set("live", "1")
	}
	d.LiveHref = "/logs?" + live.Encode()
	d.Exports = exportLinks(d.Q)

	if name := logapi.FilterLog(f); name != "*" {
		if d.View == "" {
			d.Heading = logLabel(name)
		}
		if ls, err := logapi.LogViews(ctx, h, st, name); err == nil && len(ls) == 1 {
			d.Props = propsView(ls[0])
			d.ClearExports = exportLinks("log:" + name)
		}
	}
	if d.Sel != 0 {
		d.Detail = s.logsDetailView(ctx, h, st, d.Sel, v.Get("tab"), d.Q, false)
	}
}

func exportLinks(q string) []logsLink {
	var out []logsLink
	for _, f := range []string{logapi.NDJSON, logapi.CSV, logapi.Text} {
		out = append(out, logsLink{Label: strings.ToUpper(f), Href: "/api/events/export?" + url.Values{"q": {q}, "format": {f}}.Encode()})
	}
	return out
}

// quoteFilter is s as one full-text term of the filter language.
func quoteFilter(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// filterTerm is field:value, the value quoted unless it is a plain word.
func filterTerm(field, value string) string {
	if value != "" && strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == "" {
		return field + ":" + value
	}
	return field + ":" + quoteFilter(value)
}

func logsHref(q string) string { return "/logs?" + logsQS(q) }

// The tree.

type logsNode struct {
	Label, Href, Title string
	On                 bool
	Count              int64 // warnings and worse in the last 24 hours
	Bad                bool  // some of them errors or worse
	Kids               []*logsNode
	rank               int
}

// The tree's order (§9.2): groups by rank, then logs by rank, then by name.
var (
	groupRank = map[string]int{"Custom Views": 1, "Tenant": 2, "Subsystems": 3, "External": 4,
		"Channels": 1, "Scheduler": 2, "Integrations": 3, "MCP": 1, "Plugins": 2, "Model": 4, "Memory": 5,
		"Sandbox": 6, "Dashboard": 7, "Peering": 8, "Backup and Privacy": 9, "Eval": 10}
	logRank   = map[string]int{"system": 1, "security": 2, "turns": 3, "subagents": 4}
	logLabels = map[string]string{"subagents": "Sub-agents", "imessage": "iMessage", "mcp": "MCP"}
)

// logLabel is how the tree names a log: mcp.github is github under MCP.
func logLabel(name string) string {
	if l, ok := logLabels[name]; ok {
		return l
	}
	for _, p := range []string{"mcp.", "plugin.", "ext."} {
		if rest, ok := strings.CutPrefix(name, p); ok && rest != "" {
			return rest
		}
	}
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// logsTree is Custom Views, then every log the store holds (it appears the
// first time it is written) where catalog.Group puts it, with its warnings
// and worse of the last 24 hours from the rollups.
func logsTree(views []logapi.View, sum store.Summary, view, q, mode string) []*logsNode {
	var admin int64
	for _, l := range sum.Logs {
		admin += l.Day.Critical + l.Day.Error + l.Day.Warning
	}
	cv := &logsNode{Label: "Custom Views", rank: groupRank["Custom Views"]}
	for _, v := range views {
		n := &logsNode{Label: v.Name, Href: "/logs?" + url.Values{"view": {v.Name}}.Encode(), On: mode == "grid" && v.Name == view, Title: v.Filter}
		if v.Builtin && v.Filter == "level>=warning" {
			n.Count, n.Title = admin, v.Filter+" · the count is the last 24 hours'"
		}
		cv.Kids = append(cv.Kids, n)
	}
	out := []*logsNode{cv}
	for _, l := range sum.Logs {
		bad := l.Day.Critical + l.Day.Error
		leaf := &logsNode{Label: logLabel(l.Log), Href: logsHref("log:" + l.Log), Title: l.Log, rank: logRank[l.Log],
			On: mode == "grid" && view == "" && q == "log:"+l.Log, Count: bad + l.Day.Warning, Bad: bad > 0}
		kids := &out
		for _, g := range catalog.Group(l.Log) {
			i := slices.IndexFunc(*kids, func(n *logsNode) bool { return n.Href == "" && n.Label == g })
			if i < 0 {
				*kids = append(*kids, &logsNode{Label: g, rank: groupRank[g]})
				i = len(*kids) - 1
			}
			kids = &(*kids)[i].Kids
		}
		*kids = append(*kids, leaf)
	}
	for i, n := range out[1:] {
		out[i+1] = tidy(n)
	}
	slices.SortStableFunc(out, byRank)
	return out
}

// tidy sorts a group, adds up its counts, and folds a group that holds only
// the log of its own name (Sandbox › Sandbox) into that log.
func tidy(n *logsNode) *logsNode {
	if n.Href != "" {
		return n
	}
	for i, k := range n.Kids {
		n.Kids[i] = tidy(k)
		n.Count += n.Kids[i].Count
		n.Bad = n.Bad || n.Kids[i].Bad
	}
	slices.SortStableFunc(n.Kids, byRank)
	if len(n.Kids) == 1 && n.Kids[0].Href != "" && strings.EqualFold(n.Kids[0].Label, n.Label) {
		return n.Kids[0]
	}
	return n
}

func byRank(a, b *logsNode) int {
	ra, rb := a.rank, b.rank
	if ra == 0 {
		ra = 100
	}
	if rb == 0 {
		rb = 100
	}
	if ra != rb {
		return ra - rb
	}
	return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
}

// The grid.

type logsRow struct {
	Seq                     uint64
	Level, Lv               string // its level, and the level's short form
	Time, Stamp             string // local, to the millisecond; in full, local and UTC
	Log, Provider, ID, Task string
	Msg                     string
	Href                    string // its permalink, under the grid's filter
	Detail                  string // the detail pane's patch; "" where there is no pane
}

type logsGrid struct {
	Rows           []logsRow
	More, MoreHref string // the next page: its patch, and the page that starts there; "" at the end
}

var levelShort = map[event.Level]string{event.Critical: "CRT", event.Error: "ERR", event.Warning: "WRN", event.Info: "INF", event.Verbose: "VRB"}

func logsRowOf(e event.Event, q string, pane bool) logsRow {
	t := e.Time.In(time.Local)
	clock := t.Format("15:04:05.000")
	if y, m, dd := t.Date(); y != time.Now().Year() || m != time.Now().Month() || dd != time.Now().Day() {
		clock = t.Format("Jan 02 15:04:05")
	}
	r := logsRow{Seq: e.Seq, Level: e.Level.String(), Lv: levelShort[e.Level], Time: clock,
		Stamp: t.Format("2006-01-02 15:04:05.000 MST") + " · " + e.Time.UTC().Format("15:04:05.000") + " UTC",
		Log:   e.Log, Provider: e.Provider, Task: e.Task, Msg: e.Msg}
	if e.ID != 0 {
		r.ID = strconv.FormatUint(uint64(e.ID), 10)
	}
	seq := strconv.FormatUint(e.Seq, 10)
	r.Href = "/logs/event/" + seq
	if q != "" {
		r.Href += "?" + logsQS(q)
	}
	if pane {
		r.Detail = "/logs/detail/" + seq + "?" + logsQS(q)
	}
	return r
}

// logsGridPage is one page of the grid: the events matching f before from
// (the zero Cursor: the newest), newest first.
func logsGridPage(ctx context.Context, st *store.Store, f query.Filter, q string, from store.Cursor) (logsGrid, error) {
	evs, next, _, err := st.Query(ctx, f, from, logsPageSize, true)
	if err != nil {
		return logsGrid{}, err
	}
	var g logsGrid
	for _, e := range evs {
		g.Rows = append(g.Rows, logsRowOf(e, q, true))
	}
	if len(evs) == logsPageSize {
		c := url.Values{"q": {q}, "cursor": {next.String()}}.Encode()
		g.More, g.MoreHref = "/logs/rows?"+c, "/logs?"+c
	}
	return g, nil
}

// handleLogsRows is the grid's next page, as the sentinel under it asks when
// it scrolls into view: its rows appended, and the sentinel moved on.
func (s *Server) handleLogsRows(w http.ResponseWriter, r *http.Request) {
	_, st := s.logbookStore()
	if st == nil {
		http.Error(w, "the log book is not open here", http.StatusServiceUnavailable)
		return
	}
	v := r.URL.Query()
	f, err := query.Parse(v.Get("q"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	from, err := store.ParseCursor(v.Get("cursor"))
	if err != nil {
		http.Error(w, "cursor: "+err.Error(), http.StatusBadRequest)
		return
	}
	g, err := logsGridPage(r.Context(), st, f, v.Get("q"), from)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows, err1 := logsPart("lgrows", g)
	more, err2 := logsPart("lgmore", g)
	if err := errors.Join(err1, err2); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSSEHeaders(w)
	if rows != "" {
		_, _ = w.Write(patchElements("#lg-rows", "append", rows))
	}
	_, _ = w.Write(patchElements("", "", more))
	flush(w)
}

// handleLogsCount is the grid's count: "12,345 events; 231 match".
func (s *Server) handleLogsCount(w http.ResponseWriter, r *http.Request) {
	_, st := s.logbookStore()
	if st == nil {
		http.Error(w, "the log book is not open here", http.StatusServiceUnavailable)
		return
	}
	f, err := query.Parse(r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	total, err := st.Count(ctx, query.Filter{})
	n := total
	if err == nil && len(f.Terms) > 0 {
		n, err = st.Count(ctx, f)
	}
	text := fmt.Sprintf("%s events; %s match", thousands(total), thousands(n))
	if err != nil {
		text = "can't count: " + err.Error()
	}
	_ = writeDatastarPatch(w, `<span id="lg-count" class="lg-count dim">`+template.HTMLEscapeString(text)+`</span>`)
}

// handleLogsTail is the live tail: each event matching q after the cursor in
// Last-Event-ID (or ?cursor=; neither starts at the newest), prepended to
// the grid as it is committed. Each open page subscribes on its own; each
// patch's id is its cursor, so a reconnect misses no row.
func (s *Server) handleLogsTail(w http.ResponseWriter, r *http.Request) {
	_, st := s.logbookStore()
	if st == nil {
		http.Error(w, "the log book is not open here", http.StatusServiceUnavailable)
		return
	}
	v := r.URL.Query()
	q := v.Get("q")
	f, err := query.Parse(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = v.Get("cursor")
	}
	ctx, act := event.Start(r.Context(), Stream, "stream", "logbook", "remote", r.RemoteAddr)
	defer act.End(nil)
	head, err := st.Latest(ctx)
	if err != nil {
		return
	}
	from, err := store.ParseCursor(last)
	if err != nil || from.StoreID != head.StoreID {
		from = head // another store's cursor would replay all of it: tail from now
	}
	setSSEHeaders(w)
	logapi.Tail(ctx, w, st, f, from, func(evs []event.Event, next store.Cursor, _ bool) error {
		if len(evs) == 0 {
			return nil
		}
		var g logsGrid
		for i := len(evs) - 1; i >= 0; i-- { // newest on top
			g.Rows = append(g.Rows, logsRowOf(evs[i], q, true))
		}
		rows, err := logsPart("lgrows", g)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "id: %s\n%s", next, patchElements("#lg-rows", "prepend", rows))
		return err
	})
}

// logsPart renders one of logs.html's parts.
func logsPart(name string, data any) (string, error) {
	var b bytes.Buffer
	err := logsTmpl.ExecuteTemplate(&b, name, data)
	return strings.TrimSpace(b.String()), err
}

func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// The detail pane.

type logsFact struct {
	Label, Value, Href string
}

type logsField struct {
	Key, Value string
	Sensitive  bool
}

type logsDetail struct {
	Tab       string
	Tabs      []logsLink
	Missing   string // why there is no event to show
	Row       logsRow
	Local     string
	UTC       string
	Facts     []logsFact
	Pivots    []logsFact
	Permalink string

	// Details.
	Fields []logsField
	Masked bool   // sensitive values are masked; Reveal shows them
	Reveal string // the reveal's URL
	JSON   string

	// Correlated.
	Of       string // what the timeline follows: trace <id> or activity <id>
	OfHref   string // it as a filter
	Timeline []logsRow
	More     int
	Corr     logapi.Timeline // its run record, or why there is none

	// Help.
	Spec     *logapi.Spec
	Seen     string
	Trend    template.HTML
	FindHref string
}

var logsTabs = []struct{ key, label string }{{"general", "General"}, {"details", "Details"}, {"correlated", "Correlated"}, {"help", "Help"}}

// logsDetailView is the detail pane of event seq on tab, under the grid's
// filter q. With reveal, sensitive values are shown, after filing the
// reveal's Security record (catalog.FieldRevealed, as the API files it).
func (s *Server) logsDetailView(ctx context.Context, h *logs.Hub, st *store.Store, seq uint64, tab, q string, reveal bool) *logsDetail {
	if !slices.ContainsFunc(logsTabs, func(t struct{ key, label string }) bool { return t.key == tab }) {
		tab = "general"
	}
	d := &logsDetail{Tab: tab}
	e, err := st.Get(ctx, seq)
	if errors.Is(err, store.ErrNotFound) {
		d.Missing = fmt.Sprintf("There is no event %d: it was cleared, or is past its log's retention.", seq)
		return d
	}
	if err != nil {
		d.Missing = s.flashFor(ctx, "logs event", err, "The event could not be read. The dashboard log has the reason.")
		return d
	}
	sq := strconv.FormatUint(seq, 10)
	for _, t := range logsTabs {
		v := url.Values{"tab": {t.key}}
		if q != "" {
			v.Set("q", q)
		}
		d.Tabs = append(d.Tabs, logsLink{Label: t.label, On: t.key == tab,
			Href: "/logs/event/" + sq + "?" + v.Encode(), Get: "/logs/detail/" + sq + "?" + v.Encode()})
	}
	d.Row = logsRowOf(e, q, false)
	d.Permalink = "/logs/event/" + sq
	d.Local = e.Time.In(time.Local).Format("2006-01-02 15:04:05.000 MST")
	d.UTC = e.Time.UTC().Format("2006-01-02 15:04:05.000")
	var keys []string
	for _, f := range e.Data {
		if f.Sensitive {
			keys = append(keys, f.Key)
		}
	}
	d.Masked = len(keys) > 0 && !reveal
	if !reveal {
		logapi.Mask([]event.Event{e}) // e's Data is its own copy
	} else if len(keys) > 0 {
		s.logbook.Record(ctx, h, catalog.FieldRevealed, e.Log, fmt.Sprintf("event %d, fields %s", seq, strings.Join(keys, ", ")))
	}

	switch tab {
	case "general":
		d.Facts, d.Pivots = eventFacts(e)
	case "details":
		for _, f := range e.Data {
			d.Fields = append(d.Fields, logsField{Key: f.Key, Value: f.Text(), Sensitive: f.Sensitive})
		}
		d.Reveal = "/logs/detail/" + sq + "/reveal"
		if b, err := json.MarshalIndent(e, "", "  "); err == nil {
			d.JSON = string(b)
		}
	case "correlated":
		s.correlated(ctx, st, e, q, d)
	case "help":
		dt, err := logapi.Detail(ctx, st, e)
		if err != nil {
			d.Seen = s.flashFor(ctx, "logs statistics", err, "Its statistics could not be read. The dashboard log has the reason.")
		}
		d.Spec = dt.Spec
		if dt.Stats != nil && dt.Stats.Count > 0 {
			st := dt.Stats
			d.Seen = fmt.Sprintf("Seen %s in the last 24 hours, %s in all; first seen %s, last %s.",
				times(st.Trend[6]), times(st.Count), localTime(st.First), localTime(st.Last))
			d.Trend = trendBars(st.Trend)
		}
		if e.ID != 0 {
			d.FindHref = logsHref(fmt.Sprintf("provider:%s id:%d", e.Provider, e.ID))
		}
	}
	return d
}

func times(n int64) string {
	switch n {
	case 0:
		return "never"
	case 1:
		return "once"
	}
	return thousands(n) + " times"
}

func localTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.In(time.Local).Format("2006-01-02 15:04:05")
}

// eventFacts is the General tab's key facts, and the correlation IDs that
// pivot to every event sharing them.
func eventFacts(e event.Event) (facts, pivots []logsFact) {
	add := func(label, value, href string) {
		if value != "" {
			facts = append(facts, logsFact{Label: label, Value: value, Href: href})
		}
	}
	add("Level", e.Level.String(), logsHref("level:"+e.Level.String()+" "+filterTerm("log", e.Log)))
	add("Log", e.Log, logsHref(filterTerm("log", e.Log)))
	if e.ID != 0 {
		add("Event", fmt.Sprintf("%s %d (version %d)", e.Provider, e.ID, e.Version), logsHref(fmt.Sprintf("provider:%s id:%d", e.Provider, e.ID)))
	} else {
		add("Provider", e.Provider, logsHref(filterTerm("provider", e.Provider)))
	}
	add("Task", e.Task, "")
	add("Opcode", e.Opcode, "")
	for _, k := range e.Keywords {
		add("Keyword", k, logsHref(filterTerm("tag", k)))
	}
	if e.Channel == event.Diagnostic {
		add("Channel", "diagnostic", "")
	}
	add("Actor", e.Actor, logsHref(filterTerm("actor", e.Actor)))
	add("Surface", e.Surface, logsHref(filterTerm("surface", e.Surface)))
	if p := e.Proc; p.PID != 0 || p.Role != "" {
		proc := strings.TrimSpace(p.Role + " pid " + strconv.Itoa(p.PID))
		if p.Host != "" {
			proc += " on " + p.Host
		}
		if p.Version != "" {
			proc += ", version " + p.Version
		}
		add("Process", proc, logsHref(fmt.Sprintf("pid:%d", p.PID)))
	}
	add("Boot", e.Proc.BootID, logsHref(filterTerm("boot", e.Proc.BootID)))
	c := e.Corr
	for _, p := range []struct{ label, field, id string }{{"Trace (turn)", "trace", c.TraceID}, {"Activity", "activity", c.ActivityID},
		{"Related to", "related", c.RelatedID}, {"Session", "session", c.SessionID}, {"Approval", "approval", c.ApprovalID}, {"Tool call", "tool_call", c.ToolCallID}} {
		if p.id != "" {
			pivots = append(pivots, logsFact{Label: p.label, Value: p.id, Href: logsHref(filterTerm(p.field, p.id))})
		}
	}
	return facts, pivots
}

// correlated fills in the Correlated tab: the timeline (§7) of the event's
// turn, or else its activity, and the turn's run record from runs.db.
func (s *Server) correlated(ctx context.Context, st *store.Store, e event.Event, q string, d *logsDetail) {
	id, field := e.Corr.TraceID, "trace"
	if id == "" {
		id, field = e.Corr.ActivityID, "activity"
	}
	if id == "" {
		id, field = e.Corr.RelatedID, "related"
	}
	if id == "" {
		return
	}
	d.Of, d.OfHref = field+" "+id, logsHref(filterTerm(field, id))
	evs, err := st.Timeline(ctx, id, store.MaxTimelineDepth)
	if err != nil {
		d.Corr.RunNote = s.flashFor(ctx, "logs timeline", err, "The timeline could not be read. The dashboard log has the reason.")
		return
	}
	if len(evs) > logsTimelineMax {
		d.More, evs = len(evs)-logsTimelineMax, evs[:logsTimelineMax]
	}
	for _, x := range evs {
		d.Timeline = append(d.Timeline, logsRowOf(x, q, true))
	}
	if e.Corr.TraceID == "" {
		d.Corr.RunNote = "This event is not part of a turn, so it has no run record."
		return
	}
	d.Corr.Run, d.Corr.RunNote = s.logbook.Run(ctx, e.Corr.TraceID)
}

// trendBars draws seven days of counts as bars, oldest first.
func trendBars(days [7]int64) template.HTML {
	var top int64 = 1
	for _, n := range days {
		top = max(top, n)
	}
	var b strings.Builder
	b.WriteString(`<svg class="lg-trend" viewBox="0 0 70 20" width="70" height="20" role="img" aria-label="per day, the last 7 days">`)
	for i, n := range days {
		h := float64(n) / float64(top) * 18
		fmt.Fprintf(&b, `<rect x="%d" y="%.1f" width="8" height="%.1f"><title>%d</title></rect>`, i*10, 19-h, h+1, n)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) // numbers only
}

// handleLogsDetail is the detail pane as a patch, for a click on a row or a
// tab; POSTed to …/reveal, its Details tab with the sensitive values shown.
func (s *Server) handleLogsDetail(w http.ResponseWriter, r *http.Request) {
	h, st := s.logbookStore()
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if st == nil || err != nil {
		http.Error(w, "no such event", http.StatusNotFound)
		return
	}
	reveal, tab := r.Method == http.MethodPost, r.URL.Query().Get("tab")
	if reveal {
		tab = "details"
	}
	d := s.logsDetailView(r.Context(), h, st, seq, tab, r.URL.Query().Get("q"), reveal)
	if reveal && d.Missing != "" {
		http.Error(w, "no such event", http.StatusNotFound)
		return
	}
	part, err := logsPart("lgdetail", d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = writeDatastarPatch(w, part)
}

// The actions.

type logsOption struct {
	Value, Label string
	On           bool
}

type logsProps struct {
	Log                 string
	Events, Size, Limit string
	MaxMB, MaxDays      string
	OnFull, MinLevel    []logsOption
	Enabled             bool
	Debug               string // the debug switch is on until then
	Security            bool
	Action, ClearAction string
	LastWrite, Cleared  string
}

func propsView(l logapi.Log) *logsProps {
	p := &logsProps{Log: l.Name, Events: thousands(l.Events), Size: humanBytes(l.Bytes), Enabled: l.Policy.Enabled,
		MaxMB:    strconv.FormatFloat(float64(l.Policy.MaxBytes)/(1<<20), 'f', -1, 64),
		MaxDays:  strconv.FormatFloat(float64(l.Policy.MaxAgeS)/86400, 'f', -1, 64),
		Security: l.Name == store.SecurityLog,
		Action:   "/logs/log/" + url.PathEscape(l.Name) + "/properties", ClearAction: "/logs/log/" + url.PathEscape(l.Name) + "/clear"}
	if l.Policy.MaxBytes > 0 {
		p.Limit = humanBytes(l.Policy.MaxBytes)
	}
	for _, o := range []string{store.Overwrite, store.Archive, store.Keep} {
		p.OnFull = append(p.OnFull, logsOption{Value: o, Label: o, On: o == l.Policy.OnFull})
	}
	for _, o := range []string{"", "critical", "error", "warning", "info", "verbose"} {
		label := o
		if o == "" {
			label = "every level"
		}
		p.MinLevel = append(p.MinLevel, logsOption{Value: o, Label: label, On: o == l.Policy.MinLevel})
	}
	if l.DebugUntil != nil {
		p.Debug = l.DebugUntil.In(time.Local).Format("15:04")
	}
	if l.LastWrite != nil {
		p.LastWrite = relAge(*l.LastWrite)
	}
	if l.Cleared != nil {
		p.Cleared = relAge(*l.Cleared)
	}
	return p
}

// logsBack redirects to the grid of q with a message, as the forms do. The
// log book's own refusals ("the Security log is never cleared", a bad
// max_size) answer the operator and show as they are; any other error shows
// a fixed message and goes to the log (D21).
func (s *Server) logsBack(w http.ResponseWriter, r *http.Request, v url.Values, msg string, err error) {
	var refusal *logapi.Error
	if errors.As(err, &refusal) {
		v.Set("err", refusal.Msg)
	} else if err != nil {
		v.Set("err", s.flashFor(r.Context(), "logs change", err, "Couldn't make that change. The dashboard log has the reason."))
	} else if msg != "" {
		v.Set("msg", msg)
	}
	http.Redirect(w, r, "/logs?"+v.Encode(), http.StatusSeeOther)
}

// handleLogsProperties changes a log's policy or its debug switch through
// what PATCH /api/logbook/logs/{log} calls, which files the Security record.
func (s *Server) handleLogsProperties(w http.ResponseWriter, r *http.Request) {
	h, st := s.logbookStore()
	log := r.PathValue("log")
	back := url.Values{"q": {"log:" + log}}
	if st == nil {
		s.logsBack(w, r, back, "", PlainError("The log book isn't open here."))
		return
	}
	var c logapi.PolicyChange
	switch r.FormValue("action") {
	case "reset":
		c.Reset = true
	case "debug":
		c.DebugFor = new("30m")
	case "nodebug":
		c.DebugFor = new("0")
	default:
		mb, err1 := strconv.ParseFloat(strings.TrimSpace(r.FormValue("max_mb")), 64)
		days, err2 := strconv.ParseFloat(strings.TrimSpace(r.FormValue("max_days")), 64)
		if err := errors.Join(err1, err2); err != nil || mb < 0 || days < 0 || mb > 1<<30 || days > 1e5 {
			s.logsBack(w, r, back, "", PlainError("Max size and max age are numbers; 0 means no limit."))
			return
		}
		c.MaxBytes, c.MaxAgeS = new(int64(mb*(1<<20))), new(int64(days*86400))
		c.OnFull, c.MinLevel = new(r.FormValue("on_full")), new(r.FormValue("min_level"))
		c.Enabled = new(r.FormValue("enabled") != "")
	}
	name, err := s.logbook.Change(r.Context(), h, st, log, c)
	back.Set("q", "log:"+name)
	s.logsBack(w, r, back, "Log "+name+" changed.", err)
}

// handleLogsClear clears a log through what the API's clear calls; the page
// offers an export first. The Security log is never cleared.
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	h, st := s.logbookStore()
	log := r.PathValue("log")
	back := url.Values{"q": {"log:" + log}}
	if st == nil {
		s.logsBack(w, r, back, "", PlainError("The log book isn't open here."))
		return
	}
	n, err := s.logbook.Clear(r.Context(), h, st, log)
	s.logsBack(w, r, back, fmt.Sprintf("Cleared %s events from %s.", thousands(n), log), err)
}

func (s *Server) handleLogsSaveView(w http.ResponseWriter, r *http.Request) {
	_, st := s.logbookStore()
	filter := r.FormValue("filter")
	if st == nil {
		s.logsBack(w, r, url.Values{"q": {filter}}, "", PlainError("The log book isn't open here."))
		return
	}
	v, err := s.logbook.SaveView(r.Context(), st, logapi.View{Name: r.FormValue("name"), Filter: filter})
	if err != nil {
		s.logsBack(w, r, url.Values{"q": {filter}}, "", err)
		return
	}
	s.logsBack(w, r, url.Values{"view": {v.Name}}, "Saved the view "+v.Name+".", nil)
}

func (s *Server) handleLogsDeleteView(w http.ResponseWriter, r *http.Request) {
	_, st := s.logbookStore()
	name := r.FormValue("name")
	if st == nil {
		s.logsBack(w, r, url.Values{"view": {name}}, "", PlainError("The log book isn't open here."))
		return
	}
	if err := s.logbook.DeleteView(r.Context(), st, name); err != nil {
		s.logsBack(w, r, url.Values{"view": {name}}, "", err)
		return
	}
	http.Redirect(w, r, "/logs?"+url.Values{"msg": {"Deleted the view " + name + "."}}.Encode(), http.StatusSeeOther)
}

// The Summary page.

type logsLevels struct{ Bad, Warn, All int64 }

func levelsOf(l store.Levels) logsLevels {
	return logsLevels{Bad: l.Critical + l.Error, Warn: l.Warning, All: l.Critical + l.Error + l.Warning + l.Info + l.Verbose}
}

type logsSumRow struct {
	Name, Href     string
	H, D, W        logsLevels
	Spark          template.HTML
	Size, Limit    string
	Pct            int
	Keep           string
	Last, LastFull string
	Lost           bool
	Dropped, Supp  int64
}

type logsCard struct {
	Label, Value, Href string
	Bad                bool
}

type logsTop struct {
	Provider, ID, N, Template, Href string
}

type logsSummary struct {
	Cards                   []logsCard
	Logs                    []logsSumRow
	Top                     []logsTop
	Critical, Errors, Exits []logsRow
}

// summaryView is the Summary page: what has gone wrong recently, from the
// rollups (Hub.Summary).
func summaryView(sum store.Summary) *logsSummary {
	out := &logsSummary{}
	var day store.Levels
	var lost int64
	for _, l := range sum.Logs {
		lost += l.Dropped + l.Suppressed
		day.Critical += l.Day.Critical
		day.Error += l.Day.Error
		day.Warning += l.Day.Warning
		r := logsSumRow{Name: l.Log, Href: logsHref("log:" + l.Log), H: levelsOf(l.Hour), D: levelsOf(l.Day), W: levelsOf(l.Week),
			Spark: sparkline(l.Hourly, l.Day.Critical+l.Day.Error > 0), Size: humanBytes(l.Bytes),
			Dropped: l.Dropped, Supp: l.Suppressed, Lost: l.Dropped+l.Suppressed > 0, Keep: "forever"}
		if l.MaxBytes > 0 {
			r.Limit, r.Pct = humanBytes(l.MaxBytes), int(min(100, l.Bytes*100/l.MaxBytes))
		}
		if l.MaxAge > 0 {
			r.Keep = shortDur(l.MaxAge)
		}
		if l.OnFull != "" {
			r.Keep += " · " + l.OnFull + " when full"
		}
		r.Last = "never"
		if !l.Last.IsZero() {
			r.Last, r.LastFull = relAge(l.Last), l.Last.In(time.Local).Format("2006-01-02 15:04:05")
		}
		out.Logs = append(out.Logs, r)
	}
	out.Cards = []logsCard{
		{Label: "Errors · 24 h", Value: thousands(day.Critical + day.Error), Href: logsHref("level>=error since:24h"), Bad: day.Critical+day.Error > 0},
		{Label: "Warnings · 24 h", Value: thousands(day.Warning), Href: logsHref("level:warning since:24h")},
		{Label: "Unexpected exits · 7 d", Value: strconv.Itoa(len(sum.Exits)), Href: logsHref("log:system level:critical since:7d"), Bad: len(sum.Exits) > 0},
		{Label: "Dropped or suppressed", Value: thousands(lost), Href: logsHref("log:logbook"), Bad: lost > 0},
	}
	for _, c := range sum.Top {
		t := logsTop{Provider: c.Provider, ID: strconv.FormatUint(uint64(c.ID), 10), N: thousands(c.N),
			Href: logsHref(fmt.Sprintf("provider:%s id:%d since:24h", c.Provider, c.ID))}
		if d, ok := event.Lookup(c.Provider, c.ID); ok {
			t.Template = d.Spec().Template
		}
		out.Top = append(out.Top, t)
	}
	for _, e := range sum.Critical {
		out.Critical = append(out.Critical, logsRowOf(e, "", false))
	}
	for _, e := range sum.Errors {
		out.Errors = append(out.Errors, logsRowOf(e, "", false))
	}
	for _, e := range sum.Exits {
		out.Exits = append(out.Exits, logsRowOf(e, "", false))
	}
	return out
}

// sparkline draws a log's hourly counts, oldest first, as a line.
func sparkline(hourly []int64, bad bool) template.HTML {
	if len(hourly) == 0 {
		return ""
	}
	var top int64 = 1
	for _, n := range hourly {
		top = max(top, n)
	}
	var pts strings.Builder
	for i, n := range hourly {
		fmt.Fprintf(&pts, "%d,%.1f ", i, 21-float64(n)/float64(top)*20)
	}
	class := "lg-spark"
	if bad {
		class += " bad"
	}
	return template.HTML(fmt.Sprintf(`<svg class="%s" viewBox="0 0 %d 22" preserveAspectRatio="none" role="img" aria-label="events per hour, the last 7 days; at most %d"><polyline points="%s"/></svg>`,
		class, len(hourly)-1, top, strings.TrimSpace(pts.String()))) // numbers only
}

func shortDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		days := d / (24 * time.Hour)
		if h := (d % (24 * time.Hour)) / time.Hour; h > 0 {
			return fmt.Sprintf("%dd %dh", days, h)
		}
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", d/time.Hour, (d%time.Hour)/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", d/time.Minute, (d%time.Minute)/time.Second)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// The Components view.

type logsComp struct {
	Name, Kind, State, Class string
	Uptime, Since, SinceFull string
	Error                    string
	Log, LogHref, History    string
}

type logsComps struct {
	Err    string
	Counts string
	Rows   []logsComp
}

// The components' states (lifecycle.State), in the order the view counts them.
var (
	states     = []string{"running", "starting", "degraded", "failed", "stopped"}
	stateClass = map[string]string{"running": "lg-ok", "starting": "tag", "degraded": "risk", "failed": "act-errtag", "stopped": "dim"}
)

// componentsView is what is running (§8.7): the Components hook's list, as
// GET /api/logbook/components serves it.
func (s *Server) componentsView(ctx context.Context) *logsComps {
	out := &logsComps{}
	cs, err := s.logbook.ListComponents(ctx)
	if err != nil {
		out.Err = s.flashFor(ctx, "logs components", err, "What is running could not be read. The dashboard log has the reason.")
	}
	counts := map[string]int{}
	for _, c := range cs {
		st := string(c.State)
		counts[st]++
		r := logsComp{Name: c.Name, Kind: string(c.Kind), State: st, Class: stateClass[st], Error: c.Error, Log: c.Log}
		if !c.Started.IsZero() {
			r.Uptime = shortDur(time.Since(c.Started))
		}
		if !c.Since.IsZero() {
			r.Since, r.SinceFull = relAge(c.Since), c.Since.In(time.Local).Format("2006-01-02 15:04:05")
		}
		if c.Log != "" {
			q := filterTerm("log", c.Log)
			if st == "failed" || st == "degraded" {
				q += " level>=warning"
			}
			r.LogHref = logsHref(q)
		}
		if c.Activity != "" {
			r.History = logsHref(filterTerm("activity", c.Activity))
		}
		out.Rows = append(out.Rows, r)
	}
	var parts []string
	for _, st := range states {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	out.Counts = strings.Join(parts, " · ")
	return out
}
