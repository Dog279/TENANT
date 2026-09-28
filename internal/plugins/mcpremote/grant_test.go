package mcpremote

// One OAuth grant shared by several Tenant processes (serve, the TUI,
// `tenant mcp connect`), each a handler on the same cache file, against a fake
// authorization server whose refresh token rotates. Token values are fakes and
// are never printed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

const grantURL = "https://srv.example/v1/mcp"

// grantServer is a token endpoint with a rotating refresh token: each refresh
// answers with a new one and retires the one spent. The retired one still
// works once but kills its successor, as Cloudflare's workers-oauth-provider
// (Atlassian's /v1) does; strict instead revokes the whole grant on reuse.
type grantServer struct {
	*httptest.Server
	strict    bool
	refreshes atomic.Int32
	fail503   atomic.Int32 // the next N refreshes answer 503
	hang      atomic.Bool  // refreshes wait until the client gives up

	mu        sync.Mutex
	cur, prev string
	revoked   bool
	n         int
}

func newGrantServer(t *testing.T, strict bool) *grantServer {
	a := &grantServer{strict: strict, cur: "rt-0"}
	a.Server = httptest.NewServer(http.HandlerFunc(a.token))
	t.Cleanup(a.Close)
	return a
}

func (a *grantServer) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	a.refreshes.Add(1)
	if a.hang.Load() {
		<-r.Context().Done()
		return
	}
	if a.fail503.Load() > 0 {
		a.fail503.Add(-1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	rt := r.FormValue("refresh_token")
	a.mu.Lock()
	defer a.mu.Unlock()
	ok := false
	switch {
	case a.revoked:
	case rt == a.cur:
		a.prev, ok = a.cur, true
	case rt == a.prev && !a.strict:
		ok = true // the retired token works once more; its successor dies below
	case rt == a.prev:
		a.revoked = true // reuse detected: the whole grant goes
	}
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	a.n++
	a.cur = fmt.Sprintf("rt-%d", a.n)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": fmt.Sprintf("at-%d", a.n), "refresh_token": a.cur,
		"token_type": "bearer", "expires_in": 28800,
	})
}

// live reports whether refresh token rt would still be accepted.
func (a *grantServer) live(rt string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.revoked && (rt == a.cur || !a.strict && rt == a.prev)
}

func seedGrant(t *testing.T, path, tokenURL string, expiry time.Time, reauth bool) {
	t.Helper()
	if err := saveTokenCache(path, &tokenCache{
		ServerURL: grantURL, ClientID: "cid", TokenURL: tokenURL, AuthURL: tokenURL,
		Token:        &oauth2.Token{AccessToken: "at-0", RefreshToken: "rt-0", Expiry: expiry},
		ReauthNeeded: reauth,
	}); err != nil {
		t.Fatal(err)
	}
}

// process is one Tenant process: a non-interactive handler that loads the
// cache itself, recording its auth-change announcements.
func process(path string, announced *[]bool) *persistentHandler {
	h := newPersistentHandler(grantURL, "http://127.0.0.1:1/callback", "Tenant", path, false, nil, http.DefaultClient, nil)
	var mu sync.Mutex
	h.onAuthChange = func(needs bool, _ string) { mu.Lock(); *announced = append(*announced, needs); mu.Unlock() }
	return h
}

func source(t *testing.T, h *persistentHandler) *refreshingSource {
	t.Helper()
	ts, err := h.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatalf("TokenSource: nil=%v err=%v", ts == nil, err)
	}
	return ts.(*refreshingSource)
}

