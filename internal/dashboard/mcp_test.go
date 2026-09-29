package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMCPCtl struct {
	mu           sync.Mutex
	servers      []MCPServerView
	connectURL   string
	connectGate  chan struct{} // if non-nil, Connect blocks until closed (tests async)
	removeCalls  []string
	trustCalls   []string
	connectCalls int
}

func (f *fakeMCPCtl) Servers() []MCPServerView { return f.servers }
func (f *fakeMCPCtl) Connect(url string) error {
	f.mu.Lock()
	f.connectURL = url
	f.connectCalls++
	gate := f.connectGate
	f.mu.Unlock()
	if gate != nil {
		<-gate // simulate a long host-side OAuth flow
	}
	return nil
}
func (f *fakeMCPCtl) Remove(target string) (bool, error) {
	f.mu.Lock()
	f.removeCalls = append(f.removeCalls, target)
	f.mu.Unlock()
	return true, nil
}
func (f *fakeMCPCtl) SetTrust(target, mode string) error {
	f.mu.Lock()
	f.trustCalls = append(f.trustCalls, target+"="+mode)
	f.mu.Unlock()
	return nil
}
func (f *fakeMCPCtl) calls() int      { f.mu.Lock(); defer f.mu.Unlock(); return f.connectCalls }
func (f *fakeMCPCtl) lastURL() string { f.mu.Lock(); defer f.mu.Unlock(); return f.connectURL }

func mcpServer(fc MCPControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fc != nil {
		s.SetMCP(fc)
	}
	return s
}

func TestMCP_PageNotConfigured(t *testing.T) {
	rec := get(t, mcpServer(nil), "/mcp")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Remote services aren&#39;t available on this Tenant.") {
		t.Errorf("nil mcp should render not-configured; got %d", rec.Code)
	}
}

func TestMCP_ListsServers(t *testing.T) {
	fc := &fakeMCPCtl{servers: []MCPServerView{
		{Label: "mcp:mcp.atlassian.com", URL: "https://mcp.atlassian.com/v1/mcp", Enabled: true, ToolCount: 31},
	}}
	body := get(t, mcpServer(fc), "/mcp").Body.String()
	for _, want := range []string{"mcp:mcp.atlassian.com", "31 tools", "Connect a service", "computer running Tenant"} {
		if !strings.Contains(body, want) {
			t.Errorf("mcp page missing %q:\n%s", want, body)
		}
	}
}

