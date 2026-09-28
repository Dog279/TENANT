package dashboard

// ui_test.go covers TEN-211's shared page kit: the confirm step and its
// server gate, the status strip, the chat send answers, the page shell, and
// the sweeps every template and rendered page must pass. Frozen after the
// foundation; the sweeps' allowlists live in ui_adopt_test.go.

import (
	"context"
	"html"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/control"
)

func TestConfirmStep_Args(t *testing.T) {
	if _, err := confirmStep("/x", "Remove", "Gone.", "name"); err == nil {
		t.Error("an odd number of hidden-field arguments should be an error")
	}
	v, err := confirmStep("/models/remove", "Remove", "Gone for good.", "name", "a", "kind", "b")
	if err != nil {
		t.Fatal(err)
	}
	if v.Yes != "Yes, remove" || v.Action != "/models/remove" || v.Consequence != "Gone for good." {
		t.Errorf("view = %+v", v)
	}
	if want := []HiddenField{{"name", "a"}, {"kind", "b"}}; !slices.Equal(v.Hidden, want) {
		t.Errorf("hidden = %+v, want %+v in order", v.Hidden, want)
	}
}

func TestUIConfirm_NoJS(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	v, _ := confirmStep("/models/remove", "Remove", "Your agent can no longer use it.", "name", `a"b`)
	out, err := s.uiPart("ui-confirm", v)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<details class="confirm-step">`, `<summary class="btn sm danger"><span class="c-open">Remove</span><span class="c-cancel">Cancel</span></summary>`,
		`role="group" aria-label="Remove"`, "<p>Your agent can no longer use it.</p>", `<form method="POST" action="/models/remove">`,
		`<input type="hidden" name="name" value="a&#34;b">`, `<input type="hidden" name="confirm" value="yes">`, ">Yes, remove</button>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ui-confirm missing %q:\n%s", want, out)
		}
	}
	if regexp.MustCompile(`(?i)<script|\son[a-z]+=`).MatchString(out) {
		t.Errorf("ui-confirm must work without JS:\n%s", out)
	}
}

func TestNeedConfirm(t *testing.T) {
	var reached atomic.Int32
	ok := func(w http.ResponseWriter, _ *http.Request) { reached.Add(1) }
	post := func(h http.HandlerFunc, body string) int {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h(rec, r)
		if rec.Code == http.StatusPreconditionRequired && !strings.Contains(rec.Body.String(), notConfirmed) {
			t.Errorf("428 body = %q, want %q", rec.Body.String(), notConfirmed)
		}
		return rec.Code
	}
	always := needConfirm(nil, ok)
	if code := post(always, "name=a"); code != http.StatusPreconditionRequired || reached.Load() != 0 {
		t.Errorf("no confirm: %d, handler reached %d times; want 428 and never", code, reached.Load())
	}
	if code := post(always, "name=a&confirm=yes"); code != http.StatusOK || reached.Load() != 1 {
		t.Errorf("confirm=yes: %d, handler reached %d times; want it to run once", code, reached.Load())
	}

	// Fail-closed: only the listed safe values skip the step, after trimming
	// and lowercasing; any spelling of more access confirms.
	mode := needConfirm(confirmUnless("mode", "ask", "deny", ""), ok)
	for _, v := range []string{"allow", "ALLOW", " allow", "on", "always", "yes", "1", "t", "TRUE"} {
		reached.Store(0)
		if code := post(mode, "mode="+url.QueryEscape(v)); code != http.StatusPreconditionRequired || reached.Load() != 0 {
			t.Errorf("mode=%q: %d (handler reached %d), want 428", v, code, reached.Load())
		}
	}
	for _, v := range []string{"ask", "deny", " Deny ", ""} {
		reached.Store(0)
		if code := post(mode, "mode="+url.QueryEscape(v)); code != http.StatusOK || reached.Load() != 1 {
			t.Errorf("mode=%q: %d (handler reached %d), want it to pass", v, code, reached.Load())
		}
	}
	// A missing field is "" and confirms unless "" is listed as safe.
	strict := needConfirm(confirmUnless("mode", "ask", "deny"), ok)
	if code := post(strict, "name=a"); code != http.StatusPreconditionRequired {
		t.Errorf("a missing field without \"\" listed: %d, want 428", code)
	}
}

// uiStatus GETs /ui/status and returns the patched #ui-status element.
func uiStatus(t *testing.T, s *Server) string {
	t.Helper()
	rec := get(t, s, "/ui/status")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("GET /ui/status = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var b strings.Builder
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if el, ok := strings.CutPrefix(line, "data: elements "); ok {
			b.WriteString(el)
		}
	}
	return b.String()
}

func TestUIStatus_IdleIsEmpty(t *testing.T) {
	s := New(Config{}, &chatRunner{}, nil, nil, nil, nil)
	if got, want := uiStatus(t, s), `<div id="ui-status" class="status-strip"></div>`; got != want {
		t.Errorf("idle strip = %q, want exactly %q (CSS hides it with :empty)", got, want)
	}
}

// blockRunner is a chat runner whose turns run until released, like a real
// one answering. It can also play the serve turn gate (liveness).
type blockRunner struct {
	started chan string
	release chan struct{}
	turns   atomic.Int32

	gate  bool
	age   time.Duration
	queue agent.TurnQueueStatus
}

func newBlockRunner() *blockRunner {
	return &blockRunner{started: make(chan string, 8), release: make(chan struct{})}
}

func (b *blockRunner) Turn(ctx context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	b.turns.Add(1)
	b.started <- req.UserQuery
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return &agent.TurnResult{}, nil
}
func (b *blockRunner) Interject(string)                       {}
func (b *blockRunner) ActiveTurn() (bool, time.Duration)      { return b.gate, b.age }
func (b *blockRunner) TurnQueueStatus() agent.TurnQueueStatus { return b.queue }

func chatSend(t *testing.T, s *Server, text string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/chat/send", strings.NewReader(`{"text":"`+text+`"}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec.Body.String()
}

