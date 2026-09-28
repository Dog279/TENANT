package dashboard

// ui_adopt_pages_test.go checks WP-ux-adopt (TEN-211 on the pages that
// existed before Dashboard v2, and TEN-213 F2/F3): every route §2.1 lists
// confirms with every spelling of "more", secrets never come from a URL,
// secret inputs are write-only, each page shows its states, and no control's
// raw error reaches a page.

import (
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/logs/logapi"
)

// adoptedConfirms is plan §2.1's list for the existing routes: the form a
// real page posts (without confirm), the field whose value decides, and the
// values that skip the step. safe nil means the route always confirms.
var adoptedConfirms = []struct {
	route string
	form  url.Values
	key   string
	safe  []string
}{
	{route: "/skills/forget", form: url.Values{"name": {"x"}}},
	{route: "/mcp/remove", form: url.Values{"target": {"svc"}}},
	{route: "/integrations/disconnect", form: url.Values{"integration": {"gsuite"}}},
	{route: "/settings/keys/remove", form: url.Values{"service": {"openai"}}},
	{route: "/cron/j1/delete"},
	{route: "/access/imessage/clear"},
	{route: "/logs/clear", form: url.Values{"log": {"cron"}}},
	{route: "/access/discord/operator", form: url.Values{"operator": {"4242"}}},
	{route: "/access/imessage/allow", form: url.Values{"handle": {"a@b.c"}}},
	{route: "/models/use", form: url.Values{"name": {"m"}}},
	{route: "/models/remove", form: url.Values{"name": {"m"}}},
	{route: "/tools/rules/remove", form: url.Values{"pattern": {"x"}, "mode": {"allow"}}}, // decided by the rule itself (REVSEC-1)

	{route: "/skills/trust", form: url.Values{"name": {"x"}}, key: "trusted", safe: []string{"", "false"}},
	{route: "/skills/auto", key: "mode", safe: []string{"off"}},
	{route: "/mcp/trust", form: url.Values{"target": {"svc"}}, key: "mode", safe: []string{"ask", "deny"}},
	{route: "/integrations/github/repos/mode", form: url.Values{"repo": {"a/b"}}, key: "mode", safe: []string{"read"}},
	{route: "/cron/exec", key: "enabled", safe: []string{"", "false"}},
	{route: "/posture", key: "allow_send", safe: []string{"", "false"}},
	{route: "/tools/rules/add", form: url.Values{"pattern": {"x"}}, key: "mode", safe: []string{"ask", "deny"}},
	{route: "/access/discord/exec", key: "on", safe: []string{"", "false", "off"}},
	{route: "/access/discord/enable", key: "on", safe: []string{"", "false", "off"}},
	{route: "/access/imessage/responder", key: "on", safe: []string{"", "false", "off"}},
	{route: "/access/imessage/permission", form: url.Values{"category": {"exec"}}, key: "mode", safe: []string{"ask", "deny"}},
	{route: "/access/discord/permission", form: url.Values{"category": {"exec"}}, key: "mode", safe: []string{"ask", "deny"}},
}

// moreAliases are the spellings of "let it do more" a handler's parser might
// accept; each must meet the confirm step on a conditional route.
var moreAliases = []string{"allow", "ALLOW", " allow", "on", "ON", "true", "TRUE", " true", "t", "1", "yes", "always", "write", "WRITE", "trusted", "maybe"}

