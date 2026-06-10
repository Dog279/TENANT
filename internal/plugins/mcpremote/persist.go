package mcpremote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// proactiveRefreshWindow refreshes the OAuth token this long BEFORE it actually
// expires (TEN-166). A long-lived MCP session would otherwise carry the token
// right up to expiry and die mid-use (the "connected with N tools but every call
// fails" symptom); refreshing early keeps the session continuously valid.
const proactiveRefreshWindow = 5 * time.Minute

// errNoCachedSession is returned by a non-interactive handler whose cached token
// is missing/unusable — so a launch-time silent reconnect fails cleanly instead
// of popping a browser at startup.
var errNoCachedSession = errors.New("mcpremote: no usable cached session (sign in via /configure atlassian or /mcp add)")

// persistentHandler is an auth.OAuthHandler that caches the OAuth token (+ the
// DCR client and discovered endpoints) to disk so a remote MCP server reconnects
// across restarts WITHOUT a browser. TokenSource serves/refreshes the cached
// token; Authorize runs the interactive browser flow — reusing the SDK's vetted
// discovery/PKCE/exchange via a pre-registered client whose id we keep so refresh
// works later — and only when interactive.
type persistentHandler struct {
	serverURL   string
	redirect    string
	clientName  string
	cachePath   string
	interactive bool
	fetcher     auth.AuthorizationCodeFetcher // nil ⇒ non-interactive (no browser)
	httpClient  *http.Client

	logger *slog.Logger

	mu     sync.Mutex
	cache  *tokenCache
	loaded bool
	src    *refreshingSource // memoized: ONE source per server (rotation-safe)
}

func newPersistentHandler(serverURL, redirect, clientName, cachePath string, interactive bool, fetcher auth.AuthorizationCodeFetcher, httpClient *http.Client, logger *slog.Logger) *persistentHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &persistentHandler{
		serverURL: serverURL, redirect: redirect, clientName: clientName,
		cachePath: cachePath, interactive: interactive, fetcher: fetcher, httpClient: httpClient,
		logger: logger,
	}
}

func (h *persistentHandler) ensureLoaded() {
	if h.loaded {
		return
	}
	h.loaded = true
	if c, err := loadTokenCache(h.cachePath); err == nil {
		h.cache = c
	}
}

func (h *persistentHandler) oauth2Config(c *tokenCache) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: c.AuthURL, TokenURL: c.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
		RedirectURL:  h.redirect,
		Scopes:       c.Scopes,
	}
}

// TokenSource returns a refreshing source built from the cached token (no
// browser); nil when nothing usable is cached (→ the transport calls Authorize
// on the next 401). The source is MEMOIZED per handler: every caller shares one
// mutex-guarded source so two callers can't both refresh the single-use rotating
// refresh token and strand each other (TEN-166 — the "remove + re-add" cause).
func (h *persistentHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureLoaded()
	if h.cache == nil || h.cache.Token == nil || h.cache.TokenURL == "" || h.cache.ClientID == "" {
		return nil, nil
	}
	if h.src == nil {
		h.src = &refreshingSource{
			// Long-lived source → its own background ctx (not a per-call ctx that
			// may be cancelled), carrying the issuer-reconciling HTTP client.
			ctx: context.WithValue(context.Background(), oauth2.HTTPClient, h.httpClient),
			cfg: h.oauth2Config(h.cache),
			h:   h,
			cur: h.cache.Token,
		}
	}
	return h.src, nil
}

// refreshingSource serves the cached token, refreshing it PROACTIVELY (before
// expiry) and ATOMICALLY (persist the rotated token before returning). It is the
// single source of truth for the token — guarded by its own mutex so concurrent
// callers serialize, and Atlassian's single-use refresh token is never spent
// twice. Fails safe: if a proactive refresh fails while the current token is
// still valid, it logs and returns the current token rather than breaking.
type refreshingSource struct {
	mu  sync.Mutex
	ctx context.Context
	cfg *oauth2.Config
	h   *persistentHandler
	cur *oauth2.Token
}

