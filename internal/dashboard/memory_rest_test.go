package dashboard

// memory_rest_test.go (TEN-89) unit-tests the Memory Curator REST surface.
// Helpers use the `mem` prefix so they don't collide with the rest_/ws_/auth_/
// wire_ fakes in this same package.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
)

// memFakeControl is a MemoryControl stand-in that records mutating calls and
// returns canned views for the read paths. Errors are injectable per-method to
// exercise the 4xx error paths (e.g. a stale id on delete/restore/resolve).
type memFakeControl struct {
	soul         SoulView
	facts        []FactView
	nextCur      string
	removed      []FactView
	temporal     []TemporalFactView
	temporalStat MemStats
	episodes     []EpisodeView
	working      int
	profile      string
	compProv     *CompactionProvenanceView
	compProvErr  error
	soulErr      error
	factsErr     error
	provErr      error
	temporalErr  error
	mutateErr    error // shared by SoulEdit/Resolve/Delete/Restore/Resync

	// recorded calls.
	soulEdits    []SoulEditOp
	factsQ       string
	factsLimit   int
	factsCursor  string
	removedLimit int
	provID       int64
	resolveKeep  int64
	resolveDisc  int64
	deleted      []int64
	restored     []int64
	resyncs      int
}

func (f *memFakeControl) Soul() (SoulView, error) { return f.soul, f.soulErr }

func (f *memFakeControl) SoulEdit(op SoulEditOp) error {
	f.soulEdits = append(f.soulEdits, op)
	return f.mutateErr
}

func (f *memFakeControl) Facts(q string, limit int, cursor string) ([]FactView, string, error) {
	f.factsQ, f.factsLimit, f.factsCursor = q, limit, cursor
	return f.facts, f.nextCur, f.factsErr
}

func (f *memFakeControl) FactProvenance(id int64) ([]EpisodeView, error) {
	f.provID = id
	return f.episodes, f.provErr
}

func (f *memFakeControl) ResolveFacts(keepID, discardID int64) error {
	f.resolveKeep, f.resolveDisc = keepID, discardID
	return f.mutateErr
}

func (f *memFakeControl) DeleteFact(id int64) error {
	f.deleted = append(f.deleted, id)
	return f.mutateErr
}

func (f *memFakeControl) RestoreFact(id int64) error {
	f.restored = append(f.restored, id)
	return f.mutateErr
}

func (f *memFakeControl) RemovedFacts(limit int) ([]FactView, error) {
	f.removedLimit = limit
	return f.removed, f.factsErr
}

func (f *memFakeControl) TemporalFacts() ([]TemporalFactView, MemStats, error) {
	return f.temporal, f.temporalStat, f.temporalErr
}

func (f *memFakeControl) WorkingCount() int { return f.working }

func (f *memFakeControl) UserProfile() (string, error) { return f.profile, f.soulErr }

func (f *memFakeControl) ResyncUserProfile() error {
	f.resyncs++
	return f.mutateErr
}

func (f *memFakeControl) CompactionProvenance() (*CompactionProvenanceView, error) {
	return f.compProv, f.compProvErr
}

// newMemServer builds a Server with the given fake memory control. New() wires
// the memory routes via routes() (s.mem != nil), so no explicit mount is needed.
func newMemServer(f *memFakeControl) *Server {
	return New(Config{}, nil, nil, f, agent.NewBroker(0), nil)
}

