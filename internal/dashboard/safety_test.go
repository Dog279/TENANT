package dashboard

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"tenant/internal/control"
)

// fakeSafety is the main broker's modes: it records each change as
// "surface|category|mode" and applies it as the broker does (the category is
// then chosen, and a session grant ends).
type fakeSafety struct {
	mu    sync.Mutex
	perms []control.PermissionInfo
	calls []string
	err   error
}

func (f *fakeSafety) Permissions() []control.PermissionInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.perms)
}

func (f *fakeSafety) SetPermissionFrom(surface, category, mode string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, surface+"|"+category+"|"+mode)
	if f.err != nil {
		return false, f.err
	}
	for i := range f.perms {
		if f.perms[i].Category == category {
			f.perms[i].Mode, f.perms[i].New, f.perms[i].Session = mode, false, false
		}
	}
	return true, nil
}

func (f *fakeSafety) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fakeQueue is an ApprovalControl safe to share with a stream: Decide
// resolves like the registry (first answer wins, the item leaves Pending).
type fakeQueue struct {
	mu      sync.Mutex
	items   []PendingApproval
	decided []string // "id|decision"
	err     error
}

func (f *fakeQueue) Pending() []PendingApproval {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.items)
}

func (f *fakeQueue) Decide(id, decision string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decided = append(f.decided, id+"|"+decision)
	if f.err != nil {
		return f.err
	}
	i := slices.IndexFunc(f.items, func(p PendingApproval) bool { return p.ID == id })
	if i < 0 {
		return fmt.Errorf("approval %q not found (already resolved or expired)", id)
	}
	f.items = slices.Delete(f.items, i, i+1)
	return nil
}

func (f *fakeQueue) add(p ...PendingApproval) {
	f.mu.Lock()
	f.items = append(f.items, p...)
	f.mu.Unlock()
}

func (f *fakeQueue) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.decided)
}

// safetyPerms is the eight categories as the broker lists them after an
// update: exec asks with a session grant, write allows, destructive denies,
// and the three categories the update added are New.
func safetyPerms() []control.PermissionInfo {
	var out []control.PermissionInfo
	for _, c := range []struct{ cat, title, can, mode string }{
		{"exec", "Run commands", "run commands and programs on this computer", "ask"},
		{"write", "Change files", "create and change files on this computer", "allow"},
		{"destructive", "Irreversible actions", "do things that can't be undone, like deleting files, dropping database tables or buying something", "deny"},
		{"web", "Act on websites", "click, type and submit forms on websites", "ask"},
		{"send", "Send messages", "send email, texts and posts in your name", "ask"},
		{"egress", "Share private data", "send private information it has read (mail, files, tickets) to an outside service", "ask"},
		{"mcp", "Connect remote services", "connect new remote services and trust their tools", "ask"},
		{"sandbox", "Leave the sandbox", "run a command outside the safety box after the box stopped it", "ask"},
	} {
		isNew := c.cat == "egress" || c.cat == "mcp" || c.cat == "sandbox"
		out = append(out, control.PermissionInfo{Category: c.cat, Mode: c.mode, Title: c.title, Can: c.can, New: isNew, Session: c.cat == "exec"})
	}
	return out
}

// queueItems is one request of each kind the queue lists, with detail d.
func queueItems(d string) []PendingApproval {
	return []PendingApproval{
		{ID: "ap-exec", Category: "exec", Action: "os_exec", Detail: d, AgeSecs: 30, Origin: "local", SessionOK: true},
		{ID: "ap-write", Category: "write", Action: "os_write", Detail: d, AgeSecs: 700, Origin: "local", SessionOK: true},
		{ID: "ap-rule", Category: "write", Action: "os_edit", Detail: d, AgeSecs: 20, Origin: "local"}, // a tool rule forced it
		{ID: "ap-imsg", Category: "send", Action: "imessage_send", Detail: d, AgeSecs: 10, Origin: "imessage", SessionOK: true},
		{ID: "ap-pair", Category: "pairing", Action: "peer pairing", Detail: d, AgeSecs: 5, Origin: "local"},
		{ID: "ap-alert", Category: "investigate", Action: "alert_investigate", Detail: d, AgeSecs: 4000, Origin: "local"},
	}
}

