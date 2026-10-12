package dashboard

// ssr_memory.go is TEN-111: the server-rendered Memory Curator — the SSR
// counterpart to the memory_rest.go JSON surface, built on the SAME TEN-88
// MemoryControl interface. Five read views (overview / soul / facts / removed /
// temporal) render through html/template; every mutation goes through a form
// handler in ssr_memory_forms.go, which answers in place (DESIGN.md "Forms
// that don't reload") with the page's region built by the view functions
// here. No hand-written JS: the other inline state (expand provenance, the
// resolve picker) is threaded through query params so each view is a plain
// GET that httptest can assert on.
//
// Operator language only (per TEN-90): "conflicting facts", "keep this one",
// "recently removed" — never supersede/winner/loser jargon.

import (
	"context"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Memory page/route bases (root-mounted after the TEN-110 cutover). The page
// handlers and the form/303 redirects build every memory URL from these, so
// these constants and the routes in mountSSR are the single source of truth.
const (
	memBase     = "/memory"
	memSoulURL  = "/memory/soul"
	memFactsURL = "/memory/facts"

	memRemovedURL = memFactsURL + "/removed"
)

// --- shared helpers --------------------------------------------------------

// urlWith builds base?…&k=v from key/value pairs, skipping empty values and
// sorting keys (url.Values.Encode) so generated hrefs are deterministic in
// tests. With no non-empty params it returns base unadorned.
func urlWith(base string, kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// fmtConf renders a confidence as "conf 0.82" (or "conf —" for NaN). Zero is a
// real value and renders "conf 0.00".
func fmtConf(c float64) string {
	if math.IsNaN(c) {
		return "conf —"
	}
	return "conf " + strconv.FormatFloat(c, 'f', 2, 64)
}

// atoi64 parses an int64, returning 0 on any error (the sentinel "absent").
func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// findFact returns the fact with id from facts, or a zero-text placeholder
// carrying the id when it isn't on the current page.
func findFact(facts []FactView, id int64) FactView {
	for _, f := range facts {
		if f.ID == id {
			return f
		}
	}
	return FactView{ID: id}
}

// --- overview --------------------------------------------------------------

type memOverviewData struct {
	layoutData
	Configured   bool
	WorkingCount int
	Persona      string
	HasPersona   bool
	UserFacts    int
	Instructions int
	Stats        MemStats
	Err          string
}

// handleMemoryPage renders the Memory overview: working-set size, the soul
// persona snippet + list counts, and the live/superseded/removed fact tally.
func (s *Server) handleMemoryPage(w http.ResponseWriter, r *http.Request) {
	d := memOverviewData{layoutData: layoutData{Title: "Memory", Page: "memory", Sub: "overview"}}
	if s.mem == nil {
		s.render(w, s.tmpl.memory, d)
		return
	}
	d.Configured = true
	d.WorkingCount = s.mem.WorkingCount()
	if soul, err := s.mem.Soul(); err != nil {
		d.Err = s.flashFor(r.Context(), "memory personality read", err, memReadFailed)
	} else {
		d.Persona = strings.TrimSpace(soul.Persona)
		d.HasPersona = d.Persona != ""
		d.UserFacts = len(soul.UserFacts)
		d.Instructions = len(soul.Instructions)
	}
	if _, stats, err := s.mem.TemporalFacts(); err != nil {
		if d.Err == "" {
			d.Err = s.flashFor(r.Context(), "memory stats read", err, memReadFailed)
		}
	} else {
		d.Stats = stats
	}
	s.render(w, s.tmpl.memory, d)
}

// memReadFailed is what a memory page shows when its store can't be read
// (D21): the store's own error goes to the log.
const memReadFailed = "Couldn't read your agent's memory just now. Refresh to try again; the Logs page has the reason."

// --- soul ------------------------------------------------------------------

// soulSection is one list the operator edits: each item's Edit and Remove,
// and Add, open on the page and post in place (mem-soul-body).
type soulSection struct {
	Section        string // SoulSectionUserFact | SoulSectionInstruction
	Title          string
	Items          []SoulItem
	EmptyMsg       string
	AddPlaceholder string
}

type soulData struct {
	layoutData
	Configured   bool
	Persona      string
	HasPersona   bool
	UserFacts    soulSection
	Instructions soulSection
	Msg, Err     string
}

// handleMemorySoulPage renders the soul: read-only persona prose plus the
// editable user-facts and standing-instructions lists.
func (s *Server) handleMemorySoulPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.memSoul, s.memSoulView(r.Context(), r.URL.Query()))
}

