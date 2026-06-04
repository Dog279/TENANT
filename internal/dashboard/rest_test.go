package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/agent"
)

// restSetCall records one SetEnabled / SetPluginEnabled invocation so a test
// can assert the handler forwarded the right (target, on) pair.
type restSetCall struct {
	target string
	on     bool
}

// restFakeTools is a ToolControl stand-in for the REST tests. The `rest`
// prefix keeps it from colliding with fakes in sibling test files added to
// this package concurrently (ws_test.go / auth_test.go).
type restFakeTools struct {
	tools   []ToolInfo
	plugins []string

	// canned returns for the toggle calls.
	setRet      int
	setScope    string
	setErr      error
	pluginRet   int
	pluginScope string
	pluginErr   error

	// setErrFor overrides setErr for specific targets (used by the posture
	// best-effort test, where one gated tool must error while others succeed).
	setErrFor map[string]error

	// recorded calls.
	setCalls    []restSetCall
	pluginCalls []restSetCall
}

func (f *restFakeTools) ToolList() []ToolInfo { return f.tools }
func (f *restFakeTools) Plugins() []string    { return f.plugins }

func (f *restFakeTools) SetEnabled(target string, on bool) (int, string, error) {
	f.setCalls = append(f.setCalls, restSetCall{target, on})
	if err, ok := f.setErrFor[target]; ok {
		return 0, "", err
	}
	return f.setRet, f.setScope, f.setErr
}

func (f *restFakeTools) SetPluginEnabled(label string, on bool) (int, string, error) {
	f.pluginCalls = append(f.pluginCalls, restSetCall{label, on})
	return f.pluginRet, f.pluginScope, f.pluginErr
}

// newRESTServer builds a Server with the given fake tools. New() wires the
// REST routes via routes(), so no explicit mount is needed here.
func newRESTServer(f *restFakeTools) *Server {
	return New(Config{}, nil, f, nil, agent.NewBroker(0), nil)
}

// TestRESTTools: GET /api/tools sources `destructive` from the plugin's
// authoritative Gated flag, NOT a name heuristic. The names are chosen to
// give the OPPOSITE answer under the old heuristic — a Gated tool named
// "web.search" must still be destructive=true, and a non-Gated tool named
// "gmail.send" must be destructive=false — so a regression to name-matching
// would fail this test.
func TestRESTTools(t *testing.T) {
	f := &restFakeTools{
		tools: []ToolInfo{
			{Name: "web.search", Plugin: "web", Enabled: true, Gated: true}, // gated → destructive (name says "safe")
			{Name: "gmail.send", Plugin: "gmail", Enabled: true},            // not gated → safe (name says "send")
			{Name: "calendar.read", Plugin: "cal", Enabled: true},           // not gated → safe
		},
		plugins: []string{"web", "gmail", "cal"},
	}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}

	var got []restToolView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}

	// destructive mirrors Gated, regardless of the tool name.
	want := map[string]bool{
		"web.search":    true,
		"gmail.send":    false,
		"calendar.read": false,
	}
	for _, v := range got {
		if exp, ok := want[v.Name]; !ok {
			t.Fatalf("unexpected tool %q", v.Name)
		} else if v.Destructive != exp {
			t.Errorf("%s destructive = %v, want %v", v.Name, v.Destructive, exp)
		}
	}
	// Spot-check passthrough of plugin/enabled.
	if got[0].Plugin != "web" || !got[0].Enabled {
		t.Errorf("tool[0] = %+v, want plugin=web enabled=true", got[0])
	}
}

