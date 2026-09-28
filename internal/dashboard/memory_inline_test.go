package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveMem is fakeMem behind a lock that can also explain facts, and mark
// them no longer true: what a browser test's server and the test share.
// changes lists every change it got ("delete 3"); while fail is set each
// change fails with a store error that must stay off the page.
type liveMem struct {
	mu      sync.Mutex
	m       *fakeMem
	invalid map[int64]bool
	changes []string
	fail    bool
}

const rawMemLocked = "memory store is locked by another writer"

func newLiveMem() *liveMem { return &liveMem{m: newFakeMem(), invalid: map[int64]bool{}} }

func (l *liveMem) change(what string, id any, fn func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.changes = append(l.changes, fmt.Sprint(what, " ", id))
	if l.fail {
		return errors.New(rawMemLocked)
	}
	return fn()
}

func (l *liveMem) setFail(on bool) { l.mu.Lock(); l.fail = on; l.mu.Unlock() }

func (l *liveMem) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.changes...)
}

func (l *liveMem) Soul() (SoulView, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.m.Soul() }
func (l *liveMem) SoulEdit(op SoulEditOp) error {
	return l.change("soul "+op.Action, op.ID, func() error { return l.m.SoulEdit(op) })
}
func (l *liveMem) Facts(q string, n int, c string) ([]FactView, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.Facts(q, n, c)
}
func (l *liveMem) FactProvenance(id int64) ([]EpisodeView, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.FactProvenance(id)
}
func (l *liveMem) ResolveFacts(k, d int64) error {
	return l.change("resolve", k, func() error { return l.m.ResolveFacts(k, d) })
}
func (l *liveMem) DeleteFact(id int64) error {
	return l.change("delete", id, func() error { return l.m.DeleteFact(id) })
}
func (l *liveMem) RestoreFact(id int64) error {
	return l.change("restore", id, func() error { return l.m.RestoreFact(id) })
}
func (l *liveMem) RemovedFacts(n int) ([]FactView, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.RemovedFacts(n)
}
func (l *liveMem) TemporalFacts() ([]TemporalFactView, MemStats, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m.TemporalFacts()
}
func (l *liveMem) WorkingCount() int            { return 0 }
func (l *liveMem) UserProfile() (string, error) { return "", nil }
func (l *liveMem) ResyncUserProfile() error     { return nil }
func (l *liveMem) CompactionProvenance() (*CompactionProvenanceView, error) {
	return nil, nil
}

func (l *liveMem) FactDetail(id int64) (*FactDetailView, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.m.byID[id]
	if f == nil {
		return nil, fmt.Errorf("no fact %d", id)
	}
	status := FactStatusLive
	if l.m.tombstoned[id] {
		status = FactStatusTombstoned
	}
	d := &FactDetailView{
		Fact:     TemporalFactView{ID: id, Text: f.text, Confidence: f.conf, EffectiveConfidence: f.conf, FirstSeen: f.first, LastConfirmed: f.last, Status: status},
		Versions: []VersionView{{ID: id, Text: f.text, Status: status, FirstSeen: time.Unix(f.first, 0)}},
	}
	if l.invalid[id] {
		gone := time.Unix(f.last, 0)
		d.Signals.ValidTo = &gone
	}
	return d, nil
}
func (l *liveMem) InvalidateFact(id int64) error {
	return l.change("invalidate", id, func() error { l.invalid[id] = true; return nil })
}
func (l *liveMem) RevalidateFact(id int64) error {
	return l.change("revalidate", id, func() error { delete(l.invalid, id); return nil })
}
func (l *liveMem) EditFact(int64, string) (int64, error)   { return 0, errors.New("not in this fake") }
func (l *liveMem) RestoreFactVersion(int64) (int64, error) { return 0, errors.New("not in this fake") }
func (l *liveMem) FactTurns(int64) ([]TurnView, error)     { return nil, nil }

// memInline is one in-place memory form: where it posts, what it sends, and
// the region of the page it's on.
type memInline struct {
	path   string
	form   url.Values
	region string
}