func (s *Server) memSoulView(ctx context.Context, q url.Values) soulData {
	d := soulData{
		layoutData: layoutData{Title: "Memory · Personality", Page: "memory", Sub: "soul"},
		Msg:        q.Get("msg"),
		Err:        q.Get("err"),
	}
	if s.mem == nil {
		return d
	}
	d.Configured = true
	soul, err := s.mem.Soul()
	if err != nil {
		d.Err = s.flashFor(ctx, "memory personality read", err, memReadFailed)
		return d
	}
	d.Persona = strings.TrimSpace(soul.Persona)
	d.HasPersona = d.Persona != ""
	d.UserFacts = soulSection{SoulSectionUserFact, "Things the agent knows about you", soul.UserFacts, "No facts about you yet.", "e.g. Prefers metric units."}
	d.Instructions = soulSection{SoulSectionInstruction, "Standing instructions", soul.Instructions, "No standing instructions yet.", "e.g. Always cite sources."}
	return d
}

// --- facts -----------------------------------------------------------------

type episodeRow struct {
	When     string
	Prompt   string
	Response string
	Missing  bool
	ID       int64
}

type factRow struct {
	ID   int64
	Text string
	Conf string

	// provenance (inline)
	Expanded bool
	Episodes []episodeRow
	ProvErr  string
	ProvHref string // expand, or collapse back to the list
	// DetailHref opens the provenance inspector (TEN-297); empty when the
	// memory control cannot explain facts.
	DetailHref string

	// resolve-a-conflict picker
	Pinned     bool // this is the first-picked fact ("A")
	ShowSelect bool // offer "this conflicts with A"
	SelectHref string
}

type factsData struct {
	layoutData
	Configured bool
	Query      string
	ClearHref  string
	Facts      []factRow
	Empty      bool
	EmptyMsg   string
	MoreHref   string
	Msg, Err   string
	// Self is this view (search, page of results, the open sources) that an
	// in-place form sends as back, so its answer re-renders the same view.
	Self string

	// undo banner after a delete
	ShowUndo    bool
	UndoID      int64
	RemovedHref string // link to the Recently removed page

	// resolve flow
	Resolving         bool
	CancelResolveHref string
	ResolveHint       string
	ResolveCard       bool
	KeepA             factRow
	KeepB             factRow
	ResolveAction     string // POST target; keep/discard set via hidden inputs

	RemovedNavHref  string
	TemporalNavHref string
}

// handleMemoryFactsPage renders the searchable live-fact list with inline
// provenance, a Remove that answers in place with an undo, and the two-step
// "resolve a conflict" picker, whose state is in the query. limit is fixed at
// 25 with an opaque-cursor "Load more".
func (s *Server) handleMemoryFactsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.memFacts, s.memFactsView(r.Context(), r.URL.Query()))
}

func (s *Server) memFactsView(ctx context.Context, q url.Values) factsData {
	d := factsData{
		layoutData:      layoutData{Title: "Memory · Facts", Page: "memory", Sub: "facts"},
		ResolveAction:   memFactsURL + "/resolve",
		RemovedNavHref:  memRemovedURL,
		TemporalNavHref: memFactsURL + "/temporal",
	}
	if s.mem == nil {
		return d
	}
	d.Configured = true

	query := strings.TrimSpace(q.Get("q"))
	cursor := q.Get("cursor")
	expand := atoi64(q.Get("expand"))
	removed := atoi64(q.Get("removed"))
	resolveA := atoi64(q.Get("resolve"))
	resolveB := atoi64(q.Get("with"))

	d.Query = query
	d.Self = urlWith(memFactsURL, "q", query, "cursor", cursor, "expand", q.Get("expand"))
	if query != "" {
		d.ClearHref = memFactsURL
	}
	if removed > 0 {
		d.ShowUndo = true
		d.UndoID = removed
		d.RemovedHref = memRemovedURL
	}

	facts, next, err := s.mem.Facts(query, 25, cursor)
	if err != nil {
		d.Err = s.flashFor(ctx, "memory facts read", err, memReadFailed)
		return d
	}
	d.Msg, d.Err = q.Get("msg"), q.Get("err")

	d.Resolving = resolveA > 0
	if d.Resolving {
		d.CancelResolveHref = urlWith(memFactsURL, "q", query)
		d.ResolveHint = "Resolving a conflict — pick the fact that contradicts the one above, then choose which to keep."
	}
	// Both picked → show the keep-which card (explicit choice, never automatic).
	if resolveA > 0 && resolveB > 0 {
		a := findFact(facts, resolveA)
		b := findFact(facts, resolveB)
		d.ResolveCard = true
		d.KeepA = factRow{ID: a.ID, Text: a.Text, Conf: fmtConf(a.Confidence)}
		d.KeepB = factRow{ID: b.ID, Text: b.Text, Conf: fmtConf(b.Confidence)}
	}

	for _, f := range facts {
		row := factRow{ID: f.ID, Text: f.Text, Conf: fmtConf(f.Confidence)}

		if expand == f.ID {
			row.Expanded = true
			row.ProvHref = urlWith(memFactsURL, "q", query) // collapse
			if eps, perr := s.mem.FactProvenance(f.ID); perr != nil {
				row.ProvErr = s.flashFor(ctx, "memory fact sources read", perr, "Couldn't load where this came from. Refresh to try again.")
			} else {
				row.Episodes = episodeRows(eps)
			}
		} else {
			row.ProvHref = urlWith(memFactsURL, "q", query, "expand", strconv.FormatInt(f.ID, 10))
		}
		if _, ok := s.mem.(FactInspector); ok {
			row.DetailHref = memFactsURL + "/" + strconv.FormatInt(f.ID, 10)
		}

		if d.Resolving {
			if f.ID == resolveA {
				row.Pinned = true
			} else if !d.ResolveCard {
				row.ShowSelect = true
				row.SelectHref = urlWith(memFactsURL, "q", query,
					"resolve", strconv.FormatInt(resolveA, 10),
					"with", strconv.FormatInt(f.ID, 10))
			}
		} else {
			row.SelectHref = urlWith(memFactsURL, "q", query, "resolve", strconv.FormatInt(f.ID, 10))
		}
		d.Facts = append(d.Facts, row)
	}

	if len(facts) == 0 {
		d.Empty = true
		if query != "" {
			d.EmptyMsg = "No facts match \"" + query + "\". Clear the search to see all."
		} else {
			d.EmptyMsg = "No facts yet. The agent adds facts as it learns from conversations."
		}
	}
	if next != "" {
		d.MoreHref = urlWith(memFactsURL, "q", query, "cursor", next)
	}
	return d
}