func init() {
	harnessFakes = append(harnessFakes, func(s *Server, mode fakeMode) {
		s.SetSafety(&fakeSafety{perms: safetyPerms()}, func() SafetyFacts { return SafetyFacts{Account: longName(0), Sandboxed: true} })
		// A request's detail is the agent's text, not the page's words: the
		// failing mode's plain-language sweep gets a plain one.
		d := longDetail()
		if mode == failing {
			d = "uname -a"
		}
		q := &fakeQueue{}
		for i := range 2 {
			for _, p := range queueItems(d) {
				p.ID = fmt.Sprintf("%s-%d", p.ID, i)
				q.add(p)
			}
		}
		s.SetApprovals(q)
	})
}

func safetyServer(fs *fakeSafety, fq *fakeQueue, facts SafetyFacts) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fs != nil {
		s.SetSafety(fs, func() SafetyFacts { return facts })
	}
	if fq != nil {
		s.SetApprovals(fq)
	}
	return s
}

func safetyPost(t *testing.T, s *Server, path string, vals url.Values, datastar bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if datastar {
		r.Header.Set("Datastar-Request", "true")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// flash is the redirect's msg or err, decoded.
func flash(t *testing.T, rec *httptest.ResponseRecorder) (msg, errMsg string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || u.Path != "/safety" {
		t.Fatalf("redirect to %q, want /safety", rec.Header().Get("Location"))
	}
	return u.Query().Get("msg"), u.Query().Get("err")
}

// item is one queue item's markup in page.
func item(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="q-`+id+`"`)
	if i < 0 {
		t.Fatalf("no item %s in:\n%s", id, page)
	}
	rest := page[i:]
	for _, end := range []string{`<div class="q-item"`, `<div id="approval-note"`} {
		if j := strings.Index(rest, end); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}

// Nil controls: the page is still a 200 that says what isn't there, per
// section, and the posts go back to it without a panic.
func TestSafety_NilControls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		s      *Server
		want   []string
		absent []string
	}{
		{"both nil", safetyServer(nil, nil, SafetyFacts{}), []string{"Approvals and safety settings aren't available on this Tenant."}, []string{"queue-wrap"}},
		{"no approvals", safetyServer(&fakeSafety{perms: safetyPerms()}, nil, SafetyFacts{}), []string{approvalsOff, "Run commands", "queue-wrap"}, []string{modesOff}},
		{"no modes", safetyServer(nil, &fakeQueue{}, SafetyFacts{}), []string{modesOff, "Nothing is waiting for you.", "queue-wrap"}, []string{approvalsOff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, tc.s, "/safety")
			body := html.UnescapeString(rec.Body.String())
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /safety = %d", rec.Code)
			}
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("lacks %q", w)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(body, a) {
					t.Errorf("says %q", a)
				}
			}
		})
	}
	s := safetyServer(nil, nil, SafetyFacts{})
	for _, p := range []string{"/safety/mode", "/safety/new", "/safety/approvals/ap-1?decision=deny"} {
		rec := safetyPost(t, s, p, url.Values{"category": {"exec"}, "mode": {"ask"}}, false)
		if _, e := flash(t, rec); e == "" {
			t.Errorf("POST %s with nothing wired: no error flash", p)
		}
	}
	if rec := safetyPost(t, s, "/safety/approvals/ap-1?decision=deny", nil, true); !strings.Contains(html.UnescapeString(rec.Body.String()), `role="alert">`+approvalsOff) {
		t.Errorf("a Datastar decision with no approvals: %s", rec.Body.String())
	}
}

// The modes card: a title, a New tag and the consequence of its mode on
// each row; the New banner with its one-tap form; a session grant with "Ask
// me again"; Allow behind a confirm step only when it would loosen.
func TestSafety_RendersModes(t *testing.T) {
	s := safetyServer(&fakeSafety{perms: safetyPerms()}, nil, SafetyFacts{Account: "dylan"})
	page := get(t, s, "/safety").Body.String()
	body := html.UnescapeString(page)
	for _, want := range []string{
		"These settings cover chat on this page, alerts, research and scheduled jobs that don't run commands. Discord and iMessage conversations have their own settings on the",
		"Scheduled jobs that don't run commands wait for your OK here when they need one of these.",
		"Allowed without asking until Tenant restarts.",
		"Can create and change files on this computer without asking you.",
		"Can't do things that can't be undone, like deleting files, dropping database tables or buying something. It won't ask.",
		"Asks you before it can click, type and submit forms on websites.",
		"An update added 3 new safety checks. Your agent asks you first until you choose.",
		"<b>Share private data</b>: asks before it can send private information it has read (mail, files, tickets) to an outside service.",
		`<form method="POST" action="/safety/new"><button type="submit" class="btn on">Keep them at Ask</button></form>`,
		"If nobody answers, the request is turned down after a while.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if n := strings.Count(page, `<span class="tag-new">New</span>`); n != 3 {
		t.Errorf("%d New tags, want 3", n)
	}
	exec := page[strings.Index(page, `id="perm-exec"`):strings.Index(page, `id="perm-write"`)]
	if !strings.Contains(exec, `<input type="hidden" name="mode" value="ask"><button type="submit" class="btn sm">Ask me again</button>`) {
		t.Errorf("the session grant has no Ask me again:\n%s", exec)
	}
	// Allow confirms wherever it isn't already the mode: 7 of 8 rows.
	if n := strings.Count(page, `<details class="confirm-step go">`); n != 7 {
		t.Errorf("%d Allow confirm steps, want 7 (not on write, already allowed)", n)
	}
	write := page[strings.Index(page, `id="perm-write"`):strings.Index(page, `id="perm-destructive"`)]
	if strings.Contains(write, "confirm-step") || !strings.Contains(write, `<button type="button" class="btn sm on" disabled aria-pressed="true">Allow</button>`) {
		t.Errorf("an allowed row still offers Allow:\n%s", write)
	}
	if strings.Contains(body, "Discord conversations:") {
		t.Error("a Discord line with no relay running")
	}
}

// Allow's consequence is built from the facts (D11, F9): the account
// commands run as, whether they run in the sandbox, and that outside it the
// agent could answer its own requests.
func TestSafety_AllowConsequenceFromFacts(t *testing.T) {
	const self = "With this on, your agent could also change its own settings and answer its own requests on this page."
	for _, tc := range []struct {
		name        string
		facts       SafetyFacts
		exec, sbox  string
		execHasSelf bool
	}{
		{"SYSTEM", SafetyFacts{Account: `NT AUTHORITY\SYSTEM`, System: true},
			"Let your agent run commands and programs on this computer without asking you? Commands run as SYSTEM, which has full control of this Windows computer. " + self,
			"Commands run as SYSTEM, which has full control of this Windows computer. " + self, true},
		{"account", SafetyFacts{Account: "dylan"},
			"Commands run as dylan, with everything that account can do. " + self,
			"Commands run as dylan, with everything that account can do. " + self, true},
		{"sandboxed", SafetyFacts{Account: "dylan", Sandboxed: true},
			"Let your agent run commands and programs on this computer without asking you? Commands run inside the sandbox.",
			"Commands run as dylan, with everything that account can do. " + self, false},
		{"unknown", SafetyFacts{},
			"Commands run as the account Tenant runs under, with everything that account can do.", self, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := safetyServer(&fakeSafety{perms: safetyPerms()}, nil, tc.facts).modeRows()
			exec, sbox := rows[0].Allow, rows[7].Allow
			if !strings.Contains(exec, tc.exec) || strings.Contains(exec, self) != tc.execHasSelf {
				t.Errorf("Run commands: %q", exec)
			}
			if !strings.Contains(sbox, tc.sbox) {
				t.Errorf("Leave the sandbox: %q", sbox)
			}
			if web := rows[3].Allow; web != "Let your agent click, type and submit forms on websites without asking you?" {
				t.Errorf("Act on websites: %q", web)
			}
			// exec's session grant says the same as its Allow step.
			if got := rows[0].Consequence; !strings.HasPrefix(got, "Allowed without asking until Tenant restarts.") || strings.Contains(got, self) != tc.execHasSelf {
				t.Errorf("Run commands, granted: %q", got)
			}
		})
	}
	// Sandboxed, but Leave the sandbox allowed: a command the box refuses
	// re-runs outside it unasked, so exec is as good as unsandboxed.
	perms := safetyPerms()
	perms[7].Mode = "allow"
	rows := safetyServer(&fakeSafety{perms: perms}, nil, SafetyFacts{Account: "dylan", Sandboxed: true}).modeRows()
	if exec := rows[0].Allow; strings.Contains(exec, "inside the sandbox") || !strings.Contains(exec, "Commands run as dylan") {
		t.Errorf("Run commands, the sandbox allowed: %q", exec)
	}
	if sbox := rows[7].Consequence; !strings.HasSuffix(sbox, "without asking you. Commands run as dylan, with everything that account can do. "+self) {
		t.Errorf("Leave the sandbox, allowed: %q", sbox)
	}
	page := html.UnescapeString(get(t, safetyServer(&fakeSafety{perms: safetyPerms()}, nil, SafetyFacts{System: true}), "/safety").Body.String())
	if !strings.Contains(page, "<p>Let your agent run commands and programs on this computer without asking you? Commands run as SYSTEM") {
		t.Error("the confirm step doesn't show the facts-built consequence")
	}
}

// With the Discord relay running, each row says what Discord conversations
// do (read-only, to Access), and the queue says where Discord requests are
// answered (D6).
func TestSafety_DiscordLine(t *testing.T) {
	for _, running := range []bool{true, false} {
		s := safetyServer(&fakeSafety{perms: safetyPerms()}, &fakeQueue{}, SafetyFacts{})
		s.SetAccess(&fakeAccess{view: AccessView{RelayRunning: running, DiscordPerms: []PermissionRow{
			{Category: "exec", Mode: "deny"}, {Category: "write", Mode: "allow"}, {Category: "web", Mode: "ask"},
		}}})
		body := get(t, s, "/safety").Body.String()
		for _, want := range []string{
			`<div class="perm-other">Discord conversations: not allowed. <a href="/access">Change on Access</a></div>`,
			"Discord conversations: allowed without asking.", "Discord conversations: asks first.",
			"Requests raised in a Discord conversation are answered with the buttons on the Discord card.",
		} {
			if strings.Contains(body, want) != running {
				t.Errorf("relay running %v: %q shown %v", running, want, !running)
			}
		}
	}
}

// POST /safety/mode: the broker's own path with surface=dashboard; a strict
// mode check after the fail-closed confirm gate (D2); errors via flashFor.
func TestSafety_ModeForm(t *testing.T) {
	fs := &fakeSafety{perms: safetyPerms()}
	s := safetyServer(fs, nil, SafetyFacts{})
	msg, e := flash(t, safetyPost(t, s, "/safety/mode", url.Values{"category": {"exec"}, "mode": {"deny"}}, false))
	if e != "" || msg != "Run commands: your agent can't do this now." || !slices.Equal(fs.called(), []string{"dashboard|exec|deny"}) {
		t.Errorf("deny: msg %q err %q calls %q", msg, e, fs.called())
	}
	// Loosening needs the second click, whatever the spelling.
	for _, m := range []string{"allow", "ALLOW", " allow", "on", "always", "yes", ""} {
		rec := safetyPost(t, s, "/safety/mode", url.Values{"category": {"web"}, "mode": {m}}, false)
		if rec.Code != http.StatusPreconditionRequired {
			t.Errorf("mode %q without confirm = %d, want 428", m, rec.Code)
		}
	}
	// With it, only exactly ask, allow or deny (after trimming and case).
	fs.calls = nil
	_, e = flash(t, safetyPost(t, s, "/safety/mode", url.Values{"category": {"web"}, "mode": {"on"}, "confirm": {"yes"}}, false))
	if e != "Choose Ask, Allow or Deny. Nothing changed." || len(fs.called()) != 0 {
		t.Errorf("mode=on confirmed: err %q calls %q", e, fs.called())
	}
	msg, _ = flash(t, safetyPost(t, s, "/safety/mode", url.Values{"category": {"web"}, "mode": {"ALLOW"}, "confirm": {"yes"}}, false))
	if msg != "Act on websites: your agent goes ahead without asking." || !slices.Equal(fs.called(), []string{"dashboard|web|allow"}) {
		t.Errorf("ALLOW confirmed: msg %q calls %q", msg, fs.called())
	}
	_, e = flash(t, safetyPost(t, s, "/safety/mode", url.Values{"category": {"nope"}, "mode": {"ask"}}, false))
	if e != "That safety check doesn't exist. Nothing changed." {
		t.Errorf("unknown category: %q", e)
	}
	// "Ask me again": mode=ask, no confirm, and it says the grant ended.
	fs = &fakeSafety{perms: safetyPerms()}
	s = safetyServer(fs, nil, SafetyFacts{})
	msg, _ = flash(t, safetyPost(t, s, "/safety/mode", url.Values{"category": {"exec"}, "mode": {"ask"}}, false))
	if msg != "Run commands: your agent asks you again from now on." || !slices.Equal(fs.called(), []string{"dashboard|exec|ask"}) {
		t.Errorf("Ask me again: msg %q calls %q", msg, fs.called())
	}
	// Errors: a PlainError's text shows; a raw one doesn't, and is logged.
	c := installCapture(t)
	fs.err = errors.New("settings.main.json: permission denied")
	_, e = flash(t, safetyPost(t, s, "/safety/mode", url.Values{"category": {"exec"}, "mode": {"deny"}}, false))
	if e != "Couldn't change Run commands. Nothing changed." || !c.has("2201", "action=safety mode", "permission denied") {
		t.Errorf("raw error: flash %q, logged %v", e, c.lines())
	}
	fs.err = PlainError("Settings are read-only on this computer.")
	if _, e = flash(t, safetyPost(t, s, "/safety/mode", url.Values{"category": {"exec"}, "mode": {"deny"}}, false)); e != "Settings are read-only on this computer." {
		t.Errorf("plain error: flash %q", e)
	}
}

// "Keep them at Ask" is one tap: every New category set to ask, recorded
// from the dashboard, and the banner is gone.
func TestSafety_KeepNew(t *testing.T) {
	fs := &fakeSafety{perms: safetyPerms()}
	s := safetyServer(fs, nil, SafetyFacts{})
	msg, e := flash(t, safetyPost(t, s, "/safety/new", nil, false))
	want := []string{"dashboard|egress|ask", "dashboard|mcp|ask", "dashboard|sandbox|ask"}
	if e != "" || msg != "Kept at Ask: Share private data, Connect remote services, Leave the sandbox. Your agent asks you first." || !slices.Equal(fs.called(), want) {
		t.Errorf("msg %q err %q calls %q", msg, e, fs.called())
	}
	if body := get(t, s, "/safety").Body.String(); strings.Contains(body, "/safety/new") || strings.Contains(body, "tag-new") {
		t.Error("the New banner or tags outlived the choice")
	}
	if msg, _ = flash(t, safetyPost(t, s, "/safety/new", nil, false)); msg != "There are no new safety checks to review." {
		t.Errorf("again: %q", msg)
	}
}

// The queue: oldest first whatever order Pending gives, plain headlines,
// escaped detail, buttons only for safe IDs, the session step only where
// the grant is real and visible, and the pieces the live page relies on.
func TestSafety_QueueRender(t *testing.T) {
	fq := &fakeQueue{}
	fq.add(queueItems("line\n<script>alert(1)</script>")...)
	fq.add(PendingApproval{ID: `ap-"x`, Category: "exec", Action: "os_exec", Detail: "quoted", AgeSecs: 1, Origin: "local", SessionOK: true},
		PendingApproval{ID: "ap-long", Category: "web", Action: "web_click", Detail: strings.Repeat("x\n", 9), AgeSecs: 2, Origin: "local"},
		PendingApproval{ID: "ap-wide", Category: "exec", Action: "os_exec", Detail: "curl " + strings.Repeat("q", 500) + "; rm -rf ~/Documents", AgeSecs: 3, Origin: "local"})
	s := safetyServer(&fakeSafety{perms: safetyPerms()}, fq, SafetyFacts{Account: "dylan"})
	page := get(t, s, "/safety").Body.String()

	var order []string
	for _, m := range regexp.MustCompile(`id="q-([^"]+)"`).FindAllStringSubmatch(page, -1) {
		order = append(order, html.UnescapeString(m[1]))
	}
	if want := []string{"ap-alert", "ap-write", "ap-exec", "ap-rule", "ap-imsg", "ap-pair", "ap-wide", "ap-long", `ap-"x`}; !slices.Equal(order, want) {
		t.Errorf("order %q, want oldest first %q", order, want)
	}
	for id, want := range map[string]string{
		"ap-exec":  "<b>Your agent wants to run commands and programs on this computer</b>",
		"ap-pair":  "<b>Another computer wants to pair with this one</b>",
		"ap-alert": "<b>Tenant wants to look into an alert</b>",
		"ap-imsg":  `<div class="dim">imessage_send · from iMessage · waiting less than a minute</div>`,
	} {
		if it := item(t, page, id); !strings.Contains(it, want) {
			t.Errorf("%s lacks %q:\n%s", id, want, it)
		}
	}
	if strings.Contains(page, "<script>alert") || !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the detail isn't escaped")
	}
	if it := item(t, page, "ap-alert"); !strings.Contains(it, "alert_investigate · waiting 1 h") {
		t.Errorf("the dim line:\n%s", it)
	}
	if it := item(t, page, "ap-&#34;x"); strings.Contains(it, "<button") {
		t.Errorf("an ID with a quote got buttons:\n%s", it)
	}
	if it := item(t, page, "ap-long"); !strings.Contains(it, "This request is 10 lines long. Read all of it before you allow it.") {
		t.Errorf("no long-request note:\n%s", it)
	}
	// One long line scrolls its tail out of sight just the same.
	if it := item(t, page, "ap-wide"); !strings.Contains(it, "This request is long. Read all of it before you allow it.") {
		t.Errorf("no note on a long one-line request:\n%s", it)
	}
	if strings.Contains(item(t, page, "ap-exec"), "q-long") {
		t.Error("a short request has the long-request note")
	}
	// Session step: write (local, asked by its mode) only. Not exec (not
	// sandboxed), not a rule-forced prompt, not iMessage, not pairing or an
	// alert.
	for _, id := range []string{"ap-exec", "ap-write", "ap-rule", "ap-imsg", "ap-pair", "ap-alert"} {
		if got := strings.Contains(item(t, page, id), "Allow for the rest of this session"); got != (id == "ap-write") {
			t.Errorf("%s offers the session step: %v", id, got)
		}
	}
	w := item(t, page, "ap-write")
	for _, want := range []string{
		`<details class="confirm-step go q-session" data-preserve-attr="open">`,
		"Your agent can create and change files on this computer without asking again until you turn it off on this page or Tenant restarts.",
		`data-indicator:_sending data-attr:disabled="$_sending"`,
		`<button type="button" class="btn" data-indicator:_sending`, // Deny, left
	} {
		if !strings.Contains(w, want) {
			t.Errorf("ap-write lacks %q:\n%s", want, w)
		}
	}
	// Pairing's Allow once is a step whose consequence points at the PIN.
	if p := item(t, page, "ap-pair"); !strings.Contains(p, `<details class="confirm-step go" data-preserve-attr="open"><summary class="btn on"><span class="c-open">Allow once</span>`) ||
		!strings.Contains(html.UnescapeString(p), "Only allow if the person pairing told you the same PIN") {
		t.Errorf("pairing's Allow once isn't a confirm step:\n%s", p)
	}
	// Sandboxed, exec gets the step too, but Leave the sandbox never does:
	// its grant re-runs every refused command outside the box, unasked.
	fq.add(PendingApproval{ID: "ap-sbox", Category: "sandbox", Action: "os_exec_unsandboxed", Detail: "cat ~/.ssh/id_ed25519", AgeSecs: 1, Origin: "local", SessionOK: true})
	s = safetyServer(&fakeSafety{perms: safetyPerms()}, fq, SafetyFacts{Sandboxed: true})
	page = get(t, s, "/safety").Body.String()
	if !strings.Contains(item(t, page, "ap-exec"), "Allow for the rest of this session") {
		t.Error("sandboxed exec has no session step")
	}
	if strings.Contains(item(t, page, "ap-sbox"), "Allow for the rest of this session") {
		t.Error("Leave the sandbox offers the session step")
	}
	if _, e := flash(t, safetyPost(t, s, "/safety/approvals/ap-sbox?confirm=yes&decision=approve_session", nil, false)); e != notAvailable || len(fq.calls()) != 0 {
		t.Errorf("a session grant for Leave the sandbox: err %q calls %q", e, fq.calls())
	}
	// Leave the sandbox allowed makes exec unsandboxed: no step.
	perms := safetyPerms()
	perms[7].Mode = "allow"
	page = get(t, safetyServer(&fakeSafety{perms: perms}, fq, SafetyFacts{Sandboxed: true}), "/safety").Body.String()
	if strings.Contains(item(t, page, "ap-exec"), "Allow for the rest of this session") {
		t.Error("exec offers the session step with Leave the sandbox allowed")
	}
	// No modes card, no step: the grant couldn't be seen or ended here.
	page = get(t, safetyServer(nil, fq, SafetyFacts{Sandboxed: true}), "/safety").Body.String()
	if strings.Contains(page, "Allow for the rest of this session") {
		t.Error("a session step with no modes card to end it")
	}
}

