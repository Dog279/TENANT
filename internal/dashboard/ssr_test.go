package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeTools is a minimal ToolControl for the SSR page tests.
type fakeTools struct {
	tools    []ToolInfo
	plugins  []string
	setCalls []string
}

func (f *fakeTools) ToolList() []ToolInfo { return f.tools }
func (f *fakeTools) Plugins() []string    { return f.plugins }
func (f *fakeTools) SetEnabled(target string, on bool) (int, string, error) {
	f.setCalls = append(f.setCalls, fmt.Sprintf("%s=%v", target, on))
	for i := range f.tools {
		if f.tools[i].Name == target {
			f.tools[i].Enabled = on
		}
	}
	return 1, "tool", nil
}
func (f *fakeTools) SetPluginEnabled(label string, on bool) (int, string, error) {
	f.setCalls = append(f.setCalls, fmt.Sprintf("plugin:%s=%v", label, on))
	return 1, "plugin", nil
}

func ssrServer() (*Server, *fakeTools) {
	ft := &fakeTools{
		tools: []ToolInfo{
			{Name: "web_read", Plugin: "web", Enabled: true, Gated: false},
			{Name: "sql_exec", Plugin: "sql", Enabled: false, Gated: true},
		},
		plugins: []string{"web", "sql"},
	}
	return New(Config{}, nil, ft, nil, nil, nil), ft
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestSSR_DashboardPage(t *testing.T) {
	s, _ := ssrServer()
	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	// Home was renamed Dashboard→Overview and turned into a status board (TEN-200).
	for _, want := range []string{"Overview", "Tools enabled", "web", "Read-only"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard page missing %q", want)
		}
	}
}

func TestSSR_ToolsPage(t *testing.T) {
	s, _ := ssrServer()
	rec := get(t, s, "/tools")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"web_read", "sql_exec", "destructive", "/tools/web_read/toggle"} {
		if !strings.Contains(body, want) {
			t.Errorf("tools page missing %q", want)
		}
	}
}

func TestSSR_ActivityPage(t *testing.T) {
	if rec := get(t, mustServer(), "/activity"); rec.Code != http.StatusOK {
		t.Fatalf("activity want 200, got %d", rec.Code)
	}
}

func TestSSR_CSS(t *testing.T) {
	rec := get(t, mustServer(), "/styles.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("css want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("css content-type = %q, want text/css", ct)
	}
}

func mustServer() *Server { s, _ := ssrServer(); return s }

func ssrPost(t *testing.T, s *Server, path string, vals url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func TestSSR_ToolToggleForm(t *testing.T) {
	s, ft := ssrServer()
	rec := ssrPost(t, s, "/tools/web_read/toggle", url.Values{"enabled": {"false"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/tools" {
		t.Fatalf("toggle should 303 to /tools, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if len(ft.setCalls) != 1 || ft.setCalls[0] != "web_read=false" {
		t.Errorf("expected SetEnabled(web_read,false), got %v", ft.setCalls)
	}
}

func TestSSR_DatastarServed(t *testing.T) {
	rec := get(t, mustServer(), "/datastar.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("datastar.js want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Datastar") {
		t.Error("served file does not look like the Datastar runtime")
	}
}

func TestSSR_ToolToggle_DatastarPatch(t *testing.T) {
	s, ft := ssrServer()
	r := httptest.NewRequest("POST", "/tools/web_read/toggle?enabled=false", nil)
	r.Header.Set("Datastar-Request", "true")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("datastar toggle want 200 (SSE), got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("want SSE content-type, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("SSE missing the patch event:\n%s", body)
	}
	if !strings.Contains(body, `id="tool-web_read"`) {
		t.Errorf("patch should carry the updated #tool-web_read row:\n%s", body)
	}
	if len(ft.setCalls) != 1 || ft.setCalls[0] != "web_read=false" {
		t.Errorf("toggle not applied via query param: %v", ft.setCalls)
	}
}

func TestSSR_PostureForm(t *testing.T) {
	s, ft := ssrServer()
	rec := ssrPost(t, s, "/posture", url.Values{"allow_send": {"true"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("posture should 303, got %d", rec.Code)
	}
	// Only the gated tool (sql_exec) should be flipped.
	if len(ft.setCalls) != 1 || ft.setCalls[0] != "sql_exec=true" {
		t.Errorf("posture should flip only gated tools, got %v", ft.setCalls)
	}
}

func TestSSR_PluginToggleForm(t *testing.T) {
	s, ft := ssrServer()
	rec := ssrPost(t, s, "/plugins/web/toggle", url.Values{"enabled": {"false"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("plugin toggle should 303, got %d", rec.Code)
	}
	if len(ft.setCalls) != 1 || ft.setCalls[0] != "plugin:web=false" {
		t.Errorf("expected SetPluginEnabled(web,false), got %v", ft.setCalls)
	}
}

// TestSSR_DatastarColonSyntax guards the Datastar v1.0.1 attribute syntax. The
// runtime binds COLON-form attributes (data-on:click, data-init, data-bind:foo)
// and SILENTLY IGNORES the hyphen forms (data-on-click, data-on-load,
// data-bind-text). That hyphen bug shipped in TEN-108/109 and made every
// interactive control inert in the browser while every server-side httptest
// still passed — so this test asserts the rendered markup, the one thing those
// tests can check, uses the colon syntax and none of the dead hyphen forms.
func TestSSR_DatastarColonSyntax(t *testing.T) {
	s := mustServer()
	cases := []struct {
		path, want, reject string
	}{
		{"/activity", `data-init="@get('/events', {retryMaxCount:1000000, retryMaxWait:5000})"`, "data-on-load"},
		{"/chat", `data-on:submit__prevent="@post('/chat/send')"`, "data-on-submit"},
		{"/chat", `data-bind:text`, "data-bind-text"},
		{"/chat", `data-on:click="@post('/chat/stop')"`, "data-on-click"},
		{"/tools", `data-on:click="@post('/tools/`, "data-on-click"},
	}
	for _, c := range cases {
		body := get(t, s, c.path).Body.String()
		if !strings.Contains(body, c.want) {
			t.Errorf("%s missing colon-syntax attr %q", c.path, c.want)
		}
		if strings.Contains(body, c.reject) {
			t.Errorf("%s still emits dead hyphen-syntax attr %q", c.path, c.reject)
		}
	}
}
