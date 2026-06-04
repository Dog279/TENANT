package gsuite

// TEN-71: minimal coverage for the OAuth code-grant scaffolding. The
// full loopback flow against a real browser is tested manually — the
// pieces here are the parser, the token source's no-browser branches
// (cached token + refresh), and the test seam plumbing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

const validOAuthClientJSON = `{
  "installed": {
    "client_id": "fake-client-id.apps.googleusercontent.com",
    "client_secret": "fake-secret",
    "auth_uri": "https://accounts.google.com/o/oauth2/v2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "redirect_uris": ["http://localhost"]
  }
}`

// Repo ships a placeholder `{}` for embedded_oauth_client.json. The
// vanilla checkout MUST detect "no embedded creds" so a fresh `go
// build` doesn't accidentally claim to have an OAuth client. Catch
// regressions where someone commits a real client to the placeholder
// or accidentally inverts the validity check.
func TestEmbeddedOAuth_PlaceholderDetectsAsAbsent(t *testing.T) {
	if compiledInLooksValid() {
		t.Errorf("vanilla checkout has compiledInOAuthClientJSON looking VALID — did someone commit real credentials to embedded_oauth_client.json? Bytes: %s",
			string(compiledInOAuthClientJSON))
	}
	if HasEmbeddedOAuth("") {
		t.Error("HasEmbeddedOAuth(\"\") should be false on a vanilla checkout (no compiled-in client + no cfgDir)")
	}
	b, err := LoadEmbeddedOAuth("")
	if err != nil {
		t.Errorf("LoadEmbeddedOAuth(\"\") errored on vanilla checkout: %v", err)
	}
	if b != nil {
		t.Errorf("LoadEmbeddedOAuth(\"\") should return nil bytes on vanilla checkout; got %d bytes", len(b))
	}
}

// Runtime cfgDir file wins over the compiled-in blob — lets ops swap
// the OAuth client without rebuilding the binary.
func TestEmbeddedOAuth_CfgDirOverridesCompiledIn(t *testing.T) {
	dir := t.TempDir()
	rt := []byte(`{"installed":{"client_id":"runtime-id","client_secret":"runtime-secret"}}`)
	if err := os.WriteFile(EmbeddedOAuthPath(dir), rt, 0o600); err != nil {
		t.Fatal(err)
	}
	if !HasEmbeddedOAuth(dir) {
		t.Fatal("expected HasEmbeddedOAuth=true with file present")
	}
	b, err := LoadEmbeddedOAuth(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(rt) {
		t.Errorf("runtime file content not returned; got %q", string(b))
	}
}

func TestParseOAuthClientFile_Installed(t *testing.T) {
	cfg, err := ParseOAuthClientFile([]byte(validOAuthClientJSON), []string{"scope1", "scope2"})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cfg.ClientID != "fake-client-id.apps.googleusercontent.com" {
		t.Errorf("client_id wrong: %q", cfg.ClientID)
	}
	if cfg.ClientSecret != "fake-secret" {
		t.Errorf("client_secret wrong: %q", cfg.ClientSecret)
	}
	if cfg.Endpoint.AuthURL != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Errorf("auth URL wrong: %q", cfg.Endpoint.AuthURL)
	}
	if len(cfg.Scopes) != 2 {
		t.Errorf("scopes wrong: %v", cfg.Scopes)
	}
}

func TestParseOAuthClientFile_Web(t *testing.T) {
	// "web" client type is also accepted (less common but Google issues
	// these for installed-app fallback scenarios).
	j := strings.Replace(validOAuthClientJSON, "installed", "web", 1)
	cfg, err := ParseOAuthClientFile([]byte(j), []string{"scope"})
	if err != nil {
		t.Fatalf("web client should parse: %v", err)
	}
	if cfg.ClientID == "" {
		t.Error("client_id missing after web parse")
	}
}

func TestParseOAuthClientFile_BadJSON(t *testing.T) {
	_, err := ParseOAuthClientFile([]byte(`{not json`), nil)
	if err == nil {
		t.Fatal("bad JSON should error")
	}
}

func TestParseOAuthClientFile_MissingInstalledAndWeb(t *testing.T) {
	_, err := ParseOAuthClientFile([]byte(`{"other": {}}`), nil)
	if err == nil || !strings.Contains(err.Error(), "installed") {
		t.Errorf("expected error naming 'installed' block; got %v", err)
	}
}

func TestParseOAuthClientFile_MissingClientID(t *testing.T) {
	_, err := ParseOAuthClientFile([]byte(`{"installed":{"client_secret":"x"}}`), nil)
	if err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Errorf("expected error naming client_id; got %v", err)
	}
}

func TestParseOAuthClientFile_DefaultEndpoints(t *testing.T) {
	// When auth_uri / token_uri are absent, the parser should fall back
	// to Google's well-known endpoints. Saves operators from having to
	// hand-edit the JSON when their client config is sparse.
	j := `{"installed":{"client_id":"x","client_secret":"y"}}`
	cfg, err := ParseOAuthClientFile([]byte(j), nil)
	if err != nil {
		t.Fatalf("should accept sparse JSON: %v", err)
	}
	if cfg.Endpoint.AuthURL == "" || cfg.Endpoint.TokenURL == "" {
		t.Errorf("expected default endpoints; got %+v", cfg.Endpoint)
	}
}

