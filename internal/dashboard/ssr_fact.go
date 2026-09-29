package dashboard

// ssr_fact.go is the provenance inspector's page and form actions (PRD-001 /
// TEN-297): GET /memory/facts/{id} explains one fact, and the POST actions
// edit it and restore an earlier version, each going to the new version's
// page, and mark it no longer true (or true again), answered in place. Inline
// state follows the rest of the memory pages: ?turns=1 rebuilds the source
// conversations from the archive, ?compare=a,b shows a word diff of two
// versions, ?edit=1 opens the editor. Operator language: "no longer true",
// "earlier versions", "replaced".

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// factPageData is the view model for memory_fact.html.
type factPageData struct {
	layoutData
	Configured bool
	Supported  bool
	Msg, Err   string
	// Self is this view (the fact, and its open conversations or
	// comparison) that an in-place form sends as back.
	Self string

	ID         int64
	Text       string
	Status     string
	StatusText string
	Conf       string
	EffConf    string
	Learned    string
	Checked    string
	ValidTo    string // "" = still true
	Origin     string
	Signals    []factKV
	Sources    []factSourceVM
	Removed    []string
	Versions   []factVersionVM
	Replaced   []factVersionVM
	ByNewer    *factVersionVM
	SME        []string
	Uses       string

	Editing      bool
	EditHref     string
	CancelHref   string
	EditAction   string
	TurnsHref    string
	ConflictHref string // starts the facts list's conflict picker with this fact
	ShowTurns    bool
	Turns        []factTurnVM
	Compare      *factCompareVM
}

type factKV struct{ K, V string }

type factSourceVM struct {
	ID       int64
	When     string
	Prompt   string
	Response string
	Missing  bool
}

type factVersionVM struct {
	ID          int64
	Text        string
	When        string
	Status      string
	StatusText  string
	Job         string
	Href        string
	Current     bool
	CompareHref string
	RestoreForm string
}

type factTurnVM struct {
	Episode int64
	Missing bool
	Why     string
	Events  []provenanceRowVM
}

type factCompareVM struct {
	A, B  int64
	Parts []diffPart
}

// handleMemoryFactPage renders one fact through the inspector.
func (s *Server) handleMemoryFactPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, s.tmpl.memFact, s.memFactView(r.Context(), r.PathValue("id"), r.URL.Query()))
}

