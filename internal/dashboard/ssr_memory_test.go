package dashboard

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeMem is a stateful MemoryControl for the SSR memory-page tests: mutations
// actually change what subsequent reads return, so the "303 + change state"
// DoD can be asserted end-to-end.
type fmFact struct {
	id          int64
	text        string
	conf        float64
	first, last int64
}

type fakeMem struct {
	persona      string
	userFacts    []SoulItem
	instructions []SoulItem
	byID         map[int64]*fmFact
	order        []int64
	tombstoned   map[int64]bool
	supersededBy map[int64]int64
	prov         map[int64][]EpisodeView
	compProv     *CompactionProvenanceView
	working      int
	seq          int
}

func newFakeMem() *fakeMem {
	m := &fakeMem{
		persona:      "Tenant is a careful, terse operator's agent.",
		userFacts:    []SoulItem{{ID: "uf-1", Text: "Prefers metric units."}},
		instructions: []SoulItem{{ID: "in-1", Text: "Always cite sources."}},
		byID:         map[int64]*fmFact{},
		tombstoned:   map[int64]bool{},
		supersededBy: map[int64]int64{},
		prov:         map[int64][]EpisodeView{},
		working:      7,
	}
	m.addFact(1, "User lives in Berlin.", 0.91)
	m.addFact(2, "User lives in Munich.", 0.60) // conflicts with 1
	m.addFact(3, "Project deadline is May 30.", 0.80)
	m.prov[1] = []EpisodeView{{ID: 11, Prompt: "where do I live?", Response: "You said Berlin.", Timestamp: time.Unix(1_700_000_000, 0)}}
	m.prov[2] = []EpisodeView{{ID: 12, Missing: true}}
	return m
}

func (m *fakeMem) addFact(id int64, text string, conf float64) {
	m.byID[id] = &fmFact{id: id, text: text, conf: conf, first: 1_700_000_000, last: 1_700_400_000}
	m.order = append(m.order, id)
}

func (m *fakeMem) Soul() (SoulView, error) {
	return SoulView{
		Persona:      m.persona,
		UserFacts:    append([]SoulItem(nil), m.userFacts...),
		Instructions: append([]SoulItem(nil), m.instructions...),
	}, nil
}

