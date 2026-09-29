package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeAccess records calls and returns a canned view (TEN-208).
type fakeAccess struct {
	view      AccessView
	calls     []string // ordered, e.g. "allow=+1555", "relay-enable=true"
	denyAllow bool     // when true, IMessageAllow errors (flash-error path)
}

func (f *fakeAccess) View() AccessView { return f.view }
func (f *fakeAccess) IMessageAllow(h string) (string, bool, error) {
	f.calls = append(f.calls, "allow="+h)
	if f.denyAllow {
		return "", false, fmt.Errorf("nope")
	}
	return h, true, nil
}
func (f *fakeAccess) IMessageDeny(h string) (string, bool, error) {
	f.calls = append(f.calls, "deny="+h)
	return h, true, nil
}
func (f *fakeAccess) IMessageClear() (int, error) { f.calls = append(f.calls, "clear"); return 3, nil }
func (f *fakeAccess) SetIMessageResponder(on bool) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("responder=%t", on))
	return "", nil
}
func (f *fakeAccess) SetIMessagePermission(c, m string) (bool, error) {
	f.calls = append(f.calls, "imperm="+c+":"+m)
	return true, nil
}
func (f *fakeAccess) SetRelayEnabled(on bool) error {
	f.calls = append(f.calls, fmt.Sprintf("relay-enable=%t", on))
	return nil
}
func (f *fakeAccess) SetRelayOperator(id string) error {
	f.calls = append(f.calls, "operator="+id)
	return nil
}
func (f *fakeAccess) SetRelayExec(on bool) error {
	f.calls = append(f.calls, fmt.Sprintf("relay-exec=%t", on))
	return nil
}
func (f *fakeAccess) SetRelayPermission(c, m string) (bool, error) {
	f.calls = append(f.calls, "dcperm="+c+":"+m)
	return true, nil
}

func accessServer(fa AccessControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fa != nil {
		s.SetAccess(fa)
	}
	return s
}

func accessGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func accessPost(t *testing.T, s *Server, path string, vals url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func (f *fakeAccess) has(want string) bool {
	for _, c := range f.calls {
		if c == want {
			return true
		}
	}
	return false
}

func TestAccess_NotConfigured(t *testing.T) {
	rec := accessGet(t, accessServer(nil), "/access")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `class="card state-off">Access settings aren&#39;t available`) {
		t.Errorf("nil access should render a not-configured notice; got:\n%s", rec.Body.String())
	}
}

func TestAccess_RenderShowsState(t *testing.T) {
	fa := &fakeAccess{view: AccessView{
		IMessageAvailable:  true,
		ResponderAvailable: true,
		ResponderOn:        true,
		Handles:            []string{"+15551230000", "alice@example.com"},
		IMessagePerms:      []PermissionRow{{Category: "write", Mode: "allow", Desc: "create files"}},
		DiscordAvailable:   true,
		RelayConfigured:    true,
		RelayRunning:       true,
		OperatorSet:        true,
		OperatorID:         "4242",
		DiscordPerms:       []PermissionRow{{Category: "exec", Mode: "deny", Desc: "run code"}},
	}}
	body := accessGet(t, accessServer(fa), "/access").Body.String()
	// "15551230000" (not "+...") — html/template entity-escapes '+' to "&#43;",
	// which is correct auto-escaping (a hostile handle can't inject markup).
	for _, want := range []string{"15551230000", "alice@example.com", "4242", "write", "exec", "running"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q; got:\n%s", want, body)
		}
	}
}

// TEN-208 acceptance, now through the confirm step (TEN-211): removing the
// last person and removing everyone confirm, and so does letting Discord
// messages make changes; none of it needs JS.
func TestAccess_DestructiveConfirms(t *testing.T) {
	// One person, exec off: the Remove, Remove everyone and Turn on steps.
	fa := &fakeAccess{view: AccessView{
		IMessageAvailable: true, Handles: []string{"+15551230000"}, SingleHandle: true,
		DiscordAvailable: true, RelayConfigured: true, ExecOn: false,
	}}
	body := accessGet(t, accessServer(fa), "/access").Body.String()
	for _, want := range []string{
		`<span class="c-open">Remove</span>`, inlineForm("/access/imessage/deny") + `<input type="hidden" name="handle"`,
		`<span class="c-open">Remove everyone</span>`, inlineForm("/access/imessage/clear") + `<input type="hidden" name="confirm" value="yes">`,
		inlineForm("/access/discord/exec") + `<input type="hidden" name="on" value="true"><input type="hidden" name="confirm" value="yes">`,
		"Nobody will be able to message your agent on iMessage until you add someone again.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing confirm markup %q", want)
		}
	}
	if strings.Contains(body, "onsubmit") {
		t.Error("the confirm steps need no hand-written JS")
	}

	// Two people: removing one is one click; exec on: turning it off is too.
	fa2 := &fakeAccess{view: AccessView{
		IMessageAvailable: true, Handles: []string{"a@x.com", "b@x.com"}, SingleHandle: false,
		DiscordAvailable: true, RelayConfigured: true, ExecOn: true,
	}}
	body2 := accessGet(t, accessServer(fa2), "/access").Body.String()
	if strings.Contains(body2, `<span class="c-open">Remove</span>`) {
		t.Error("removing one of several people must not need a confirm step")
	}
	if !strings.Contains(body2, `<input type="hidden" name="on" value="false">`) || strings.Contains(body2, `action="/access/discord/exec"><input type="hidden" name="on" value="true">`) {
		t.Error("exec already on: Turn off is one click, and there's no Turn on step")
	}
}