// Connect must NOT block the HTTP handler — even if the underlying flow takes
// "forever", the request returns promptly with a 303 (the async contract).
func TestMCP_ConnectIsAsync(t *testing.T) {
	fc := &fakeMCPCtl{connectGate: make(chan struct{})} // never closed → Connect would block forever
	s := mcpServer(fc)
	done := make(chan int, 1)
	go func() { done <- submitForm(t, s, "/mcp/connect", "url=https://x/v1/mcp").Code }()
	select {
	case code := <-done:
		if code != http.StatusSeeOther {
			t.Fatalf("want 303, got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect handler blocked on the host-side flow — must be async")
	}
	// the goroutine eventually invoked Connect with the URL
	deadline := time.After(time.Second)
	for fc.calls() == 0 {
		select {
		case <-deadline:
			t.Fatal("Connect was never called")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if fc.lastURL() != "https://x/v1/mcp" {
		t.Errorf("Connect URL = %q", fc.lastURL())
	}
	close(fc.connectGate) // let the goroutine finish
}

func TestMCP_ConnectRejectsEmpty(t *testing.T) {
	fc := &fakeMCPCtl{}
	submitForm(t, mcpServer(fc), "/mcp/connect", "url=")
	time.Sleep(20 * time.Millisecond)
	if fc.calls() != 0 {
		t.Error("empty URL should not trigger a connect")
	}
}

func TestMCP_Remove(t *testing.T) {
	fc := &fakeMCPCtl{}
	rec := submitForm(t, mcpServer(fc), "/mcp/remove", "target=mcp:x&confirm=yes")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if len(fc.removeCalls) != 1 || fc.removeCalls[0] != "mcp:x" {
		t.Errorf("remove calls = %v", fc.removeCalls)
	}
}

func TestMCP_MutationsNilSafe(t *testing.T) {
	s := mcpServer(nil)
	for _, p := range []string{"/mcp/connect", "/mcp/remove"} {
		if rec := submitForm(t, s, p, "url=x&target=y&confirm=yes"); rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control: want 303, got %d", p, rec.Code)
		}
	}
}

// REVSEC-9 and UX-7: the page shows a service's address without the parts
// that carry a key; Reconnect names the service instead of posting its
// address; an address in the page's URL is refused; and an address that
// can't work is refused on the page instead of failing out of sight.
func TestMCP_AddressStaysOffThePageAndOutOfURLs(t *testing.T) {
	s, _ := ssrServer()
	f := &fakeMCPCtl{servers: []MCPServerView{{Label: "mcp:tools", URL: "https://user:pw@tools.example/mcp?api_key=SEKRIT#frag", Enabled: true, NeedsReauth: true}}}
	s.SetMCP(f)
	body := get(t, s, "/mcp").Body.String()
	if strings.Contains(body, "SEKRIT") || strings.Contains(body, "pw@") || !strings.Contains(body, "https://tools.example/mcp") ||
		!strings.Contains(body, `<input type="hidden" name="target" value="mcp:tools">`) {
		t.Errorf("the Remote services page shows the key, or Reconnect posts the address:\n%s", body)
	}
	connected := func() (n int, u string) {
		for i := 0; i < 50; i++ {
			f.mu.Lock()
			n, u = f.connectCalls, f.connectURL
			f.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return n, u
	}
	if rec := ssrPost(t, s, "/mcp/connect", url.Values{"target": {"mcp:tools"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reconnect: %d", rec.Code)
	}
	if n, u := connected(); n != 1 || !strings.Contains(u, "SEKRIT") {
		t.Errorf("reconnect by name: %d calls with %q, want the saved address", n, u)
	}
	for _, tc := range []struct {
		target string
		form   url.Values
	}{
		{"/mcp/connect?url=https%3A%2F%2Fx.example%2Fmcp%3Fapi_key%3DQUERYKEY", url.Values{"url": {"https://x.example/mcp"}}},
		{"/mcp/connect", url.Values{"url": {"not a url"}}},
		{"/mcp/connect", url.Values{"url": {"file:///etc/passwd"}}},
		{"/mcp/connect", url.Values{"url": {"https://"}}},
	} {
		rec := ssrPost(t, s, tc.target, tc.form)
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") || strings.Contains(loc, "QUERYKEY") {
			t.Errorf("%s %v: %s, want refused on the page", tc.target, tc.form, loc)
		}
	}
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connectCalls != 1 {
		t.Errorf("a refused address still connected: %d calls, last %q", f.connectCalls, f.connectURL)
	}
}

// Trust and Disconnect answer a Datastar post in place, with a control or
// without one; a refused change is an alert that leaves the page alone; a
// plain post still gets its 303; Don't ask is still a 428 without its step.
// Connect and Reconnect stay plain posts that reload (mcp.go says why).
func TestMCP_InlineTrustAndDisconnect(t *testing.T) {
	fc := &fakeMCPCtl{servers: []MCPServerView{{Label: "mcp:tools", URL: "https://tools.example/mcp", Enabled: true, Trust: "ask", NeedsReauth: true}}}
	for _, c := range []MCPControl{fc, nil} {
		for _, p := range []struct {
			path string
			form url.Values
		}{
			{"/mcp/trust", url.Values{"target": {"mcp:tools"}, "mode": {"allow"}, "confirm": {"yes"}}},
			{"/mcp/remove", url.Values{"target": {"mcp:tools"}, "confirm": {"yes"}}},
		} {
			body := inlineAnswer(t, mcpServer(c), p.path, p.form, "mcp-body")
			if c == nil && !strings.Contains(body, "Remote services aren&#39;t available on this Tenant.") {
				t.Errorf("%s with no control: the region doesn't say so:\n%s", p.path, body)
			}
		}
	}
	s := mcpServer(fc)
	body := inlineAnswer(t, s, "/mcp/trust", url.Values{"target": {"mcp:tools"}, "mode": {"deny"}}, "mcp-body")
	if !strings.Contains(body, `<span class="toast-text">mcp:tools: your agent now can&#39;t use its tools.</span>`) {
		t.Errorf("Block's toast:\n%s", body)
	}
	inlineError(t, s, "/mcp/trust", url.Values{"target": {""}, "mode": {"deny"}}, "mcp-body", "Pick Ask first, Don't ask or Block for a connected service.")

	if rec := sendForm(s.Handler(), http.MethodPost, "/mcp/trust", url.Values{"target": {"mcp:tools"}, "mode": {"ask"}}, nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/mcp?msg=") {
		t.Errorf("a plain post: %d to %q, want 303 to /mcp with its flash", rec.Code, rec.Header().Get("Location"))
	}
	fc.mu.Lock()
	calls := len(fc.trustCalls)
	fc.mu.Unlock()
	rec := sendForm(s.Handler(), http.MethodPost, "/mcp/trust", url.Values{"target": {"mcp:tools"}, "mode": {"allow"}}, datastar)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if rec.Code != http.StatusPreconditionRequired || len(fc.trustCalls) != calls {
		t.Errorf("Don't ask without its confirm step over Datastar: %d, trust calls %v; want 428 and nothing changed", rec.Code, fc.trustCalls)
	}

	page := get(t, s, "/mcp").Body.String()
	if n := strings.Count(page, `<form method="POST" action="/mcp/connect" class="inline">`); n != 2 {
		t.Errorf("Connect and Reconnect: %d plain forms, want 2:\n%s", n, page)
	}
}