func (s *Server) memFactView(ctx context.Context, rawID string, q url.Values) factPageData {
	d := factPageData{layoutData: layoutData{Title: "Memory · Fact", Page: "memory", Sub: "facts"}}
	if s.mem == nil {
		return d
	}
	d.Configured = true
	ins, ok := s.mem.(FactInspector)
	if !ok {
		return d
	}
	d.Supported = true
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		d.Err = "That isn't a fact id."
		return d
	}
	d.Msg, d.Err = q.Get("msg"), q.Get("err")
	det, err := ins.FactDetail(id)
	if err != nil {
		d.Err = s.flashFor(ctx, "fact read", err, "Couldn't load that fact. It may have been removed; the Logs page has the reason.")
		return d
	}
	base := memFactsURL + "/" + i64(id)
	f := det.Fact
	d.ID, d.Text, d.Status, d.StatusText = f.ID, f.Text, f.Status, statusWord(f.Status)
	d.Conf = fmtConf(f.Confidence)
	d.EffConf = strconv.FormatFloat(f.EffectiveConfidence, 'f', 2, 64)
	d.Learned = fmtUnix(f.FirstSeen)
	d.Checked = fmtUnix(f.LastConfirmed)
	if det.Signals.ValidTo != nil {
		d.ValidTo = fmtWhen(*det.Signals.ValidTo)
	}
	switch {
	case det.Origin == nil:
		d.Origin = "not recorded (learned before Tenant kept this)"
	case det.Origin.Model != "":
		d.Origin = jobWord(det.Origin.Job) + " · " + det.Origin.Model + " · " + fmtWhen(det.Origin.RecordedAt)
	default:
		d.Origin = jobWord(det.Origin.Job) + " · " + fmtWhen(det.Origin.RecordedAt)
	}
	sig := det.Signals
	d.Signals = []factKV{
		{"Importance", strconv.FormatFloat(sig.Importance, 'f', 2, 64)},
		{"Pinned", yesNo(sig.Pinned)},
		{"Protected", yesNo(sig.Protected)},
	}
	if sig.ValidFrom != nil {
		d.Signals = append(d.Signals, factKV{"True since", fmtWhen(*sig.ValidFrom)})
	}
	for _, src := range det.Sources {
		vm := factSourceVM{ID: src.ID, Missing: src.Missing, Prompt: clipProv(src.Prompt), Response: clipProv(src.Response)}
		if !src.Timestamp.IsZero() {
			vm.When = fmtWhen(src.Timestamp)
		}
		d.Sources = append(d.Sources, vm)
	}
	for _, rs := range det.RemovedSources {
		d.Removed = append(d.Removed, removedSourceText(rs))
	}
	for _, v := range det.Versions {
		vm := versionVM(v, id)
		if v.ID != id {
			vm.CompareHref = urlWith(base, "compare", i64(min(v.ID, id))+","+i64(max(v.ID, id)))
			if f.Status == FactStatusLive || vm.Status == FactStatusSuperseded {
				vm.RestoreForm = base + "/restore-version"
			}
		}
		d.Versions = append(d.Versions, vm)
	}
	for _, v := range det.Replaced {
		d.Replaced = append(d.Replaced, versionVM(v, id))
	}
	if det.ReplacedBy != nil {
		vm := versionVM(*det.ReplacedBy, id)
		d.ByNewer = &vm
	}
	d.SME = det.Downstream.SMESections
	d.Uses = strconv.FormatInt(det.Downstream.Retrievals, 10) + " retrieval(s)"
	if sig.LastAccessed != nil {
		d.Uses += ", last " + fmtWhen(*sig.LastAccessed)
	}

	d.Editing = q.Get("edit") == "1" && f.Status == FactStatusLive
	d.EditHref = urlWith(base, "edit", "1")
	d.CancelHref = base
	d.EditAction = base + "/edit"
	d.TurnsHref = urlWith(base, "turns", "1")
	d.ConflictHref = urlWith(memFactsURL, "resolve", i64(id))
	d.Self = urlWith(base, "turns", q.Get("turns"), "compare", q.Get("compare"))
	if q.Get("turns") == "1" {
		d.ShowTurns = true
		turns, err := ins.FactTurns(id)
		if err != nil {
			d.Err = s.flashFor(ctx, "fact conversations read", err, "Couldn't read those conversations back from the archive.")
		}
		for _, t := range turns {
			vm := factTurnVM{Episode: t.EpisodeID, Missing: t.Missing, Why: t.Why}
			for _, ev := range t.Events {
				vm.Events = append(vm.Events, provenanceRowVM{When: ev.When.Local().Format("2006-01-02 15:04:05"), Role: ev.Role, Content: clipProv(ev.Content)})
			}
			d.Turns = append(d.Turns, vm)
		}
	}
	if a, b, ok := parseCompare(q.Get("compare")); ok {
		ta, tb := versionText(det.Versions, a), versionText(det.Versions, b)
		if ta != "" && tb != "" {
			d.Compare = &factCompareVM{A: a, B: b, Parts: wordDiff(ta, tb)}
		}
	}
	return d
}

func versionVM(v VersionView, current int64) factVersionVM {
	return factVersionVM{
		ID: v.ID, Text: v.Text, When: fmtWhen(v.FirstSeen),
		Status: v.Status, StatusText: statusWord(v.Status), Job: jobWord(v.Job),
		Href: memFactsURL + "/" + i64(v.ID), Current: v.ID == current,
	}
}

// statusWord is a fact status in operator language (TEN-90).
func statusWord(status string) string {
	switch status {
	case FactStatusLive:
		return "current"
	case FactStatusSuperseded:
		return "replaced"
	case FactStatusTombstoned:
		return "removed"
	}
	return status
}

