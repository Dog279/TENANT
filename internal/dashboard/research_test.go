package dashboard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
)

// fakeResearch is a ResearchControl the test drives: set replaces the live
// view and wakes every Watch, as the adapter's bump does.
type fakeResearch struct {
	mu       sync.Mutex
	live     ResearchLiveView
	changed  chan struct{}
	rows     []ResearchRunRow
	report   ResearchReportView
	err      error // History and Report fail with it
	startErr error
	stopOK   bool
	calls    []string
}

func newFakeResearch() *fakeResearch { return &fakeResearch{changed: make(chan struct{})} }

func (f *fakeResearch) Watch() (ResearchLiveView, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live, f.changed
}

func (f *fakeResearch) set(v ResearchLiveView) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = v
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeResearch) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeResearch) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeResearch) Start(q string, skip bool) error {
	f.record(fmt.Sprintf("start %q %v", q, skip))
	return f.startErr
}
func (f *fakeResearch) Clarify(a string) error { f.record(fmt.Sprintf("clarify %q", a)); return nil }
func (f *fakeResearch) Replay(id string) error { f.record("replay " + id); return nil }
func (f *fakeResearch) Stop() bool             { f.record("stop"); return f.stopOK }
func (f *fakeResearch) History(int) ([]ResearchRunRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows), f.err
}
func (f *fakeResearch) Report(string) (ResearchReportView, error) { return f.report, f.err }
func (f *fakeResearch) Delete(id string) error {
	f.record("delete " + id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = slices.DeleteFunc(f.rows, func(r ResearchRunRow) bool { return r.ID == id })
	return nil
}

// The page in the harness: populated is a run in progress over a long list
// and a long report; failing is a failed run whose history and report reads
// fail with the raw store text.
func init() {
	harnessFakes = append(harnessFakes, func(s *Server, mode fakeMode) { s.SetResearch(harnessResearch(mode)) })
	harnessPages = append(harnessPages, "/research/r-1")
}

func harnessResearch(mode fakeMode) *fakeResearch {
	f := newFakeResearch()
	if mode == failing {
		f.err = errors.New(rawNoHistory)
		f.live = ResearchLiveView{Phase: "failed", Question: longName(1), Err: "Research stopped because of an error. The full log has the details.",
			Lines: []string{"Couldn't save the report to your agent's notes."}}
		return f
	}
	var qs, lines []string
	for i := range 12 {
		qs = append(qs, longName(i)+" "+longURL(i))
		lines = append(lines, "A helper finished: "+longURL(i))
		f.rows = append(f.rows, ResearchRunRow{ID: fmt.Sprintf("r-%d", i), Question: longName(i), State: "done", StateText: "Finished",
			When: "Sep 27, 14:05", Took: "4 min", Model: longName(i + 30), ReplayOf: map[bool]string{true: "r-0"}[i%3 == 1], Sources: i, Live: i == 0})
		f.report.Refs = append(f.report.Refs, ResearchRef{N: i + 1, URL: longURL(i)})
		f.report.Findings = append(f.report.Findings, ResearchFinding{Question: longName(i), State: "Finished", Body: longURL(i)})
	}
	f.report.Refs = append(f.report.Refs, ResearchRef{N: 13, URL: "javascript:alert(1)"})
	f.report.ID, f.report.Question, f.report.StateText, f.report.When, f.report.Took, f.report.Model = "r-1", longName(1), "Finished", "Sep 27, 14:05", "4 min", longName(40)
	f.report.Body = "# " + longName(2) + "\n" + strings.Repeat("w", 300) + " [1] [12]\n\n" + longURL(3)
	f.live = ResearchLiveView{SavesToNotes: true, HistoryOn: true, Phase: "researching", Running: true, RunID: "r-0", Question: longName(0),
		Round: 1, Rounds: 2, Questions: qs, Progress: "Questions 1-3 are in progress.", Elapsed: "4 min", Lines: lines}
	return f
}

func researchServer(f *fakeResearch) *Server {
	s := New(Config{}, nil, nil, nil, agent.NewBroker(0), nil)
	if f != nil {
		s.SetResearch(f)
	}
	return s
}

func postResearch(t *testing.T, s *Server, path string, vals url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, postForm(path, vals))
	return rec
}