var memInlineForms = []memInline{
	{"/memory/soul/edit", url.Values{"section": {"user_fact"}, "action": {"edit"}, "item": {"uf-1"}, "text": {"Prefers imperial units."}}, "mem-soul-body"},
	{"/memory/facts/delete", url.Values{"fact": {"3"}, "back": {"/memory/facts?q=lives"}}, "mem-facts-body"},
	{"/memory/facts/delete", url.Values{"fact": {"1"}, "back": {"/memory/facts/1"}}, "mem-fact-body"},
	{"/memory/facts/restore", url.Values{"fact": {"1"}, "back": {"/memory/facts/removed"}}, "mem-removed-body"},
	{"/memory/facts/restore", url.Values{"fact": {"1"}, "back": {"/memory/facts"}}, "mem-facts-body"},
	{"/memory/facts/restore", url.Values{"fact": {"1"}, "back": {"/memory/facts/1"}}, "mem-fact-body"},
	{"/memory/facts/valid", url.Values{"fact": {"1"}, "valid": {"false"}, "back": {"/memory/facts/1?turns=1"}}, "mem-fact-body"},
	{"/memory/facts/valid", url.Values{"fact": {"1"}, "valid": {"true"}}, "mem-fact-body"},
}

// Every memory form that answers in place does so for a Datastar post: the
// toast, the status strip and its page's region, never a 303 the fetch would
// follow; with no memory the region says so. A plain post still gets the 303.
func TestMemory_InlineAnswersEveryForm(t *testing.T) {
	for _, f := range memInlineForms {
		for _, mem := range []MemoryControl{newLiveMem(), nil} {
			s := New(Config{}, nil, nil, mem, nil, nil)
			rec := sendForm(s.Handler(), http.MethodPost, f.path, f.form, datastar)
			body := rec.Body.String()
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Location") != "" {
				t.Errorf("%s to %s (memory %v): %d %q to %q, want 200 text/event-stream", f.path, f.region, mem != nil, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Location"))
			}
			for _, want := range []string{"data: mode replace\ndata: elements <div id=\"toast\"", `data: elements <div id="ui-status"`, `data: elements <div id="` + f.region + `">`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s to %s (memory %v) is missing %q:\n%s", f.path, f.region, mem != nil, want, body)
				}
			}
			if mem == nil && !strings.Contains(body, "Memory isn&#39;t available") {
				t.Errorf("%s with no memory: the region doesn't say so:\n%s", f.path, body)
			}
			if rec := sendForm(s.Handler(), http.MethodPost, f.path, f.form, nil); rec.Code != http.StatusSeeOther {
				t.Errorf("a plain %s (memory %v): %d, want 303", f.path, mem != nil, rec.Code)
			}
		}
	}
}