// jobWords name what made a fact, in operator language.
var jobWords = map[string]string{
	"distill":     "learned from a conversation",
	"consolidate": "merged during memory upkeep",
	"edit":        "your edit",
	"restore":     "your restore of an earlier version",
	"import":      "imported",
	"remember":    "saved by the agent",
	"mcp":         "saved by an MCP client",
	"eval":        "an eval run",
}

func jobWord(job string) string {
	if w, ok := jobWords[job]; ok {
		return w
	}
	return job
}

// removedSourceText names a source a forget or retention deleted, in operator
// language.
func removedSourceText(rs RemovedSourceView) string {
	what := rs.Kind + " " + rs.ID
	switch rs.Kind {
	case "episode":
		what = "conversation #" + rs.ID
	case "run":
		what = "run record " + rs.ID
	}
	by := "by " + rs.Reason
	if rs.Reason == "forget" {
		by = "by a forget"
	}
	return what + ", " + by + " on " + rs.RemovedAt.Local().Format("2006-01-02") + "."
}

func versionText(vs []VersionView, id int64) string {
	for _, v := range vs {
		if v.ID == id {
			return v.Text
		}
	}
	return ""
}

func parseCompare(s string) (int64, int64, bool) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, false
	}
	x, err1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	y, err2 := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	return x, y, err1 == nil && err2 == nil && x != y
}

func fmtUnix(sec int64) string {
	if sec == 0 {
		return "—"
	}
	return fmtWhen(time.Unix(sec, 0))
}

// fmtWhen shows a time on the fact page, always in local time: the stores
// hand back some times in UTC and some in local.
func fmtWhen(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// --- form actions -----------------------------------------------------------

// inspector is the fact a form acts on and the control to act with. With
// none, a plain post goes to the facts list and an in-place one re-renders
// the fact's page, which says why.
func (s *Server) inspector(w http.ResponseWriter, r *http.Request) (FactInspector, int64, bool) {
	id, ok := memFormID(w, r)
	if !ok {
		return nil, 0, false
	}
	ins, ok := s.mem.(FactInspector)
	if !ok {
		s.formDone(w, r, memFactsURL, "", "", s.memRegion(r.Context(), memReturn(r, memFactsURL+"/"+i64(id))))
		return nil, 0, false
	}
	return ins, id, true
}

// handleMemoryFactEditForm saves a new version: POST /memory/facts/{id}/edit.
func (s *Server) handleMemoryFactEditForm(w http.ResponseWriter, r *http.Request) {
	ins, id, ok := s.inspector(w, r)
	if !ok {
		return
	}
	base := memFactsURL + "/" + i64(id)
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Redirect(w, r, urlWith(base, "edit", "1", "err", "A fact can't be empty."), http.StatusSeeOther)
		return
	}
	newID, err := ins.EditFact(id, text)
	if err != nil {
		http.Redirect(w, r, urlWith(base, "edit", "1", "err", s.flashFor(r.Context(), "fact edit", err, "Couldn't save that.")), http.StatusSeeOther)
		return
	}
	s.log.Info("dashboard: fact edited", "id", id, "new", newID)
	redirectFlash(w, r, memFactsURL+"/"+i64(newID), "Saved as a new version; the earlier one is kept below.", "")
}

// handleMemoryFactValidForm marks a fact no longer true (valid=false) or true
// again (valid=true): POST /memory/facts/valid {fact, valid, back}, answered in
// place on the fact's page (back, the view it was on). One route for both,
// so after the morph the button that takes the other's place is the same
// form, and its throttle drops a double tap's second post, which would undo
// the first.
func (s *Server) handleMemoryFactValidForm(w http.ResponseWriter, r *http.Request) {
	valid := r.FormValue("valid")
	if valid != "true" && valid != "false" {
		writeError(w, http.StatusBadRequest, "valid must be true or false")
		return
	}
	ins, id, ok := s.inspector(w, r)
	if !ok {
		return
	}
	done, failed, msg, fn := "invalidated", "Couldn't mark that fact no longer true.", "Marked no longer true.", ins.InvalidateFact
	if valid == "true" {
		done, failed, msg, fn = "revalidated", "Couldn't mark that fact true again.", "Marked true again.", ins.RevalidateFact
	}
	back := memReturn(r, memFactsURL+"/"+i64(id))
	region := s.memRegion(r.Context(), back)
	if err := fn(id); err != nil {
		s.formDone(w, r, back, "", s.flashFor(r.Context(), "fact "+done, err, failed), region)
		return
	}
	s.log.Info("dashboard: fact "+done, "id", id)
	s.formDone(w, r, back, msg, "", region)
}