// flashOf is the flash a 303 carries: "msg: …" or "err: …".
func flashOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || u.Path != "/research" {
		t.Fatalf("redirect to %q, want /research", rec.Header().Get("Location"))
	}
	if e := u.Query().Get("err"); e != "" {
		return "err: " + e
	}
	if m := u.Query().Get("msg"); m != "" {
		return "msg: " + m
	}
	return ""
}

func TestResearch_NilControl(t *testing.T) {
	s := researchServer(nil)
	for _, p := range []string{"/research", "/research/abc"} {
		if rec := get(t, s, p); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Research isn&#39;t available on this Tenant.") {
			t.Errorf("GET %s = %d, want 200 and the off card", p, rec.Code)
		}
	}
	if rec := get(t, s, "/research/live"); rec.Code != http.StatusNoContent {
		t.Errorf("GET /research/live = %d, want 204", rec.Code)
	}
	for _, p := range []string{"/research/start", "/research/clarify", "/research/stop", "/research/replay", "/research/delete"} {
		if got := flashOf(t, postResearch(t, s, p, url.Values{"question": {"q"}, "id": {"abc"}, "confirm": {"yes"}})); got != "" {
			t.Errorf("POST %s flashed %q, want a plain redirect", p, got)
		}
	}
}

// Every state the page can be in (DESIGN.md §6), each in plain words.
func TestResearchPage_States(t *testing.T) {
	report := &ResearchReportView{ID: "r-1", Question: "What is fakeness?", StateText: "Finished", Body: "Answer [1].", Refs: []ResearchRef{{N: 1, URL: "https://example.com/a"}}}
	cases := []struct {
		name string
		path string
		set  func(f *fakeResearch, s *Server)
		want []string
		not  []string
	}{
		{name: "off", path: "/research", set: nil, want: []string{`class="card state-off"`}},
		{name: "empty", path: "/research", want: []string{`class="empty"`, "No research yet", `id="research-live">`, "The report is kept in the list below.", `<details class="advanced" data-preserve-attr="open">`},
			not: []string{`class="busy"`}},
		{name: "saves to notes", path: "/research", set: func(f *fakeResearch, _ *Server) { f.live.SavesToNotes = true },
			want: []string{"saved to your agent's notes so it can use what it learned later"}},
		{name: "web off", path: "/research", set: func(f *fakeResearch, _ *Server) { f.live.WebOff = true },
			want: []string{"Web browsing is turned off, so research can only use what your agent already has in its notes and memory."}},
		{name: "web off on a service", path: "/research", set: func(f *fakeResearch, _ *Server) {
			f.live.WebOff, f.live.WebOffWhy = true, "Web browsing is off for research on this server because the browser would have full control of the computer."
		}, want: []string{"because the browser would have full control of the computer"}, not: []string{"Web browsing is turned off"}},
		{name: "loading, checking", path: "/research", set: func(f *fakeResearch, _ *Server) {
			f.live = ResearchLiveView{Phase: "checking", Running: true, Question: "What is fakeness?", Elapsed: "less than a minute"}
		}, want: []string{`class="loading">Checking your question`, `<div class="busy" role="status">`, "Research is running", "less than a minute", `action="/research/stop"`}},
		{name: "loading, researching", path: "/research", set: func(f *fakeResearch, _ *Server) {
			f.live = ResearchLiveView{Phase: "researching", Running: true, Round: 1, Rounds: 2, Questions: []string{"What is A?", "What is B?", "What is C?"}, Progress: "Questions 1-3 are in progress."}
		}, want: []string{"Round 1 of 2: looking into 3 questions", "Questions 1-3 are in progress.", "<li>What is B?</li>"}},
		{name: "busy, waiting for approvals", path: "/research", set: func(f *fakeResearch, s *Server) {
			f.live = ResearchLiveView{Phase: "writing", Running: true}
			s.SetApprovals(&fakeApprovals{pending: []PendingApproval{{ID: "a"}, {ID: "b"}}})
		}, want: []string{"Writing the report", "2 actions are waiting for your OK. Research may be paused until you answer.", `<a href="/safety">`}},
		{name: "busy elsewhere", path: "/research", set: func(f *fakeResearch, _ *Server) { f.live = ResearchLiveView{Elsewhere: true} },
			want: []string{"Research is running from the terminal", "Start another when it finishes."}, not: []string{`action="/research/stop"`}},
		{name: "busy, a second start", path: "/research?err=" + url.QueryEscape("Research is already running. Watch it below or stop it."),
			want: []string{`<div class="banner err" role="alert">Research is already running. Watch it below or stop it.</div>`}},
		{name: "clarify", path: "/research", set: func(f *fakeResearch, _ *Server) {
			f.live = ResearchLiveView{Phase: "clarify", Question: "rust", Clarify: []string{"The language or the metal?"}}
		}, want: []string{"Your question could mean a few things.", "<li>The language or the metal?</li>", `action="/research/clarify"`, `name="answer"`, "Research it as typed"}},
		{name: "failed", path: "/research", set: func(f *fakeResearch, _ *Server) {
			f.live = ResearchLiveView{Phase: "failed", Question: "q", Err: "Your agent's model didn't answer. Check the Model page, then try again."}
		}, want: []string{`<div class="banner err" role="alert">Your agent&#39;s model didn&#39;t answer.`}},
		{name: "stopped", path: "/research", set: func(f *fakeResearch, _ *Server) { f.live = ResearchLiveView{Phase: "stopped"} },
			want: []string{"Stopped. No report was written. What was found so far is in the list below."}},
		{name: "done", path: "/research", set: func(f *fakeResearch, _ *Server) {
			f.live = ResearchLiveView{Phase: "done", RunID: "r-1", Report: report, Lines: []string{"Saved to your agent's notes as research/what-is-fakeness.md."}}
		}, want: []string{`<a href="#ref-1" class="cite">[1]</a>`, `<li id="ref-1">`, "<summary>Details</summary>", `href="/logs?q=log:research"`}},
		{name: "history", path: "/research", set: func(f *fakeResearch, _ *Server) {
			f.rows = []ResearchRunRow{{ID: "r-1", Question: "What is fakeness?", StateText: "Finished", When: "Sep 27, 14:05", Took: "4 min", Sources: 1}, {ID: "r-2", Question: "Now", StateText: "Running now", Live: true}}
		}, want: []string{`href="/research/r-1"`, "Finished · Sep 27, 14:05 · took 4 min · 1 source<", replayOf("r-1"), deleteOf("r-1"), "Running now"},
			not: []string{`value="r-2"`}},
		{name: "error", path: "/research", set: func(f *fakeResearch, _ *Server) { f.err = errors.New(rawNoHistory) },
			want: []string{`<div class="banner err" role="alert">Couldn&#39;t read the list of past research.</div>`}, not: []string{rawNoHistory}},
		{name: "error, plain", path: "/research", set: func(f *fakeResearch, _ *Server) { f.err = PlainError("History isn't here.") },
			want: []string{`role="alert">History isn&#39;t here.</div>`}},
		{name: "success", path: "/research?msg=Deleted+from+the+list.", want: []string{`<div class="banner ok" role="status">Deleted from the list.</div>`}},
		{name: "run page", path: "/research/r-1", set: func(f *fakeResearch, _ *Server) { f.report = *report },
			want: []string{"What is fakeness?", `href="#ref-1"`, `<form method="POST" action="/research/replay" class="inline"><input type="hidden" name="id" value="r-1">`,
				`<form method="POST" action="/research/delete"><input type="hidden" name="id" value="r-1">`}},
		{name: "run page, still running", path: "/research/r-1", set: func(f *fakeResearch, _ *Server) {
			f.report = *report
			f.report.StateText, f.report.Live = "Running from the terminal", true
		},
			want: []string{"Running from the terminal"}, not: []string{`action="/research/replay"`, `action="/research/delete"`}},
		{name: "run page, gone", path: "/research/r-9", set: func(f *fakeResearch, _ *Server) { f.err = PlainError("That research isn't in the list any more.") },
			want: []string{`role="alert">That research isn&#39;t in the list any more.`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeResearch()
			s := researchServer(f)
			if c.name == "off" {
				s = researchServer(nil)
			}
			if c.set != nil {
				c.set(f, s)
			}
			rec := get(t, s, c.path)
			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d", c.path, rec.Code)
			}
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q", w)
				}
			}
			for _, n := range c.not {
				if strings.Contains(body, n) {
					t.Errorf("unexpected %q", n)
				}
			}
			checkPlain(t, "research "+c.name, visibleWords(body, false), map[string]bool{})
		})
	}
}

