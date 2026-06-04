package dashboard

// wire_test.go (TEN-81) is the dashboard's end-to-end wire-contract test. The
// sibling rest_/ws_/auth_ tests cover units in isolation; this one exercises
// the ASSEMBLED, SECURED server exactly as Run() wires it —
// s.secure(s.Handler()) over a real httptest.Server — so a regression in how
// routing and the auth/CORS envelope compose (not just each in isolation) is
// caught. Helpers use the `wire` prefix to avoid clashing with the rest_/ws_/
// auth_ fakes in this same package.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/agent"
)

// wireSetCall records one SetEnabled invocation so the test can assert the
// secured POST path reached the tool control with the right (target, on).
type wireSetCall struct {
	target string
	on     bool
}

// wireFakeTools is a ToolControl stand-in for the wire-contract test.
type wireFakeTools struct {
	tools    []ToolInfo
	plugins  []string
	setRet   int
	setScope string
	setCalls []wireSetCall
}

func (f *wireFakeTools) ToolList() []ToolInfo { return f.tools }
func (f *wireFakeTools) Plugins() []string    { return f.plugins }

func (f *wireFakeTools) SetEnabled(target string, on bool) (int, string, error) {
	f.setCalls = append(f.setCalls, wireSetCall{target, on})
	return f.setRet, f.setScope, nil
}

func (f *wireFakeTools) SetPluginEnabled(label string, on bool) (int, string, error) {
	return 0, "plugin", nil
}

// wireFakeRunner is a no-op AgentRunner — the wire test exercises the REST +
// health surface through the secure envelope, not turn execution (the WS turn
// path has its own unit coverage in ws_test.go). Present so New() gets a
// non-nil runner, matching the live wiring.
type wireFakeRunner struct{}

func (wireFakeRunner) Turn(context.Context, agent.TurnRequest) (*agent.TurnResult, error) {
	return &agent.TurnResult{}, nil
}
func (wireFakeRunner) Interject(string) {}

// wireServer assembles the server and returns it secured exactly as Run()
// does: secure() wraps the routed Handler(). The caller fronts it with an
// httptest.Server.
func wireServer(f *wireFakeTools) http.Handler {
	s := New(Config{Auth: "secret"}, wireFakeRunner{}, f, nil, agent.NewBroker(0), nil)
	return s.secure(s.Handler())
}

// wireGet issues a GET through the live server, attaching the bearer token
// when one is given. Returns status and body.
func wireGet(t *testing.T, base, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestWireContract drives the assembled+secured server over a real socket and
// asserts the contract the live process exposes: protected REST behind bearer
// auth, an exempt health probe, and a working write path through the same
// envelope.
func TestWireContract(t *testing.T) {
	f := &wireFakeTools{
		tools: []ToolInfo{
			{Name: "gmail.send", Plugin: "gmail", Enabled: true},
			{Name: "web.search", Plugin: "web", Enabled: true},
		},
		plugins: []string{"gmail", "web"},
		setRet:  1, setScope: "tool",
	}
	ts := httptest.NewServer(wireServer(f))
	defer ts.Close()

	// GET /api/tools WITHOUT a token → 401 (secure envelope rejects it before
	// the routed handler runs).
	t.Run("api/tools without token → 401", func(t *testing.T) {
		code, _ := wireGet(t, ts.URL, "/api/tools", "")
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})

	// GET /api/tools WITH the token → 200 + the JSON tool list (auth passes,
	// routing reaches handleTools).
	t.Run("api/tools with token → 200 + tool list", func(t *testing.T) {
		code, body := wireGet(t, ts.URL, "/api/tools", "secret")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", code, body)
		}
		var got []restToolView
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode tool list: %v (body %q)", err, body)
		}
		if len(got) != 2 {
			t.Fatalf("tool list len = %d, want 2: %+v", len(got), got)
		}
		names := map[string]bool{got[0].Name: true, got[1].Name: true}
		if !names["gmail.send"] || !names["web.search"] {
			t.Fatalf("tool list = %+v, want gmail.send + web.search", got)
		}
	})

	// GET /healthz WITHOUT a token → 200 (liveness probe is exempt from auth).
	t.Run("healthz without token → 200 (exempt)", func(t *testing.T) {
		code, body := wireGet(t, ts.URL, "/healthz", "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (healthz must be exempt)", code)
		}
		var m map[string]string
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("decode healthz: %v (body %q)", err, body)
		}
		if m["status"] != "ok" {
			t.Fatalf("healthz body = %v, want status=ok", m)
		}
	})

	// POST /api/tools/{name} WITH the token toggles via the fake (write path
	// works through the secured envelope, and the toggle reaches the control).
	t.Run("post api/tools/{name} with token toggles the fake", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/tools/gmail.send",
			strings.NewReader(`{"enabled":false}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200 (body %q)", resp.StatusCode, b)
		}
		if len(f.setCalls) != 1 {
			t.Fatalf("SetEnabled called %d times, want 1", len(f.setCalls))
		}
		if c := f.setCalls[0]; c.target != "gmail.send" || c.on != false {
			t.Fatalf("SetEnabled called with %+v, want {gmail.send false}", c)
		}
	})
}
