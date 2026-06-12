package dashboard

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeIntegrationsCtl struct {
	mu          sync.Mutex
	list        []IntegrationView
	fields      map[string][]IntegrationField
	configCalls []string // "id|k=v,..."
	probeCalls  []string
	discCalls   []string
	probeGate   chan struct{}
}

func (f *fakeIntegrationsCtl) Integrations() []IntegrationView { return f.list }
func (f *fakeIntegrationsCtl) Fields(id string) ([]IntegrationField, error) {
	return f.fields[id], nil
}
func (f *fakeIntegrationsCtl) Configure(id string, values map[string]string) (string, error) {
	f.mu.Lock()
	// deterministic order not needed; record id + a stable key list
	parts := id + "|"
	for k, v := range values {
		parts += k + "=" + v + ","
	}
	f.configCalls = append(f.configCalls, parts)
	f.mu.Unlock()
	return "saved " + id, nil
}
func (f *fakeIntegrationsCtl) Probe(id string) (string, error) {
	f.mu.Lock()
	f.probeCalls = append(f.probeCalls, id)
	gate := f.probeGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return "ok", nil
}
func (f *fakeIntegrationsCtl) Disconnect(id string) (string, error) {
	f.mu.Lock()
	f.discCalls = append(f.discCalls, id)
	f.mu.Unlock()
	return "disconnected " + id, nil
}
func (f *fakeIntegrationsCtl) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.probeCalls)
}

func intgServer(fc IntegrationsControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fc != nil {
		s.SetIntegrations(fc)
	}
	return s
}

func TestIntegrations_PageNotConfigured(t *testing.T) {
	rec := get(t, intgServer(nil), "/integrations")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "aren't configured") {
		t.Errorf("nil integrations should render not-configured; got %d", rec.Code)
	}
}

func TestIntegrations_ListsWithStatusAndFields(t *testing.T) {
	fc := &fakeIntegrationsCtl{
		list: []IntegrationView{
			{ID: "gsuite", Label: "Google Workspace", Configured: true, Enabled: true, SetupHint: "Connect your Google account"},
			{ID: "brave", Label: "Brave Search", Configured: false},
		},
		fields: map[string][]IntegrationField{
			"gsuite": {{Key: "email", Prompt: "Email", Required: true}},
			"brave":  {{Key: "api_key", Prompt: "API key", Secret: true, Required: true}},
		},
	}
	body := get(t, intgServer(fc), "/integrations").Body.String()
	for _, want := range []string{"Google Workspace", "connected", "Brave Search", "not connected", "Connect your Google account", `type="password"`, "Test connection"} {
		if !strings.Contains(body, want) {
			t.Errorf("integrations page missing %q:\n%s", want, body)
		}
	}
	// Disconnect only on the configured one
	if strings.Count(body, "Disconnect") != 1 {
		t.Errorf("exactly the connected integration should offer Disconnect; got %d", strings.Count(body, "Disconnect"))
	}
}

func TestIntegrations_SaveCollectsFieldValues(t *testing.T) {
	fc := &fakeIntegrationsCtl{fields: map[string][]IntegrationField{
		"brave": {{Key: "api_key", Secret: true, Required: true}},
	}}
	rec := submitForm(t, intgServer(fc), "/integrations/brave/save", "api_key=sk-xyz")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if len(fc.configCalls) != 1 || !strings.Contains(fc.configCalls[0], "brave|api_key=sk-xyz") {
		t.Errorf("configure calls = %v", fc.configCalls)
	}
}

func TestIntegrations_SaveRejectsEmpty(t *testing.T) {
	fc := &fakeIntegrationsCtl{fields: map[string][]IntegrationField{"brave": {{Key: "api_key"}}}}
	submitForm(t, intgServer(fc), "/integrations/brave/save", "api_key=")
	if len(fc.configCalls) != 0 {
		t.Errorf("empty save should be rejected; got %v", fc.configCalls)
	}
}

func TestIntegrations_TestIsAsync(t *testing.T) {
	fc := &fakeIntegrationsCtl{probeGate: make(chan struct{})} // Probe blocks forever
	s := intgServer(fc)
	done := make(chan int, 1)
	go func() { done <- submitForm(t, s, "/integrations/gsuite/test", "").Code }()
	select {
	case code := <-done:
		if code != http.StatusSeeOther {
			t.Fatalf("want 303, got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test handler blocked on the probe — must be async")
	}
	deadline := time.After(time.Second)
	for fc.probes() == 0 {
		select {
		case <-deadline:
			t.Fatal("Probe was never called")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(fc.probeGate)
}

func TestIntegrations_Disconnect(t *testing.T) {
	fc := &fakeIntegrationsCtl{}
	submitForm(t, intgServer(fc), "/integrations/gsuite/disconnect", "")
	if len(fc.discCalls) != 1 || fc.discCalls[0] != "gsuite" {
		t.Errorf("disconnect calls = %v", fc.discCalls)
	}
}

func TestIntegrations_MutationsNilSafe(t *testing.T) {
	s := intgServer(nil)
	for _, p := range []string{"/integrations/x/save", "/integrations/x/test", "/integrations/x/disconnect"} {
		if rec := submitForm(t, s, p, "k=v"); rec.Code != http.StatusSeeOther {
			t.Errorf("%s with nil control: want 303, got %d", p, rec.Code)
		}
	}
}