// replayOf and deleteOf are the in-place Run again form and Delete step of
// the listed run id: static URLs, the ID a field.
func replayOf(id string) string {
	return `<form method="POST" action="/research/replay" class="inline" data-on:submit__prevent__throttle.1s="@post('/research/replay', {contentType:'form', retryMaxCount:0})"><input type="hidden" name="id" value="` + id + `">`
}

func deleteOf(id string) string {
	return inlineForm("/research/delete") + `<input type="hidden" name="id" value="` + id + `"><input type="hidden" name="confirm" value="yes">`
}

func TestResearchStart_Form(t *testing.T) {
	f := newFakeResearch()
	s := researchServer(f)
	if got := flashOf(t, postResearch(t, s, "/research/start", url.Values{"question": {"  "}})); got != "err: Type a question first." {
		t.Errorf("empty question: %q", got)
	}
	if got := flashOf(t, postResearch(t, s, "/research/start", url.Values{"question": {strings.Repeat("é", 2001)}})); !strings.HasPrefix(got, "err: That's too long") {
		t.Errorf("2001 characters: %q", got)
	}
	if got := flashOf(t, postResearch(t, s, "/research/start", url.Values{"question": {strings.Repeat("é", 2000)}})); !strings.HasPrefix(got, "msg: ") {
		t.Errorf("2000 characters: %q", got)
	}
	flashOf(t, postResearch(t, s, "/research/start", url.Values{"question": {" What is fakeness? "}, "skip_questions": {"1"}}))
	flashOf(t, postResearch(t, s, "/research/start", url.Values{"question": {"Why?"}}))
	if c := f.called(); len(c) != 3 || c[1] != `start "What is fakeness?" true` || c[2] != `start "Why?" false` {
		t.Errorf("Start calls = %q", c)
	}
	f.startErr = PlainError("Research is already running. Watch it below or stop it.")
	if got := flashOf(t, postResearch(t, s, "/research/start", url.Values{"question": {"again"}})); got != "err: Research is already running. Watch it below or stop it." {
		t.Errorf("busy: %q", got)
	}
	f.startErr = errors.New("resolve planner: " + rawProviderErr)
	if got := flashOf(t, postResearch(t, s, "/research/start", url.Values{"question": {"again"}})); got != "err: Couldn't start the research." {
		t.Errorf("a raw error: %q", got)
	}
	flashOf(t, postResearch(t, s, "/research/clarify", url.Values{"answer": {" 2024 only "}}))
	flashOf(t, postResearch(t, s, "/research/clarify", nil))
	if c := f.called(); !slices.Equal(c[len(c)-2:], []string{`clarify "2024 only"`, `clarify ""`}) {
		t.Errorf("Clarify calls = %q", c)
	}
}