// Access renders each safety check by its title, tags the ones an update
// added (D27), and loosening one is a confirm step while asking or blocking
// is one click.
func TestAccess_PermissionRowsShowTitlesAndNew(t *testing.T) {
	fa := &fakeAccess{view: AccessView{
		DiscordAvailable: true, RelayConfigured: true,
		DiscordPerms: []PermissionRow{
			{Category: "egress", Mode: "ask", Title: "Share private data", Can: "send private information it has read to an outside service", New: true},
			{Category: "exec", Mode: "deny", Title: "Run commands", Can: "run commands and programs on this computer"},
		},
	}}
	body := accessGet(t, accessServer(fa), "/access").Body.String()
	for _, want := range []string{
		`<strong>Share private data</strong> <span class="tag-new">New</span>`,
		"Asks you before it can send private information it has read to an outside service.",
		`>Keep asking</button>`,
		`<strong>Run commands</strong>`, "Can't run commands and programs on this computer. It won't ask.",
		"Discord conversations can run commands and programs on this computer without asking you.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(visibleWords(body, false), "egress") {
		t.Error("a raw category ID shows on the page")
	}
}

func TestAccess_PerChannelDegradation(t *testing.T) {
	// iMessage available but responder off-platform; Discord present but no token.
	fa := &fakeAccess{view: AccessView{IMessageAvailable: true, ResponderAvailable: false, DiscordAvailable: true, RelayConfigured: false}}
	body := accessGet(t, accessServer(fa), "/access").Body.String()
	if !strings.Contains(body, "Answering iMessages needs a Mac.") {
		t.Errorf("responder-unavailable should show the macOS-only note; got:\n%s", body)
	}
	if !strings.Contains(body, "add a bot token") {
		t.Errorf("relay-unconfigured should point to Integrations; got:\n%s", body)
	}
}

