package dashboard

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

type fakeSecrets struct {
	views    []ServiceKeyView
	setCalls []string // "credID=value"
	rmCalls  []string
	setErr   error
}

func (f *fakeSecrets) List() []ServiceKeyView { return f.views }
func (f *fakeSecrets) SetSecret(id, v string) error {
	f.setCalls = append(f.setCalls, id+"="+v)
	return f.setErr
}
func (f *fakeSecrets) RemoveSecret(id string) error {
	f.rmCalls = append(f.rmCalls, id)
	return nil
}

func keysServer(fs SecretsControl) *Server {
	s := New(Config{}, nil, nil, nil, nil, nil)
	if fs != nil {
		s.SetSecrets(fs)
	}
	return s
}

func TestKeys_PageNotConfigured(t *testing.T) {
	s := keysServer(nil)
	rec := get(t, s, "/settings/keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "isn't configured") {
		t.Errorf("nil control should render not-configured; got:\n%s", rec.Body.String())
	}
}

func TestKeys_PageListsRows(t *testing.T) {
	fs := &fakeSecrets{views: []ServiceKeyView{
		{CredID: "openai", Name: "OpenAI", Category: "LLM provider", Set: true},
		{CredID: "tavily", Name: "Tavily Search", Category: "Web search", EnvDetected: true, EnvVar: "TAVILY_API_KEY"},
		{CredID: "skill:discord:token", Name: "Discord bot token", Category: "Integration", Required: true},
	}}
	s := keysServer(fs)
	body := get(t, s, "/settings/keys").Body.String()
	for _, want := range []string{"OpenAI", "Tavily Search", "Discord bot token", "LLM provider", "Web search", "Integration", "stored", "required", "TAVILY_API_KEY", "Replace", "Add", "Remove"} {
		if !strings.Contains(body, want) {
			t.Errorf("keys page missing %q", want)
		}
	}
}

// Hostile catalog name must be HTML-escaped, never reflected as live markup.
func TestKeys_PageEscapesFields(t *testing.T) {
	fs := &fakeSecrets{views: []ServiceKeyView{
		{CredID: "x", Name: `<img src=x onerror=alert(1)>`, Category: `</script><b>cat</b>`, Set: true},
	}}
	s := keysServer(fs)
	body := get(t, s, "/settings/keys").Body.String()
	if strings.Contains(body, "<img src=x onerror=") || strings.Contains(body, "<b>cat</b>") {
		t.Error("hostile field rendered as raw markup — XSS hole")
	}
	if !strings.Contains(body, "&lt;img") {
		t.Errorf("expected escaped markup in body:\n%s", body)
	}
}

func TestKeys_SetForm(t *testing.T) {
	fs := &fakeSecrets{}
	s := keysServer(fs)
	rec := cronPostForm(t, s, "/settings/keys/openai/set", "value=sk-abc123")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if len(fs.setCalls) != 1 || fs.setCalls[0] != "openai=sk-abc123" {
		t.Errorf("set not wired: %v", fs.setCalls)
	}
}

// C1: a secret smuggled in the QUERY STRING is rejected and never reaches the
// control; the value appears in no response body or Location header.
func TestKeys_SetRejectsQueryValue(t *testing.T) {
	fs := &fakeSecrets{}
	s := keysServer(fs)
	rec := cronPostForm(t, s, "/settings/keys/openai/set?value=sk-LEAKED", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if len(fs.setCalls) != 0 {
		t.Errorf("query-string value must NOT reach the control: %v", fs.setCalls)
	}
	if strings.Contains(rec.Header().Get("Location"), "sk-LEAKED") {
		t.Error("leaked secret into the redirect Location")
	}
}

func TestKeys_SetEmpty(t *testing.T) {
	fs := &fakeSecrets{}
	s := keysServer(fs)
	rec := cronPostForm(t, s, "/settings/keys/openai/set", "value=")
	if rec.Code != http.StatusSeeOther || len(fs.setCalls) != 0 {
		t.Fatalf("empty value: code=%d calls=%v", rec.Code, fs.setCalls)
	}
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Errorf("expected an error flash, got %q", rec.Header().Get("Location"))
	}
}

// C2: a SetSecret error must NOT be reflected (it can carry key fragments) — the
// flash is a fixed generic string; neither the error text nor the submitted
// value appears in the response.
func TestKeys_SetErrorNoLeak(t *testing.T) {
	fs := &fakeSecrets{setErr: errors.New("upstream rejected key sk-SECRETFRAG")}
	s := keysServer(fs)
	rec := cronPostForm(t, s, "/settings/keys/openai/set", "value=sk-SECRETFRAGxyz")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "sk-SECRETFRAG") {
		t.Errorf("error/value leaked into Location: %q", loc)
	}
	if !strings.Contains(loc, "err=") {
		t.Errorf("expected generic error flash, got %q", loc)
	}
}

func TestKeys_RemoveForm(t *testing.T) {
	fs := &fakeSecrets{}
	s := keysServer(fs)
	rec := cronPostForm(t, s, "/settings/keys/skill:discord:token/remove", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	if len(fs.rmCalls) != 1 || fs.rmCalls[0] != "skill:discord:token" {
		t.Errorf("remove not wired: %v", fs.rmCalls)
	}
}

func TestKeys_FormsNilSafe(t *testing.T) {
	s := keysServer(nil)
	if rec := cronPostForm(t, s, "/settings/keys/openai/set", "value=x"); rec.Code != http.StatusSeeOther {
		t.Errorf("nil set: want 303, got %d", rec.Code)
	}
	if rec := cronPostForm(t, s, "/settings/keys/openai/remove", ""); rec.Code != http.StatusSeeOther {
		t.Errorf("nil remove: want 303, got %d", rec.Code)
	}
}

// The rendered page must never carry a populated value attribute on the inputs
// (write-only): inputs are type=password with no value=.
func TestKeys_InputsAreWriteOnly(t *testing.T) {
	fs := &fakeSecrets{views: []ServiceKeyView{{CredID: "openai", Name: "OpenAI", Category: "LLM provider", Set: true}}}
	s := keysServer(fs)
	body := get(t, s, "/settings/keys").Body.String()
	if !strings.Contains(body, `type="password"`) {
		t.Error("key input should be a password field")
	}
	if strings.Contains(body, `name="value" value=`) || strings.Contains(body, `value="sk-`) {
		t.Error("input must not carry a value attribute (write-only)")
	}
}