func onDisk(t *testing.T, path string) *tokenCache {
	t.Helper()
	c, err := loadTokenCache(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Two processes that both loaded the grant refresh at the same moment: one
// spends the refresh token, the other adopts the result with no network call,
// and every later refresh spends the live token, so the grant never strands.
// Before the cache lock, the second spent its stale copy and the last save
// won, leaving a dead token on disk.
func TestSharedGrant_ProcessesDoNotStrandIt(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			a := newGrantServer(t, strict)
			path := tokenCachePath(t.TempDir(), grantURL)
			seedGrant(t, path, a.URL, time.Now().Add(-time.Minute), false)
			var announced []bool
			serve, tui := source(t, process(path, &announced)), source(t, process(path, &announced))

			var wg sync.WaitGroup
			toks := make([]*oauth2.Token, 2)
			for i, s := range []*refreshingSource{serve, tui} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					toks[i], _ = s.Token()
				}()
			}
			wg.Wait()
			if toks[0] == nil || toks[1] == nil || toks[0].AccessToken != toks[1].AccessToken || a.refreshes.Load() != 1 {
				t.Fatalf("want one refresh, adopted by the other process: got %d refreshes, tokens equal=%v",
					a.refreshes.Load(), toks[0] != nil && toks[1] != nil && toks[0].AccessToken == toks[1].AccessToken)
			}

			// Later, each process's copy expires in turn: each spends only the
			// live token, or adopts what the other saved.
			for _, s := range []*refreshingSource{serve, tui, serve, tui} {
				s.cur.Expiry = time.Now().Add(-time.Minute)
				if _, err := s.Token(); err != nil {
					t.Fatalf("refresh after rotation: %v", err)
				}
			}
			if a.refreshes.Load() != 3 {
				t.Errorf("want 3 refreshes (every other expiry adopts), got %d", a.refreshes.Load())
			}
			c := onDisk(t, path)
			if !a.live(c.Token.RefreshToken) || c.ReauthNeeded || len(announced) != 0 {
				t.Fatalf("grant stranded: disk token live=%v reauth_needed=%v announced=%v", a.live(c.Token.RefreshToken), c.ReauthNeeded, announced)
			}
		})
	}
}

// markReauth writes only the re-auth fields: a newer token another process
// saved stays, where the old whole-struct save put the stale one back.
func TestMarkReauth_KeepsNewerDiskToken(t *testing.T) {
	path := tokenCachePath(t.TempDir(), grantURL)
	seedGrant(t, path, "https://as.example/token", time.Now().Add(-time.Hour), false)
	var announced []bool
	h := process(path, &announced)
	h.Health() // loads the cache as it is now

	newer := onDisk(t, path)
	newer.Token = &oauth2.Token{AccessToken: "at-9", RefreshToken: "rt-9", Expiry: time.Now().Add(time.Hour)}
	if err := saveTokenCache(path, newer); err != nil {
		t.Fatal(err)
	}
	h.markReauth("invalid_grant")
	if c := onDisk(t, path); c.Token.RefreshToken != "rt-9" || !c.ReauthNeeded {
		t.Fatalf("markReauth: newer token kept=%v reauth_needed=%v", c.Token.RefreshToken == "rt-9", c.ReauthNeeded)
	}
	h.clearReauth()
	if c := onDisk(t, path); c.Token.RefreshToken != "rt-9" || c.ReauthNeeded {
		t.Fatalf("clearReauth: newer token kept=%v reauth_needed=%v", c.Token.RefreshToken == "rt-9", c.ReauthNeeded)
	}
}

// A refresh that fails for a reason that can pass (a 5xx, a timeout) keeps the
// grant and raises no alarm; the next call heals. It used to persist
// reauth_needed and DM the operator twice, dead then recovered.
func TestRefresh_TransientFailureRaisesNoAlarm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *grantServer)
	}{
		{"503", func(_ *testing.T, a *grantServer) { a.fail503.Store(1) }},
		{"timeout", func(t *testing.T, a *grantServer) {
			a.hang.Store(true)
			old := refreshTimeout
			refreshTimeout = 100 * time.Millisecond
			t.Cleanup(func() { refreshTimeout = old })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newGrantServer(t, true)
			path := tokenCachePath(t.TempDir(), grantURL)
			seedGrant(t, path, a.URL, time.Now().Add(-time.Hour), false)
			var announced []bool
			s := source(t, process(path, &announced))
			tc.setup(t, a)

			start := time.Now()
			if _, err := s.Token(); err == nil {
				t.Fatal("want the refresh error")
			}
			if time.Since(start) > 10*time.Second {
				t.Fatalf("the refresh hung for %v", time.Since(start))
			}
			if c := onDisk(t, path); c.ReauthNeeded || c.Token.RefreshToken != "rt-0" || len(announced) != 0 {
				t.Fatalf("transient failure: reauth_needed=%v token kept=%v announced=%v", c.ReauthNeeded, c.Token.RefreshToken == "rt-0", announced)
			}
			a.hang.Store(false)
			if _, err := s.Token(); err != nil || len(announced) != 0 {
				t.Fatalf("next call: err=%v announced=%v", err, announced)
			}
		})
	}
}