// episodeRows projects provenance episodes for display; a forgotten source
// (Missing) renders an "unavailable" note rather than breaking the list.
func episodeRows(eps []EpisodeView) []episodeRow {
	out := make([]episodeRow, 0, len(eps))
	for _, e := range eps {
		row := episodeRow{ID: e.ID, Missing: e.Missing}
		if !e.Missing {
			if !e.Timestamp.IsZero() {
				row.When = e.Timestamp.Format("2006-01-02 15:04")
			}
			row.Prompt = snippetStr(e.Prompt, 240)
			row.Response = snippetStr(e.Response, 240)
		}
		out = append(out, row)
	}
	return out
}

// --- recently removed ------------------------------------------------------

type removedRow struct {
	ID       int64
	Text     string
	Conf     string
	Restored bool // the answer to its Restore (restoredRow)
}

type removedData struct {
	layoutData
	Configured bool
	Rows       []removedRow
	Empty      bool
	Msg, Err   string
	BackHref   string
}

// handleMemoryRemovedPage lists tombstoned facts so an operator can restore
// one. Restore is a POST (never a GET link) so a prefetch can't undelete.
func (s *Server) handleMemoryRemovedPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.memRemoved, s.memRemovedView(r.Context(), r.URL.Query()))
}

func (s *Server) memRemovedView(ctx context.Context, q url.Values) removedData {
	d := removedData{
		layoutData: layoutData{Title: "Memory · Recently removed", Page: "memory", Sub: "removed"},
		BackHref:   memFactsURL,
		Msg:        q.Get("msg"),
		Err:        q.Get("err"),
	}
	if s.mem == nil {
		return d
	}
	d.Configured = true
	facts, err := s.mem.RemovedFacts(50)
	if err != nil {
		d.Err = s.flashFor(ctx, "memory removed read", err, memReadFailed)
		return d
	}
	for _, f := range facts {
		d.Rows = append(d.Rows, removedRow{ID: f.ID, Text: f.Text, Conf: fmtConf(f.Confidence)})
	}
	d.Empty = len(d.Rows) == 0
	return d
}

// --- temporal overview -----------------------------------------------------

type temporalRowVM struct {
	Text          string
	Status        string // "Live" | "Replaced" | "Removed"
	StatusClass   string // ov class: live | sup | tomb
	Confidence    string
	Effective     string
	FirstSeen     string
	LastConfirmed string
}

type temporalData struct {
	layoutData
	Configured bool
	Stats      MemStats
	Rows       []temporalRowVM
	SVG        template.HTML
	Empty      bool
	Err        string
	BackHref   string // All shows replaced and removed facts too (?all=1); the default is the
	// live working set.
	All bool
}