// handleMemoryFactRestoreVersionForm makes an earlier version current again:
// POST /memory/facts/{id}/restore-version with version=<id>.
func (s *Server) handleMemoryFactRestoreVersionForm(w http.ResponseWriter, r *http.Request) {
	ins, id, ok := s.inspector(w, r)
	if !ok {
		return
	}
	base := memFactsURL + "/" + i64(id)
	version, err := strconv.ParseInt(r.FormValue("version"), 10, 64)
	if err != nil {
		http.Redirect(w, r, urlWith(base, "err", "Pick a version to restore."), http.StatusSeeOther)
		return
	}
	newID, err := ins.RestoreFactVersion(version)
	if err != nil {
		http.Redirect(w, r, urlWith(base, "err", s.flashFor(r.Context(), "fact restore version", err, "Couldn't restore that version.")), http.StatusSeeOther)
		return
	}
	s.log.Info("dashboard: fact version restored", "version", version, "new", newID)
	redirectFlash(w, r, memFactsURL+"/"+i64(newID), "Restored as a new version.", "")
}

// --- JSON ---------------------------------------------------------------------

type factEditRequest struct {
	Text string `json:"text"`
}

type factRestoreVersionRequest struct {
	Version int64 `json:"version"`
}

type factIDResponse struct {
	ID int64 `json:"id"`
}

type factTurnsResponse struct {
	Turns []TurnView `json:"turns"`
}

// mountFactInspectorREST registers the inspector's JSON routes, which answer
// 501 when the memory control cannot explain facts.
func (s *Server) mountFactInspectorREST(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/memory/facts/{id}", s.restFact(func(ins FactInspector, w http.ResponseWriter, _ *http.Request, id int64) {
		det, err := ins.FactDetail(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, det)
	}))
	mux.HandleFunc("GET /api/memory/facts/{id}/turns", s.restFact(func(ins FactInspector, w http.ResponseWriter, _ *http.Request, id int64) {
		turns, err := ins.FactTurns(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, factTurnsResponse{Turns: turns})
	}))
	mux.HandleFunc("POST /api/memory/facts/{id}/edit", s.restFact(func(ins FactInspector, w http.ResponseWriter, r *http.Request, id int64) {
		var req factEditRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			writeError(w, http.StatusBadRequest, "text is required")
			return
		}
		newID, err := ins.EditFact(id, strings.TrimSpace(req.Text))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Info("memory: fact edited", "id", id, "new", newID)
		writeJSON(w, http.StatusOK, factIDResponse{ID: newID})
	}))
	mux.HandleFunc("POST /api/memory/facts/{id}/invalidate", s.restFact(func(ins FactInspector, w http.ResponseWriter, _ *http.Request, id int64) {
		if err := ins.InvalidateFact(id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("POST /api/memory/facts/{id}/revalidate", s.restFact(func(ins FactInspector, w http.ResponseWriter, _ *http.Request, id int64) {
		if err := ins.RevalidateFact(id); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("POST /api/memory/facts/{id}/restore-version", s.restFact(func(ins FactInspector, w http.ResponseWriter, r *http.Request, id int64) {
		var req factRestoreVersionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		version := req.Version
		if version == 0 {
			version = id
		}
		newID, err := ins.RestoreFactVersion(version)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Info("memory: fact version restored", "version", version, "new", newID)
		writeJSON(w, http.StatusOK, factIDResponse{ID: newID})
	}))
}

func (s *Server) restFact(h func(FactInspector, http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ins, ok := s.mem.(FactInspector)
		if !ok {
			writeError(w, http.StatusNotImplemented, "this server's memory cannot explain facts")
			return
		}
		id, ok := memPathID(w, r)
		if !ok {
			return
		}
		h(ins, w, r, id)
	}
}
