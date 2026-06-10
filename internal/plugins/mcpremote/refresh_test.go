package mcpremote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// rotatingTokenServer issues a fresh access+refresh token on every refresh_token
// grant (mimics Atlassian's single-use rotating refresh tokens). failAll makes
// every refresh return 400 (to test fail-safe).
func rotatingTokenServer(t *testing.T, count *int32, failAll bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if failAll {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		n := atomic.AddInt32(count, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("acc-%d", n),
			"refresh_token": fmt.Sprintf("ref-%d", n), // rotation: new refresh token each time
			"token_type":    "bearer",
			"expires_in":    3600,
		})
	}))
}

func newSourceFor(t *testing.T, srvURL string, tok *oauth2.Token) (*refreshingSource, string) {
	t.Helper()
	dir := t.TempDir()
	path := tokenCachePath(dir, "https://srv.example/v1/mcp")
	h := newPersistentHandler("https://srv.example/v1/mcp", "http://127.0.0.1:8765/callback", "Tenant", path, false, nil, http.DefaultClient, nil)
	h.cache = &tokenCache{ServerURL: "https://srv.example/v1/mcp", ClientID: "cid", TokenURL: srvURL, Token: tok}
	h.loaded = true
	ts, err := h.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("TokenSource: ts=%v err=%v", ts, err)
	}
	return ts.(*refreshingSource), path
}

// TestRefresh_ProactiveAndPersisted: a token inside the proactive window is
// refreshed once, the rotated token is persisted, and the fresh token is then
// reused without further network calls (no refresh spam).
func TestRefresh_ProactiveAndPersisted(t *testing.T) {
	var count int32
	srv := rotatingTokenServer(t, &count, false)
	defer srv.Close()
	// Expires in 1 min < 5-min window → must refresh.
	src, path := newSourceFor(t, srv.URL, &oauth2.Token{
		AccessToken: "acc-0", RefreshToken: "ref-0", TokenType: "bearer",
		Expiry: time.Now().Add(1 * time.Minute),
	})

	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "acc-1" {
		t.Fatalf("expected proactive refresh to acc-1, got %q", tok.AccessToken)
	}
	if atomic.LoadInt32(&count) != 1 {
		t.Fatalf("expected exactly 1 refresh, got %d", count)
	}
	// Rotated token persisted (so a restart uses ref-1, not the spent ref-0).
	c, err := loadTokenCache(path)
	if err != nil || c.Token.AccessToken != "acc-1" || c.Token.RefreshToken != "ref-1" {
		t.Fatalf("rotated token not persisted: %+v err=%v", c, err)
	}
	// The new token has a full TTL → reused, NO second refresh (no spam).
	tok2, _ := src.Token()
	if tok2.AccessToken != "acc-1" || atomic.LoadInt32(&count) != 1 {
		t.Fatalf("expected reuse without refresh; tok=%q count=%d", tok2.AccessToken, count)
	}
}

// TestRefresh_ReusesFreshToken: a token far from expiry is returned as-is, no network.
func TestRefresh_ReusesFreshToken(t *testing.T) {
	var count int32
	srv := rotatingTokenServer(t, &count, false)
	defer srv.Close()
	src, _ := newSourceFor(t, srv.URL, &oauth2.Token{
		AccessToken: "acc-fresh", RefreshToken: "ref-0", TokenType: "bearer",
		Expiry: time.Now().Add(1 * time.Hour),
	})
	tok, err := src.Token()
	if err != nil || tok.AccessToken != "acc-fresh" || atomic.LoadInt32(&count) != 0 {
		t.Fatalf("fresh token should be reused with no refresh: tok=%q count=%d err=%v", tok.AccessToken, count, err)
	}
}

// TestRefresh_FailSafe: when a proactive refresh fails but the current token is
// still valid, return the current token (don't break a working session).
func TestRefresh_FailSafe(t *testing.T) {
	var count int32
	srv := rotatingTokenServer(t, &count, true) // every refresh 400s
	defer srv.Close()
	src, _ := newSourceFor(t, srv.URL, &oauth2.Token{
		AccessToken: "acc-valid", RefreshToken: "ref-0", TokenType: "bearer",
		Expiry: time.Now().Add(2 * time.Minute), // inside window but still valid
	})
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("fail-safe should not error while the token is valid: %v", err)
	}
	if tok.AccessToken != "acc-valid" {
		t.Fatalf("expected the still-valid current token, got %q", tok.AccessToken)
	}
}

// TestRefresh_HardFailWhenExpired: refresh fails AND the token is already expired
// → propagate the error (the SDK will trigger re-auth).
func TestRefresh_HardFailWhenExpired(t *testing.T) {
	var count int32
	srv := rotatingTokenServer(t, &count, true)
	defer srv.Close()
	src, _ := newSourceFor(t, srv.URL, &oauth2.Token{
		AccessToken: "acc-dead", RefreshToken: "ref-0", TokenType: "bearer",
		Expiry: time.Now().Add(-time.Hour), // already expired
	})
	if _, err := src.Token(); err == nil {
		t.Fatal("expected an error when refresh fails and the token is expired")
	}
}
