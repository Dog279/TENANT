package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	if !strings.Contains(rec.Body.String(), "isn't configured") {
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

func TestAccess_PerChannelDegradation(t *testing.T) {
	// iMessage available but responder off-platform; Discord present but no token.
	fa := &fakeAccess{view: AccessView{IMessageAvailable: true, ResponderAvailable: false, DiscordAvailable: true, RelayConfigured: false}}
	body := accessGet(t, accessServer(fa), "/access").Body.String()
	if !strings.Contains(body, "macOS-only") {
		t.Errorf("responder-unavailable should show the macOS-only note; got:\n%s", body)
	}
	if !strings.Contains(body, "add a bot token") {
		t.Errorf("relay-unconfigured should point to Integrations; got:\n%s", body)
	}
}

func TestAccess_AllowForm(t *testing.T) {
	fa := &fakeAccess{}
	rec := accessPost(t, accessServer(fa), "/access/imessage/allow", url.Values{"handle": {"+1 555 123 0000"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/access") {
		t.Fatalf("want 303 to /access, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if !fa.has("allow=+1 555 123 0000") {
		t.Errorf("allow not forwarded: %v", fa.calls)
	}
}

func TestAccess_AllowFormEmptyValidates(t *testing.T) {
	fa := &fakeAccess{}
	rec := accessPost(t, accessServer(fa), "/access/imessage/allow", url.Values{"handle": {"  "}})
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
	rec := accessPost(t, accessServer(fa), "/access/imessage/allow", url.Values{"handle": {"x@y.com"}})
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Errorf("a control error should redirect with ?err, got %q", rec.Header().Get("Location"))
	}
}

func TestAccess_DenyClear(t *testing.T) {
	fa := &fakeAccess{}
	s := accessServer(fa)
	accessPost(t, s, "/access/imessage/deny", url.Values{"handle": {"bob@x.com"}})
	accessPost(t, s, "/access/imessage/clear", url.Values{})
	if !fa.has("deny=bob@x.com") || !fa.has("clear") {
		t.Errorf("deny/clear not forwarded: %v", fa.calls)
	}
}

func TestAccess_Toggles(t *testing.T) {
	fa := &fakeAccess{}
	s := accessServer(fa)
	accessPost(t, s, "/access/imessage/responder", url.Values{"on": {"true"}})
	accessPost(t, s, "/access/imessage/responder", url.Values{"on": {"false"}})
	accessPost(t, s, "/access/discord/enable", url.Values{"on": {"true"}})
	accessPost(t, s, "/access/discord/exec", url.Values{"on": {"true"}})
	for _, want := range []string{"responder=true", "responder=false", "relay-enable=true", "relay-exec=true"} {
		if !fa.has(want) {
			t.Errorf("missing %q in %v", want, fa.calls)
		}
	}
}

func TestAccess_OperatorAndPermissions(t *testing.T) {
	fa := &fakeAccess{}
	s := accessServer(fa)
	accessPost(t, s, "/access/discord/operator", url.Values{"operator": {"4242"}})
	accessPost(t, s, "/access/imessage/permission", url.Values{"category": {"write"}, "mode": {"deny"}})
	accessPost(t, s, "/access/discord/permission", url.Values{"category": {"exec"}, "mode": {"allow"}})
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
		rec := accessPost(t, s, path, url.Values{})
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control should 303, got %d", path, rec.Code)
		}
	}
}