// A refused grant is the one failure that means signing in again: persisted
// and announced once.
func TestRefresh_InvalidGrantMarksReauth(t *testing.T) {
	a := newGrantServer(t, true)
	a.mu.Lock()
	a.revoked = true
	a.mu.Unlock()
	path := tokenCachePath(t.TempDir(), grantURL)
	seedGrant(t, path, a.URL, time.Now().Add(-time.Hour), false)
	var announced []bool
	s := source(t, process(path, &announced))
	var re *oauth2.RetrieveError
	if _, err := s.Token(); !errors.As(err, &re) || re.ErrorCode != "invalid_grant" {
		t.Fatalf("want invalid_grant, got %v", err)
	}
	if _, err := s.Token(); err == nil {
		t.Fatal("a dead grant must keep failing in a non-interactive handler")
	}
	if c := onDisk(t, path); !c.ReauthNeeded || len(announced) != 1 || !announced[0] {
		t.Fatalf("invalid_grant: reauth_needed=%v announced=%v", c.ReauthNeeded, announced)
	}
}

// mcpAuthServer is an MCP endpoint that always answers 401, with AS metadata,
// registration and a token endpoint that refuses every grant on the same
// origin, so an interactive connect can reach the browser step.
func mcpAuthServer(t *testing.T, registered *atomic.Int32) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token",
				"registration_endpoint": srv.URL + "/register", "response_types_supported": []string{"code"},
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/register":
			registered.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "new-cid", "redirect_uris": []string{"http://127.0.0.1:1/callback"}})
		case "/v1/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func connectWith(t *testing.T, endpoint string, h *persistentHandler) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr := &mcp.StreamableClientTransport{Endpoint: endpoint, DisableStandaloneSSE: true, OAuthHandler: h}
	s, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, tr, nil)
	if s != nil {
		_ = s.Close()
	}
	return err
}

// With a dead grant cached, `tenant mcp connect` and /enable used to fail
// before any request was sent, so the 401 that opens the browser never came.
// An interactive handler now drops the dead token (flagged already, or refused
// on this refresh) and reaches the browser, with the client it registered.
func TestDeadGrant_InteractiveReachesBrowser(t *testing.T) {
	for _, flagged := range []bool{true, false} {
		t.Run(fmt.Sprintf("flagged=%v", flagged), func(t *testing.T) {
			var registered, fetched atomic.Int32
			srv := mcpAuthServer(t, &registered)
			endpoint := srv.URL + "/v1/mcp"
			path := tokenCachePath(t.TempDir(), endpoint)
			if err := saveTokenCache(path, &tokenCache{ServerURL: endpoint, ClientID: "cid", TokenURL: srv.URL + "/token", AuthURL: srv.URL + "/authorize",
				Token:        &oauth2.Token{AccessToken: "at-old", RefreshToken: "rt-dead", Expiry: time.Now().Add(-time.Hour)},
				ReauthNeeded: flagged}); err != nil {
				t.Fatal(err)
			}
			fetcher := func(context.Context, *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
				fetched.Add(1)
				return nil, errors.New("browser step reached")
			}
			h := newPersistentHandler(endpoint, "http://127.0.0.1:1/callback", "Tenant", path, true, fetcher, srv.Client(), nil)
			if flagged {
				if ts, _ := h.TokenSource(context.Background()); ts != nil {
					t.Fatal("a flagged dead grant must give an interactive handler a nil TokenSource")
				}
			}
			_ = connectWith(t, endpoint, h)
			h.mu.Lock()
			kept := h.cache.ClientID == "cid"
			h.mu.Unlock()
			if fetched.Load() != 1 || registered.Load() != 0 || !kept {
				t.Fatalf("browser fetches=%d registrations=%d client kept=%v", fetched.Load(), registered.Load(), kept)
			}
		})
	}
}

// A plain 403 (not insufficient_scope) makes the SDK's handler sign nothing
// in; its nil TokenSource used to be called, a nil-pointer panic.
func TestAuthorize_Plain403DoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	endpoint := srv.URL + "/v1/mcp"
	path := tokenCachePath(t.TempDir(), endpoint)
	if err := saveTokenCache(path, &tokenCache{ServerURL: endpoint, ClientID: "cid", TokenURL: srv.URL + "/token", AuthURL: srv.URL + "/authorize",
		Token: &oauth2.Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(4 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	fetcher := func(context.Context, *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		return nil, errors.New("no browser expected")
	}
	h := newPersistentHandler(endpoint, "http://127.0.0.1:1/callback", "Tenant", path, true, fetcher, srv.Client(), nil)
	if err := connectWith(t, endpoint, h); err == nil {
		t.Fatal("a 403 must fail the connect")
	}
}