func awaitStart(t *testing.T, b *blockRunner) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the turn never started")
	}
}

func awaitIdle(t *testing.T, s *Server) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if uiStatus(t, s) == `<div id="ui-status" class="status-strip"></div>` {
			return
		}
	}
	t.Fatalf("the strip never went idle: %s", uiStatus(t, s))
}

func TestUIStatus_BusyDuringChat(t *testing.T) {
	b := newBlockRunner()
	s := New(Config{}, b, nil, nil, nil, nil)
	chatSend(t, s, "hello")
	awaitStart(t, b)
	got := uiStatus(t, s)
	for _, want := range []string{"Your agent is answering a message", "New messages wait until it", `data-on-interval__duration.3s="@get('/ui/status')"`, `action="/chat/stop"`, ">Stop</button>"} {
		if !strings.Contains(got, want) {
			t.Errorf("busy strip missing %q:\n%s", want, got)
		}
	}
	// Waiting on the operator: the strip says so and links to the answer,
	// once (the busy line's link stands in for the count's).
	s.SetApprovals(&fakeApprovals{pending: []PendingApproval{{ID: "ap-1", Category: "exec"}}})
	got = uiStatus(t, s)
	if !strings.Contains(got, "paused until you answer a request") || strings.Count(got, `href="/safety">Answer it`) != 1 {
		t.Errorf("a turn waiting on an approval should say so, with one link:\n%s", got)
	}
	s.SetApprovals(nil)
	close(b.release)
	awaitIdle(t, s)
}

