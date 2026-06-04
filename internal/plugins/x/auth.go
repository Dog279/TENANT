// Package x is Tenant's X (Twitter) connector — a native-Go port of
// the auth + thin-request layer that the `xurl` CLI provides (the
// piece Hermes wraps for X access). No shelling out to a Rust binary,
// no SDK: the X API v2 is plain REST and the OAuth2 PKCE flow is
// ~100 lines of stdlib, same minimal-deps single-binary discipline as
// the gsuite plugin.
//
//   - App-only Bearer token: read-only public data, zero flow.
//   - OAuth2 Authorization-Code + PKCE: user context (post/reply/
//     delete as the user). Refresh token cached in the data dir;
//     X rotates refresh tokens so every refresh is re-persisted.
package x

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	authorizeURL   = "https://twitter.com/i/oauth2/authorize"
	tokenURL       = "https://api.twitter.com/2/oauth2/token"
	scopeRead      = "tweet.read users.read offline.access"
	scopeReadWrite = "tweet.read tweet.write users.read offline.access"
)

// httpDoer is the seam every X call + token exchange goes through
// (tests inject an httptest server instead of hitting api.twitter.com).
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// tokenSource yields a bearer/access token for the Authorization header.
type tokenSource interface {
	token(ctx context.Context) (string, error)
	// userContext reports whether this token can act AS the user
	// (post/delete). App-only bearer cannot.
	userContext() bool
}

// --- App-only bearer ---

type bearerSource struct{ tok string }

func (b bearerSource) token(context.Context) (string, error) {
	if b.tok == "" {
		return "", fmt.Errorf("x: no bearer token configured")
	}
	return b.tok, nil
}
func (bearerSource) userContext() bool { return false }

// --- OAuth2 PKCE user context ---

const refreshSkew = 60 * time.Second

// tokenStore is the on-disk credential (data dir, 0600). Plain JSON —
// you can read it, same transparency ethos as the wiki sidecar.
type tokenStore struct {
	ClientID string    `json:"client_id"`
	Access   string    `json:"access_token"`
	Refresh  string    `json:"refresh_token"`
	Expiry   time.Time `json:"expiry"`
	Scopes   string    `json:"scopes"`
}

func loadStore(path string) (*tokenStore, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s tokenStore
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("x: token store %s is corrupt: %w", path, err)
	}
	return &s, nil
}

func (s *tokenStore) save(path string) error {
	b, _ := json.MarshalIndent(s, "", " ")
	return os.WriteFile(path, b, 0o600)
}

type pkceSource struct {
	path  string
	http  httpDoer
	clock func() time.Time
	tok   string // token endpoint (overridable in tests)

	mu sync.Mutex
	st *tokenStore
}

func newPKCESource(path string, h httpDoer, clock func() time.Time) (*pkceSource, error) {
	if clock == nil {
		clock = time.Now
	}
	st, err := loadStore(path)
	if err != nil {
		return nil, fmt.Errorf("x: no cached user token (%v) — run `tenant x --login --client-id …` first", err)
	}
	if st.Refresh == "" {
		return nil, fmt.Errorf("x: token store has no refresh_token — re-run `tenant x --login`")
	}
	return &pkceSource{path: path, http: h, clock: clock, tok: tokenURL, st: st}, nil
}

func (p *pkceSource) userContext() bool { return true }

func (p *pkceSource) token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st.Access != "" && p.clock().Add(refreshSkew).Before(p.st.Expiry) {
		return p.st.Access, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {p.st.Refresh},
		"client_id":     {p.st.ClientID},
	}
	tr, err := exchange(ctx, p.http, p.tok, form)
	if err != nil {
		return "", err
	}
	p.st.Access = tr.AccessToken
	if tr.RefreshToken != "" {
		p.st.Refresh = tr.RefreshToken // X rotates refresh tokens
	}
	p.st.Expiry = p.clock().Add(time.Duration(tr.ttl()) * time.Second)
	if err := p.st.save(p.path); err != nil {
		return "", fmt.Errorf("x: persist refreshed token: %w", err)
	}
	return p.st.Access, nil
}

