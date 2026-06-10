package atlassian

// oauth.go is Path B: OAuth 2.0 3LO (TEN-59), mirroring the Atlassian connector
// login. Authorize() runs the one-time browser + localhost-callback flow and
// caches the token (0600). openOAuthClient() (used by Open) loads the cached
// token, proactively refreshes it, resolves the cloudId, and builds a client
// routed at api.atlassian.com/ex/jira/{cloudId} with auto-renewal — so a
// restart never needs re-login until the refresh token itself is revoked.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	v2 "github.com/ctreminiom/go-atlassian/v2/jira/v2"
	"github.com/ctreminiom/go-atlassian/v2/pkg/infra/oauth2"
	"github.com/ctreminiom/go-atlassian/v2/service/common"
)

// defaultScopes — read+write Jira plus offline_access (REQUIRED for a refresh
// token, i.e. for the cache to survive a restart).
var defaultScopes = []string{"read:jira-work", "write:jira-work", "read:jira-user", "offline_access"}

const defaultCallback = "127.0.0.1:8765"

func callbackAddr(cfg Config) string {
	if cfg.OAuthCallback != "" {
		return cfg.OAuthCallback
	}
	return defaultCallback
}

func tokenPath(cfg Config) string {
	if cfg.TokenPath != "" {
		return cfg.TokenPath
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "tenant", "atlassian-token.json")
	}
	return "atlassian-token.json"
}

func oauthConfig(cfg Config) *common.OAuth2Config {
	secret := cfg.ClientSecret
	if secret == "" {
		secret = os.Getenv("ATLASSIAN_CLIENT_SECRET")
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = defaultScopes
	}
	return &common.OAuth2Config{
		ClientID:     cfg.ClientID,
		ClientSecret: secret,
		RedirectURI:  "http://" + callbackAddr(cfg) + "/callback",
		Scopes:       scopes,
	}
}

// fileTokenStore persists the OAuth token as 0600 JSON (matches credentials.json
// discipline). Implements oauth2.TokenStore so go-atlassian's auto-renewal
// writes refreshed tokens straight back to disk.
type fileTokenStore struct{ path string }

var _ oauth2.TokenStore = fileTokenStore{}

func (s fileTokenStore) GetToken(_ context.Context) (*common.OAuth2Token, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var t common.OAuth2Token
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("atlassian: parse token cache %s: %w", s.path, err)
	}
	return &t, nil
}

func (s fileTokenStore) SetToken(_ context.Context, t *common.OAuth2Token) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

func (s fileTokenStore) GetRefreshToken(ctx context.Context) (string, error) {
	t, err := s.GetToken(ctx)
	if err != nil {
		return "", err
	}
	return t.RefreshToken, nil
}

func (s fileTokenStore) SetRefreshToken(ctx context.Context, refreshToken string) error {
	t, err := s.GetToken(ctx)
	if err != nil || t == nil {
		t = &common.OAuth2Token{}
	}
	t.RefreshToken = refreshToken
	return s.SetToken(ctx, t)
}

// openOAuthClient loads the cached token, refreshes it, resolves the cloudId,
// and returns a cloudId-routed auto-renewing client. Non-interactive: errors
// clearly (telling the operator to run login) when no token is cached.
func openOAuthClient(ctx context.Context, cfg Config) (*v2.Client, string, error) {
	store := fileTokenStore{path: tokenPath(cfg)}
	tok, err := store.GetToken(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("atlassian: no OAuth token cached at %s — run `tenant atlassian login` first: %w", store.path, err)
	}
	if tok.RefreshToken == "" {
		return nil, "", fmt.Errorf("atlassian: cached token has no refresh_token (re-run login with offline_access scope)")
	}
	config := oauthConfig(cfg)

	// Bootstrap client just to reach the OAuth2 service (refresh + cloudId).
	boot, err := v2.New(http.DefaultClient, "https://api.atlassian.com", v2.WithOAuth(config))
	if err != nil {
		return nil, "", fmt.Errorf("atlassian: oauth bootstrap: %w", err)
	}
	fresh, err := boot.OAuth.RefreshAccessToken(ctx, tok.RefreshToken)
	if err != nil {
		return nil, "", fmt.Errorf("atlassian: refresh failed (re-run login): %w", err)
	}
	if fresh.RefreshToken == "" { // Atlassian may not rotate the refresh token
		fresh.RefreshToken = tok.RefreshToken
	}
	if serr := store.SetToken(ctx, fresh); serr != nil {
		return nil, "", fmt.Errorf("atlassian: persist refreshed token: %w", serr)
	}

	cloudID, err := resolveCloudID(ctx, boot, fresh.AccessToken, cfg.SiteURL)
	if err != nil {
		return nil, "", err
	}
	site := "https://api.atlassian.com/ex/jira/" + cloudID

	// Option ORDER matters: the token store must be wired BEFORE auto-renewal is
	// set up, or the library captures a nil store and never writes refreshed/
	// rotated tokens back to disk (so a restart after rotation forces re-login).
	// This mirrors go-atlassian's own storage example ordering.
	c, err := v2.New(http.DefaultClient, site,
		v2.WithOAuth(config),
		v2.WithTokenStore(store),
		v2.WithAutoRenewalToken(fresh))
	if err != nil {
		return nil, "", fmt.Errorf("atlassian: build oauth client: %w", err)
	}
	return c, site, nil
}

