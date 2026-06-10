package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAtlassianSkillKind_Shape(t *testing.T) {
	k := atlassianSkillKind()
	if k.ID != "atlassian" || !k.Wired {
		t.Fatalf("kind id=%q wired=%v", k.ID, k.Wired)
	}
	byKey := map[string]skillKindField{}
	for _, f := range k.Fields {
		byKey[f.Key] = f
	}
	for _, want := range []string{"auth", "site", "client_id", "client_secret", "email", "api_token"} {
		if _, ok := byKey[want]; !ok {
			t.Errorf("missing field %q", want)
		}
	}
	// Secrets must route to credentials.json.
	if !byKey["client_secret"].Secret || !byKey["api_token"].Secret {
		t.Error("client_secret and api_token must be Secret fields")
	}
	// OAuth fields shown only in oauth mode; token fields only in token mode.
	if byKey["client_id"].ShowIf == nil || byKey["client_id"].ShowIf(map[string]string{"auth": "token"}) {
		t.Error("client_id should be hidden in token mode")
	}
	if byKey["api_token"].ShowIf == nil || byKey["api_token"].ShowIf(map[string]string{"auth": "oauth"}) {
		t.Error("api_token should be hidden in oauth mode")
	}
	// Registered in the production catalog.
	if _, ok := skillKinds["atlassian"]; !ok {
		t.Error("atlassian not registered in skillKinds")
	}
}

func TestValidateAtlassianSite(t *testing.T) {
	if err := validateAtlassianSite("https://x.atlassian.net"); err != nil {
		t.Errorf("valid site rejected: %v", err)
	}
	if err := validateAtlassianSite("x.atlassian.net"); err == nil {
		t.Error("non-https site should be rejected")
	}
}

func TestProbeAtlassian_TokenMode(t *testing.T) {
	var authSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"total": 0, "issues": []any{}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	creds, _ := loadCredentials(dir)
	creds.set(skillSecretID("atlassian", "api_token"), "tok")
	settings := map[string]string{"auth": "token", "site": srv.URL, "email": "e@x.com"}

	id, err := probeAtlassianWith(context.Background(), creds, settings, dir, nil, nil)
	if err != nil {
		t.Fatalf("token probe: %v", err)
	}
	if !strings.Contains(id, "e@x.com") {
		t.Errorf("identity = %q, want it to mention the email", id)
	}
	if !strings.HasPrefix(authSeen, "Basic ") {
		t.Errorf("token mode must use Basic auth, got %q", authSeen)
	}
}

func TestProbeAtlassian_OAuthRequiresClientCreds(t *testing.T) {
	dir := t.TempDir()
	creds, _ := loadCredentials(dir)
	settings := map[string]string{"auth": "oauth", "site": "https://x.atlassian.net"} // no client_id/secret
	// nil openBrowser → must error on missing creds BEFORE any browser attempt.
	_, err := probeAtlassianWith(context.Background(), creds, settings, dir, nil, nil)
	if err == nil {
		t.Fatal("oauth mode without client_id/secret must error (no browser)")
	}
	if !strings.Contains(err.Error(), "client_id") {
		t.Errorf("error should name the missing creds: %v", err)
	}
}

func TestProbeAtlassian_MCPMode(t *testing.T) {
	creds, _ := loadCredentials(t.TempDir())
	var gotURL string
	connect := func(_ context.Context, url string) (string, error) {
		gotURL = url
		return "connected — 9 tools", nil
	}
	// auth=mcp routes to the connector with the OFFICIAL endpoint, no site needed.
	id, err := probeAtlassianWith(context.Background(), creds, map[string]string{"auth": "mcp"}, "", nil, connect)
	if err != nil {
		t.Fatalf("mcp probe: %v", err)
	}
	if gotURL != atlassianMCPURL {
		t.Errorf("connector called with %q, want the official %q", gotURL, atlassianMCPURL)
	}
	if id != "connected — 9 tools" {
		t.Errorf("identity = %q", id)
	}
}

func TestProbeAtlassian_MCPModeNoConnector(t *testing.T) {
	creds, _ := loadCredentials(t.TempDir())
	// auth=mcp with no connector wired must error (not silently no-op).
	if _, err := probeAtlassianWith(context.Background(), creds, map[string]string{"auth": "mcp"}, "", nil, nil); err == nil {
		t.Error("mcp mode without a connector must error")
	}
}

func TestProbeAtlassian_RequiresSite(t *testing.T) {
	creds, _ := loadCredentials(t.TempDir())
	if _, err := probeAtlassianWith(context.Background(), creds, map[string]string{"auth": "token"}, "", nil, nil); err == nil {
		t.Error("missing site must error")
	}
}