// Every listed route answers 428 without its confirm step, and its handler
// never runs (with nil controls a reached handler answers 303); with
// confirm=yes, or a safe value, it runs.
func TestAdoptedRoutes_NeedConfirm(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	post := func(route string, form url.Values) int {
		r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		return rec.Code
	}
	with := func(base url.Values, kv ...string) url.Values {
		v := url.Values{}
		for k, vs := range base {
			v[k] = vs
		}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	for _, c := range adoptedConfirms {
		if c.safe == nil {
			if code := post(c.route, c.form); code != http.StatusPreconditionRequired {
				t.Errorf("%s without confirm = %d, want 428", c.route, code)
			}
			if code := post(c.route, with(c.form, "confirm", "yes")); code != http.StatusSeeOther {
				t.Errorf("%s with confirm=yes = %d, want the handler (303)", c.route, code)
			}
			continue
		}
		for _, v := range moreAliases {
			if isSafe := func() bool {
				for _, sv := range c.safe {
					if strings.EqualFold(strings.TrimSpace(v), sv) {
						return true
					}
				}
				return false
			}(); isSafe {
				continue
			}
			if code := post(c.route, with(c.form, c.key, v)); code != http.StatusPreconditionRequired {
				t.Errorf("%s %s=%q without confirm = %d, want 428", c.route, c.key, v, code)
			}
			if code := post(c.route, with(c.form, c.key, v, "confirm", "yes")); code != http.StatusSeeOther {
				t.Errorf("%s %s=%q with confirm=yes = %d, want 303", c.route, c.key, v, code)
			}
		}
		if code := post(c.route, c.form); (code == http.StatusPreconditionRequired) == contains(c.safe, "") {
			t.Errorf("%s with %s missing = %d; the step is skipped only when \"\" is safe", c.route, c.key, code)
		}
		for _, v := range c.safe {
			if code := post(c.route, with(c.form, c.key, v)); code != http.StatusSeeOther {
				t.Errorf("%s %s=%q (safe) = %d, want no step (303)", c.route, c.key, v, code)
			}
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// A confirmed POST that reaches a real control does the thing; the one
// without its step leaves the control untouched.
func TestAdoptedRoutes_ControlUntouchedWithoutConfirm(t *testing.T) {
	fc := &fakeCronCtl{jobs: []CronJobView{{ID: "j1"}}}
	fs := &fakeSkillCtl{}
	fr := &fakeRules{}
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetCron(fc)
	s.SetSkills(fs)
	s.SetRules(fr)
	for _, p := range []struct{ route, body string }{
		{"/cron/j1/delete", ""}, {"/cron/exec", "enabled=true"},
		{"/skills/forget", "name=x"}, {"/skills/trust", "name=x&trusted=true"}, {"/skills/auto", "mode=on"},
		{"/tools/rules/add", "pattern=x&mode=allow"}, {"/tools/rules/remove", "pattern=x&mode=deny"},
	} {
		if rec := submitForm(t, s, p.route, p.body); rec.Code != http.StatusPreconditionRequired {
			t.Errorf("%s = %d, want 428", p.route, rec.Code)
		}
	}
	if len(fc.removeCalls)+len(fc.execCalls)+len(fs.forgetCalls)+len(fs.trustCalls)+len(fr.sets)+len(fr.removes) != 0 || fs.autoSet != "" {
		t.Errorf("a control was called without its confirm step: cron %v %v, skills %v %v %q, rules %v %v",
			fc.removeCalls, fc.execCalls, fs.forgetCalls, fs.trustCalls, fs.autoSet, fr.sets, fr.removes)
	}
}

// F2: a secret in the query string is refused before any control or
// comparison sees it, the refusal is logged (KeyInURL), and the secret is
// never echoed into the redirect.
func TestAdopt_SecretsRefusedFromQuery(t *testing.T) {
	c := installCapture(t)
	secrets := &fakeSecrets{}
	models := &fakeModelCtl{}
	integ := &fakeIntegrationsCtl{fields: map[string][]IntegrationField{"brave": {{Key: "api_key", Secret: true, Required: true}}}}
	s := New(Config{Auth: "s3cret"}, nil, nil, nil, nil, nil)
	s.SetSecrets(secrets)
	s.SetModels(models)
	s.SetIntegrations(integ)
	for _, p := range []struct{ route, body string }{
		{"/settings/keys/set?value=sk-leak", "service=openai&value=sk-leak"},
		{"/models/add?key=sk-leak", "kind=anthropic&key=sk-leak"},
		{"/integrations/save?api_key=sk-leak", "integration=brave&api_key=sk-leak"},
	} {
		rec := submitForm(t, s, p.route, p.body)
		loc, _ := url.QueryUnescape(rec.Header().Get("Location"))
		if rec.Code != http.StatusSeeOther || !strings.Contains(loc, secretInURL) || strings.Contains(loc, "sk-leak") {
			t.Errorf("%s = %d %q, want 303 with %q and no secret", p.route, rec.Code, loc, secretInURL)
		}
	}
	if len(secrets.setCalls)+len(models.addCalls)+len(integ.configCalls) != 0 {
		t.Errorf("a control saw a secret from the URL: %v %v %v", secrets.setCalls, models.addCalls, integ.configCalls)
	}

	// Sign-in: even the right key in the URL signs nobody in.
	rec := httptest.NewRecorder()
	s.handleLogin(rec, postForm("/auth/login?token=s3cret", url.Values{"token": {"s3cret"}}))
	if rec.Code != http.StatusBadRequest || len(rec.Result().Cookies()) != 0 || strings.Contains(rec.Body.String(), "s3cret") {
		t.Errorf("sign-in with the key in the URL = %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
	if n := strings.Count(strings.Join(c.lines(), "\n"), "2203 "); n != 4 {
		t.Errorf("KeyInURL records = %d, want 4: %q", n, c.lines())
	}
}

// F3: a failed save's reason, which can quote the secret, goes to the log
// masked, and the page shows a fixed message.
func TestAdopt_SecretErrorsStayOutOfURLs(t *testing.T) {
	models := &failingModelCtl{fakeModelCtl{}}
	integ := &failingIntegrations{fakeIntegrationsCtl{fields: map[string][]IntegrationField{"brave": {{Key: "api_key", Secret: true}}}}}
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetModels(models)
	s.SetIntegrations(integ)
	for _, p := range []struct{ route, body string }{
		{"/models/add", "kind=anthropic&key=sk-leak"},
		{"/integrations/save", "integration=brave&api_key=sk-leak"},
	} {
		loc, _ := url.QueryUnescape(submitForm(t, s, p.route, p.body).Header().Get("Location"))
		if !strings.Contains(loc, "err=Couldn't") || strings.Contains(loc, "sk-leak") || strings.Contains(loc, "bad key") {
			t.Errorf("%s → %q, want a fixed message only", p.route, loc)
		}
	}
}

type failingModelCtl struct{ fakeModelCtl }

func (failingModelCtl) AddCloud(_, key string) (string, error) {
	return "", errors.New("bad key " + key)
}

type failingIntegrations struct{ fakeIntegrationsCtl }

func (*failingIntegrations) Configure(_ string, v map[string]string) (string, error) {
	return "", errors.New("bad key " + v["api_key"])
}

// Secret inputs are write-only: type=password, no value, no autofill.
func TestAdopt_SecretInputsAreWriteOnly(t *testing.T) {
	s := harnessServer(t, Config{}, populated, nil)
	s.SetIntegrations(&fakeIntegrationsCtl{
		list:   []IntegrationView{{ID: "brave", Label: "Brave"}},
		fields: map[string][]IntegrationField{"brave": {{Key: "api_key", Secret: true}}},
	})
	pw := regexp.MustCompile(`<input[^>]*type="password"[^>]*>`)
	for _, p := range []string{"/models", "/integrations", "/settings/keys"} {
		inputs := pw.FindAllString(get(t, s, p).Body.String(), -1)
		if len(inputs) == 0 {
			t.Errorf("%s has no password input", p)
		}
		for _, in := range inputs {
			if strings.Contains(in, "value=") || !strings.Contains(in, `autocomplete="`) || strings.Contains(in, `autocomplete="on"`) {
				t.Errorf("%s: %s is not write-only", p, in)
			}
		}
	}
	rec := httptest.NewRecorder()
	authedServer().handleSettingsPage(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if in := pw.FindString(rec.Body.String()); !strings.Contains(in, `name="token"`) || strings.Contains(in, "value=") || !strings.Contains(in, `autocomplete="off"`) {
		t.Errorf("the sign-in key input: %q", in)
	}
}

// Each existing page shows each state that can happen to it (DESIGN.md §6).
// off: a nil control; empty: a control with nothing in it; error and
// success: the flash, with its role; busy: the status strip every page
// polls; loading: where a result arrives after the page.
func TestExistingPages_States(t *testing.T) {
	emptyMem := &fakeMem{byID: map[int64]*fmFact{}, tombstoned: map[int64]bool{}, supersededBy: map[int64]int64{}, prov: map[int64][]EpisodeView{}}
	withEmpty := func(set func(s *Server)) *Server {
		s := New(Config{}, nil, &fakeTools{}, emptyMem, nil, nil)
		if set != nil {
			set(s)
		}
		return s
	}
	const (
		off    = `class="card state-off"`
		empty  = `class="empty"`
		errBar = `<div class="banner err" role="alert">Nope.</div>`
		okBar  = `<div class="banner ok" role="status">Saved.</div>`
		// The strip starts idle and fills from /ui/status (busy, below).
		statusWrap = `data-init="@get('/ui/status')" data-on-interval__duration.20s="@get('/ui/status')"`
	)
	for _, c := range []struct {
		route string
		off   bool            // a nil control renders ui-off
		empty func(s *Server) // installs an empty control; nil = no empty state
		flash bool            // ?msg= and ?err= show
		errOK bool            // ?err= only (the page has no success flash)
	}{
		{route: "/", empty: func(*Server) {}},
		{route: "/logs", off: true, flash: true},
		{route: "/memory", off: true},
		{route: "/memory/soul", off: true, empty: func(*Server) {}, errOK: true},
		{route: "/memory/facts", off: true, empty: func(*Server) {}, errOK: true},
		{route: "/memory/facts/1", off: true, errOK: true},
		{route: "/memory/facts/removed", off: true, empty: func(*Server) {}},
		{route: "/memory/facts/temporal", off: true, empty: func(*Server) {}},
		{route: "/memory/provenance", off: true, empty: func(*Server) {}},
		{route: "/skills", off: true, empty: func(s *Server) { s.SetSkills(&fakeSkillCtl{}) }, flash: true},
		{route: "/eval", off: true, empty: func(s *Server) { s.SetEval(&fakeEvalCtl{}) }, flash: true},
		{route: "/tools", empty: func(s *Server) { s.tools = &fakeTools{} }, flash: true},
		{route: "/mcp", off: true, empty: func(s *Server) { s.SetMCP(&fakeMCPCtl{}) }, flash: true},
		{route: "/cron", off: true, empty: func(s *Server) { s.SetCron(&fakeCronCtl{}) }, flash: true},
		{route: "/access", off: true, empty: func(s *Server) { s.SetAccess(&fakeAccess{view: AccessView{IMessageAvailable: true}}) }, flash: true},
		{route: "/models", off: true, empty: func(s *Server) { s.SetModels(&fakeModelCtl{}) }, flash: true},
		{route: "/integrations", off: true, empty: func(s *Server) { s.SetIntegrations(&fakeIntegrationsCtl{}) }, flash: true},
		{route: "/settings/keys", off: true, empty: func(s *Server) { s.SetSecrets(&fakeSecrets{}) }, flash: true},
	} {
		nilServer := New(Config{}, nil, nil, nil, nil, nil)
		if body := get(t, nilServer, c.route).Body.String(); c.off != strings.Contains(body, off) {
			t.Errorf("%s with a nil control: ui-off shown = %v, want %v", c.route, !c.off, c.off)
		} else if !strings.Contains(body, statusWrap) {
			t.Errorf("%s lacks the status strip", c.route)
		}
		if c.empty != nil {
			if body := get(t, withEmpty(c.empty), c.route).Body.String(); !strings.Contains(body, empty) {
				t.Errorf("%s with nothing in it lacks ui-empty", c.route)
			}
		}
		if c.flash || c.errOK {
			s := withEmpty(c.empty)
			switch c.route {
			case "/logs":
				s = harnessServer(t, Config{}, populated, nil)
			case "/memory/facts/1":
				s = New(Config{}, nil, nil, &inspFake{detail: sampleDetail()}, nil, nil)
			}
			body := get(t, s, c.route+"?msg=Saved.&err=Nope.").Body.String()
			if !strings.Contains(body, errBar) || (c.flash && !strings.Contains(body, okBar)) {
				t.Errorf("%s: the flash doesn't show with its roles", c.route)
			}
		}
	}

	// busy: the strip every page loads says what the agent is busy with.
	gate := newBlockRunner()
	gate.gate, gate.queue = true, agent.TurnQueueStatus{ActiveSource: "cron"}
	if got := uiStatus(t, New(Config{}, gate, nil, nil, nil, nil)); !strings.Contains(got, "Your agent is busy with a scheduled job or an alert") {
		t.Errorf("the busy strip: %s", got)
	}

	// loading: a quality check in progress, and the Logs count.
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetEval(&fakeEvalCtl{sched: EvalScheduleView{Running: true, RunDone: 3, RunTotal: 10}})
	if body := get(t, s, "/eval").Body.String(); !strings.Contains(body, `<span class="loading">Check in progress</span>`) ||
		!strings.Contains(body, `<progress class="evalbar" value="3" max="10">`) || !strings.Contains(body, `data-on-interval__duration.20s="window.location.reload()"`) {
		t.Errorf("a running check should show loading and refresh itself:\n%s", body)
	}
	if body := get(t, harnessServer(t, Config{}, populated, nil), "/logs?q=log:cron").Body.String(); !strings.Contains(body, `class="lg-count dim loading"`) {
		t.Error("the Logs count should show loading until it arrives")
	}
	// error on a read: the failing store shows a plain alert.
	if body := get(t, harnessServer(t, Config{}, failing, nil), "/memory/facts").Body.String(); !strings.Contains(body, `role="alert">`+html.EscapeString(memReadFailed)) {
		t.Errorf("a failing memory read should show %q", memReadFailed)
	}
}

// No control's own error text reaches any page (D21), even when every read
// that can fail does.
func TestRendered_NoRawControlErrors(t *testing.T) {
	for _, mode := range []fakeMode{populated, failing} {
		for route, page := range renderAllPages(t, mode) {
			for _, raw := range []string{rawStoreClosed, rawProviderErr, rawNoGoal, rawGoalCleared, rawNoHistory, rawSavedToWiki, "invalid_grant"} {
				if strings.Contains(page, raw) {
					t.Errorf("%s shows the raw error %q", route, raw)
				}
			}
		}
	}
}

// Home answers "is anything waiting for my OK?", no included (D13).
func TestHome_WaitingForYourOK(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if body := get(t, s, "/").Body.String(); !strings.Contains(body, `<a href="/safety" class="card stat linkcard">`) ||
		!strings.Contains(body, `<div class="stat-label">Waiting for your OK</div>`) || !strings.Contains(body, `<span class="dim">nothing</span>`) {
		t.Errorf("home with nothing waiting:\n%s", body)
	}
	s.SetApprovals(&fakeApprovals{pending: []PendingApproval{{ID: "a"}, {ID: "b"}}})
	if body := get(t, s, "/").Body.String(); !strings.Contains(body, `<span class="needs">2</span>`) {
		t.Errorf("home with 2 waiting:\n%s", body)
	}
}

// Names from outside (a skill the agent saved, a remote service's tools)
// are shown in words and can't aim a form elsewhere.
func TestAdopt_PlainNamesAndSafeActions(t *testing.T) {
	const evil = "../../settings/keys/openai/remove?x="
	sk := New(Config{}, nil, nil, nil, nil, nil)
	sk.SetSkills(&fakeSkillCtl{skills: []SkillView{{Name: evil, Status: "proposed"}, {Name: evil, Status: "live", Untrusted: true}, {Name: evil, Status: "live", Enabled: true}}})
	page := get(t, sk, "/skills").Body.String()
	for _, m := range regexp.MustCompile(`action="([^"]*)"`).FindAllStringSubmatch(page, -1) {
		if strings.Contains(html.UnescapeString(m[1]), "settings") {
			t.Errorf("a skill's name aims a form: action=%q", m[1])
		}
	}
	if n := strings.Count(page, `<input type="hidden" name="name" value="../../settings/keys/openai/remove?x=">`); n != 7 {
		t.Errorf("the skill's name is a form field in %d forms, want 7 (accept, dismiss, trust, remove, turn off, stop trusting, remove)", n)
	}
	for in, want := range map[string]string{
		"mcp:127.0.0.1:9000:*":          "every tool on 127.0.0.1:9000",
		"mcp:127.0.0.1:9000:write_note": "write_note on 127.0.0.1:9000",
		"delete_*":                      "delete_*",
		"mcp:odd":                       "mcp:odd",
	} {
		if got := (ToolRuleRow{Pattern: in}).Plain(); got != want {
			t.Errorf("Plain(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (ToolInfo{Name: "mcp:127.0.0.1:9000:write_note"}).Label(); got != "write_note" {
		t.Errorf("tool label = %q", got)
	}
	for in, want := range map[string]string{"": "Other tools", "mcp:mcp.atlassian.com": "mcp.atlassian.com (remote service)", "web": "web"} {
		if got := groupLabel(in); got != want {
			t.Errorf("groupLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// A new repository can't be made writable past the confirm step, whether
// by mode=write or a ":write" suffix the control would read as write.
func TestAdopt_GitHubAddWriteConfirms(t *testing.T) {
	gh := &fakeGitHubRepos{}
	s := ghServer(gh)
	if rec := ghPost(t, s, "/integrations/github/repos/add", "repo=a/b&mode=write"); rec.Code != http.StatusPreconditionRequired {
		t.Errorf("add with mode=write and no confirm = %d, want 428", rec.Code)
	}
	for _, repo := range []string{"a/b:w", "a/b:write", "a/b:RW", "a/b:w&confirm=yes"} {
		loc, _ := url.QueryUnescape(ghPost(t, s, "/integrations/github/repos/add", "repo="+strings.Replace(url.QueryEscape(repo), "%26confirm%3Dyes", "&confirm=yes", 1)).Header().Get("Location"))
		if !strings.Contains(loc, "err=Enter the repository as owner/name.") {
			t.Errorf("add %q → %q, want it refused", repo, loc)
		}
	}
	if len(gh.addCalls) != 0 {
		t.Fatalf("Add ran without the confirm step: %v", gh.addCalls)
	}
	ghPost(t, s, "/integrations/github/repos/add", "repo=a/b&mode=write&confirm=yes")
	ghPost(t, s, "/integrations/github/repos/add", "repo=c/d")
	if strings.Join(gh.addCalls, " ") != "a/b:write c/d:read" {
		t.Errorf("Add calls = %v, want a/b writable (confirmed) and c/d watch-only", gh.addCalls)
	}
}

// Removing the last iMessage contact cuts everyone off, so the server
// confirms it; removing one of several doesn't.
func TestAdopt_LastIMessageContactConfirms(t *testing.T) {
	fa := &fakeAccess{view: AccessView{IMessageAvailable: true, Handles: []string{"a@b.c"}, SingleHandle: true}}
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetAccess(fa)
	if rec := submitForm(t, s, "/access/imessage/deny", "handle=a@b.c"); rec.Code != http.StatusPreconditionRequired || len(fa.calls) != 0 {
		t.Errorf("removing the last contact without confirm = %d, calls %v", rec.Code, fa.calls)
	}
	submitForm(t, s, "/access/imessage/deny", "handle=a@b.c&confirm=yes")
	fa.view.SingleHandle = false
	submitForm(t, s, "/access/imessage/deny", "handle=a@b.c")
	if strings.Join(fa.calls, " ") != "deny=a@b.c deny=a@b.c" {
		t.Errorf("calls = %v", fa.calls)
	}
}

// A switch to a model that can't answer yet says so, plainly.
func TestAdopt_ModelSwitchWarns(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetModels(&warnModelCtl{})
	loc, _ := url.QueryUnescape(submitForm(t, s, "/models/use", "name=m&confirm=yes").Header().Get("Location"))
	if !strings.Contains(loc, "err=Switched to m, but it isn't ready to answer.") || strings.Contains(loc, "UNREACHABLE") {
		t.Errorf("switch to an unreachable model → %q", loc)
	}
}

type warnModelCtl struct{ fakeModelCtl }

func (*warnModelCtl) Use(string) (string, error) {
	return `⚠ switched to "m" but the endpoint is UNREACHABLE at http://x (refused)`, nil
}

// The Logs page's forms show the log book's own refusals as they are, and
// a fixed message for anything else (D21).
func TestAdopt_LogsChangeErrorsArePlain(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	back := func(err error) string {
		rec := httptest.NewRecorder()
		s.logsDone(rec, httptest.NewRequest(http.MethodPost, "/logs/clear", nil), url.Values{}, "", err, nil)
		u, _ := url.Parse(rec.Header().Get("Location"))
		return u.Query().Get("err")
	}
	if got := back(&logapi.Error{Status: http.StatusForbidden, Msg: "the Security log is never cleared"}); got != "the Security log is never cleared" {
		t.Errorf("a refusal → %q", got)
	}
	if got := back(errors.New(rawStoreClosed)); strings.Contains(got, rawStoreClosed) || !strings.HasPrefix(got, "Couldn't make that change.") {
		t.Errorf("a store error → %q", got)
	}
}
