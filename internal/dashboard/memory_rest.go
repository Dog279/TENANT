package dashboard

// memory_rest.go is TEN-89: the Memory Curator's JSON control surface. It is
// the HTTP layer over the TEN-88 MemoryControl interface (memory.go) — view
// and curate the agent's soul (T0) and distilled facts (T3), with provenance,
// deletion, contradiction resolution, and restore. It follows rest.go's
// conventions exactly (Go 1.22 method+pattern routing, writeJSON/writeError
// envelopes, DisallowUnknownFields on bodies) and is mounted from routes()
// only when s.mem is non-nil.

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// memFactsResponse is the GET /api/memory/facts (and .../removed) payload:
// the page of facts plus the opaque keyset cursor for the next page. NextCursor
// is "" when no more remain (and is always "" for the removed list, which is
// unpaginated beyond its limit).
type memFactsResponse struct {
	Facts      []FactView `json:"facts"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// memTemporalResponse is the GET /api/memory/facts/temporal payload: the full
// enumeration of facts projected onto the knowledge-time axis plus the count
// summary. Unpaginated by design (the Overview needs the whole set to lay out
// the timeline), so there is no cursor.
type memTemporalResponse struct {
	Facts []TemporalFactView `json:"facts"`
	Stats MemStats           `json:"stats"`
}

// memEpisodesResponse is the GET /api/memory/facts/{id}/provenance payload.
type memEpisodesResponse struct {
	Episodes []EpisodeView `json:"episodes"`
}

// memResolveRequest is the body of POST /api/memory/facts/resolve. The verb is
// deliberately symmetric ("resolve a contradiction"): keep_id supersedes
// discard_id (see MemoryControl.ResolveFacts).
type memResolveRequest struct {
	KeepID    int64 `json:"keep_id"`
	DiscardID int64 `json:"discard_id"`
}

// memCountResponse is the GET /api/memory/working/count payload.
type memCountResponse struct {
	Count int `json:"count"`
}

// memUserProfileResponse is the GET /api/memory/userprofile payload: the
// synthesized always-on user model as markdown.
type memUserProfileResponse struct {
	Markdown string `json:"markdown"`
}

// mountMemoryREST registers the Memory Curator API on mux using Go 1.22
// method+pattern routing. Called from routes() (guarded by s.mem != nil).
//
// Go 1.22 mux precedence makes the literal "removed"/"resolve" segments win
// over the "{id}" wildcard at the same position, so the order below is for
// readability only — there is no pattern conflict to panic on at mount. (We
// register no bare GET/POST /api/memory/facts/{id}, only the more-specific
// {id}/provenance and {id}/restore plus DELETE {id}, so the literals never
// even compete with a same-method wildcard.)
func (s *Server) mountMemoryREST(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/memory/soul", s.handleMemSoul)
	mux.HandleFunc("POST /api/memory/soul", s.handleMemSoulEdit)

	mux.HandleFunc("GET /api/memory/facts", s.handleMemFacts)
	mux.HandleFunc("GET /api/memory/facts/removed", s.handleMemRemovedFacts)
	mux.HandleFunc("GET /api/memory/facts/temporal", s.handleMemTemporal)
	mux.HandleFunc("GET /api/memory/facts/{id}/provenance", s.handleMemProvenance)
	mux.HandleFunc("POST /api/memory/facts/resolve", s.handleMemResolve)
	mux.HandleFunc("POST /api/memory/facts/{id}/restore", s.handleMemRestore)
	mux.HandleFunc("DELETE /api/memory/facts/{id}", s.handleMemDelete)

	mux.HandleFunc("GET /api/memory/working/count", s.handleMemWorkingCount)

	mux.HandleFunc("GET /api/memory/userprofile", s.handleMemUserProfile)
	mux.HandleFunc("POST /api/memory/userprofile/resync", s.handleMemResyncUserProfile)
}

// handleMemSoul returns the editable soul: GET /api/memory/soul -> SoulView.
func (s *Server) handleMemSoul(w http.ResponseWriter, _ *http.Request) {
	soul, err := s.mem.Soul()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, soul)
}

// handleMemSoulEdit applies one soul mutation: POST /api/memory/soul with a
// SoulEditOp body. A bad op (unknown section/action, missing target) is the
// store's error, surfaced as 400. The edit mutates the agent's identity, so
// it's audited.
func (s *Server) handleMemSoulEdit(w http.ResponseWriter, r *http.Request) {
	var op SoulEditOp
	if !decodeJSON(w, r, &op) {
		return
	}
	if err := s.mem.SoulEdit(op); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("memory: soul edited", "section", op.Section, "action", op.Action, "id", op.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMemFacts returns a page of live facts:
// GET /api/memory/facts?q=&limit=&cursor= -> {facts, next_cursor}. limit is
// best-effort parsed (a bad/absent value is left 0 for the store to default).
func (s *Server) handleMemFacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	facts, next, err := s.mem.Facts(q.Get("q"), limit, q.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, memFactsResponse{Facts: facts, NextCursor: next})
}

// handleMemRemovedFacts lists tombstoned facts for the restore view:
// GET /api/memory/facts/removed?limit= -> {facts}. "removed" is a literal
// segment that the Go 1.22 mux prefers over {id}.
func (s *Server) handleMemRemovedFacts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	facts, err := s.mem.RemovedFacts(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, memFactsResponse{Facts: facts})
}

// handleMemTemporal returns the full knowledge-time projection for the
// Overview: GET /api/memory/facts/temporal -> {facts, stats}. "temporal" is a
// literal segment the Go 1.22 mux prefers over {id}. Unpaginated by design.
func (s *Server) handleMemTemporal(w http.ResponseWriter, _ *http.Request) {
	facts, stats, err := s.mem.TemporalFacts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, memTemporalResponse{Facts: facts, Stats: stats})
}

// handleMemProvenance resolves a fact's source turns:
// GET /api/memory/facts/{id}/provenance -> {episodes}. A forgotten source is a
// marked EpisodeView (Missing=true), not an error, so only a bad id is 4xx.
func (s *Server) handleMemProvenance(w http.ResponseWriter, r *http.Request) {
	id, ok := memPathID(w, r)
	if !ok {
		return
	}
	eps, err := s.mem.FactProvenance(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, memEpisodesResponse{Episodes: eps})
}

// handleMemResolve records a contradiction resolution:
// POST /api/memory/facts/resolve {keep_id,discard_id} -> keep supersedes
// discard. A stale/missing id is the store's error, surfaced as 400. Audited.
func (s *Server) handleMemResolve(w http.ResponseWriter, r *http.Request) {
	var req memResolveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.mem.ResolveFacts(req.KeepID, req.DiscardID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("memory: resolved facts", "keep_id", req.KeepID, "discard_id", req.DiscardID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMemRestore un-tombstones a fact:
// POST /api/memory/facts/{id}/restore -> RestoreFact(id). A stale/missing id is
// the store's error, surfaced as 400 (not 500). Audited.
func (s *Server) handleMemRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := memPathID(w, r)
	if !ok {
		return
	}
	if err := s.mem.RestoreFact(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("memory: restored fact", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMemDelete tombstones a fact (recoverable via restore):
// DELETE /api/memory/facts/{id} -> DeleteFact(id). A stale/missing id is the
// store's error, surfaced as 400 (not 500). Audited.
func (s *Server) handleMemDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := memPathID(w, r)
	if !ok {
		return
	}
	if err := s.mem.DeleteFact(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("memory: deleted fact", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMemWorkingCount reports the T1 working-set size:
// GET /api/memory/working/count -> {count}.
func (s *Server) handleMemWorkingCount(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, memCountResponse{Count: s.mem.WorkingCount()})
}

// handleMemUserProfile returns the synthesized user model:
// GET /api/memory/userprofile -> {markdown}.
func (s *Server) handleMemUserProfile(w http.ResponseWriter, _ *http.Request) {
	md, err := s.mem.UserProfile()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, memUserProfileResponse{Markdown: md})
}

// handleMemResyncUserProfile regenerates the profile from current T3 facts:
// POST /api/memory/userprofile/resync. The resync rewrites the always-on user
// model, so it's audited.
func (s *Server) handleMemResyncUserProfile(w http.ResponseWriter, _ *http.Request) {
	if err := s.mem.ResyncUserProfile(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("memory: resynced user profile")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// memPathID parses the {id} path value as an int64, writing a 400 and
// returning ok=false on a malformed id so a non-numeric path is a client error
// rather than a store call with a zero id.
func memPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid fact id: "+r.PathValue("id"))
		return 0, false
	}
	return id, true
}

// decodeJSON reads a JSON body into v with unknown-field rejection, writing a
// 400 and returning ok=false on malformed input — mirroring rest.go's
// decodeToggle so a typo'd field is a client error, not a silent no-op.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}