// memDo drives one request through the server's Handler and returns the recorder.
func memDo(s *Server, method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// TestMemSoul: GET /api/memory/soul returns the canned SoulView shape.
func TestMemSoul(t *testing.T) {
	f := &memFakeControl{soul: SoulView{
		Persona:      "helpful",
		UserFacts:    []SoulItem{{ID: "u1", Text: "likes go"}},
		Instructions: []SoulItem{{ID: "i1", Text: "be terse"}},
	}}
	rec := memDo(newMemServer(f), http.MethodGet, "/api/memory/soul", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got SoulView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Persona != "helpful" || len(got.UserFacts) != 1 || got.UserFacts[0].ID != "u1" {
		t.Fatalf("soul = %+v, want persona=helpful + one user fact u1", got)
	}
	if len(got.Instructions) != 1 || got.Instructions[0].Text != "be terse" {
		t.Fatalf("instructions = %+v, want one 'be terse'", got.Instructions)
	}
}

// TestMemSoulEdit: POST /api/memory/soul forwards the SoulEditOp verbatim.
func TestMemSoulEdit(t *testing.T) {
	f := &memFakeControl{}
	rec := memDo(newMemServer(f), http.MethodPost, "/api/memory/soul",
		`{"section":"user_fact","action":"edit","id":"u1","text":"loves go"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(f.soulEdits) != 1 {
		t.Fatalf("SoulEdit called %d times, want 1", len(f.soulEdits))
	}
	want := SoulEditOp{Section: SoulSectionUserFact, Action: SoulActionEdit, ID: "u1", Text: "loves go"}
	if f.soulEdits[0] != want {
		t.Fatalf("SoulEdit op = %+v, want %+v", f.soulEdits[0], want)
	}
}

// TestMemSoulEditBadJSON: a malformed body is a 400 and never reaches the control.
func TestMemSoulEditBadJSON(t *testing.T) {
	f := &memFakeControl{}
	rec := memDo(newMemServer(f), http.MethodPost, "/api/memory/soul", `{"section":`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(f.soulEdits) != 0 {
		t.Fatalf("SoulEdit called %d times on bad body, want 0", len(f.soulEdits))
	}
}

// TestMemFacts: GET /api/memory/facts forwards q/limit/cursor and returns
// {facts, next_cursor}.
func TestMemFacts(t *testing.T) {
	f := &memFakeControl{
		facts:   []FactView{{ID: 7, Text: "fact", Confidence: 0.9, SourceEpisodes: []int64{1, 2}}},
		nextCur: "cur2",
	}
	rec := memDo(newMemServer(f), http.MethodGet, "/api/memory/facts?q=go&limit=5&cursor=cur1", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if f.factsQ != "go" || f.factsLimit != 5 || f.factsCursor != "cur1" {
		t.Fatalf("Facts called with (q=%q,limit=%d,cursor=%q), want (go,5,cur1)", f.factsQ, f.factsLimit, f.factsCursor)
	}
	var got memFactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Facts) != 1 || got.Facts[0].ID != 7 || got.NextCursor != "cur2" {
		t.Fatalf("resp = %+v, want one fact id=7 + next_cursor=cur2", got)
	}
}

// TestMemRemovedFacts: GET /api/memory/facts/removed forwards limit and returns
// {facts}. The LITERAL "removed" segment must route here, NOT to the {id} paths.
func TestMemRemovedFacts(t *testing.T) {
	f := &memFakeControl{removed: []FactView{{ID: 3, Text: "gone"}}}
	rec := memDo(newMemServer(f), http.MethodGet, "/api/memory/facts/removed?limit=10", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if f.removedLimit != 10 {
		t.Fatalf("RemovedFacts limit = %d, want 10", f.removedLimit)
	}
	// The {id}/restore|delete paths must NOT have been hit by the literal route.
	if len(f.deleted) != 0 || len(f.restored) != 0 {
		t.Fatalf("literal 'removed' leaked into {id} handlers: deleted=%v restored=%v", f.deleted, f.restored)
	}
	var got memFactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Facts) != 1 || got.Facts[0].ID != 3 {
		t.Fatalf("resp = %+v, want one removed fact id=3", got)
	}
}

// TestMemTemporal: GET /api/memory/facts/temporal returns {facts, stats}. The
// LITERAL "temporal" segment must route here, NOT to the {id}/provenance path.
func TestMemTemporal(t *testing.T) {
	f := &memFakeControl{
		temporal: []TemporalFactView{
			{ID: 7, Text: "live one", Confidence: 0.9, EffectiveConfidence: 0.9,
				FirstSeen: 100, LastConfirmed: 200, Status: FactStatusLive},
			{ID: 8, Text: "old one", Confidence: 0.5, EffectiveConfidence: 0.25,
				FirstSeen: 50, LastConfirmed: 60, SupersededBy: 7, Status: FactStatusSuperseded},
		},
		temporalStat: MemStats{Total: 2, Live: 1, Superseded: 1},
	}
	rec := memDo(newMemServer(f), http.MethodGet, "/api/memory/facts/temporal", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	// The literal route must NOT have leaked into the {id} provenance handler.
	if f.provID != 0 || len(f.deleted) != 0 || len(f.restored) != 0 {
		t.Fatalf("literal 'temporal' leaked into {id} handlers: prov=%d deleted=%v restored=%v", f.provID, f.deleted, f.restored)
	}
	var got memTemporalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Facts) != 2 || got.Facts[0].ID != 7 || got.Facts[1].Status != FactStatusSuperseded {
		t.Fatalf("facts = %+v, want id=7 live then id=8 superseded", got.Facts)
	}
	if got.Stats != (MemStats{Total: 2, Live: 1, Superseded: 1}) {
		t.Fatalf("stats = %+v, want {2,1,1,0}", got.Stats)
	}
}

// TestMemTemporalError: a store error on the temporal enumeration is a 500 JSON
// envelope (it is a read of the whole store, not a per-id lookup).
func TestMemTemporalError(t *testing.T) {
	f := &memFakeControl{temporalErr: memErr("scan failed")}
	rec := memDo(newMemServer(f), http.MethodGet, "/api/memory/facts/temporal", "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	var env map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env["error"] == "" {
		t.Fatalf("expected error envelope, got %v", env)
	}
}

// TestMemProvenance: GET /api/memory/facts/{id}/provenance parses the id and
// returns {episodes}.
func TestMemProvenance(t *testing.T) {
	ts := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	f := &memFakeControl{episodes: []EpisodeView{
		{ID: 11, Prompt: "hi", Response: "yo", Timestamp: ts},
		{ID: 12, Missing: true},
	}}
	rec := memDo(newMemServer(f), http.MethodGet, "/api/memory/facts/42/provenance", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if f.provID != 42 {
		t.Fatalf("FactProvenance id = %d, want 42", f.provID)
	}
	var got memEpisodesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Episodes) != 2 || got.Episodes[0].ID != 11 || !got.Episodes[1].Missing {
		t.Fatalf("episodes = %+v, want id=11 then a Missing one", got.Episodes)
	}
}

// TestMemResolve: POST /api/memory/facts/resolve forwards {keep_id,discard_id}.
// The LITERAL "resolve" segment must route here, NOT to the {id} paths.
func TestMemResolve(t *testing.T) {
	f := &memFakeControl{}
	rec := memDo(newMemServer(f), http.MethodPost, "/api/memory/facts/resolve",
		`{"keep_id":5,"discard_id":9}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if f.resolveKeep != 5 || f.resolveDisc != 9 {
		t.Fatalf("ResolveFacts(%d,%d), want (5,9)", f.resolveKeep, f.resolveDisc)
	}
}

// TestMemDelete: DELETE /api/memory/facts/{id} forwards the parsed id.
func TestMemDelete(t *testing.T) {
	f := &memFakeControl{}
	rec := memDo(newMemServer(f), http.MethodDelete, "/api/memory/facts/77", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(f.deleted) != 1 || f.deleted[0] != 77 {
		t.Fatalf("DeleteFact calls = %v, want [77]", f.deleted)
	}
}

// TestMemRestore: POST /api/memory/facts/{id}/restore forwards the parsed id.
func TestMemRestore(t *testing.T) {
	f := &memFakeControl{}
	rec := memDo(newMemServer(f), http.MethodPost, "/api/memory/facts/88/restore", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(f.restored) != 1 || f.restored[0] != 88 {
		t.Fatalf("RestoreFact calls = %v, want [88]", f.restored)
	}
}

// TestMemWorkingCount: GET /api/memory/working/count returns {count}.
func TestMemWorkingCount(t *testing.T) {
	f := &memFakeControl{working: 13}
	rec := memDo(newMemServer(f), http.MethodGet, "/api/memory/working/count", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got memCountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Count != 13 {
		t.Fatalf("count = %d, want 13", got.Count)
	}
}

// TestMemUserProfile: GET /api/memory/userprofile returns {markdown}.
func TestMemUserProfile(t *testing.T) {
	f := &memFakeControl{profile: "# user\nlikes go"}
	rec := memDo(newMemServer(f), http.MethodGet, "/api/memory/userprofile", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got memUserProfileResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Markdown != "# user\nlikes go" {
		t.Fatalf("markdown = %q, want the canned profile", got.Markdown)
	}
}

// TestMemResyncUserProfile: POST /api/memory/userprofile/resync calls the control.
func TestMemResyncUserProfile(t *testing.T) {
	f := &memFakeControl{}
	rec := memDo(newMemServer(f), http.MethodPost, "/api/memory/userprofile/resync", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if f.resyncs != 1 {
		t.Fatalf("ResyncUserProfile called %d times, want 1", f.resyncs)
	}
}

// TestMemBadID: a non-numeric {id} is a 400 on every {id} route, and never
// reaches the control.
func TestMemBadID(t *testing.T) {
	cases := []struct {
		name, method, target string
	}{
		{"provenance", http.MethodGet, "/api/memory/facts/abc/provenance"},
		{"delete", http.MethodDelete, "/api/memory/facts/abc"},
		{"restore", http.MethodPost, "/api/memory/facts/abc/restore"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &memFakeControl{}
			rec := memDo(newMemServer(f), tc.method, tc.target, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if f.provID != 0 || len(f.deleted) != 0 || len(f.restored) != 0 {
				t.Fatalf("bad id reached control: prov=%d deleted=%v restored=%v", f.provID, f.deleted, f.restored)
			}
		})
	}
}

// TestMemResolveBadJSON: a malformed resolve body is a 400 and never resolves.
func TestMemResolveBadJSON(t *testing.T) {
	f := &memFakeControl{}
	rec := memDo(newMemServer(f), http.MethodPost, "/api/memory/facts/resolve", `{"keep_id":`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if f.resolveKeep != 0 || f.resolveDisc != 0 {
		t.Fatalf("ResolveFacts ran on bad body: keep=%d discard=%d", f.resolveKeep, f.resolveDisc)
	}
}

// TestMemStaleMutation: a store error on a stale/missing id is a 4xx JSON
// envelope (NOT 500) for delete/restore/resolve.
func TestMemStaleMutation(t *testing.T) {
	cases := []struct {
		name, method, target, body string
	}{
		{"delete", http.MethodDelete, "/api/memory/facts/77", ""},
		{"restore", http.MethodPost, "/api/memory/facts/77/restore", ""},
		{"resolve", http.MethodPost, "/api/memory/facts/resolve", `{"keep_id":1,"discard_id":2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &memFakeControl{mutateErr: memErr("no such fact")}
			rec := memDo(newMemServer(f), tc.method, tc.target, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 on stale id", rec.Code)
			}
			var env map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env["error"] == "" {
				t.Fatalf("expected error envelope, got %v", env)
			}
		})
	}
}

// TestMemNilNotMounted: a Server built with mem=nil must NOT mount the memory
// routes (the existing dashboard tests construct servers that way) — the SPA
// catch-all answers /api/memory/* instead of a memory handler.
func TestMemNilNotMounted(t *testing.T) {
	s := New(Config{}, nil, nil, nil, agent.NewBroker(0), nil)
	rec := memDo(s, http.MethodGet, "/api/memory/soul", "")
	// The GET / SPA FileServer handles the path; it is NOT 200 application/json
	// from a memory handler. A 404/redirect from the file server is fine; the
	// point is the route was not registered (and New didn't panic).
	if rec.Code == http.StatusOK && rec.Header().Get("Content-Type") == "application/json" {
		t.Fatalf("mem=nil server answered /api/memory/soul as JSON (route mounted): %q", rec.Body.String())
	}
}

// TestMemAuth: the memory REST surface sits behind the same bearer envelope as
// the rest of /api — 401 without a token, 200 with (mirrors wire_test.go).
func TestMemAuth(t *testing.T) {
	f := &memFakeControl{facts: []FactView{{ID: 1, Text: "fact"}}}
	s := New(Config{Auth: "secret"}, nil, nil, f, agent.NewBroker(0), nil)
	h := s.secure(s.Handler())

	t.Run("no token → 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/memory/facts", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("with token → 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/memory/facts", nil)
		req.Header.Set("Authorization", "Bearer secret")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
	})
}

// memErr is a sentinel error type for the stale-mutation tests.
type memErr string

func (e memErr) Error() string { return string(e) }