func TestUIStatus_QueuedBehindGate(t *testing.T) {
	b := newBlockRunner()
	b.gate, b.age = true, 90*time.Second
	b.queue = agent.TurnQueueStatus{Depth: 1, ActiveSource: "cron", Entries: []agent.TurnQueueEntry{{Position: 1, Source: agent.RouteSurfaceDashboard, ETA: 40 * time.Second}}}
	s := New(Config{}, b, nil, nil, nil, nil)

	// The gate is busy and this dashboard asked nothing: no Stop, the source named.
	got := uiStatus(t, s)
	if !strings.Contains(got, "Your agent is busy with a scheduled job or an alert") || strings.Contains(got, "/chat/stop") {
		t.Errorf("someone else's turn: %s", got)
	}

	chatSend(t, s, "hello")
	awaitStart(t, b) // the gate holds it: Turn blocks
	got = uiStatus(t, s)
	for _, want := range []string{"busy with a scheduled job or an alert", "Your message is #1 in line, about 40s.", `action="/chat/stop"`, ">Cancel</button>"} {
		if !strings.Contains(got, want) {
			t.Errorf("queued strip missing %q:\n%s", want, got)
		}
	}
	b.queue.Entries[0].ETA = 0
	if got = uiStatus(t, s); !strings.Contains(got, "Your message is next. It starts when the current request finishes.") {
		t.Errorf("ETA ≤ 0: %s", got)
	}
	for src, want := range map[string]string{"relay": "a Discord or iMessage conversation", "tui": "a message from the terminal", "peer": "another request", "": "another request"} {
		b.queue.ActiveSource = src
		if got = uiStatus(t, s); !strings.Contains(got, "busy with "+want) {
			t.Errorf("ActiveSource %q: %s", src, got)
		}
	}
	close(b.release)
	b.gate, b.queue = false, agent.TurnQueueStatus{}
	awaitIdle(t, s)
}

// fakeSafety is the main broker's category modes.
type fakeSafety struct{ perms []control.PermissionInfo }

func (f *fakeSafety) Permissions() []control.PermissionInfo { return f.perms }
func (f *fakeSafety) SetPermissionFrom(_, _, _ string) (bool, error) {
	return false, nil
}