// Stop and Delete need their second click (D20); the page offers it.
func TestResearchDeleteAndStop_Confirm(t *testing.T) {
	f := newFakeResearch()
	f.rows = []ResearchRunRow{{ID: "r-1", Question: "What is fakeness?", StateText: "Finished"}}
	f.live = ResearchLiveView{Phase: "writing", Running: true}
	s := researchServer(f)
	body := get(t, s, "/research").Body.String()
	for _, want := range []string{
		`<details class="confirm-step" data-preserve-attr="open"><summary class="btn sm danger"><span class="c-open">Stop</span>`,
		`It ends now and no report is written.`, `<form method="POST" action="/research/stop" data-on:submit__prevent__throttle.1s="@post('/research/stop', {contentType:'form', retryMaxCount:0})"><input type="hidden" name="confirm" value="yes"><button type="submit" class="btn sm danger">Yes, stop</button>`,
		deleteOf("r-1"), `A copy already saved to your agent&#39;s notes stays there.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, p := range []string{"/research/stop", "/research/delete"} {
		if rec := postResearch(t, s, p, url.Values{"id": {"r-1"}}); rec.Code != http.StatusPreconditionRequired {
			t.Errorf("POST %s without confirm = %d, want 428", p, rec.Code)
		}
	}
	if c := f.called(); len(c) != 0 {
		t.Fatalf("unconfirmed posts reached the control: %q", c)
	}
	if got := flashOf(t, postResearch(t, s, "/research/stop", url.Values{"confirm": {"yes"}})); got != "err: Nothing is running, so there's nothing to stop." {
		t.Errorf("stop with nothing running: %q", got)
	}
	f.stopOK = true
	if got := flashOf(t, postResearch(t, s, "/research/stop", url.Values{"confirm": {"yes"}})); !strings.HasPrefix(got, "msg: Stopping the research.") {
		t.Errorf("stop: %q", got)
	}
	if got := flashOf(t, postResearch(t, s, "/research/delete", url.Values{"id": {"r-1"}, "confirm": {"yes"}})); got != "msg: Deleted from the list." {
		t.Errorf("delete: %q", got)
	}
	if c := f.called(); !slices.Equal(c, []string{"stop", "stop", "delete r-1"}) {
		t.Errorf("calls = %q", c)
	}
	if body := get(t, s, "/research?msg=Deleted+from+the+list.").Body.String(); strings.Contains(body, "/research/r-1") || !strings.Contains(body, `role="status">Deleted from the list.`) {
		t.Error("the deleted row is still listed, or the flash is missing")
	}
}

func TestResearchReplay(t *testing.T) {
	f := newFakeResearch()
	s := researchServer(f)
	if got := flashOf(t, postResearch(t, s, "/research/replay", url.Values{"id": {"r-1"}})); !strings.HasPrefix(got, "msg: Running it again") {
		t.Errorf("replay: %q", got)
	}
	if c := f.called(); !slices.Equal(c, []string{"replay r-1"}) {
		t.Errorf("calls = %q", c)
	}
}

// reportHTML is the XSS boundary: escape first, then only the two fixed
// tags. Sources link only when they're http(s), in any letter case.
func TestResearchReport_Escapes(t *testing.T) {
	f := newFakeResearch()
	f.report = ResearchReportView{ID: "r-1", Question: "<i>q</i>",
		Body: "<script>alert(1)</script> [1]\n# Title\n<b>not bold</b> [x](javascript:alert(1)) [2]",
		Refs: []ResearchRef{
			{N: 1, URL: "https://ex.com/a?b=<c>"}, {N: 2, URL: "javascript:alert(1)"}, {N: 3, URL: "JaVaScRiPt:alert(1)"},
			{N: 4, URL: "data:text/html,<script>alert(1)</script>"}, {N: 5, URL: "wiki:notes/x.md"}, {N: 6, URL: "HTTP://ex.com/b"},
			{N: 7, URL: "https://evil.example/\u202emoc.elgoog"},
		}}
	body := get(t, researchServer(f), "/research/r-1").Body.String()
	for _, bad := range []string{"<script>alert", "<b>not bold", "<i>q", `href="javascript`, `href="JaVaScRiPt`, `href="data:`, `href="wiki:`, `href="#ZgotmplZ"`, "\u202e"} {
		if strings.Contains(body, bad) {
			t.Errorf("the report page has %q", bad)
		}
	}
	for _, want := range []string{
		`&lt;script&gt;alert(1)&lt;/script&gt; <a href="#ref-1" class="cite">[1]</a>`, `<strong class="rh">Title</strong>`,
		`&lt;b&gt;not bold&lt;/b&gt; [x](javascript:alert(1)) <a href="#ref-2" class="cite">[2]</a>`,
		`<a href="https://ex.com/a?b=%3cc%3e" rel="noopener noreferrer nofollow" target="_blank">`,
		`<a href="HTTP://ex.com/b" rel="noopener noreferrer nofollow"`,
		`<li id="ref-2">[2] javascript:alert(1)</li>`, `<li id="ref-3">[3] JaVaScRiPt:alert(1)</li>`, `<li id="ref-5">[5] wiki:notes/x.md</li>`,
		`target="_blank">https://evil.example/⟨U&#43;202E⟩moc.elgoog</a>`, // a link's text can't be reordered to fake where it goes
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the report page lacks %q", want)
		}
	}
	for ref, web := range map[string]bool{"https://a.example": true, "HTTPS://a.example": true, "http://a.example": true, "javascript:alert(1)": false, " javascript:x": false,
		"JaVaScRiPt:x": false, "data:text/html,x": false, "wiki:x": false, "//evil.example": false, "": false, "ftp://a.example": false,
		"https://93.184.215.14/p": true, "https://[2606:4700::1]/": true} {
		if got := (ResearchRef{URL: ref}).Web(); got != web {
			t.Errorf("Web(%q) = %v, want %v", ref, got, web)
		}
	}
}