func TestAccess_AllowForm(t *testing.T) {
	fa := &fakeAccess{}
	rec := accessPost(t, accessServer(fa), "/access/imessage/allow", url.Values{"handle": {"+1 555 123 0000"}, "confirm": {"yes"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/access") {
		t.Fatalf("want 303 to /access, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if !fa.has("allow=+1 555 123 0000") {
		t.Errorf("allow not forwarded: %v", fa.calls)
	}
}

func TestAccess_AllowFormEmptyValidates(t *testing.T) {
	fa := &fakeAccess{}
	rec := accessPost(t, accessServer(fa), "/access/imessage/allow", url.Values{"handle": {"  "}, "confirm": {"yes"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("empty handle should 303 with ?err, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(fa.calls) != 0 {
		t.Errorf("empty handle must not call the control: %v", fa.calls)
	}
}

func TestAccess_DenyFormEmptyValidates(t *testing.T) {
	fa := &fakeAccess{}
	rec := accessPost(t, accessServer(fa), "/access/imessage/deny", url.Values{"handle": {"  "}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("empty deny handle should 303 with ?err, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(fa.calls) != 0 {
		t.Errorf("empty deny handle must not call the control: %v", fa.calls)
	}
}

func TestAccess_AllowErrorFlashes(t *testing.T) {
	fa := &fakeAccess{denyAllow: true}
	rec := accessPost(t, accessServer(fa), "/access/imessage/allow", url.Values{"handle": {"x@y.com"}, "confirm": {"yes"}})
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "err=") || strings.Contains(loc, "nope") {
		t.Errorf("a control error should redirect with a plain ?err and not its own text (D21), got %q", loc)
	}
}

func TestAccess_DenyClear(t *testing.T) {
	fa := &fakeAccess{}
	s := accessServer(fa)
	accessPost(t, s, "/access/imessage/deny", url.Values{"handle": {"bob@x.com"}})
	accessPost(t, s, "/access/imessage/clear", url.Values{"confirm": {"yes"}})
	if !fa.has("deny=bob@x.com") || !fa.has("clear") {
		t.Errorf("deny/clear not forwarded: %v", fa.calls)
	}
}

func TestAccess_Toggles(t *testing.T) {
	fa := &fakeAccess{}
	s := accessServer(fa)
	yes := func(v url.Values) url.Values { v.Set("confirm", "yes"); return v }
	accessPost(t, s, "/access/imessage/responder", yes(url.Values{"on": {"true"}}))
	accessPost(t, s, "/access/imessage/responder", url.Values{"on": {"false"}})
	accessPost(t, s, "/access/discord/enable", yes(url.Values{"on": {"true"}}))
	accessPost(t, s, "/access/discord/exec", yes(url.Values{"on": {"true"}}))
	for _, want := range []string{"responder=true", "responder=false", "relay-enable=true", "relay-exec=true"} {
		if !fa.has(want) {
			t.Errorf("missing %q in %v", want, fa.calls)
		}
	}
}

func TestAccess_OperatorAndPermissions(t *testing.T) {
	fa := &fakeAccess{}
	s := accessServer(fa)
	accessPost(t, s, "/access/discord/operator", url.Values{"operator": {"4242"}, "confirm": {"yes"}})
	accessPost(t, s, "/access/imessage/permission", url.Values{"category": {"write"}, "mode": {"deny"}})
	accessPost(t, s, "/access/discord/permission", url.Values{"category": {"exec"}, "mode": {"allow"}, "confirm": {"yes"}})
	// Anything but exactly ask, allow or deny is refused before the control,
	// even with its confirm step (D2).
	accessPost(t, s, "/access/discord/permission", url.Values{"category": {"exec"}, "mode": {"always"}, "confirm": {"yes"}})
	if fa.has("dcperm=exec:always") {
		t.Errorf("an unknown mode reached the control: %v", fa.calls)
	}
	for _, want := range []string{"operator=4242", "imperm=write:deny", "dcperm=exec:allow"} {
		if !fa.has(want) {
			t.Errorf("missing %q in %v", want, fa.calls)
		}
	}
}

// Every POST handler must nil-guard the control: 303 to /access, no panic/500.
func TestAccess_NilGuardPostRedirects(t *testing.T) {
	s := accessServer(nil)
	for _, path := range []string{
		"/access/imessage/allow", "/access/imessage/deny", "/access/imessage/clear",
		"/access/imessage/responder", "/access/imessage/permission",
		"/access/discord/enable", "/access/discord/operator", "/access/discord/exec",
		"/access/discord/permission",
	} {
		rec := accessPost(t, s, path, url.Values{"confirm": {"yes"}})
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control should 303, got %d", path, rec.Code)
		}
	}
}

// inlineForm is the opening tag of a form that posts in place (formDone).
func inlineForm(action string) string {
	return `<form method="POST" action="` + action + `" data-on:submit__prevent__throttle.1s="@post('` + strings.ReplaceAll(action, "/", `\/`) + `', {contentType:'form', retryMaxCount:0})">`
}

// permAccess is fakeAccess whose Discord safety settings and iMessage list
// change when set, as the real control's do, and whose settings fail while
// fail is set. denied lists every Remove it got, set every safety setting
// ("category=mode"). A browser test's server and the test share it.
type permAccess struct {
	fakeAccess
	mu     sync.Mutex
	fail   bool
	denied []string
	set    []string
}

func (f *permAccess) View() AccessView {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.view
	v.Handles, v.IMessagePerms, v.DiscordPerms = slices.Clone(v.Handles), slices.Clone(v.IMessagePerms), slices.Clone(v.DiscordPerms)
	return v
}

func (f *permAccess) IMessageDeny(h string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied = append(f.denied, h)
	i := slices.Index(f.view.Handles, h)
	if i < 0 {
		return h, false, nil
	}
	f.view.Handles = slices.Delete(f.view.Handles, i, i+1)
	f.view.SingleHandle = len(f.view.Handles) == 1
	return h, true, nil
}

func (f *permAccess) SetRelayPermission(c, m string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = append(f.set, c+"="+m)
	if f.fail {
		return false, errors.New("settings file is locked")
	}
	for i, p := range f.view.DiscordPerms {
		if p.Category == c {
			f.view.DiscordPerms[i].Mode, f.view.DiscordPerms[i].New = m, false
		}
	}
	return true, nil
}

func (f *permAccess) setFail(on bool) {
	f.mu.Lock()
	f.fail = on
	f.mu.Unlock()
}

func datastar(r *http.Request) { r.Header.Set("Datastar-Request", "true") }

// Every Access form answers a Datastar post in place: the toast, the status
// strip and #access-body, never a 303 the fetch would follow. With no control
// the region says so.
func TestAccess_InlineAnswersEveryForm(t *testing.T) {
	forms := map[string]url.Values{
		"/access/imessage/allow":      {"handle": {"a@b.c"}},
		"/access/imessage/deny":       {"handle": {"a@b.c"}},
		"/access/imessage/clear":      {},
		"/access/imessage/responder":  {"on": {"true"}},
		"/access/imessage/permission": {"category": {"exec"}, "mode": {"deny"}},
		"/access/discord/enable":      {"on": {"true"}},
		"/access/discord/operator":    {"operator": {"4242"}},
		"/access/discord/exec":        {"on": {"true"}},
		"/access/discord/permission":  {"category": {"exec"}, "mode": {"allow"}},
	}
	full := &permAccess{fakeAccess: fakeAccess{view: AccessView{IMessageAvailable: true, DiscordAvailable: true, RelayConfigured: true, DiscordPerms: harnessPerms(false)}}}
	for path, f := range forms {
		for _, fa := range []AccessControl{full, nil} {
			rec := sendForm(accessServer(fa).Handler(), http.MethodPost, path, withField(f, "confirm", "yes"), datastar)
			body := rec.Body.String()
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("Location") != "" {
				t.Errorf("%s (control %v): %d %q to %q, want 200 text/event-stream", path, fa != nil, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Location"))
			}
			for _, want := range []string{"data: mode replace\ndata: elements <div id=\"toast\"", `data: elements <div id="ui-status"`, `data: elements <div id="access-body">`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s (control %v) is missing %q:\n%s", path, fa != nil, want, body)
				}
			}
			if fa == nil && !strings.Contains(body, "Access settings aren&#39;t available") {
				t.Errorf("%s with no control: the region doesn't say so:\n%s", path, body)
			}
		}
	}
}

// A setting changed in place: the toast says what changed and the row shows
// it; an error is an alert with the plain message; a plain post still gets
// the 303 and its flash; a loosening without its confirm step is still 428.
func TestAccess_InlinePermission(t *testing.T) {
	c := installCapture(t)
	fa := &permAccess{fakeAccess: fakeAccess{view: AccessView{DiscordAvailable: true, RelayConfigured: true, DiscordPerms: harnessPerms(false)}}}
	s := accessServer(fa)
	post := func(f url.Values, set func(*http.Request)) *httptest.ResponseRecorder {
		return sendForm(s.Handler(), http.MethodPost, "/access/discord/permission", f, set)
	}

	body := post(url.Values{"category": {"exec"}, "mode": {"deny"}}, datastar).Body.String()
	for _, want := range []string{
		`<div id="toast" class="toast ok"><span class="toast-text">Discord conversations: Run commands is blocked.</span>`,
		"Can't run commands and programs on this computer. It won't ask.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the in-place answer is missing %q:\n%s", want, body)
		}
	}

	fa.setFail(true)
	body = post(url.Values{"category": {"write"}, "mode": {"deny"}}, datastar).Body.String()
	if !strings.Contains(body, `<div id="toast" class="toast err" role="alert"><span class="toast-text">Couldn&#39;t change that safety setting.</span>`) || strings.Contains(body, "locked") {
		t.Errorf("a failed change: want the plain alert and not the control's own words:\n%s", body)
	}
	if strings.Contains(body, `id="access-body"`) {
		t.Errorf("a failed change re-rendered the page, which closes an open step:\n%s", body)
	}
	if !c.has("2201 ", "action=relay permission") {
		t.Errorf("the failure isn't logged as ActionFailed: %q", c.lines())
	}
	fa.setFail(false)

	rec := post(url.Values{"category": {"write"}, "mode": {"ask"}}, nil)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/access?msg=") {
		t.Errorf("a plain post: %d to %q, want 303 to /access with its flash", rec.Code, loc)
	}

	// The toast is read out: it's replaced inside a live region that was
	// there first. A post that gets no toast has #toast-fail.
	page := accessGet(t, s, "/access").Body.String()
	for _, want := range []string{`<div class="toast-wrap" aria-live="polite"`, `<div id="toast-fail" class="toast err" role="alert" style="display:none" data-show="$_formfail">`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q", want)
		}
	}

	post(url.Values{"category": {"web"}, "mode": {"deny"}}, datastar)
	rec = post(url.Values{"category": {"web"}, "mode": {"allow"}}, datastar)
	if web := fa.View().DiscordPerms[3]; rec.Code != http.StatusPreconditionRequired || web.Mode != "deny" {
		t.Errorf("Allow without its confirm step over Datastar: %d and %s is %s, want 428 and nothing changed", rec.Code, web.Category, web.Mode)
	}
}