func (m *fakeMem) SoulEdit(op SoulEditOp) error {
	var list *[]SoulItem
	switch op.Section {
	case SoulSectionUserFact:
		list = &m.userFacts
	case SoulSectionInstruction:
		list = &m.instructions
	default:
		return fmt.Errorf("unknown section %q", op.Section)
	}
	switch op.Action {
	case SoulActionAdd:
		if strings.TrimSpace(op.Text) == "" {
			return fmt.Errorf("empty text")
		}
		m.seq++
		*list = append(*list, SoulItem{ID: fmt.Sprintf("s%d", m.seq), Text: op.Text})
	case SoulActionEdit:
		for i := range *list {
			if (*list)[i].ID == op.ID {
				(*list)[i].Text = op.Text
				return nil
			}
		}
		return fmt.Errorf("no such item %q", op.ID)
	case SoulActionRemove:
		for i := range *list {
			if (*list)[i].ID == op.ID {
				*list = append((*list)[:i], (*list)[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("no such item %q", op.ID)
	default:
		return fmt.Errorf("unknown action %q", op.Action)
	}
	return nil
}

func (m *fakeMem) live(id int64) bool { return !m.tombstoned[id] && m.supersededBy[id] == 0 }

func (m *fakeMem) Facts(q string, _ int, _ string) ([]FactView, string, error) {
	var out []FactView
	for _, id := range m.order {
		if !m.live(id) {
			continue
		}
		f := m.byID[id]
		if q != "" && !strings.Contains(strings.ToLower(f.text), strings.ToLower(q)) {
			continue
		}
		out = append(out, FactView{ID: f.id, Text: f.text, Confidence: f.conf})
	}
	return out, "", nil
}

func (m *fakeMem) FactProvenance(id int64) ([]EpisodeView, error) {
	if m.byID[id] == nil {
		return nil, fmt.Errorf("no such fact %d", id)
	}
	return m.prov[id], nil
}

func (m *fakeMem) ResolveFacts(keepID, discardID int64) error {
	if m.byID[keepID] == nil || m.byID[discardID] == nil {
		return fmt.Errorf("bad ids %d/%d", keepID, discardID)
	}
	m.supersededBy[discardID] = keepID
	return nil
}

func (m *fakeMem) DeleteFact(id int64) error {
	if m.byID[id] == nil {
		return fmt.Errorf("no such fact %d", id)
	}
	m.tombstoned[id] = true
	return nil
}

func (m *fakeMem) RestoreFact(id int64) error {
	if m.byID[id] == nil {
		return fmt.Errorf("no such fact %d", id)
	}
	m.tombstoned[id] = false
	return nil
}

func (m *fakeMem) RemovedFacts(_ int) ([]FactView, error) {
	var out []FactView
	for _, id := range m.order {
		if m.tombstoned[id] {
			f := m.byID[id]
			out = append(out, FactView{ID: f.id, Text: f.text, Confidence: f.conf})
		}
	}
	return out, nil
}

func (m *fakeMem) TemporalFacts() ([]TemporalFactView, MemStats, error) {
	var out []TemporalFactView
	var st MemStats
	for _, id := range m.order {
		f := m.byID[id]
		status := FactStatusLive
		switch {
		case m.tombstoned[id]:
			status = FactStatusTombstoned
			st.Tombstoned++
		case m.supersededBy[id] != 0:
			status = FactStatusSuperseded
			st.Superseded++
		default:
			st.Live++
		}
		st.Total++
		out = append(out, TemporalFactView{
			ID: f.id, Text: f.text, Confidence: f.conf, EffectiveConfidence: f.conf,
			FirstSeen: f.first, LastConfirmed: f.last, SupersededBy: m.supersededBy[id],
			Tombstoned: m.tombstoned[id], Status: status,
		})
	}
	return out, st, nil
}

func (m *fakeMem) WorkingCount() int            { return m.working }
func (m *fakeMem) UserProfile() (string, error) { return "profile", nil }
func (m *fakeMem) ResyncUserProfile() error     { return nil }
func (m *fakeMem) CompactionProvenance() (*CompactionProvenanceView, error) {
	return m.compProv, nil
}

func memServer() (*Server, *fakeMem) {
	fm := newFakeMem()
	return New(Config{}, nil, &fakeTools{}, fm, nil, nil), fm
}

// --- page renders ----------------------------------------------------------

func TestSSR_MemoryOverview(t *testing.T) {
	s, _ := memServer()
	rec := get(t, s, "/memory")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Working memory", "careful, terse", `href="/memory"`, "Live", "Superseded"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
}

func TestSSR_MemorySoulRenders(t *testing.T) {
	s, _ := memServer()
	body := get(t, s, "/memory/soul").Body.String()
	for _, want := range []string{"Prefers metric units.", "Always cite sources.", "careful, terse", "Add", "Edit", "Remove"} {
		if !strings.Contains(body, want) {
			t.Errorf("soul page missing %q", want)
		}
	}
	// The edit endpoint only appears inside a form; opening the add affordance
	// (?add=) should render that form posting to soul/edit with action=add.
	add := get(t, s, "/memory/soul?add=user_fact").Body.String()
	for _, want := range []string{`action="/memory/soul/edit"`, `name="action" value="add"`, `value="user_fact"`} {
		if !strings.Contains(add, want) {
			t.Errorf("soul add form missing %q", want)
		}
	}
}

func TestSSR_MemoryFactsRenders(t *testing.T) {
	s, _ := memServer()
	body := get(t, s, "/memory/facts").Body.String()
	for _, want := range []string{"User lives in Berlin.", "Why does the agent know this?", "Resolve a conflict", "Remove"} {
		if !strings.Contains(body, want) {
			t.Errorf("facts page missing %q", want)
		}
	}
}

func TestSSR_MemoryFactsSearch(t *testing.T) {
	s, _ := memServer()
	body := get(t, s, "/memory/facts?q=Munich").Body.String()
	if !strings.Contains(body, "Munich") {
		t.Error("search should keep the matching fact")
	}
	if strings.Contains(body, "Project deadline") {
		t.Error("search should drop non-matching facts")
	}
}

func TestSSR_MemoryFactsProvenance(t *testing.T) {
	s, _ := memServer()
	body := get(t, s, "/memory/facts?expand=1").Body.String()
	for _, want := range []string{"where do I live?", "You said Berlin.", "Hide"} {
		if !strings.Contains(body, want) {
			t.Errorf("expanded provenance missing %q", want)
		}
	}
}

func TestSSR_MemoryTemporal(t *testing.T) {
	s, _ := memServer()
	body := get(t, s, "/memory/facts/temporal").Body.String()
	for _, want := range []string{"<svg", "transaction time only", "First learned", "Project deadline is May 30."} {
		if !strings.Contains(body, want) {
			t.Errorf("temporal page missing %q", want)
		}
	}
}

func TestSSR_MemoryRemovedEmpty(t *testing.T) {
	s, _ := memServer()
	body := get(t, s, "/memory/facts/removed").Body.String()
	if !strings.Contains(body, "Nothing here") {
		t.Error("removed page should show its empty state when nothing is tombstoned")
	}
}

// --- mutations: 303 + observable state change ------------------------------

func TestSSR_MemorySoulAdd(t *testing.T) {
	s, fm := memServer()
	rec := ssrPost(t, s, "/memory/soul/edit", url.Values{
		"section": {"user_fact"}, "action": {"add"}, "text": {"Likes dark mode."},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/memory/soul" {
		t.Fatalf("soul add should 303 to /memory/soul, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(fm.userFacts) != 2 || fm.userFacts[1].Text != "Likes dark mode." {
		t.Fatalf("soul add did not change state: %+v", fm.userFacts)
	}
	if !strings.Contains(get(t, s, "/memory/soul").Body.String(), "Likes dark mode.") {
		t.Error("added fact should render on the soul page")
	}
}

func TestSSR_MemorySoulRemove(t *testing.T) {
	s, fm := memServer()
	rec := ssrPost(t, s, "/memory/soul/edit", url.Values{
		"section": {"instruction"}, "action": {"remove"}, "id": {"in-1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("soul remove should 303, got %d", rec.Code)
	}
	if len(fm.instructions) != 0 {
		t.Errorf("soul remove did not change state: %+v", fm.instructions)
	}
}

func TestSSR_MemorySoulEditBadSection(t *testing.T) {
	s, _ := memServer()
	rec := ssrPost(t, s, "/memory/soul/edit", url.Values{
		"section": {"nonsense"}, "action": {"add"}, "text": {"x"},
	})
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/memory/soul?err=") {
		t.Fatalf("bad soul edit should 303 back with ?err=, got %d %q", rec.Code, loc)
	}
}

func TestSSR_MemoryFactDeleteThenRestore(t *testing.T) {
	s, fm := memServer()
	rec := ssrPost(t, s, "/memory/facts/1/delete", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/memory/facts?removed=1" {
		t.Fatalf("delete should 303 to ?removed=1, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if !fm.tombstoned[1] {
		t.Fatal("delete did not tombstone fact 1")
	}
	if strings.Contains(get(t, s, "/memory/facts").Body.String(), "User lives in Berlin.") {
		t.Error("tombstoned fact should drop out of the live list")
	}
	if !strings.Contains(get(t, s, "/memory/facts/removed").Body.String(), "User lives in Berlin.") {
		t.Error("tombstoned fact should appear under Recently removed")
	}
	// Restore via the removed page.
	rrec := ssrPost(t, s, "/memory/facts/1/restore", url.Values{"back": {"/memory/facts/removed"}})
	if rrec.Code != http.StatusSeeOther || rrec.Header().Get("Location") != "/memory/facts/removed" {
		t.Fatalf("restore should 303 back to removed, got %d %q", rrec.Code, rrec.Header().Get("Location"))
	}
	if fm.tombstoned[1] {
		t.Error("restore did not un-tombstone fact 1")
	}
}

func TestSSR_MemoryResolve(t *testing.T) {
	s, fm := memServer()
	rec := ssrPost(t, s, "/memory/facts/resolve", url.Values{
		"keep_id": {"1"}, "discard_id": {"2"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/memory/facts" {
		t.Fatalf("resolve should 303 to facts, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if fm.supersededBy[2] != 1 {
		t.Fatalf("resolve should record 2 superseded by 1, got %v", fm.supersededBy)
	}
	if strings.Contains(get(t, s, "/memory/facts").Body.String(), "User lives in Munich.") {
		t.Error("the discarded fact should leave the live list")
	}
}

func TestSSR_MemoryResolvePicker(t *testing.T) {
	s, _ := memServer()
	// First pick A=1 selected, others offer "this conflicts with it".
	one := get(t, s, "/memory/facts?resolve=1").Body.String()
	if !strings.Contains(one, "Selected") || !strings.Contains(one, "This conflicts with it") {
		t.Error("resolve mode should pin A and offer conflict selection on others")
	}
	// Both picked → keep-which card with both fact texts.
	both := get(t, s, "/memory/facts?resolve=1&with=2").Body.String()
	for _, want := range []string{"Keep which fact?", "Keep this one, remove the other", "User lives in Berlin.", "User lives in Munich."} {
		if !strings.Contains(both, want) {
			t.Errorf("resolve card missing %q", want)
		}
	}
}

// --- nil-safety + SVG unit -------------------------------------------------

func TestSSR_MemoryNilSafe(t *testing.T) {
	s := New(Config{}, nil, &fakeTools{}, nil, nil, nil) // no MemoryControl
	for _, p := range []string{"/memory", "/memory/soul", "/memory/facts", "/memory/facts/removed", "/memory/facts/temporal"} {
		rec := get(t, s, p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s want 200 on nil mem, got %d", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "isn't configured") {
			t.Errorf("%s should render the not-configured state", p)
		}
	}
	// Mutations must not panic with a nil store; they just 303.
	if rec := ssrPost(t, s, "/memory/facts/1/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Errorf("delete on nil mem should 303, got %d", rec.Code)
	}
}

func TestRenderTemporalSVG(t *testing.T) {
	if renderTemporalSVG(nil) != "" {
		t.Error("empty fact set should render no SVG")
	}
	out := string(renderTemporalSVG([]TemporalFactView{
		{ID: 1, Text: "<b>danger</b>", Status: FactStatusLive, FirstSeen: 100, LastConfirmed: 200, EffectiveConfidence: 0.9},
		{ID: 2, Text: "gone", Status: FactStatusTombstoned, FirstSeen: 150, LastConfirmed: 250, EffectiveConfidence: 0.2},
	}))
	if !strings.Contains(out, "<svg") || !strings.Contains(out, "</svg>") {
		t.Error("expected an <svg> document")
	}
	// Full-width responsiveness (TEN-112): the svg element fills its container
	// (width=100% + viewBox, height derived via CSS) and must NOT pin a fixed
	// pixel height, which would letterbox it to a tiny centered box.
	open := out[strings.Index(out, "<svg") : strings.Index(out, ">")+1]
	if !strings.Contains(open, `width="100%"`) || !strings.Contains(open, "viewBox=") {
		t.Errorf("svg should be responsive (width=100%% + viewBox): %q", open)
	}
	if strings.Contains(open, "height=") {
		t.Errorf("svg must not pin a fixed height (breaks full-width scaling): %q", open)
	}
	if !strings.Contains(out, "&lt;b&gt;danger&lt;/b&gt;") {
		t.Errorf("SVG text nodes must be html-escaped:\n%s", out)
	}
	if strings.Contains(out, "<b>danger</b>") {
		t.Error("raw fact text must not appear unescaped in the SVG")
	}
}