func TestUIStatus_ApprovalsAndNotices(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetApprovals(&fakeApprovals{pending: []PendingApproval{{ID: "ap-1"}, {ID: "ap-2"}}})
	s.SetSafety(&fakeSafety{perms: []control.PermissionInfo{
		{Category: "exec", Mode: "allow", Title: "Run commands"},
		{Category: "egress", Mode: "ask", Title: "Share private data", New: true},
		{Category: "mcp", Mode: "ask", Title: "Connect remote services", New: true},
	}}, nil)
	s.SetAccess(&fakeAccess{view: AccessView{DiscordPerms: []PermissionRow{{Category: "sandbox", Mode: "ask", New: true}}}})
	got := uiStatus(t, s)
	for _, want := range []string{
		`2 actions are waiting for your OK. <a href="/safety">Answer it</a>`,
		`An update added 2 new safety checks: Share private data, Connect remote services. Your agent asks you first until you choose. <a href="/safety">Review</a>`,
		`Discord conversations: 1 new safety check asks you first until you choose. <a href="/access">Review</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("strip missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "data-on-interval") {
		t.Error("only a busy strip polls itself")
	}
}

// The count comes from Pending() on every request, not from events: a
// request the stream never announced still shows on the next poll.
func TestUIStatus_CountsPendingWithoutEvent(t *testing.T) {
	fa := &fakeApprovals{}
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetApprovals(fa)
	if got := uiStatus(t, s); strings.Contains(got, "waiting") {
		t.Fatalf("nothing pending: %s", got)
	}
	fa.pending = []PendingApproval{{ID: "ap-9", Category: "exec"}}
	if got := uiStatus(t, s); !strings.Contains(got, "1 action is waiting for your OK") {
		t.Errorf("a pending request with no event: %s", got)
	}
}

// Every open page polls the strip every 20s: request records would bury the
// requests that matter, so the poll leaves none.
func TestUIStatus_NotRequestLogged(t *testing.T) {
	c := installCapture(t)
	h := New(Config{Addr: "example.com", Auth: "tok"}, nil, nil, nil, nil, nil).served()
	for _, p := range []string{"/ui/status", "/tools"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		r.Header.Set("Authorization", "Bearer tok")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if !c.has("9001 ", "route=/tools") || c.has("9001 ", "route=/ui/status") {
		t.Errorf("want a record for /tools and none for /ui/status: %q", c.lines())
	}
}

func TestLayout_StatusWrapPolls(t *testing.T) {
	body := get(t, New(Config{}, nil, nil, nil, nil, nil), "/tools").Body.String()
	// The poll sits on the wrapper, which is never patched; #ui-status itself
	// carries neither, so a patch can't stack a second poll (§2.7).
	const want = `<div class="status-wrap" data-init="@get('/ui/status')" data-on-interval__duration.20s="@get('/ui/status')"><div id="ui-status" class="status-strip"></div></div>`
	if !strings.Contains(body, want) {
		t.Errorf("layout lacks the polled status wrapper %q", want)
	}
}

func TestChatSend_BusyRejectsWithReason(t *testing.T) {
	b := newBlockRunner()
	s := New(Config{}, b, nil, nil, nil, nil)
	chatSend(t, s, "first")
	awaitStart(t, b)
	got := chatSend(t, s, "second message")
	if !strings.Contains(got, "Not sent: your agent is still answering.") || strings.Contains(got, "second message") {
		t.Errorf("a send while busy should explain and not echo:\n%s", got)
	}
	if !strings.Contains(got, `id="ui-status"`) || strings.Contains(got, "datastar-patch-signals") {
		t.Errorf("it refreshes the strip and leaves the text in the box:\n%s", got)
	}
	close(b.release)
	awaitIdle(t, s)
	if n := b.turns.Load(); n != 1 {
		t.Errorf("Turn called %d times, want 1", n)
	}
}

func TestChatSend_StartedClearsBox(t *testing.T) {
	b := newBlockRunner()
	s := New(Config{}, b, nil, nil, nil, nil)
	got := chatSend(t, s, "hello there")
	awaitStart(t, b)
	for _, want := range []string{"hello there", "event: datastar-patch-signals\ndata: signals {\"text\":\"\"}\n\n", `id="ui-status"`, "answering a message"} {
		if !strings.Contains(got, want) {
			t.Errorf("a sent message: missing %q:\n%s", want, got)
		}
	}
	close(b.release)
	awaitIdle(t, s)

	if got := chatSend(t, New(Config{}, nil, nil, nil, nil, nil), "hi"); !strings.Contains(got, "Chat isn't available on this Tenant.") {
		t.Errorf("no agent: %s", got)
	}
}

func TestVisibleText(t *testing.T) {
	for in, want := range map[string]string{
		"rm -rf \u202E/tmp":         "rm -rf ⟨U+202E⟩/tmp",
		"a\u200Bb\uFEFFc\u2066":     "a⟨U+200B⟩b⟨U+FEFF⟩c⟨U+2066⟩",
		"bell\x07\x1b[2J":           "bell⟨U+0007⟩⟨U+001B⟩[2J",
		"a\r\nb\rc":                 "a\nb⟨U+000D⟩c",
		"top\n\n\n\n\nhidden":       "top\n⟨4 blank lines⟩\nhidden",
		"top\n \n\t\n\nhidden":      "top\n⟨3 blank lines⟩\nhidden",
		"two\n\n\nblank lines stay": "two\n\n\nblank lines stay",
		"tabs\tand ünïcödé stay 中文": "tabs\tand ünïcödé stay 中文",
	} {
		if got := visibleText(in); got != want {
			t.Errorf("visibleText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWaited(t *testing.T) {
	for secs, want := range map[int]string{0: "waiting less than a minute", 59: "waiting less than a minute", 60: "waiting 1 min", 3599: "waiting 59 min", 3600: "waiting 1 h", 7300: "waiting 2 h"} {
		if got := waited(secs); got != want {
			t.Errorf("waited(%d) = %q, want %q", secs, got, want)
		}
	}
}

func TestPages_ShellAndStates(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil) // every control nil
	for _, p := range harnessPages {
		rec := get(t, s, p)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with nil controls = %d, want 200", p, rec.Code)
			continue
		}
		body := rec.Body.String()
		for _, want := range []string{`id="ui-status"`, `popovertarget="nav"`, `id="nav" popover`, `<form method="POST" action="/auth/logout"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks the shell's %q", p, want)
			}
		}
	}
	// The new pages: off with a nil control, flash in both kinds.
	for _, p := range []string{"/safety", "/agents", "/goal", "/research", "/research/r-1"} {
		body := get(t, s, p+"?msg=Saved.&err=Nope.").Body.String()
		for _, want := range []string{`class="card state-off"`, `<div class="banner ok" role="status">Saved.</div>`, `<div class="banner err" role="alert">Nope.</div>`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", p, want)
			}
		}
	}
	if rec := get(t, s, "/research/live"); rec.Code != http.StatusNoContent {
		t.Errorf("/research/live without research = %d, want 204", rec.Code)
	}
}