// The decision URLs: escaped, and carrying confirm=yes only from inside a
// confirm step.
func TestSafety_DecideURL(t *testing.T) {
	for _, tc := range []struct {
		it       ApprovalItem
		decision string
		want     string
	}{
		{ApprovalItem{ID: "ap-1"}, "approve", "/safety/approvals/ap-1?decision=approve"},
		{ApprovalItem{ID: "ap-1"}, "deny", "/safety/approvals/ap-1?decision=deny"},
		{ApprovalItem{ID: "ap-1"}, "approve_session", "/safety/approvals/ap-1?confirm=yes&decision=approve_session"},
		{ApprovalItem{ID: "ap-1", Pairing: true}, "approve", "/safety/approvals/ap-1?confirm=yes&decision=approve"},
		{ApprovalItem{ID: "ap-1", Pairing: true}, "deny", "/safety/approvals/ap-1?decision=deny"},
		{ApprovalItem{ID: "a/'b"}, "deny", "/safety/approvals/a%2F%27b?decision=deny"},
	} {
		if got := tc.it.DecideURL(tc.decision); got != tc.want {
			t.Errorf("%+v %s = %q, want %q", tc.it, tc.decision, got, tc.want)
		}
	}
}

// POST /safety/approvals/{id}: Datastar gets the queue and the note in
// place; a lost race, a refused choice or an unknown one changes nothing and
// never reaches Decide when the page can tell.
func TestSafety_Decide(t *testing.T) {
	newQ := func() (*Server, *fakeQueue) {
		fq := &fakeQueue{}
		fq.add(queueItems("x")...)
		return safetyServer(&fakeSafety{perms: safetyPerms()}, fq, SafetyFacts{Account: "dylan"}), fq
	}
	decide := func(s *Server, id, decision string, confirm, datastar bool) *httptest.ResponseRecorder {
		q := url.Values{"decision": {decision}}
		if confirm {
			q.Set("confirm", "yes")
		}
		return safetyPost(t, s, "/safety/approvals/"+url.PathEscape(id)+"?"+q.Encode(), nil, datastar)
	}

	s, fq := newQ()
	rec := decide(s, "ap-exec", "approve", false, true)
	body := rec.Body.String()
	if !strings.Contains(body, "event: datastar-patch-elements") || !strings.Contains(body, `id="approval-queue"`) ||
		!strings.Contains(body, `<div id="approval-note"><div class="banner ok" role="status">Allowed once. Your agent is carrying on.</div></div>`) ||
		strings.Contains(body, `id="q-ap-exec"`) || !slices.Equal(fq.calls(), []string{"ap-exec|approve"}) {
		t.Errorf("approve: calls %q\n%s", fq.calls(), body)
	}
	if msg, _ := flash(t, decide(s, "ap-write", "deny", false, false)); msg != "Denied. Your agent was told no." {
		t.Errorf("deny without Datastar: %q", msg)
	}

	// Refused before Decide: no Always on the web, unknown values, and a
	// session grant where it isn't offered.
	s, fq = newQ()
	for _, tc := range []struct{ id, decision string }{
		{"ap-write", "approve_always"}, {"ap-write", "bogus"}, {"ap-write", "APPROVE"},
		{"ap-exec", "approve_session"}, // exec, not sandboxed
		{"ap-imsg", "approve_session"}, // iMessage's grant would live where this page can't show it
		{"ap-rule", "approve_session"}, // a tool rule asked: "session" is only this time
		{"ap-pair", "approve_session"},
	} {
		if _, e := flash(t, decide(s, tc.id, tc.decision, true, false)); e != notAvailable {
			t.Errorf("%s %s: err %q", tc.id, tc.decision, e)
		}
	}
	if len(fq.calls()) != 0 {
		t.Errorf("Decide called for a refused choice: %q", fq.calls())
	}

	// The confirm gate: the session grant and pairing's Allow need the
	// second click; Deny never does.
	for _, tc := range []struct {
		id, decision string
		code         int
	}{
		{"ap-write", "approve_session", http.StatusPreconditionRequired},
		{"ap-pair", "approve", http.StatusPreconditionRequired},
		{"ap-write", "bogus", http.StatusPreconditionRequired},
		{"ap-pair", "deny", http.StatusSeeOther},
	} {
		if rec := decide(s, tc.id, tc.decision, false, false); rec.Code != tc.code {
			t.Errorf("%s %s without confirm = %d, want %d", tc.id, tc.decision, rec.Code, tc.code)
		}
	}
	if msg, _ := flash(t, decide(s, "ap-write", "approve_session", true, false)); msg != "Allowed for this session." {
		t.Errorf("session: %q", msg)
	}
	if msg, _ := flash(t, decide(s, "ap-alert", "approve", false, false)); msg != "Allowed once. Your agent is carrying on." {
		t.Errorf("an alert's Allow once: %q", msg)
	}
	if want := []string{"ap-pair|deny", "ap-write|approve_session", "ap-alert|approve"}; !slices.Equal(fq.calls(), want) {
		t.Errorf("calls %q, want %q", fq.calls(), want)
	}

	// Answered elsewhere: gone from Pending, or Decide lost the race.
	if _, e := flash(t, decide(s, "ap-write", "approve", false, false)); e != alreadyAnswered {
		t.Errorf("already gone: %q", e)
	}
	c := installCapture(t)
	s, fq = newQ()
	fq.err = errors.New(`approval "ap-exec" not found (already resolved or expired)`)
	body = decide(s, "ap-exec", "deny", false, true).Body.String()
	if !strings.Contains(html.UnescapeString(body), `<div class="banner err" role="alert">`+alreadyAnswered) || strings.Contains(body, "not found") ||
		!c.has("2201", "action=approval decide") {
		t.Errorf("a lost race:\n%s", body)
	}
}

