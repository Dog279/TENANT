package dashboard

// logbook.go mounts the log book's API v2 (internal/logs/logapi, TEN-378)
// behind the dashboard's auth and limits, and keeps v1's JSON feed,
// GET /api/activity, as a thin shim over logbook.db.

import (
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"tenant/internal/logs"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
)

func (s *Server) mountLogbook(mux *http.ServeMux) {
	s.logbook = &logapi.API{Hub: func() *logs.Hub { return s.logs }}
	s.logbook.Mount(mux)
}

// SetRunRecords lets a timeline show its trace's run record from runs.db.
func (s *Server) SetRunRecords(fn logapi.RunLookup) { s.logbook.Runs = fn }

// SetSecurityLog lets the API verify and export the Security log's chain,
// whose key, anchor and witness the composition root holds.
func (s *Server) SetSecurityLog(sl logapi.SecurityLog) { s.logbook.Security = sl }

// activityBacklog is how far back a v1 client replays when its cursor is
// not this store's: the v1 ring's size.
const activityBacklog = 10000

// handleActivityStore is GET /api/activity over logbook.db: the same
// projection, filters and paging as the ring's (handleActivity), whose
// cursor is now the store's seq and whose epoch is the store's ID. A cursor
// survives a restart; one from another store (or none) replays the newest
// activityBacklog events. v1 served the text mirror's lines, so diagnostic
// events are left out. Without a store, the ring answers.
func (s *Server) handleActivityStore(w http.ResponseWriter, r *http.Request) {
	_, st := s.logbookStore()
	if st == nil {
		s.handleActivity(w, r)
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	head, err := st.Latest(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	epoch := hex.EncodeToString(head.StoreID[:])
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	if e := q.Get("epoch"); since == 0 || e != "" && e != epoch {
		since = head.Seq - min(head.Seq, activityBacklog)
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > activityBacklog {
		limit = activityBacklog
	}
	f, err := query.Parse(activityQuery(activityFilterFrom(r)))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	evs, next, _, err := st.Query(ctx, f, store.Cursor{StoreID: head.StoreID, Seq: since}, limit, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := restActivityResponse{Events: []restActivityEvent{}, Cursor: next.Seq, Epoch: epoch}
	if len(evs) < limit { // nothing else up to the head matched
		resp.Cursor = max(next.Seq, head.Seq)
	}
	for _, e := range evs {
		en := logs.EntryFromEvent(e)
		en.Seq = e.Seq
		resp.Events = append(resp.Events, restEntry(en))
	}
	writeJSON(w, http.StatusOK, resp)
}

// activityQuery is v1's source, level and trace filter in the filter
// language, over the text mirror's lines.
func activityQuery(f activityFilter) string {
	return strings.TrimSpace("channel:operational " + f.logsQuery())
}
