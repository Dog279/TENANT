package dashboard

// ssr_memory_forms.go is TEN-111: the mutation handlers behind the Memory
// curator pages. Each is a POST that drives the SAME MemoryControl methods the
// JSON REST layer uses (memory_rest.go), then answers with formDone (DESIGN.md
// "Forms that don't reload"): a Datastar post gets a toast and the page's
// region re-rendered in place (memRegion); a plain post still gets the 303
// (POST/redirect/GET) with its flash, so a browser refresh can't replay the
// mutation. Values come from r.FormValue. An in-place form posts to a literal
// URL (F1), so the fact it acts on is its fact field (memFormID), and the view
// it was on is its back field.

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// soulDone is the toast for each soul edit.
var soulDone = map[string]string{SoulActionAdd: "Added.", SoulActionEdit: "Saved.", SoulActionRemove: "Removed."}

// handleMemorySoulEditForm applies one add/edit/remove to a soul list:
// POST memSoulURL/edit {section,action,item,text} -> SoulEdit (item, not id:
// see memFormID). A bad op (unknown section/action, missing target) is an
// error toast (or ?err banner) rather than a dead 400.
func (s *Server) handleMemorySoulEditForm(w http.ResponseWriter, r *http.Request) {
	region := s.memRegion(r.Context(), memSoulURL)
	if s.mem == nil {
		s.formDone(w, r, memSoulURL, "", "", region)
		return
	}
	op := SoulEditOp{
		Section: r.FormValue("section"),
		Action:  r.FormValue("action"),
		ID:      r.FormValue("item"),
		Text:    strings.TrimSpace(r.FormValue("text")),
	}
	if err := s.mem.SoulEdit(op); err != nil {
		s.formDone(w, r, memSoulURL, "", s.flashFor(r.Context(), "personality "+op.Action, err, "Couldn't save that change."), region)
		return
	}
	s.log.Info("dashboard: ssr soul edited", "section", op.Section, "action", op.Action, "id", op.ID)
	s.formDone(w, r, memSoulURL, soulDone[op.Action], "", region)
}

// handleMemoryFactDeleteForm tombstones a fact (recoverable):
// POST memFactsURL/delete {fact, back} -> DeleteFact. back is the view the
// Remove was on (the list, with its search, or the fact's page); it comes
// back with ?removed=, which on the list is the Undo banner.
func (s *Server) handleMemoryFactDeleteForm(w http.ResponseWriter, r *http.Request) {
	id, ok := memFormID(w, r)
	if !ok {
		return
	}
	back := memReturn(r, memFactsURL)
	region := s.memRegion(r.Context(), back)
	if s.mem == nil {
		s.formDone(w, r, back, "", "", region)
		return
	}
	if err := s.mem.DeleteFact(id); err != nil {
		s.formDone(w, r, back, "", s.flashFor(r.Context(), "fact delete", err, "Couldn't remove that fact."), region)
		return
	}
	s.log.Info("dashboard: ssr fact removed", "id", id)
	page := addParam(back, "removed", i64(id))
	s.formDone(w, r, page, "Fact removed.", "", s.memRegion(r.Context(), page))
}

// handleMemoryFactRestoreForm un-tombstones a fact:
// POST memFactsURL/restore {fact, back} -> RestoreFact, answered on the view
// the restore was on (the Undo banner on the facts list, the Recently
// removed page or the fact's page), defaulting to the facts list. The return
// path is validated to our own surface to avoid an open redirect.
func (s *Server) handleMemoryFactRestoreForm(w http.ResponseWriter, r *http.Request) {
	id, ok := memFormID(w, r)
	if !ok {
		return
	}
	back := memReturn(r, memFactsURL)
	region := s.memRegion(r.Context(), back)
	if s.mem == nil {
		s.formDone(w, r, back, "", "", region)
		return
	}
	if err := s.mem.RestoreFact(id); err != nil {
		s.formDone(w, r, back, "", s.flashFor(r.Context(), "fact restore", err, "Couldn't restore that fact."), region)
		return
	}
	s.log.Info("dashboard: ssr fact restored", "id", id)
	s.formDone(w, r, back, "Fact restored.", "", region)
}

// handleMemoryResolveForm records a conflict resolution: keep_id stays, the
// other is tombstoned (ResolveFacts). POST memFactsURL/resolve
// {keep_id,discard_id,q} -> 303 to the list preserving search. NEVER automatic
// — the operator chose which to keep. It stays a full page load: the picker's
// state is in the URL (?resolve=&with=), which the answer must clear, and the
// card sits at the top of the page, where the load lands.
func (s *Server) handleMemoryResolveForm(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	if s.mem == nil {
		http.Redirect(w, r, urlWith(memFactsURL, "q", q), http.StatusSeeOther)
		return
	}
	keep := atoi64(r.FormValue("keep_id"))
	discard := atoi64(r.FormValue("discard_id"))
	if err := s.mem.ResolveFacts(keep, discard); err != nil {
		http.Redirect(w, r, urlWith(memFactsURL, "q", q, "err",
			s.flashFor(r.Context(), "fact resolve", err, "Couldn't resolve that.")), http.StatusSeeOther)
		return
	}
	s.log.Info("dashboard: ssr facts resolved", "keep", keep, "discard", discard)
	http.Redirect(w, r, urlWith(memFactsURL, "q", q), http.StatusSeeOther)
}

// --- form helpers ----------------------------------------------------------

// memFormID is the fact a form acts on: the {id} in its path, or its fact
// field on a literal route. A malformed id is a 400, not a store call with a
// zero id. The field isn't named "id": an input named id hides the form's own
// id (form.id is the input), so Datastar's morph can't match the form with
// its new copy and replaces it, which resets its throttle.
func memFormID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	if raw == "" {
		raw = r.FormValue("fact")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid fact id: "+raw)
		return 0, false
	}
	return id, true
}

// memReturn returns a safe in-surface redirect target from the "back" form
// value, or fallback. Only paths under memBase are honored (no open redirect).
func memReturn(r *http.Request, fallback string) string {
	back := r.FormValue("back")
	if strings.HasPrefix(back, memBase) {
		return back
	}
	return fallback
}

// memRegion renders, for an in-place answer, the region of the memory page
// at back (a path from memReturn, with its view's query): what the page
// would show if loaded there now.
func (s *Server) memRegion(ctx context.Context, back string) func() (string, error) {
	return func() (string, error) {
		u, err := url.Parse(back)
		if err != nil {
			return "", err
		}
		q := u.Query()
		var buf bytes.Buffer
		id, fact := strings.CutPrefix(u.Path, memFactsURL+"/")
		switch {
		case u.Path == memSoulURL:
			err = s.tmpl.memSoul.ExecuteTemplate(&buf, "mem-soul-body", s.memSoulView(ctx, q))
		case u.Path == memFactsURL+"/removed":
			err = s.tmpl.memRemoved.ExecuteTemplate(&buf, "mem-removed-body", s.memRemovedView(ctx, q))
		case fact:
			err = s.tmpl.memFact.ExecuteTemplate(&buf, "mem-fact-body", s.memFactView(ctx, id, q))
		default:
			err = s.tmpl.memFacts.ExecuteTemplate(&buf, "mem-facts-body", s.memFactsView(ctx, q))
		}
		return buf.String(), err
	}
}

// addParam appends key=value to a same-origin path's query.
func addParam(path, key, value string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + key + "=" + url.QueryEscape(value)
}

// i64 formats an int64 as a decimal string (urlWith handles URL-encoding).
func i64(n int64) string { return strconv.FormatInt(n, 10) }