// handleMemoryTemporalPage renders the read-only knowledge-time overview: a
// server-generated SVG timeline (lifelines from first-seen to last-confirmed,
// status-shaped end markers) plus a table. HONESTY: the store records only
// transaction time, so a lifeline ends at last-confirmed — it does NOT claim to
// know when a fact stopped being true. Confidence is the store's server-side
// effective (decayed) value as of the request.
func (s *Server) handleMemoryTemporalPage(w http.ResponseWriter, r *http.Request) {
	d := temporalData{
		layoutData: layoutData{Title: "Memory · Knowledge time", Page: "memory", Sub: "temporal"},
		BackHref:   memFactsURL,
	}
	if s.mem == nil {
		s.render(w, s.tmpl.memTemporal, d)
		return
	}
	d.Configured = true
	facts, stats, err := s.mem.TemporalFacts()
	if err != nil {
		d.Err = s.flashFor(r.Context(), "memory timeline read", err, memReadFailed)
		s.render(w, s.tmpl.memTemporal, d)
		return
	}
	d.Stats = stats
	// Live facts only, unless asked for the history (TEN-402 follow-up): the
	// working set is what the operator reads; replaced and removed facts are a
	// click away and never crowd the timeline or the table.
	d.All = r.URL.Query().Get("all") == "1"
	if !d.All {
		live := facts[:0:0]
		for _, f := range facts {
			if f.Status == FactStatusLive {
				live = append(live, f)
			}
		}
		facts = live
	}
	d.Empty = len(facts) == 0
	for _, f := range facts {
		label, cls := temporalStatusLabel(f.Status)
		d.Rows = append(d.Rows, temporalRowVM{
			Text:          f.Text,
			Status:        label,
			StatusClass:   cls,
			Confidence:    strconv.FormatFloat(f.Confidence, 'f', 2, 64),
			Effective:     strconv.FormatFloat(f.EffectiveConfidence, 'f', 2, 64),
			FirstSeen:     fmtUnixDate(f.FirstSeen),
			LastConfirmed: fmtUnixDate(f.LastConfirmed),
		})
	}
	d.SVG = renderTemporalSVG(facts)
	s.render(w, s.tmpl.memTemporal, d)
}

type provenanceData struct {
	layoutData
	Configured bool
	HasSummary bool
	Err        string
	Summary    string
	Origin     string
	MsgCount   int
	Range      string
	Rows       []provenanceRowVM
}

type provenanceRowVM struct {
	When    string
	Role    string
	Content string
}

// handleMemoryProvenancePage renders the audit view of the latest compaction
// summary (TEN-104): its source range + the original archived turns it replaced,
// rehydrated read-only from the archive. "Paged out, not lost."
func (s *Server) handleMemoryProvenancePage(w http.ResponseWriter, r *http.Request) {
	d := provenanceData{layoutData: layoutData{Title: "Memory · Summaries", Page: "memory", Sub: "provenance"}}
	if s.mem == nil {
		s.render(w, s.tmpl.memProvenance, d)
		return
	}
	d.Configured = true
	prov, err := s.mem.CompactionProvenance()
	if err != nil {
		d.Err = s.flashFor(r.Context(), "memory summary read", err, memReadFailed)
		s.render(w, s.tmpl.memProvenance, d)
		return
	}
	if prov == nil || !prov.HasSummary {
		s.render(w, s.tmpl.memProvenance, d) // HasSummary stays false → "nothing compacted yet"
		return
	}
	d.HasSummary = true
	d.Summary = prov.Summary
	d.Origin = prov.Origin
	if d.Origin == "" {
		d.Origin = "working"
	}
	d.MsgCount = prov.MsgCount
	d.Range = provRange(prov.After, prov.Before)
	for _, ev := range prov.Events {
		d.Rows = append(d.Rows, provenanceRowVM{
			When:    ev.When.Format("2006-01-02 15:04"),
			Role:    ev.Role,
			Content: clipProv(ev.Content),
		})
	}
	s.render(w, s.tmpl.memProvenance, d)
}

func provRange(after, before time.Time) string {
	if after.IsZero() && before.IsZero() {
		return "full session"
	}
	return after.Format("2006-01-02 15:04") + " – " + before.Format("2006-01-02 15:04")
}

func clipProv(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 500 {
		return string(r[:500]) + "…"
	}
	return s
}

// temporalStatusLabel maps a fact lifecycle status to operator-facing copy +
// the SVG/legend CSS class. "Removed" (not "tombstoned") per TEN-90, and
// "Replaced" (not "superseded") per TEN-211.
func temporalStatusLabel(status string) (label, class string) {
	switch status {
	case FactStatusTombstoned:
		return "Removed", "tomb"
	case FactStatusSuperseded:
		return "Replaced", "sup"
	default:
		return "Live", "live"
	}
}