// tokenResp is the X OAuth2 token endpoint response.
type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (t tokenResp) ttl() int {
	if t.ExpiresIn <= 0 {
		return 7200 // X access tokens are ~2h
	}
	return t.ExpiresIn
}

func exchange(ctx context.Context, h httpDoer, tokenURL string, form url.Values) (*tokenResp, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.Do(req)
	if err != nil {
		return nil, fmt.Errorf("x: token endpoint: %w", err)
	}
	defer resp.Body.Close()
	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("x: decode token response: %w", err)
	}
	if resp.StatusCode/100 != 2 || tr.AccessToken == "" {
		return nil, fmt.Errorf("x: token exchange failed: %s %s (status %d)",
			tr.Error, tr.ErrorDesc, resp.StatusCode)
	}
	return &tr, nil
}

// --- PKCE primitives (S256) ---

func newVerifier() string {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b) // 64 chars, in [43,128]
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func authCodeURL(clientID, redirectURI, scopes, state, codeChallenge string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {scopes},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return authorizeURL + "?" + q.Encode()
}

// DefaultRedirectURI is the localhost callback the PKCE flow listens
// on; it must be registered as a redirect URL in the X app settings.
const DefaultRedirectURI = "http://localhost:8723/callback"

// LoginConfig configures the one-time PKCE consent (`tenant x --login`).
type LoginConfig struct {
	ClientID, RedirectURI, TokenPath string
	AllowPost                        bool
	HTTP                             httpDoer
	Emit                             func(authURL string)
}

// Login runs the interactive PKCE flow and persists the token store.
// Scopes downgrade to read-only when posting isn't enabled (least
// privilege: the resulting token literally cannot tweet).
func Login(ctx context.Context, cfg LoginConfig) error {
	h := cfg.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	if cfg.RedirectURI == "" {
		cfg.RedirectURI = DefaultRedirectURI
	}
	scopes := scopeRead
	if cfg.AllowPost {
		scopes = scopeReadWrite
	}
	emit := cfg.Emit
	if emit == nil {
		emit = func(u string) { fmt.Fprintln(os.Stderr, "Open this URL to authorize Tenant on X:\n  "+u) }
	}
	st, err := loginPKCE(ctx, h, cfg.ClientID, cfg.RedirectURI, scopes, emit)
	if err != nil {
		return err
	}
	return st.save(cfg.TokenPath)
}

// loginPKCE runs the one-time browser consent: print the auth URL,
// catch the redirect on a localhost listener, exchange the code. The
// interactive listener is operator glue; the security-critical
// exchange/refresh paths are unit-tested.
func loginPKCE(ctx context.Context, h httpDoer, clientID, redirectURI, scopes string,
	emit func(authURL string)) (*tokenStore, error) {
	if clientID == "" {
		return nil, fmt.Errorf("x: --client-id is required for OAuth2 PKCE login")
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		return nil, fmt.Errorf("x: bad redirect URI: %w", err)
	}
	verifier := newVerifier()
	state := randState()
	emit(authCodeURL(clientID, redirectURI, scopes, state, challenge(verifier)))

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Addr: u.Host}
	mux := http.NewServeMux()
	mux.HandleFunc(u.Path, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("x: OAuth state mismatch (possible CSRF) — aborted")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			errCh <- fmt.Errorf("x: authorization denied: %s", e)
			return
		}
		_, _ = w.Write([]byte("Tenant: X authorization complete — you can close this tab."))
		codeCh <- r.URL.Query().Get("code")
	})
	srv.Handler = mux
	go func() { _ = srv.ListenAndServe() }()
	defer srv.Close()

	var code string
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errCh:
		return nil, err
	case code = <-codeCh:
	}

	tr, err := exchange(ctx, h, tokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"client_id":     {clientID},
	})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &tokenStore{
		ClientID: clientID, Access: tr.AccessToken, Refresh: tr.RefreshToken,
		Expiry: now.Add(time.Duration(tr.ttl()) * time.Second), Scopes: scopes,
	}, nil
}