// What each in-place answer shows: the toast says what changed, and the
// region shows it in the view the form was on. An error is an alert with the
// plain message, and leaves the region alone, so an open step keeps what was
// typed in it.
func TestMemory_InlineChanges(t *testing.T) {
	c := installCapture(t)
	lm := newLiveMem()
	s := New(Config{}, nil, nil, lm, nil, nil)
	post := func(path string, f url.Values) string {
		t.Helper()
		return sendForm(s.Handler(), http.MethodPost, path, f, datastar).Body.String()
	}
	toast := func(msg string) string {
		return `<div id="toast" class="toast ok"><span class="toast-text">` + msg + `</span>`
	}
	for _, tc := range []struct {
		path      string
		form      url.Values
		want, not []string
	}{
		{"/memory/soul/edit", url.Values{"section": {"user_fact"}, "action": {"edit"}, "item": {"uf-1"}, "text": {"Prefers imperial units."}},
			[]string{toast("Saved."), `<div class="soul-text">Prefers imperial units.</div>`}, []string{`<div class="soul-text">Prefers metric units.</div>`}},
		{"/memory/soul/edit", url.Values{"section": {"instruction"}, "action": {"add"}, "text": {"Answer in German."}},
			[]string{toast("Added."), "Answer in German.", `id="soul-add-instruction-2"`}, nil},
		// From the list, searching: the fact goes, the search stays, and the
		// Undo banner brings it back to the same view.
		{"/memory/facts/delete", url.Values{"fact": {"1"}, "back": {"/memory/facts?q=lives"}},
			[]string{toast("Fact removed."), `<span>Fact removed.</span>`, `<input type="hidden" name="fact" value="1">`, `<input type="hidden" name="back" value="/memory/facts?q=lives">`, "User lives in Munich.", `value="lives"`},
			[]string{"User lives in Berlin.", "Project deadline"}},
		{"/memory/facts/restore", url.Values{"fact": {"1"}, "back": {"/memory/facts?q=lives"}},
			[]string{toast("Fact restored."), "User lives in Berlin."}, []string{"banner undo"}},
		// On the fact's own page: it shows removed, with Restore.
		{"/memory/facts/delete", url.Values{"fact": {"3"}, "back": {"/memory/facts/3"}},
			[]string{toast("Fact removed."), "#3 · removed", `action="/memory/facts/restore"`}, []string{"No longer true"}},
		{"/memory/facts/restore", url.Values{"fact": {"3"}, "back": {"/memory/facts/3"}},
			[]string{toast("Fact restored."), "#3 · current", "No longer true"}, nil},
		{"/memory/facts/valid", url.Values{"fact": {"3"}, "valid": {"false"}, "back": {"/memory/facts/3"}},
			[]string{toast("Marked no longer true."), "It's true again", `<input type="hidden" name="valid" value="true">`, "No longer true since"}, nil},
		{"/memory/facts/delete", url.Values{"fact": {"2"}, "back": {"/memory/facts"}}, []string{toast("Fact removed.")}, nil},
		{"/memory/facts/restore", url.Values{"fact": {"2"}, "back": {"/memory/facts/removed"}},
			[]string{toast("Fact restored."), "Nothing here."}, []string{"User lives in Munich."}},
	} {
		body := post(tc.path, tc.form)
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s %v: the answer lacks %q:\n%s", tc.path, tc.form, w, body)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(body, n) {
				t.Errorf("%s %v: the answer still has %q:\n%s", tc.path, tc.form, n, body)
			}
		}
	}

	lm.setFail(true)
	for _, f := range memInlineForms {
		body := post(f.path, f.form)
		if !strings.Contains(body, `<div id="toast" class="toast err" role="alert"><span class="toast-text">Couldn&#39;t `) || strings.Contains(body, rawMemLocked) {
			t.Errorf("a failed %s: want the plain alert and not the store's words:\n%s", f.path, body)
		}
		if strings.Contains(body, `id="`+f.region+`"`) {
			t.Errorf("a failed %s re-rendered the page, which closes an open step:\n%s", f.path, body)
		}
	}
	if !c.has("2201 ", "action=fact delete") {
		t.Errorf("the failure isn't logged as ActionFailed: %q", c.lines())
	}
	lm.setFail(false)

	// A plain post: the 303 and its flash, on the view it came from.
	for path, want := range map[string]string{
		"/memory/facts/delete?fact=1&back=%2Fmemory%2Ffacts%3Fq%3Dlives": "/memory/facts?q=lives&removed=1&msg=Fact+removed.",
		"/memory/facts/restore?fact=1&back=%2Fmemory%2Ffacts%2F1":        "/memory/facts/1?msg=Fact+restored.",
		"/memory/facts/valid?fact=1&valid=false":                         "/memory/facts/1?msg=Marked+no+longer+true.",
		"/memory/facts/delete?fact=1&back=https%3A%2F%2Fevil.example":    "/memory/facts?removed=1&msg=Fact+removed.",
	} {
		if loc := sendForm(s.Handler(), http.MethodPost, path, nil, nil).Header().Get("Location"); loc != want {
			t.Errorf("a plain %s went to %q, want %q", path, loc, want)
		}
	}
	if rec := sendForm(s.Handler(), http.MethodPost, "/memory/facts/delete", url.Values{"fact": {"x"}}, datastar); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed id: %d, want 400", rec.Code)
	}
}
