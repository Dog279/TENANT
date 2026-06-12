package dashboard

import (
	"net/http"
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
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "aren't configured") {
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
	rec := submitForm(t, mcpServer(fc), "/mcp/remove", "target=mcp:x")
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
		if rec := submitForm(t, s, p, "url=x&target=y"); rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control: want 303, got %d", p, rec.Code)
		}
	}
}