// TestRESTSetTool: POST /api/tools/{name} forwards (name, enabled) to
// SetEnabled and echoes its (changed, scope) result.
func TestRESTSetTool(t *testing.T) {
	f := &restFakeTools{setRet: 3, setScope: "plugin:gmail"}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tools/gmail.send",
		strings.NewReader(`{"enabled":true}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(f.setCalls) != 1 {
		t.Fatalf("SetEnabled called %d times, want 1", len(f.setCalls))
	}
	if c := f.setCalls[0]; c.target != "gmail.send" || c.on != true {
		t.Fatalf("SetEnabled called with %+v, want {gmail.send true}", c)
	}

	var resp restToggleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Changed != 3 || resp.Scope != "plugin:gmail" {
		t.Fatalf("resp = %+v, want {3 plugin:gmail}", resp)
	}
}

// TestRESTSetToolDisable confirms enabled=false is forwarded faithfully (not
// defaulted), guarding against a decode that drops the field.
func TestRESTSetToolDisable(t *testing.T) {
	f := &restFakeTools{setRet: 1, setScope: "tool"}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tools/fs.delete_file",
		strings.NewReader(`{"enabled":false}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(f.setCalls) != 1 || f.setCalls[0].on != false || f.setCalls[0].target != "fs.delete_file" {
		t.Fatalf("calls = %+v, want one {fs.delete_file false}", f.setCalls)
	}
}

// TestRESTSetToolBadJSON: a malformed body is a 400 and never reaches the
// tool control.
func TestRESTSetToolBadJSON(t *testing.T) {
	f := &restFakeTools{}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tools/gmail.send",
		strings.NewReader(`{"enabled":`)) // truncated
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(f.setCalls) != 0 {
		t.Fatalf("SetEnabled was called %d times on bad body, want 0", len(f.setCalls))
	}
}

