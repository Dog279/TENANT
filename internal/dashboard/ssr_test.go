package dashboard

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	for _, want := range []string{"Overview", "Tools enabled", "Look only", "No services connected yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard page missing %q", want)
		}
	}
}

// UX-5: Connected services are the integrations set up and the remote
// services connected, by name, not every built-in plugin.
func TestSSR_ConnectedServicesAreTheConnectedOnes(t *testing.T) {
	s, _ := ssrServer()
	s.SetIntegrations(&fakeIntegrationsCtl{list: []IntegrationView{{ID: "gsuite", Label: "Google (Gmail, Calendar, Drive) — business / SA + DWD", Configured: true}, {ID: "github", Label: "GitHub"}}})
	s.SetMCP(&fakeMCPCtl{servers: []MCPServerView{{Label: "mcp:atlassian", Enabled: true}, {Label: "mcp:gone"}}})
	body := get(t, s, "/").Body.String()
	for _, want := range []string{`<a class="chip" href="/integrations">Google</a>`, `<a class="chip" href="/mcp">atlassian</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("Overview lacks %q", want)
		}
	}
	for _, not := range []string{">GitHub<", ">gone<", `class="chip">web<`, `class="chip">sql<`} {
		if strings.Contains(body, not) {
			t.Errorf("Overview lists %q, which isn't connected", not)
		}
	}
}

// UX-2: with one tool that changes things on and another off, the agent can
// change things: Overview must not say "Look only", and the Tools page must
// not show "All off" as the current state.
func TestSSR_WhatItMayDoFollowsTheToolsOn(t *testing.T) {
	s, ft := ssrServer()
	ft.tools = append(ft.tools, ToolInfo{Name: "os_exec", Plugin: "os", Enabled: true, Gated: true})
	body := get(t, s, "/").Body.String()
	if strings.Contains(body, "Look only") || !strings.Contains(body, `Change things <span class="dim">· 1 tool</span>`) || !strings.Contains(body, `href="/safety" class="card stat linkcard"`) {
		t.Errorf("Overview with os_exec on:\n%s", body)
	}
	tools := get(t, s, "/tools").Body.String()
	if strings.Contains(tools, `class="btn on">All off`) || strings.Contains(tools, `<span class="btn on">All on`) {
		t.Errorf("Tools with some tools that change things on shows all off or all on as current")
	}
	ft.tools[len(ft.tools)-1].Enabled = false
	if body := get(t, s, "/").Body.String(); !strings.Contains(body, "Look only") {
		t.Errorf("Overview with every tool that changes things off should say Look only")
	}
	if tools := get(t, s, "/tools").Body.String(); !strings.Contains(tools, `class="btn on">All off`) {
		t.Errorf("Tools with every tool that changes things off should show All off as current")
	}
}

func TestSSR_ToolsPage(t *testing.T) {
	s, _ := ssrServer()
	rec := get(t, s, "/tools")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	// The toggle URL is JS-escaped inside data-on:click ("/" → "\/").
	for _, want := range []string{"web_read", "sql_exec", "can change things", `\/tools\/web_read\/toggle`} {
		if !strings.Contains(body, want) {
			t.Errorf("tools page missing %q", want)
		}
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

// The stylesheet and Datastar: the hashed URL is kept a year (immutable),
// any other URL is revalidated by its ETag (304), and gzip goes only to a
// client that takes it, the same bytes inside.
func TestSSR_AssetCaching(t *testing.T) {
	h := mustServer().Handler()
	with := func(kv ...string) func(*http.Request) {
		return func(r *http.Request) {
			for i := 0; i < len(kv); i += 2 {
				r.Header.Set(kv[i], kv[i+1])
			}
		}
	}
	for _, a := range []*asset{ssrCSS, datastarJS} {
		plain := sendForm(h, http.MethodGet, a.url, nil, nil)
		refused := sendForm(h, http.MethodGet, a.url, nil, with("Accept-Encoding", "br, gzip;Q=0"))
		gz := sendForm(h, http.MethodGet, a.url, nil, with("Accept-Encoding", "gzip, deflate, br, zstd"))
		for name, rec := range map[string]*httptest.ResponseRecorder{"identity": plain, "gzip;Q=0": refused, "gzip": gz} {
			hd := rec.Header()
			if rec.Code != http.StatusOK || hd.Get("Cache-Control") != "private, max-age=31536000, immutable" || hd.Get("Vary") != "Accept-Encoding" || hd.Get("ETag") == "" {
				t.Errorf("%s %s: %d %v, want 200, immutable, Vary and an ETag", a.url, name, rec.Code, hd)
			}
		}
		if plain.Header().Get("Content-Encoding") != "" || refused.Header().Get("Content-Encoding") != "" ||
			!bytes.Equal(plain.Body.Bytes(), a.body) || !bytes.Equal(refused.Body.Bytes(), a.body) {
			t.Errorf("%s without gzip: want the asset's own bytes, unencoded", a.url)
		}
		zipped := gz.Body.Bytes()
		zr, err := gzip.NewReader(bytes.NewReader(zipped))
		if err != nil {
			t.Fatalf("%s gzip: %v", a.url, err)
		}
		if got, err := io.ReadAll(zr); gz.Header().Get("Content-Encoding") != "gzip" || err != nil || !bytes.Equal(got, a.body) || len(zipped) >= len(a.body) {
			t.Errorf("%s gzip: Content-Encoding %q, %d bytes, want the asset's bytes gzipped smaller (err %v)", a.url, gz.Header().Get("Content-Encoding"), len(zipped), err)
		}
		if plain.Header().Get("ETag") == gz.Header().Get("ETag") {
			t.Errorf("%s: identity and gzip share the ETag %s", a.url, gz.Header().Get("ETag"))
		}
		path, _, _ := strings.Cut(a.url, "?")
		for _, u := range []string{path, path + "?v=0000000000000000"} { // no hash, and an old one
			rec := sendForm(h, http.MethodGet, u, nil, with("Accept-Encoding", "gzip"))
			etag := rec.Header().Get("ETag")
			if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "private, no-cache" || etag == "" {
				t.Errorf("%s: %d %v, want 200, no-cache and an ETag", u, rec.Code, rec.Header())
			}
			if nm := sendForm(h, http.MethodGet, u, nil, with("Accept-Encoding", "gzip", "If-None-Match", etag)); nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
				t.Errorf("%s If-None-Match %s: %d with %d bytes, want an empty 304", u, etag, nm.Code, nm.Body.Len())
			}
			if other := sendForm(h, http.MethodGet, u, nil, with("If-None-Match", etag)); other.Code != http.StatusOK {
				t.Errorf("%s: the gzip ETag matched the unencoded bytes (%d)", u, other.Code)
			}
		}
	}
}

// Every page links the assets by their hash; the sign-in page, open signed
// out, links neither (they need the sign-in) and carries its own style.
func TestSSR_PagesLinkHashedAssets(t *testing.T) {
	for p, body := range renderAllPages(t, populated) {
		for _, want := range []string{`<link rel="stylesheet" href="` + ssrCSS.url + `">`, `<script type="module" src="` + datastarJS.url + `">`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s doesn't link %s", p, want)
			}
		}
	}
	h := inventoryServer(Config{Addr: "example.com", Auth: "s3cret"}).served()
	signIn := sendForm(h, http.MethodGet, "/settings", nil, nil)
	if body := signIn.Body.String(); signIn.Code != http.StatusOK || !strings.Contains(body, "<style>") || strings.Contains(body, "styles.css") || strings.Contains(body, "datastar.js") {
		t.Errorf("signed-out sign-in page: %d, want 200 with its own <style> and no asset link:\n%s", signIn.Code, body)
	}
	for _, a := range []*asset{ssrCSS, datastarJS} {
		if rec := sendForm(h, http.MethodGet, a.url, nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s signed out = %d, want 401: the assets are private", a.url, rec.Code)
		}
	}
}

// New bytes get a new hash, so a browser never keeps an old asset for a new
// build; the same bytes keep it.
func TestSSR_AssetHash(t *testing.T) {
	a, b, a2 := newAsset("/x.css", "text/css", []byte("a{}")), newAsset("/x.css", "text/css", []byte("b{}")), newAsset("/x.css", "text/css", []byte("a{}"))
	if a.url == b.url || a.url != a2.url || !strings.HasPrefix(a.url, "/x.css?v=") || len(a.hash) != 16 {
		t.Errorf("urls %s, %s, %s: want a 16-character hash that follows the bytes", a.url, b.url, a2.url)
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

// A remote tool server names its own tools. A name with "../" must stay one
// path segment, so its rendered toggle button still reaches the toggle route
// and can't aim at another one, such as a key's remove (F1).
func TestSSR_ToolToggleURLEscapesName(t *testing.T) {
	s, ft := ssrServer()
	name := "../../settings/keys/openai/remove?confirm=yes&x="
	row, err := s.renderToolRow(ToolInfo{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`data-on:click="@post\('([^']*)'\)"`).FindStringSubmatch(row)
	if m == nil {
		t.Fatalf("no @post toggle on the row:\n%s", row)
	}
	var u string // the browser's view: HTML-decoded, then the JS string decoded
	if err := json.Unmarshal([]byte(`"`+html.UnescapeString(m[1])+`"`), &u); err != nil {
		t.Fatalf("decode %q: %v", m[1], err)
	}
	if strings.Contains(u, "../") {
		t.Fatalf("toggle URL %q lets the name leave its path segment", u)
	}
	ssrPost(t, s, u, nil)
	if len(ft.setCalls) != 1 || ft.setCalls[0] != name+"=true" {
		t.Errorf("POST %s should toggle the tool itself, got %v", u, ft.setCalls)
	}
}

// A bare CR ends an SSE line, so agent text carrying one could close a patch
// and start a datastar-patch-signals event, which Datastar evaluates.
func TestPatchElements_NoBareCR(t *testing.T) {
	got := string(patchElements("#x", "append", "a\rb\r\nc"))
	if strings.Contains(got, "\r") {
		t.Fatalf("CR reached the SSE stream: %q", got)
	}
	for _, l := range []string{"data: elements a\n", "data: elements b\n", "data: elements c\n"} {
		if !strings.Contains(got, l) {
			t.Errorf("want %q in %q", l, got)
		}
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
	rec := ssrPost(t, s, "/posture", url.Values{"allow_send": {"true"}, "confirm": {"yes"}})
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
		{"/chat", `data-on:submit__prevent="@post('/chat/send', {requestCancellation:'disabled'})"`, "data-on-submit"}, // a double tap keeps the first answer
		{"/chat", `data-bind:text`, "data-bind-text"},
		{"/chat", `data-on:click="@post('/chat/stop')"`, "data-on-click"},
		{"/tools", `data-on:click="@post('\/tools\/`, "data-on-click"}, // JS-escaped ToggleURL
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
