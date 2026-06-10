package mcpremote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

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

	mu     sync.Mutex
	cache  *tokenCache
	loaded bool
}

func newPersistentHandler(serverURL, redirect, clientName, cachePath string, interactive bool, fetcher auth.AuthorizationCodeFetcher, httpClient *http.Client) *persistentHandler {
	return &persistentHandler{
		serverURL: serverURL, redirect: redirect, clientName: clientName,
		cachePath: cachePath, interactive: interactive, fetcher: fetcher, httpClient: httpClient,
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
// on the next 401).
func (h *persistentHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureLoaded()
	if h.cache == nil || h.cache.Token == nil || h.cache.TokenURL == "" || h.cache.ClientID == "" {
		return nil, nil
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, h.httpClient)
	base := h.oauth2Config(h.cache).TokenSource(ctx, h.cache.Token)
	return &persistingSource{base: base, h: h, last: h.cache.Token.AccessToken}, nil
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
	if err := saveTokenCache(h.cachePath, h.cache); err != nil {
		return fmt.Errorf("persist token: %w", err)
	}
	return nil
}

// persistingSource writes the token back to disk whenever it rotates (refresh).
type persistingSource struct {
	base oauth2.TokenSource
	h    *persistentHandler
	last string
}

func (s *persistingSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		s.h.mu.Lock()
		if s.h.cache != nil {
			s.h.cache.Token = t
			_ = saveTokenCache(s.h.cachePath, s.h.cache)
		}
		s.h.mu.Unlock()
	}
	return t, nil
}

func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}
