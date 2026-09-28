package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
)

// inspFake is a MemoryControl that can also explain facts (TEN-297).
type inspFake struct {
	memFakeControl
	detail    *FactDetailView
	detailErr error
	turns     []TurnView
	actErr    error

	edits       []string
	editedID    int64
	invalidated []int64
	revalidated []int64
	restoredVer []int64
}

func (f *inspFake) FactDetail(id int64) (*FactDetailView, error) {
	if f.detailErr != nil {
		return nil, f.detailErr
	}
	d := *f.detail
	d.Fact.ID = id
	return &d, nil
}

func (f *inspFake) EditFact(id int64, text string) (int64, error) {
	f.editedID = id
	f.edits = append(f.edits, text)
	return 99, f.actErr
}

func (f *inspFake) InvalidateFact(id int64) error {
	f.invalidated = append(f.invalidated, id)
	return f.actErr
}

func (f *inspFake) RevalidateFact(id int64) error {
	f.revalidated = append(f.revalidated, id)
	return f.actErr
}

func (f *inspFake) RestoreFactVersion(v int64) (int64, error) {
	f.restoredVer = append(f.restoredVer, v)
	return 100, f.actErr
}

func (f *inspFake) FactTurns(id int64) ([]TurnView, error) { return f.turns, nil }

func sampleDetail() *FactDetailView {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	gone := now.Add(-time.Hour)
	return &FactDetailView{
		Fact:           TemporalFactView{Text: "User lives in Munich", Confidence: 0.9, EffectiveConfidence: 0.8, FirstSeen: now.Unix(), LastConfirmed: now.Unix(), Status: FactStatusLive},
		Origin:         &OriginView{Job: "distill", Model: "glm-5", RecordedAt: now},
		Signals:        SignalsView{Importance: 0.7, Pinned: true, AccessCount: 4, LastAccessed: &now},
		Sources:        []EpisodeView{{ID: 11, Prompt: "I moved to Munich", Response: "noted", Timestamp: now}, {ID: 12, Missing: true}},
		RemovedSources: []RemovedSourceView{{Kind: "episode", ID: "10", RemovedAt: gone, Reason: "forget"}},
		Versions: []VersionView{
			{ID: 5, Text: "User lives in Berlin", FirstSeen: gone, Status: FactStatusSuperseded, Job: "distill"},
			{ID: 7, Text: "User lives in Munich", FirstSeen: now, Status: FactStatusLive, Job: "edit"},
		},
		Replaced:   []VersionView{{ID: 5, Text: "User lives in Berlin", Status: FactStatusSuperseded, Job: "edit"}},
		Downstream: DownstreamView{SMESections: []string{"About the user"}, Retrievals: 4},
	}
}

func newInspServer(f *inspFake) *Server { return New(Config{}, nil, nil, f, agent.NewBroker(0), nil) }