// TestRESTSetToolError: a SetEnabled error surfaces as a 4xx JSON error.
func TestRESTSetToolError(t *testing.T) {
	f := &restFakeTools{setErr: errRestUnknownTool}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tools/nope",
		strings.NewReader(`{"enabled":true}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var env map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env["error"] == "" {
		t.Fatalf("expected error envelope, got %v", env)
	}
}

// TestRESTSetPlugin: POST /api/plugins/{label} forwards to SetPluginEnabled.
func TestRESTSetPlugin(t *testing.T) {
	f := &restFakeTools{pluginRet: 5, pluginScope: "plugin"}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/plugins/gmail",
		strings.NewReader(`{"enabled":false}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(f.pluginCalls) != 1 {
		t.Fatalf("SetPluginEnabled called %d times, want 1", len(f.pluginCalls))
	}
	if c := f.pluginCalls[0]; c.target != "gmail" || c.on != false {
		t.Fatalf("SetPluginEnabled called with %+v, want {gmail false}", c)
	}

	var resp restToggleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Changed != 5 || resp.Scope != "plugin" {
		t.Fatalf("resp = %+v, want {5 plugin}", resp)
	}
}

// TestRESTStatus: GET /api/status returns plugins plus enabled/total counts.
func TestRESTStatus(t *testing.T) {
	f := &restFakeTools{
		tools: []ToolInfo{
			{Name: "a", Enabled: true},
			{Name: "b", Enabled: false},
			{Name: "c", Enabled: true},
		},
		plugins: []string{"gmail", "fs"},
	}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var st restStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Status != "ok" {
		t.Errorf("status = %q, want ok", st.Status)
	}
	if st.ToolsTotal != 3 || st.ToolsEnabled != 2 {
		t.Errorf("counts = enabled %d / total %d, want 2 / 3", st.ToolsEnabled, st.ToolsTotal)
	}
	if strings.Join(st.Plugins, ",") != "gmail,fs" {
		t.Errorf("plugins = %v, want [gmail fs]", st.Plugins)
	}
}

// TestRESTGetPosture: allow_send is true iff every gated tool is enabled.
// Non-gated tools never affect the answer.
func TestRESTGetPosture(t *testing.T) {
	cases := []struct {
		name  string
		tools []ToolInfo
		want  bool
	}{
		{
			name: "all gated enabled -> allow_send",
			tools: []ToolInfo{
				{Name: "gmail.send", Plugin: "gmail", Enabled: true, Gated: true},
				{Name: "fs.delete", Plugin: "fs", Enabled: true, Gated: true},
				{Name: "web.search", Plugin: "web", Enabled: false}, // non-gated, ignored
			},
			want: true,
		},
		{
			name: "one gated disabled -> read-only",
			tools: []ToolInfo{
				{Name: "gmail.send", Plugin: "gmail", Enabled: true, Gated: true},
				{Name: "fs.delete", Plugin: "fs", Enabled: false, Gated: true}, // disabled
				{Name: "web.search", Plugin: "web", Enabled: true},             // non-gated, ignored
			},
			want: false,
		},
		{
			name: "no gated tools -> read-only",
			tools: []ToolInfo{
				{Name: "web.search", Plugin: "web", Enabled: true},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRESTServer(&restFakeTools{tools: tc.tools})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/posture", nil)
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			var resp restPostureResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.AllowSend != tc.want {
				t.Errorf("allow_send = %v, want %v", resp.AllowSend, tc.want)
			}
		})
	}
}

// TestRESTSetPosture: POST flips every GATED tool to allow_send via
// SetEnabled and skips non-gated tools entirely.
func TestRESTSetPosture(t *testing.T) {
	f := &restFakeTools{
		tools: []ToolInfo{
			{Name: "gmail.send", Plugin: "gmail", Enabled: true, Gated: true},
			{Name: "fs.delete", Plugin: "fs", Enabled: true, Gated: true},
			{Name: "web.search", Plugin: "web", Enabled: true}, // non-gated
			{Name: "calendar.read", Plugin: "cal", Enabled: true},
		},
	}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/posture",
		strings.NewReader(`{"allow_send":false}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	// Exactly the two gated tools were toggled, each to false.
	if len(f.setCalls) != 2 {
		t.Fatalf("SetEnabled called %d times, want 2 (gated only): %+v", len(f.setCalls), f.setCalls)
	}
	gotTargets := map[string]bool{}
	for _, c := range f.setCalls {
		gotTargets[c.target] = true
		if c.on != false {
			t.Errorf("SetEnabled(%q) on = %v, want false", c.target, c.on)
		}
	}
	if !gotTargets["gmail.send"] || !gotTargets["fs.delete"] {
		t.Errorf("toggled targets = %v, want gmail.send + fs.delete", gotTargets)
	}
	if gotTargets["web.search"] || gotTargets["calendar.read"] {
		t.Errorf("non-gated tool was toggled: %v", gotTargets)
	}

	var resp restPostureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AllowSend != false || resp.Changed != 2 || resp.Skipped != 0 {
		t.Errorf("resp = %+v, want {allow_send:false changed:2 skipped:0}", resp)
	}
}

// TestRESTSetPostureBestEffort: a SetEnabled error on one gated tool is
// skipped (counted in `skipped`), the others still change, and the request
// stays 200 — enabling a gated tool of an unconfigured plugin must not fail
// the whole posture flip.
func TestRESTSetPostureBestEffort(t *testing.T) {
	f := &restFakeTools{
		tools: []ToolInfo{
			{Name: "gmail.send", Plugin: "gmail", Enabled: false, Gated: true},
			{Name: "slack.post", Plugin: "slack", Enabled: false, Gated: true}, // will error
			{Name: "fs.delete", Plugin: "fs", Enabled: false, Gated: true},
			{Name: "web.search", Plugin: "web", Enabled: true}, // non-gated, untouched
		},
		setErrFor: map[string]error{"slack.post": errRestUnknownTool},
	}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/posture",
		strings.NewReader(`{"allow_send":true}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	// All three gated tools were attempted; the non-gated one was not.
	if len(f.setCalls) != 3 {
		t.Fatalf("SetEnabled called %d times, want 3 (gated only): %+v", len(f.setCalls), f.setCalls)
	}

	var resp restPostureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AllowSend != true || resp.Changed != 2 || resp.Skipped != 1 {
		t.Errorf("resp = %+v, want {allow_send:true changed:2 skipped:1}", resp)
	}
}

// TestRESTSetPostureBadJSON: a malformed body is a 400 and never toggles
// any tool.
func TestRESTSetPostureBadJSON(t *testing.T) {
	f := &restFakeTools{
		tools: []ToolInfo{{Name: "gmail.send", Plugin: "gmail", Enabled: true, Gated: true}},
	}
	s := newRESTServer(f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/posture",
		strings.NewReader(`{"allow_send":`)) // truncated
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(f.setCalls) != 0 {
		t.Fatalf("SetEnabled called %d times on bad body, want 0", len(f.setCalls))
	}
}

// errRestUnknownTool is a sentinel for the error-path test.
var errRestUnknownTool = restErr("unknown tool")

type restErr string

func (e restErr) Error() string { return string(e) }