func (s *refreshingSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reuse a non-expiring token, or one comfortably before expiry.
	if s.cur != nil && s.cur.AccessToken != "" &&
		(s.cur.Expiry.IsZero() || time.Until(s.cur.Expiry) >= proactiveRefreshWindow) {
		return s.cur, nil
	}
	if s.cur == nil || s.cur.RefreshToken == "" {
		return nil, errors.New("mcpremote: no refresh token cached")
	}
	// Force a refresh via the rotating refresh token (Expiry in the past makes the
	// oauth2 reuse source perform the refresh_token grant immediately).
	refreshed, err := s.cfg.TokenSource(s.ctx, &oauth2.Token{
		RefreshToken: s.cur.RefreshToken,
		Expiry:       time.Unix(1, 0),
	}).Token()
	if err != nil {
		// Fail safe: a still-valid current token beats a hard failure.
		if s.cur.Valid() {
			s.h.logger.Warn("mcp: proactive token refresh failed; using still-valid token",
				"server", s.h.serverURL, "err", err.Error())
			return s.cur, nil
		}
		s.h.logger.Warn("mcp: token refresh failed (re-auth needed)",
			"server", s.h.serverURL, "err", err.Error())
		return nil, err
	}
	s.cur = refreshed
	// Persist the rotated token IMMEDIATELY so the new single-use refresh token
	// survives a crash/restart — non-atomic persistence is the documented way
	// Atlassian clients strand themselves.
	s.h.mu.Lock()
	if s.h.cache != nil {
		s.h.cache.Token = refreshed
		if err := saveTokenCache(s.h.cachePath, s.h.cache); err != nil {
			s.h.logger.Warn("mcp: failed to persist refreshed token",
				"server", s.h.serverURL, "err", err.Error())
		}
	}
	s.h.mu.Unlock()
	return refreshed, nil
}

// Authorize discovers + dynamically-registers a client (once), runs the browser
// auth-code flow via the SDK's handler (proven; honors the server's PKCE/resource
// quirks) with that client, then captures + persists the token. Non-interactive
// handlers return errNoCachedSession instead of opening a browser.
func (h *persistentHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureLoaded()
	if !h.interactive || h.fetcher == nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return errNoCachedSession
	}
	if h.cache == nil {
		h.cache = &tokenCache{ServerURL: h.serverURL}
	}

	// Discover endpoints + register a client ONCE, so a later refresh has the
	// client_id + token endpoint without re-doing either.
	if h.cache.AuthURL == "" || h.cache.TokenURL == "" || h.cache.ClientID == "" {
		issuer := originOf(h.serverURL)
		asm, err := auth.GetAuthServerMetadata(ctx, issuer, h.httpClient)
		if err != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return fmt.Errorf("discover auth server: %w", err)
		}
		if asm == nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return fmt.Errorf("no authorization server metadata at %s", issuer)
		}
		h.cache.AuthURL = asm.AuthorizationEndpoint
		h.cache.TokenURL = asm.TokenEndpoint
		if len(h.cache.Scopes) == 0 {
			h.cache.Scopes = asm.ScopesSupported
		}
		if h.cache.ClientID == "" {
			if asm.RegistrationEndpoint == "" {
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				return errors.New("server has no dynamic registration endpoint")
			}
			reg, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
				ClientName:    h.clientName,
				RedirectURIs:  []string{h.redirect},
				GrantTypes:    []string{"authorization_code", "refresh_token"},
				ResponseTypes: []string{"code"},
			}, h.httpClient)
			if err != nil {
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				return fmt.Errorf("dynamic client registration: %w", err)
			}
			h.cache.ClientID = reg.ClientID
			h.cache.ClientSecret = reg.ClientSecret
		}
		_ = saveTokenCache(h.cachePath, h.cache) // creds+endpoints; token persisted below
	}

	// Delegate the browser auth-code/PKCE/exchange to the SDK handler using OUR
	// pre-registered client (so the minted token is tied to a client_id we kept).
	var secretAuth *oauthex.ClientSecretAuth
	if h.cache.ClientSecret != "" {
		secretAuth = &oauthex.ClientSecretAuth{ClientSecret: h.cache.ClientSecret}
	}
	inner, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              h.redirect,
		AuthorizationCodeFetcher: h.fetcher,
		Client:                   h.httpClient,
		PreregisteredClient:      &oauthex.ClientCredentials{ClientID: h.cache.ClientID, ClientSecretAuth: secretAuth},
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return fmt.Errorf("build auth handler: %w", err)
	}
	if err := inner.Authorize(ctx, req, resp); err != nil { // inner closes resp.Body
		return err
	}
	ts, err := inner.TokenSource(ctx)
	if err != nil {
		return err
	}
	tok, err := ts.Token()
	if err != nil {
		return err
	}
	h.cache.Token = tok
	h.src = nil // drop the memoized source so it rebuilds with the fresh token
	if err := saveTokenCache(h.cachePath, h.cache); err != nil {
		return fmt.Errorf("persist token: %w", err)
	}
	return nil
}

func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}