// Chat carries the queue's targets above the composer, filled by /events.
func TestSafety_ChatHasQueue(t *testing.T) {
	body := get(t, New(Config{}, nil, nil, nil, nil, nil), "/chat").Body.String()
	q := strings.Index(body, `<div class="chat-q"><div id="approval-queue"></div><div id="approval-note"></div></div>`)
	if q < 0 || q < strings.Index(body, `id="chat-log"`) || q > strings.Index(body, `class="composer"`) {
		t.Errorf("no queue between the transcript and the composer:\n%s", body)
	}
}

// The page's states (DESIGN.md §6).
func TestSafety_States(t *testing.T) {
	fq := &fakeQueue{}
	withQ := safetyServer(&fakeSafety{perms: safetyPerms()}, fq, SafetyFacts{})
	for _, tc := range []struct {
		state, path string
		s           *Server
		want        string
	}{
		{"off", "/safety", safetyServer(nil, nil, SafetyFacts{}), `<div class="card state-off">Approvals and safety settings aren&#39;t available on this Tenant.</div>`},
		{"empty", "/safety", withQ, `<div class="q-empty"><div class="empty">Nothing is waiting for you. When your agent needs your OK, it appears here right away.</div></div>`},
		{"error", "/safety?err=Nope.", withQ, `<div class="banner err" role="alert">Nope.</div>`},
		{"success", "/safety?msg=Saved.", withQ, `<div class="banner ok" role="status">Saved.</div>`},
	} {
		if body := get(t, tc.s, tc.path).Body.String(); !strings.Contains(body, tc.want) {
			t.Errorf("%s: lacks %s", tc.state, tc.want)
		}
	}
	// Busy: a waiting request shows on every page's strip, linking here.
	fq.add(PendingApproval{ID: "ap-1", Category: "exec"})
	if got := uiStatus(t, withQ); !strings.Contains(got, `1 action is waiting for your OK. <a href="/safety">Answer it</a>`) {
		t.Errorf("busy: the strip says %s", got)
	}
}
