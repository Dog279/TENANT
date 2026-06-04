package dashboard

// ssr_memory_forms.go is TEN-111: the mutation handlers behind the Memory
// curator pages. Each is a POST that drives the SAME MemoryControl methods the
// JSON REST layer uses (memory_rest.go), then 303-redirects (POST/redirect/GET)
// so a browser refresh can't replay the mutation. Values come from r.FormValue.
// Errors surface by redirecting back with an ?err= the target page renders.

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// handleMemorySoulEditForm applies one add/edit/remove to a soul list:
// POST memSoulURL/edit {section,action,id,text} -> SoulEdit -> 303 back to the
// soul page. A bad op (unknown section/action, missing target) round-trips as
// an ?err banner rather than a dead 400.
func (s *Server) handleMemorySoulEditForm(w http.ResponseWriter, r *http.Request) {
	if s.mem == nil {
		http.Redirect(w, r, memSoulURL, http.StatusSeeOther)
		return
	}
	op := SoulEditOp{
		Section: r.FormValue("section"),
		Action:  r.FormValue("action"),
		ID:      r.FormValue("id"),
		Text:    strings.TrimSpace(r.FormValue("text")),
	}
	if err := s.mem.SoulEdit(op); err != nil {
		s.log.Warn("dashboard: ssr soul edit", "section", op.Section, "action", op.Action, "err", err)
		http.Redirect(w, r, memSoulURL+"?err="+url.QueryEscape("Couldn't save that change: "+err.Error()), http.StatusSeeOther)
		return
	}
	s.log.Info("dashboard: ssr soul edited", "section", op.Section, "action", op.Action, "id", op.ID)
	http.Redirect(w, r, memSoulURL, http.StatusSeeOther)
}

// handleMemoryFactDeleteForm tombstones a fact (recoverable):
// POST memFactsURL/{id}/delete -> DeleteFact -> 303 to the list with ?removed=
// (which renders the Undo banner) preserving the active search.
func (s *Server) handleMemoryFactDeleteForm(w http.ResponseWriter, r *http.Request) {
	id, ok := memFormID(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.FormValue("q"))
	if s.mem != nil {
		if err := s.mem.DeleteFact(id); err != nil {
			s.log.Warn("dashboard: ssr fact delete", "id", id, "err", err)
			http.Redirect(w, r, urlWith(memFactsURL, "q", q, "err",
				"Couldn't remove that fact: "+err.Error()), http.StatusSeeOther)
			return
		}
		s.log.Info("dashboard: ssr fact removed", "id", id)
	}
	http.Redirect(w, r, urlWith(memFactsURL, "q", q, "removed", i64(id)), http.StatusSeeOther)
}

// handleMemoryFactRestoreForm un-tombstones a fact:
// POST memFactsURL/{id}/restore -> RestoreFact -> 303 back to wherever the
// restore was triggered (the Undo banner on the facts list, or the Recently
// removed page), defaulting to the facts list. The return path is validated to
// our own surface to avoid an open redirect.
func (s *Server) handleMemoryFactRestoreForm(w http.ResponseWriter, r *http.Request) {
	id, ok := memFormID(w, r)
	if !ok {
		return
	}
	back := memReturn(r, memFactsURL)
	if s.mem != nil {
		if err := s.mem.RestoreFact(id); err != nil {
			s.log.Warn("dashboard: ssr fact restore", "id", id, "err", err)
			http.Redirect(w, r, addErr(back, "Couldn't restore that fact: "+err.Error()), http.StatusSeeOther)
			return
		}
		s.log.Info("dashboard: ssr fact restored", "id", id)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleMemoryResolveForm records a conflict resolution: keep_id stays, the
// other is tombstoned (ResolveFacts). POST memFactsURL/resolve
// {keep_id,discard_id,q} -> 303 to the list preserving search. NEVER automatic
// — the operator chose which to keep.
func (s *Server) handleMemoryResolveForm(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	if s.mem == nil {
		http.Redirect(w, r, urlWith(memFactsURL, "q", q), http.StatusSeeOther)
		return
	}
	keep := atoi64(r.FormValue("keep_id"))
	discard := atoi64(r.FormValue("discard_id"))
	if err := s.mem.ResolveFacts(keep, discard); err != nil {
		s.log.Warn("dashboard: ssr resolve", "keep", keep, "discard", discard, "err", err)
		http.Redirect(w, r, urlWith(memFactsURL, "q", q, "err",
			"Couldn't resolve that: "+err.Error()), http.StatusSeeOther)
		return
	}
	s.log.Info("dashboard: ssr facts resolved", "keep", keep, "discard", discard)
	http.Redirect(w, r, urlWith(memFactsURL, "q", q), http.StatusSeeOther)
}

// --- form helpers ----------------------------------------------------------

// memFormID parses the {id} path value, writing a 400 on a malformed id so a
// non-numeric path is a client error, not a store call with a zero id.
func memFormID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	return memPathID(w, r) // reuse memory_rest.go's parser (same {id} pattern)
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

// addErr appends an ?err / &err to a same-origin path.
func addErr(path, msg string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "err=" + url.QueryEscape(msg)
}

// i64 formats an int64 as a decimal string (urlWith handles URL-encoding).
func i64(n int64) string { return strconv.FormatInt(n, 10) }