// The nav: every page in its group, labelled as D15 says, in order.
func TestNav_GroupsAndLabels(t *testing.T) {
	body := get(t, New(Config{}, nil, nil, nil, nil, nil), "/").Body.String()
	nav := body[strings.Index(body, `<nav class="nav"`):strings.Index(body, "</nav>")]
	var got []string
	for _, m := range regexp.MustCompile(`<div class="nav-group">([^<]+)</div>|<a href="([^"]+)"[^>]*>([^<]+)</a>`).FindAllStringSubmatch(nav, -1) {
		if m[1] != "" {
			got = append(got, "["+m[1]+"]")
		} else {
			got = append(got, m[2]+" "+html.UnescapeString(m[3]))
		}
	}
	want := []string{"/ Overview",
		"[Activity]", "/chat Chat", "/activity Activity feed", "/goal Autopilot", "/logs Logs",
		"[Knowledge]", "/memory Memory", "/skills Skills", "/eval Quality", "/research Research",
		"[Capabilities]", "/tools Tools", "/agents Helpers", "/mcp Remote services", "/cron Scheduled jobs",
		"[Safety]", "/safety Approvals & safety", "/access Access",
		"[Settings]", "/models Model", "/integrations Integrations", "/settings/keys Provider keys",
	}
	if !slices.Equal(got, want) {
		t.Errorf("nav =\n%q\nwant\n%q", got, want)
	}
	for p, title := range map[string]string{"/safety": "Approvals &amp; safety", "/agents": "Helpers", "/goal": "Autopilot", "/research": "Research"} {
		if b := get(t, New(Config{}, nil, nil, nil, nil, nil), p).Body.String(); !strings.Contains(b, "<h1>"+title+"</h1>") {
			t.Errorf("%s: the page title should be its nav label %q", p, title)
		}
	}
}

func TestTemplates_NoInlineJS(t *testing.T) {
	js := regexp.MustCompile(`(?i)<script|\son[a-z]+=`)
	used := map[string]bool{}
	walkTemplates(t, func(name, body string) {
		body = strings.Replace(body, `<script type="module" src="/datastar.js"></script>`, "", 1) // the one script: Datastar
		if !js.MatchString(body) {
			return
		}
		if inlineJSAllow[name] {
			used[name] = true
			return
		}
		t.Errorf("%s has inline JS (%q); the dashboard ships no hand-written JS", name, js.FindString(body))
	})
	for name := range inlineJSAllow {
		if !used[name] {
			t.Errorf("%s is clean now: remove it from inlineJSAllow", name)
		}
	}
}