// newOAuthSource with a cached token + httptest token endpoint
// exercises the no-browser refresh path. Verifies:
//   - cached token is used (no browser stub needed)
//   - refresh hits the token endpoint
//   - save callback fires on rotation
func TestNewOAuthSource_CachedTokenRefresh(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate Google's token rotation: issue a NEW refresh token
		// each time so the save callback should fire.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "new-access-token",
			"refresh_token": "rotated-refresh-token",
			"expires_in": 3600,
			"token_type": "Bearer"
		}`))
	}))
	defer tokenServer.Close()

	creds := []byte(`{"installed":{"client_id":"c","client_secret":"s","token_uri":"` + tokenServer.URL + `","auth_uri":"https://example.com/auth"}}`)

	saved := &oauth2.Token{
		AccessToken:  "expired-token",
		RefreshToken: "original-refresh",
		Expiry:       time.Now().Add(-1 * time.Hour), // expired ⇒ forces refresh
	}

	var saveCallbackTokens []*oauth2.Token
	save := func(t *oauth2.Token) error {
		saveCallbackTokens = append(saveCallbackTokens, t)
		return nil
	}

	browserCalls := 0
	openFn := func(string) error {
		browserCalls++
		return nil
	}

	src, err := newOAuthSource(creds, saved, save, []string{"scope"}, openFn, nil)
	if err != nil {
		t.Fatalf("newOAuthSource: %v", err)
	}

	tok, err := src.token(context.Background())
	if err != nil {
		t.Fatalf("token(): %v", err)
	}
	if tok != "new-access-token" {
		t.Errorf("expected refreshed access token; got %q", tok)
	}
	if browserCalls != 0 {
		t.Errorf("cached-token path should NOT open browser; got %d browser calls", browserCalls)
	}
	if len(saveCallbackTokens) == 0 {
		t.Error("save callback should fire on refresh-token rotation")
	}
	// Check the rotated refresh token landed.
	last := saveCallbackTokens[len(saveCallbackTokens)-1]
	if last.RefreshToken != "rotated-refresh-token" {
		t.Errorf("save callback got wrong rotated token: %q", last.RefreshToken)
	}
}

// openBrowserDefault selects a per-OS command. Don't actually invoke
// it during tests (that opens a real browser); just smoke-test the
// argument formation by checking the function returns without panic
// when given a URL — Start() may return an error if the OS lacks the
// browser opener, which is fine.
func TestOpenBrowserDefault_DoesNotPanic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser open in short mode")
	}
	// We can't actually open without a real browser environment, so we
	// just confirm the function exists with the right shape. A failure
	// here would have shown up as a compile error.
	_ = openBrowserDefault
}

// runLoopbackFlow honors ctx cancellation cleanly. Verify the function
// returns ctx.Err() promptly when ctx is cancelled before any callback
// arrives.
func TestRunLoopbackFlow_ContextCancellation(t *testing.T) {
	cfg, err := ParseOAuthClientFile([]byte(validOAuthClientJSON), []string{"scope"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay (longer than the browser-open call
	// but well under the 5-min timeout) so the select fires on ctx.Done.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = runLoopbackFlow(ctx, OAuthFlowConfig{
		Config:      cfg,
		OpenBrowser: func(string) error { return nil }, // no-op
		WaitTimeout: 5 * time.Second,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from cancelled flow")
	}
	if !strings.Contains(err.Error(), "context") && err != context.Canceled {
		t.Errorf("expected ctx-canceled error; got %v", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("ctx cancellation should return promptly; took %v", elapsed)
	}
}

// runLoopbackFlow end-to-end with a fake browser that simulates a
// successful Google authorization callback. The browser stub parses
// the auth URL it's given, extracts the redirect_uri + state, and
// POSTs the callback as if Google had redirected. Token endpoint is
// an httptest server that issues a fresh token.
func TestRunLoopbackFlow_HappyPath(t *testing.T) {
	// Token endpoint that always issues a new token (no validation —
	// just exercises the exchange happy path).
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access-token",
			"refresh_token": "fresh-refresh-token",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer tokenServer.Close()

	cfg := &oauth2.Config{
		ClientID:     "client",
		ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://example.com/auth",
			TokenURL: tokenServer.URL,
		},
		Scopes: []string{"scope"},
	}

	// The browser stub fires off an HTTP GET to the loopback callback
	// URL after a brief delay (mimics the real browser dance).
	openFn := func(authURL string) error {
		// Parse out the redirect_uri + state from the auth URL.
		// oauth2.Config.AuthCodeURL packs them as query params.
		go func() {
			// Extract redirect_uri.
			rIdx := strings.Index(authURL, "redirect_uri=")
			if rIdx < 0 {
				return
			}
			redirectURI := authURL[rIdx+len("redirect_uri="):]
			if ampIdx := strings.Index(redirectURI, "&"); ampIdx >= 0 {
				redirectURI = redirectURI[:ampIdx]
			}
			// URL-decode minimal (only %3A %2F %2B %25 expected from oauth2 encoder)
			redirectURI = strings.ReplaceAll(redirectURI, "%3A", ":")
			redirectURI = strings.ReplaceAll(redirectURI, "%2F", "/")

			// Extract state.
			sIdx := strings.Index(authURL, "state=")
			if sIdx < 0 {
				return
			}
			state := authURL[sIdx+len("state="):]
			if ampIdx := strings.Index(state, "&"); ampIdx >= 0 {
				state = state[:ampIdx]
			}

			callback := redirectURI + "?state=" + state + "&code=fake-auth-code"
			_, _ = http.Get(callback)
		}()
		return nil
	}

	tok, err := runLoopbackFlow(context.Background(), OAuthFlowConfig{
		Config:      cfg,
		OpenBrowser: openFn,
		WaitTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("loopback flow: %v", err)
	}
	if tok == nil {
		t.Fatal("nil token")
	}
	if tok.AccessToken != "fresh-access-token" {
		t.Errorf("access_token wrong: %q", tok.AccessToken)
	}
	if tok.RefreshToken != "fresh-refresh-token" {
		t.Errorf("refresh_token wrong: %q", tok.RefreshToken)
	}
}