func TestFactInspectorJSON(t *testing.T) {
	f := &inspFake{detail: sampleDetail(), turns: []TurnView{{EpisodeID: 11, SessionID: "s", Events: []ProvenanceEventView{{Role: "user", Content: "I moved to Munich"}}}}}
	s := newInspServer(f)

	rec := memDo(s, http.MethodGet, "/api/memory/facts/7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail = %d %s", rec.Code, rec.Body)
	}
	var got FactDetailView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Fact.ID != 7 || got.Origin.Job != "distill" || len(got.Versions) != 2 || got.Downstream.Retrievals != 4 {
		t.Fatalf("detail = %+v", got)
	}
	if rec := memDo(s, http.MethodGet, "/api/memory/facts/7/turns", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "I moved to Munich") {
		t.Fatalf("turns = %d %s", rec.Code, rec.Body)
	}
	if rec := memDo(s, http.MethodPost, "/api/memory/facts/7/edit", `{"text":"  User lives in Hamburg "}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":99`) {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body)
	}
	if f.editedID != 7 || f.edits[0] != "User lives in Hamburg" {
		t.Fatalf("edit reached the control as %d %q", f.editedID, f.edits)
	}
	if rec := memDo(s, http.MethodPost, "/api/memory/facts/7/edit", `{"text":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty edit = %d", rec.Code)
	}
	if rec := memDo(s, http.MethodPost, "/api/memory/facts/7/edit", `{"txt":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	memDo(s, http.MethodPost, "/api/memory/facts/7/invalidate", "")
	memDo(s, http.MethodPost, "/api/memory/facts/7/revalidate", "")
	if !reflect.DeepEqual(f.invalidated, []int64{7}) || !reflect.DeepEqual(f.revalidated, []int64{7}) {
		t.Fatalf("invalidate/revalidate = %v %v", f.invalidated, f.revalidated)
	}
	if rec := memDo(s, http.MethodPost, "/api/memory/facts/7/restore-version", `{"version":5}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":100`) {
		t.Fatalf("restore-version = %d %s", rec.Code, rec.Body)
	}
	if !reflect.DeepEqual(f.restoredVer, []int64{5}) {
		t.Fatalf("restored %v", f.restoredVer)
	}
	// Without a version, the fact in the URL is the one made current again.
	if rec := memDo(s, http.MethodPost, "/api/memory/facts/7/restore-version", `{}`); rec.Code != http.StatusOK || !reflect.DeepEqual(f.restoredVer, []int64{5, 7}) {
		t.Fatalf("restore-version without a version = %d, restored %v", rec.Code, f.restoredVer)
	}
	if rec := memDo(s, http.MethodGet, "/api/memory/facts/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", rec.Code)
	}
	// The literal routes still win over {id}.
	if rec := memDo(s, http.MethodGet, "/api/memory/facts/removed", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"fact"`) {
		t.Fatalf("removed route shadowed: %d %s", rec.Code, rec.Body)
	}
}

func TestFactInspectorAbsent(t *testing.T) {
	s := newMemServer(&memFakeControl{facts: []FactView{{ID: 3, Text: "x"}}})
	if rec := memDo(s, http.MethodGet, "/api/memory/facts/3", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("GET detail without an inspector = %d", rec.Code)
	}
	rec := memDo(s, http.MethodGet, "/memory/facts/3", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "explain individual facts") {
		t.Fatalf("page without an inspector = %d\n%s", rec.Code, rec.Body)
	}
	if list := memDo(s, http.MethodGet, "/memory/facts", ""); strings.Contains(list.Body.String(), "Everything about it") {
		t.Fatal("the list links to an inspector that is not there")
	}
}

func TestFactPageRendersTheInspector(t *testing.T) {
	f := &inspFake{memFakeControl: memFakeControl{facts: []FactView{{ID: 3, Text: "User lives in Munich"}}}, detail: sampleDetail(), turns: []TurnView{
		{EpisodeID: 11, Events: []ProvenanceEventView{{Role: "user", Content: "I moved to Munich"}, {Role: "assistant", Content: "noted"}}},
		{EpisodeID: 12, Missing: true, Why: "the episode was deleted"},
	}}
	s := newInspServer(f)
	body := memDo(s, http.MethodGet, "/memory/facts/7", "").Body.String()
	for _, want := range []string{"User lives in Munich", "learned from a conversation · glm-5", "#7 · current", "· replaced · learned from a conversation",
		"Replaced <a href=\"/memory/facts/5\">#5</a> (your edit)", "Importance: 0.70", "Pinned: yes", "I moved to Munich",
		"no longer in memory", "A source was deleted: conversation #10, by a forget on " + time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC).Local().Format("2006-01-02") + ".", "Replaced", "User lives in Berlin", "About the user", "4 retrieval(s)",
		"Make this current again", "No longer true", `href="/memory/facts?resolve=7"`} {
		if !strings.Contains(body, want) {
			t.Errorf("fact page lacks %q", want)
		}
	}
	// One instant (learned, origin, last use) reads the same everywhere on the
	// page, in local time, whatever zone the store handed back.
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, want := range []string{"Learned " + now.Local().Format("2006-01-02 15:04"), "glm-5 · " + now.Local().Format("2006-01-02 15:04"),
		"4 retrieval(s), last " + now.Local().Format("2006-01-02 15:04")} {
		if !strings.Contains(body, want) {
			t.Errorf("fact page lacks %q (times in local time)", want)
		}
	}
	for _, internal := range []string{"superseded", "tombstoned", "distill ·"} {
		if strings.Contains(body, internal) {
			t.Errorf("fact page shows the internal word %q", internal)
		}
	}
	turns := memDo(s, http.MethodGet, "/memory/facts/7?turns=1", "").Body.String()
	if !strings.Contains(turns, "[assistant]") || !strings.Contains(turns, "the episode was deleted") {
		t.Fatalf("turns view:\n%s", turns)
	}
	cmp := memDo(s, http.MethodGet, "/memory/facts/7?compare=5,7", "").Body.String()
	if !strings.Contains(cmp, "<del>Berlin</del>") || !strings.Contains(cmp, "<ins>Munich</ins>") {
		t.Fatalf("compare view lacks the word diff:\n%s", cmp)
	}
	if ed := memDo(s, http.MethodGet, "/memory/facts/7?edit=1", "").Body.String(); !strings.Contains(ed, "<textarea") {
		t.Fatal("edit mode has no editor")
	}
	if list := memDo(s, http.MethodGet, "/memory/facts", ""); !strings.Contains(list.Body.String(), `href="/memory/facts/3">Everything about it`) {
		t.Fatalf("the facts list does not link to the inspector:\n%s", list.Body)
	}
}

// memDoForm posts an HTML form through the server's handler.
func memDoForm(s *Server, target string, form url.Values) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func TestFactPageForms(t *testing.T) {
	f := &inspFake{detail: sampleDetail()}
	s := newInspServer(f)
	rec := memDoForm(s, "/memory/facts/7/edit", url.Values{"text": {"User lives in Hamburg"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/memory/facts/99?") {
		t.Fatalf("edit form = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = memDoForm(s, "/memory/facts/7/edit", url.Values{"text": {"  "}})
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "edit=1") || !strings.Contains(loc, "err=") {
		t.Fatalf("empty edit redirect = %s", loc)
	}
	rec = memDoForm(s, "/memory/facts/7/restore-version", url.Values{"version": {"5"}})
	if !strings.HasPrefix(rec.Header().Get("Location"), "/memory/facts/100?") {
		t.Fatalf("restore-version redirect = %s", rec.Header().Get("Location"))
	}
	rec = memDoForm(s, "/memory/facts/valid", url.Values{"fact": {"7"}, "valid": {"false"}})
	if loc := rec.Header().Get("Location"); !reflect.DeepEqual(f.invalidated, []int64{7}) || loc != "/memory/facts/7?msg=Marked+no+longer+true." {
		t.Fatalf("invalidated %v, then to %s", f.invalidated, loc)
	}
	memDoForm(s, "/memory/facts/valid", url.Values{"fact": {"7"}, "valid": {"true"}})
	for _, v := range []string{"", "yes", "TRUE"} {
		if rec := memDoForm(s, "/memory/facts/valid", url.Values{"fact": {"7"}, "valid": {v}}); rec.Code != http.StatusBadRequest {
			t.Errorf("valid=%q: %d, want 400", v, rec.Code)
		}
	}
	if !reflect.DeepEqual(f.invalidated, []int64{7}) || !reflect.DeepEqual(f.revalidated, []int64{7}) {
		t.Fatalf("invalidated %v, revalidated %v", f.invalidated, f.revalidated)
	}
	f.actErr = errors.New("boom")
	for valid, want := range map[string]string{
		"false": "Couldn't mark that fact no longer true.",
		"true":  "Couldn't mark that fact true again.",
	} {
		rec := memDoForm(s, "/memory/facts/valid", url.Values{"fact": {"7"}, "valid": {valid}})
		// The plain message only: the store's own error stays in the log (D21).
		if loc, _ := url.QueryUnescape(rec.Header().Get("Location")); !strings.Contains(loc, want) || strings.Contains(loc, "boom") {
			t.Errorf("failed valid=%s redirect = %s", valid, loc)
		}
	}
}

// Viewing an older version: it can't be edited, the current version isn't
// offered as something to make current, and comparisons read oldest first.
func TestFactPageOfAnOlderVersion(t *testing.T) {
	det := sampleDetail()
	det.Fact.Status, det.Fact.Text = FactStatusSuperseded, "User lives in Berlin"
	det.Replaced = nil
	det.ReplacedBy = &VersionView{ID: 7, Text: "User lives in Munich", Status: FactStatusLive, Job: "edit"}
	s := newInspServer(&inspFake{detail: det})
	body := memDo(s, http.MethodGet, "/memory/facts/5", "").Body.String()
	if strings.Contains(body, "Make this current again") {
		t.Error("the current version is offered as something to make current")
	}
	if !strings.Contains(body, "compare=5%2C7") {
		t.Errorf("the compare link isn't oldest first:\n%s", body)
	}
	if !strings.Contains(body, `Replaced by <a href="/memory/facts/7">#7</a> (your edit)`) {
		t.Error("the newer version isn't linked")
	}
	if strings.Contains(memDo(s, http.MethodGet, "/memory/facts/5?edit=1", "").Body.String(), "<textarea") {
		t.Error("an older version opened the editor")
	}
	if strings.Contains(memDo(s, http.MethodGet, "/memory/facts/5?compare=5,5", "").Body.String(), "#5 → #5") {
		t.Error("compared a version with itself")
	}
}

func TestWordDiff(t *testing.T) {
	got := wordDiff("User lives in Berlin now", "User lives in Munich now")
	want := []diffPart{{"User lives in", "same"}, {"Berlin", "del"}, {"Munich", "ins"}, {"now", "same"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wordDiff = %+v", got)
	}
	if got := wordDiff("", "new"); len(got) != 1 || got[0].Op != "ins" {
		t.Fatalf("diff from empty = %+v", got)
	}
}