// REVSEC-3: cookies aren't kept apart by port, so a source on this machine,
// its network, the tailnet or a name the dashboard answers to is plain
// text: a click there would hand the listener the sign-in cookie.
func TestResearchRef_NoLinkIntoThisMachineOrNetwork(t *testing.T) {
	for _, ref := range []string{"http://127.0.0.1:9999/x", "http://127.1:9999/", "http://0x7f.0.0.1/", "http://2130706433/", "http://localhost:8080/",
		"http://LOCALHOST./", "http://app.localhost/", "http://[::1]:9/", "http://[::ffff:127.0.0.1]/", "http://0.0.0.0:80/", "http://10.1.2.3/",
		"http://192.168.1.4/", "http://172.20.0.1/", "http://169.254.169.254/latest/meta-data", "http://[fe80::1]/", "http://100.101.102.103/",
		"https://node.tailnet-name.ts.net:8443/", "http://printer.local/", "http://nas/", "http://router.lan/", "http://x.home.arpa/", "http://db.internal/",
		// Chrome folds these to 127.0.0.1: an ideographic or fullwidth dot, a fullwidth digit, and the same percent-encoded.
		"http://127.0.0.1\u3002:9999/", "http://127.0.0.1\uff0e:9999/", "http://127.0.0.\uff11:9999/", "http://127.0.0.1%E3%80%82:9999/"} {
		if (ResearchRef{URL: ref}).Web() {
			t.Errorf("Web(%q) = true, want plain text", ref)
		}
	}
	f := newFakeResearch()
	f.report = ResearchReportView{ID: "r-1", Question: "q", StateText: "Finished", Body: "[1] [2]",
		Refs: []ResearchRef{{N: 1, URL: "https://dash.example.com:9999/steal"}, {N: 2, URL: "https://news.example/a"}}}
	s := researchServer(f)
	s.cfg.AllowedHosts = []string{"dash.example.com:443"}
	body := get(t, s, "/research/r-1").Body.String()
	if strings.Contains(body, `href="https://dash.example.com`) || !strings.Contains(body, `href="https://news.example/a"`) {
		t.Errorf("a source on the dashboard's own name must be text, a public one a link:\n%s", body)
	}
}