// resolveCloudID picks the Atlassian site's cloudId: the one whose URL matches
// the configured SiteURL host, else the first accessible resource.
func resolveCloudID(ctx context.Context, c *v2.Client, accessToken, siteURL string) (string, error) {
	resources, err := c.OAuth.GetAccessibleResources(ctx, accessToken)
	if err != nil {
		return "", fmt.Errorf("atlassian: list accessible resources: %w", err)
	}
	if len(resources) == 0 {
		return "", errors.New("atlassian: OAuth token has no accessible Jira sites")
	}
	want := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(siteURL, "https://"), "http://"), "/")
	for _, r := range resources {
		host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(r.URL, "https://"), "http://"), "/")
		if want != "" && host == want {
			return r.ID, nil
		}
	}
	return resources[0].ID, nil
}

// Authorize runs the one-time interactive 3LO flow: print the consent URL, spin
// up the localhost callback, capture the code, exchange it, and cache the token.
// Returns the resolved site name for confirmation. Blocks until the operator
// completes consent or ctx is cancelled / the timeout elapses.
func Authorize(ctx context.Context, cfg Config) (string, error) {
	if cfg.ClientID == "" {
		return "", errors.New("atlassian: login requires ClientID (+ ClientSecret)")
	}
	config := oauthConfig(cfg)
	boot, err := v2.New(http.DefaultClient, "https://api.atlassian.com", v2.WithOAuth(config))
	if err != nil {
		return "", fmt.Errorf("atlassian: oauth bootstrap: %w", err)
	}
	state, err := randomState()
	if err != nil {
		return "", err
	}
	authURL, err := boot.OAuth.GetAuthorizationURL(config.Scopes, state)
	if err != nil {
		return "", fmt.Errorf("atlassian: build authorization URL: %w", err)
	}

	code, err := awaitCallbackCode(ctx, callbackAddr(cfg), state, authURL.String(), cfg.OAuthOpenBrowser)
	if err != nil {
		return "", err
	}
	tok, err := boot.OAuth.ExchangeAuthorizationCode(ctx, code)
	if err != nil {
		return "", fmt.Errorf("atlassian: exchange code: %w", err)
	}
	store := fileTokenStore{path: tokenPath(cfg)}
	if err := store.SetToken(ctx, tok); err != nil {
		return "", fmt.Errorf("atlassian: cache token: %w", err)
	}
	site := cfg.SiteURL
	if cid, cerr := resolveCloudID(ctx, boot, tok.AccessToken, cfg.SiteURL); cerr == nil {
		site = "cloudId " + cid
	}
	return site, nil
}

// awaitCallbackCode prints the consent URL, serves the localhost callback, and
// returns the authorization code once the operator consents (state-verified).
func awaitCallbackCode(ctx context.Context, addr, state, authURL string, open func(string) error) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("atlassian: bind callback %s: %w", addr, err)
	}
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization denied: "+e, http.StatusBadRequest)
			done <- result{err: fmt.Errorf("atlassian: authorization denied: %s", e)}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			done <- result{err: errors.New("atlassian: oauth state mismatch (possible CSRF) — retry login")}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			done <- result{err: errors.New("atlassian: callback missing authorization code")}
			return
		}
		_, _ = w.Write([]byte("Atlassian connected. You can close this tab and return to Tenant."))
		done <- result{code: code}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(os.Stderr, "\nOpen this URL to authorize Tenant's Atlassian access:\n\n  %s\n\nWaiting for the callback on http://%s/callback …\n", authURL, addr)
	if open != nil {
		_ = open(authURL) // best-effort auto-open; the printed URL is the fallback
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Minute):
		return "", errors.New("atlassian: login timed out after 5m")
	case res := <-done:
		return res.code, res.err
	}
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