func walkTemplates(t *testing.T, fn func(name, body string)) {
	t.Helper()
	err := fs.WalkDir(ssrFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		b, err := ssrFS.ReadFile(p)
		fn(path.Base(p), string(b))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// bannedWords is §2.9's list: words the operator shouldn't have to know.
var bannedWords = []struct {
	word string
	re   *regexp.Regexp
}{
	{"toolmux", regexp.MustCompile(`(?i)\btool ?mux\b`)},
	{"broker", regexp.MustCompile(`(?i)\bbrokers?\b`)},
	{"episodic", regexp.MustCompile(`(?i)\bepisodic\b`)},
	{"soul", regexp.MustCompile(`(?i)\bsouls?\b`)},
	{"compaction", regexp.MustCompile(`(?i)\bcompaction\b`)},
	{"superseded", regexp.MustCompile(`(?i)\bsupersed(e|ed|es)\b`)},
	{"posture", regexp.MustCompile(`(?i)\bposture\b`)},
	{"gated", regexp.MustCompile(`(?i)\bgated\b`)},
	{"cron-exec", regexp.MustCompile(`(?i)\bcron-exec\b`)},
	{"credentials.json", regexp.MustCompile(`(?i)\bcredentials\.json\b`)},
	{"allowlist", regexp.MustCompile(`(?i)\ballow-?lists?\b`)},
	{"handles", regexp.MustCompile(`(?i)\bhandles\b`)},
	{"exec mode", regexp.MustCompile(`(?i)\bexec mode\b`)},
	{"planner", regexp.MustCompile(`(?i)\bplanners?\b`)},
	{"wiki", regexp.MustCompile(`(?i)\bwiki\b`)},
	{"sub-agent", regexp.MustCompile(`(?i)\bsub-?agents?\b`)},
	{"cycle", regexp.MustCompile(`(?i)\bcycles?\b`)},
	{"wave", regexp.MustCompile(`(?i)\bwaves?\b`)},
	{"turn", regexp.MustCompile(`(?i)\bturns?\b`)}, // the agent's, not "turn on"/"turn off" (turnOnOff)
	{"SSE", regexp.MustCompile(`(?i)\bsse\b`)},
	{"CSRF", regexp.MustCompile(`(?i)\bcsrf\b`)},
	{"bearer", regexp.MustCompile(`(?i)\bbearer\b`)},
	{"egress", regexp.MustCompile(`(?i)\begress\b`)},
	{"raw category id", regexp.MustCompile(`(?i)\b(exec|mcp)\b`)},
	{"slash command", regexp.MustCompile(`(?:^|\s)/[a-z][a-z-]*\b`)},
	{"backticked command", regexp.MustCompile("`[^`]+`")},
	{"emoji status marker", regexp.MustCompile(`[\x{2600}-\x{27BF}\x{1F300}-\x{1FAFF}]`)},
}

var (
	tmplAction  = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	scriptStyle = regexp.MustCompile(`(?is)<(script|style)\b.*?</(?:script|style)>|<!--.*?-->`)
	keptAttr    = regexp.MustCompile(`\s(?:placeholder|title|aria-label)="([^"]*)"`)
	anyTag      = regexp.MustCompile(`(?s)<[^>]*>`)
	turnOnOff   = regexp.MustCompile(`(?i)\bturn(s|ed|ing)?\s+(it\s+|them\s+)?(on|off)\b`)
)

// visibleWords is what a page shows: its text and the placeholder, title
// and aria-label values, without markup (or template actions).
func visibleWords(h string, isTemplate bool) string {
	if isTemplate {
		h = tmplAction.ReplaceAllString(h, " ")
	}
	h = scriptStyle.ReplaceAllString(h, " ")
	var kept []string
	for _, m := range keptAttr.FindAllStringSubmatch(h, -1) {
		kept = append(kept, m[1])
	}
	h = anyTag.ReplaceAllString(h, " ") + " " + strings.Join(kept, " ")
	return turnOnOff.ReplaceAllString(html.UnescapeString(h), " ")
}

// checkPlain reports each banned word on one page or template (key) that
// its allowlist entry doesn't list, and records the entries it used.
func checkPlain(t *testing.T, key, text string, used map[string]bool) {
	t.Helper()
	for _, b := range bannedWords {
		m := b.re.FindString(text)
		if m == "" {
			continue
		}
		if slices.Contains(plainLanguageAllow[key], b.word) {
			used[key+"\x00"+b.word] = true
			continue
		}
		t.Errorf("%s says %q (banned: %s); see DESIGN.md's voice rules", key, strings.TrimSpace(m), b.word)
	}
}

// checkStale fails on allowlist entries of keys that no longer need them.
func checkStale(t *testing.T, keys func(string) bool, used map[string]bool) {
	t.Helper()
	for key, words := range plainLanguageAllow {
		if !keys(key) {
			continue
		}
		for _, w := range words {
			if !used[key+"\x00"+w] {
				t.Errorf("%s no longer says %q: remove it from plainLanguageAllow", key, w)
			}
		}
	}
}

func TestTemplates_PlainLanguage(t *testing.T) {
	used := map[string]bool{}
	walkTemplates(t, func(name, body string) {
		if name == "logs.html" { // the log book's filter language is for experts
			return
		}
		checkPlain(t, name, visibleWords(body, true), used)
	})
	checkStale(t, func(k string) bool { return strings.HasSuffix(k, ".html") }, used)
}

func TestRendered_PlainLanguage(t *testing.T) {
	used := map[string]bool{}
	for route, page := range renderAllPages(t, failing) {
		if route == "/logs" {
			continue
		}
		checkPlain(t, route, visibleWords(page, false), used)
	}
	checkStale(t, func(k string) bool { return strings.HasPrefix(k, "/") }, used)
}