// waitFor polls a stream's output until it holds want n times.
func waitFor(t *testing.T, rec *syncResponse, want string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(rec.String(), want) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the stream never had %q %d times:\n%s", want, n, rec.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The stream sends the whole panel on open, again at each change, and the
// history whenever a run starts or ends; the report arrives without a reload.
func TestResearchLive_StreamsPhasesThenReport(t *testing.T) {
	f := newFakeResearch()
	f.rows = []ResearchRunRow{{ID: "r-0", Question: "Earlier", StateText: "Finished"}}
	s := researchServer(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &syncResponse{}
	done := make(chan struct{})
	go func() {
		s.handleResearchLive(rec, httptest.NewRequest("GET", "/research/live", nil).WithContext(ctx))
		close(done)
	}()
	waitFor(t, rec, `data: elements <div id="research-live">`, 1) // the snapshot
	waitFor(t, rec, `data: elements <div id="research-history">`, 1)

	f.set(ResearchLiveView{Phase: "researching", Running: true, RunID: "r-1", Round: 1, Rounds: 2, Questions: []string{"What is A?", "What is B?"}})
	waitFor(t, rec, "Round 1 of 2: looking into 2 questions", 1)
	waitFor(t, rec, `data: elements <div id="research-history">`, 2) // the new run's row
	f.set(ResearchLiveView{Phase: "writing", Running: true, RunID: "r-1"})
	waitFor(t, rec, "Writing the report", 1)
	f.set(ResearchLiveView{Phase: "done", RunID: "r-1", Report: &ResearchReportView{ID: "r-1", Body: "Answer [1].", Refs: []ResearchRef{{N: 1, URL: "https://example.com/a"}}}})
	waitFor(t, rec, `<a href="#ref-1" class="cite">[1]</a>`, 1)
	waitFor(t, rec, `data: elements <div id="research-history">`, 3) // it finished
	if n := strings.Count(rec.String(), "event: datastar-patch-elements"); n != 4 {
		t.Errorf("%d patches, want 4 (snapshot, researching, writing, done): the writing step re-sent the history?\n%s", n, rec.String())
	}
	f.set(ResearchLiveView{Phase: "done", RunID: "r-1", Elsewhere: true})
	waitFor(t, rec, `data: elements <div id="research-history">`, 4) // a terminal run started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream didn't end with its request")
	}
}

// A partial that is patched must not open a stream itself.
func TestResearchLive_PartialHasNoDataInit(t *testing.T) {
	s := researchServer(nil)
	for _, v := range []ResearchLiveView{{}, {Running: true, Phase: "writing"}, {Phase: "done", Report: &ResearchReportView{Body: "x"}}} {
		for name, data := range map[string]any{"research-live": s.researchLive(v), "research-history": researchHistoryData{}} {
			out, err := s.researchPart(name, data)
			if err != nil || strings.Contains(out, "data-init") || strings.Contains(out, "data-on-interval") {
				t.Errorf("%s (%s): err %v, or it opens a request:\n%s", name, v.Phase, err, out)
			}
		}
	}
}

// /research/live is a stream: the finite write timeout never cuts it.
func TestResearchLive_OutlivesWriteTimeout(t *testing.T) {
	f := newFakeResearch()
	s := researchServer(f)
	const timeout = 40 * time.Millisecond
	hs := httptest.NewServer(s.handlerWithWriteTimeout(timeout))
	defer hs.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+"/research/live", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	events := 0
	next := func() {
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the stream closed after %d patches", events)
				}
				if strings.HasPrefix(l, "event: datastar-patch-elements") {
					events++
					return
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("no patch after %d", events)
			}
		}
	}
	next() // the snapshot
	time.Sleep(3 * timeout)
	f.set(ResearchLiveView{Phase: "writing", Running: true})
	next()
}
