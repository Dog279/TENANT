package mcpremote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"tenant/internal/datalock"
)

// proactiveRefreshWindow refreshes the OAuth token this long BEFORE it actually
// expires (TEN-166). A long-lived MCP session would otherwise carry the token
// right up to expiry and die mid-use (the "connected with N tools but every call
// fails" symptom); refreshing early keeps the session continuously valid.
const proactiveRefreshWindow = 5 * time.Minute

// refreshTimeout bounds one refresh_token grant, so a hung authorization server
// fails the call instead of holding it (and the cache lock) forever.
var refreshTimeout = 30 * time.Second

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

	// ctx is the connection's: its events go to the connector's log, under
	// its Connection activity. label names the connector in them.
	ctx   context.Context
	label string

	// onAuthChange fires on the healthy↔needs-re-auth transitions (not on every
	// failing call), so the operator is told once when a connector dies and
	// once when it is back.
	onAuthChange func(needsReauth bool, err string)

	mu       sync.Mutex
	cache    *tokenCache
	loaded   bool
	src      *refreshingSource // memoized: ONE source per server (rotation-safe)
	lockWarn sync.Once         // CacheNotLocked, once per connection
}

// Health reports the connector's persisted authorization state.
func (h *persistentHandler) Health() Health {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureLoaded()
	return h.cache.health()
}

// markReauth records that the refresh grant failed with no usable token left —
// persisted so it survives a restart — and fires onAuthChange on the transition.
// When a sign-in elsewhere has saved a valid token since the refusal, it takes
// that token on instead and returns it.
func (h *persistentHandler) markReauth(errText string) *oauth2.Token {
	h.mu.Lock()
	h.ensureLoaded()
	if h.cache == nil {
		h.cache = &tokenCache{ServerURL: h.serverURL}
	}
	transition := !h.cache.ReauthNeeded
	if transition {
		h.cache.ReauthNeeded, h.cache.ReauthError, h.cache.ReauthSince = true, errText, time.Now().Unix()
	}
	cb := h.onAuthChange
	h.mu.Unlock()
	if !transition {
		return nil
	}
	if fresh := h.saveReauth(); fresh != nil {
		h.mu.Lock()
		fresh.ReauthNeeded, fresh.ReauthError, fresh.ReauthSince = false, "", 0 // never announced here
		h.cache = fresh
		h.mu.Unlock()
		return fresh.Token
	}
	NeedsReauth.Log(h.ctx, "server", h.label, "url", h.serverURL, "err", errText, "fix", h.fixCommand())
	if cb != nil {
		cb(true, errText)
	}
	return nil
}

// clearReauth records a valid token being seen again (a browser re-auth, or a
// refresh that succeeded after all) and fires onAuthChange on the transition.
// Cheap when nothing was flagged — it runs on the hot path.
func (h *persistentHandler) clearReauth() {
	h.mu.Lock()
	if h.cache == nil || !h.cache.ReauthNeeded {
		h.mu.Unlock()
		return
	}
	h.cache.ReauthNeeded, h.cache.ReauthError, h.cache.ReauthSince = false, "", 0
	cb := h.onAuthChange
	h.mu.Unlock()
	h.saveReauth()
	h.reauthorized(cb)
}

// saveReauth writes this handler's re-auth state into the cache on disk, and
// nothing else, under the cache lock: the token in memory may be older than one
// another process has saved since, and writing it back would strand the grant.
// A flag is not set over a valid token other than this handler's (a sign-in
// elsewhere since the refused refresh): that cache is returned instead.
func (h *persistentHandler) saveReauth() (fresh *tokenCache) {
	h.mu.Lock()
	mem := *h.cache
	h.mu.Unlock()
	unlock, err := h.lockCache()
	defer unlock()
	var c *tokenCache
	if err == nil {
		c, err = loadTokenCache(h.cachePath)
	}
	if errors.Is(err, os.ErrNotExist) {
		c, err = &mem, nil // nothing newer on disk to keep
	}
	if err == nil && mem.ReauthNeeded && c.Token.Valid() && (mem.Token == nil || c.Token.AccessToken != mem.Token.AccessToken) {
		return c
	}
	if err == nil {
		c.ReauthNeeded, c.ReauthError, c.ReauthSince = mem.ReauthNeeded, mem.ReauthError, mem.ReauthSince
		err = saveTokenCache(h.cachePath, c)
	}
	if err != nil {
		StateNotSaved.Log(h.ctx, "server", h.label, "what", "the re-auth state", "err", err)
	}
	return nil
}

// lockCache takes the lock beside the cache file and returns its release.
// serve, the TUI, research, eval and `tenant mcp connect` each hold this
// sign-in, and its refresh token is single-use, so every refresh and every
// write of the cache happens under it. The wait outlasts a holder's refresh
// (datalock.File alone gives up sooner); a holder busy even longer is an
// error, not something to race. When the lock can't be taken at all (another
// account's lock file on Windows), it logs once and goes on unlocked, as
// before. release is never nil.
func (h *persistentHandler) lockCache() (release func(), err error) {
	if h.cachePath == "" {
		return func() {}, nil
	}
	deadline := time.Now().Add(refreshTimeout + 5*time.Second)
	l, err := datalock.File(h.cachePath, true)
	for errors.Is(err, datalock.ErrBusy) && time.Now().Before(deadline) {
		l, err = datalock.File(h.cachePath, true)
	}
	if errors.Is(err, datalock.ErrBusy) {
		return func() {}, fmt.Errorf("the saved sign-in stayed locked by another process: %w", err)
	}
	if err != nil {
		h.lockWarn.Do(func() { CacheNotLocked.Log(h.ctx, "server", h.label, "err", err) })
		return func() {}, nil
	}
	return func() { _ = l.Release() }, nil
}

// fixCommand is FixCommand for this connector, whose cache is in <config>/mcp.
func (h *persistentHandler) fixCommand() string {
	dir := ""
	if h.cachePath != "" {
		dir = filepath.Dir(filepath.Dir(h.cachePath))
	}
	return FixCommand(dir, h.serverURL)
}

// reauthorized reports a connector whose sign-in works again.
func (h *persistentHandler) reauthorized(cb func(bool, string)) {
	Reauthorized.Log(h.ctx, "server", h.label)
	if cb != nil {
		cb(false, "")
	}
}

// newPersistentHandler builds the handler for serverURL. ctx is the
// connection's, for its events (nil: the mcp log).
func newPersistentHandler(serverURL, redirect, clientName, cachePath string, interactive bool, fetcher auth.AuthorizationCodeFetcher, httpClient *http.Client, ctx context.Context) *persistentHandler {
	if ctx == nil {
		ctx = context.Background()
	}
	return &persistentHandler{
		serverURL: serverURL, redirect: redirect, clientName: clientName,
		cachePath: cachePath, interactive: interactive, fetcher: fetcher, httpClient: httpClient,
		ctx: ctx,
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
	// A dead sign-in blocks its own repair: its token fails before any request
	// is sent, so no 401 ever reaches Authorize. An interactive handler forgets
	// it, keeping the client and endpoints, so the request goes out without one
	// and its 401 opens the browser.
	if h.interactive && h.cache != nil && h.cache.ReauthNeeded && !h.cache.Token.Valid() {
		h.cache.Token, h.src = nil, nil
	}
	if h.cache == nil || h.cache.Token == nil || h.cache.TokenURL == "" || h.cache.ClientID == "" {
		return nil, nil
	}
	if h.src == nil {
		h.src = &refreshingSource{
			// Long-lived source → its own background ctx (not a per-call ctx that
			// may be cancelled), carrying the issuer-reconciling HTTP client.
			ctx: context.WithValue(context.Background(), oauth2.HTTPClient, h.httpClient),
			h:   h,
			cur: h.cache.Token,
		}
	}
	return h.src, nil
}

// refreshingSource serves the cached token, refreshing it PROACTIVELY (before
// expiry) and ATOMICALLY (persist the rotated token before returning). Its
// mutex serializes the callers in this process and the cache lock (lockCache)
// serializes processes, so Atlassian's single-use refresh token is never spent
// twice. Fails safe: if a proactive refresh fails while the current token is
// still valid, it logs and returns the current token rather than breaking.
type refreshingSource struct {
	mu    sync.Mutex
	ctx   context.Context
	h     *persistentHandler
	cur   *oauth2.Token
	spent string // the refresh token this source last spent
}

func (s *refreshingSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reuse a non-expiring token, or one comfortably before expiry.
	if s.cur != nil && s.cur.AccessToken != "" &&
		(s.cur.Expiry.IsZero() || time.Until(s.cur.Expiry) >= proactiveRefreshWindow) {
		s.h.clearReauth() // a valid token in hand ⇒ whatever was flagged is over
		return s.cur, nil
	}
	tok, err := s.refresh()
	if err == nil {
		s.cur = tok
		s.h.clearReauth()
		return tok, nil
	}
	switch {
	case s.cur.Valid():
		// Fail safe: a still-valid current token beats a hard failure.
		RefreshFailed.Log(s.h.ctx, "server", s.h.label, "err", err)
		return s.cur, nil
	case !refused(err):
		RefreshRetry.Log(s.h.ctx, "server", s.h.label, "err", err)
		return nil, err
	}
	// Persisted + logged and announced once; every surface reads it.
	if tok := s.h.markReauth(err.Error()); tok != nil {
		s.cur = tok // signed in again elsewhere meanwhile
		return tok, nil
	}
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if !s.h.interactive {
		return nil, err
	}
	// A browser sign-in may run (a connect is under way): forget the token,
	// not the client, so this request goes out without one and its 401 runs
	// Authorize.
	s.h.cache.Token, s.h.src = nil, nil
	return nil, nil
}

// refused reports a refresh error that only signing in again cures: the
// server refused the grant or the client, or answered with a 4xx other than a
// timeout or a rate limit. The rest (unreachable, a 5xx, the timeout) may
// pass, and raise no alarm.
func refused(err error) bool {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return false
	}
	if re.ErrorCode == "invalid_grant" || re.ErrorCode == "invalid_client" {
		return true
	}
	st := 0
	if re.Response != nil {
		st = re.Response.StatusCode
	}
	return st >= 400 && st < 500 && st != http.StatusRequestTimeout && st != http.StatusTooManyRequests
}

// refresh renews the token from the cache on disk, under the cache lock, then
// takes on what the disk holds, keeping this process's re-auth state: that is
// this process's to announce.
func (s *refreshingSource) refresh() (*oauth2.Token, error) {
	s.h.mu.Lock()
	mem := *s.h.cache
	s.h.mu.Unlock()
	c, err := s.refreshLocked(&mem)
	if err != nil {
		return nil, err
	}
	s.h.mu.Lock()
	s.h.adopt(c)
	s.h.mu.Unlock()
	return c.Token, nil
}

// adopt takes on c, read from disk, keeping this handler's re-auth state:
// that is this process's to announce. h.mu is held.
func (h *persistentHandler) adopt(c *tokenCache) {
	if h.cache != nil {
		c.ReauthNeeded, c.ReauthError, c.ReauthSince = h.cache.ReauthNeeded, h.cache.ReauthError, h.cache.ReauthSince
	}
	h.cache = c
}

// refreshLocked re-reads the cache under its lock: another process may have
// refreshed, or signed in again, since this one loaded it. A token there that
// differs from this process's and is still valid is adopted with no network
// call, which is also how a long-running serve picks up a re-auth done
// elsewhere. Otherwise it spends the refresh token on disk, not this process's
// copy (which may be rotated away already), and saves before unlocking. mem
// is this process's cache, for when the disk has none, or still holds the
// refresh token this source spent (its save failed).
func (s *refreshingSource) refreshLocked(mem *tokenCache) (*tokenCache, error) {
	unlock, err := s.h.lockCache()
	defer unlock()
	if err != nil {
		return nil, err
	}
	c, err := loadTokenCache(s.h.cachePath)
	if err != nil || c.Token == nil || s.spent != "" && c.Token.RefreshToken == s.spent {
		c = mem
	}
	if c.Token.Valid() && (s.cur == nil || c.Token.AccessToken != s.cur.AccessToken) {
		return c, nil
	}
	if c.Token == nil || c.Token.RefreshToken == "" {
		return nil, errors.New("mcpremote: no refresh token cached")
	}
	ctx, cancel := context.WithTimeout(s.ctx, refreshTimeout)
	defer cancel()
	// Force a refresh via the rotating refresh token (Expiry in the past makes the
	// oauth2 reuse source perform the refresh_token grant immediately).
	refreshed, err := s.h.oauth2Config(c).TokenSource(ctx, &oauth2.Token{
		RefreshToken: c.Token.RefreshToken,
		Expiry:       time.Unix(1, 0),
	}).Token()
	if err != nil {
		return nil, err
	}
	s.spent = c.Token.RefreshToken
	c.Token = refreshed
	c.ReauthNeeded, c.ReauthError, c.ReauthSince = false, "", 0
	// Persist the rotated token IMMEDIATELY, before unlocking, so the new
	// single-use refresh token survives a crash/restart and is what the next
	// process to lock reads — non-atomic persistence is the documented way
	// Atlassian clients strand themselves.
	if err := saveTokenCache(s.h.cachePath, c); err != nil {
		TokenNotSaved.Log(s.h.ctx, "server", s.h.label, "err", err)
	}
	TokenRefreshed.Log(s.h.ctx, "server", s.h.label, "expires", refreshed.Expiry)
	return c, nil
}

// Authorize discovers + dynamically-registers a client (once), runs the browser
// auth-code flow via the SDK's handler (proven; honors the server's PKCE/resource
// quirks) with that client, then captures + persists the token. Non-interactive
// handlers return errNoCachedSession instead of opening a browser, unless a
// sign-in elsewhere has saved a token they can take on.
func (h *persistentHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureLoaded()
	if !h.interactive || h.fetcher == nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		// Signed in again elsewhere (`tenant mcp connect`) while this token
		// still looked valid: take that sign-in on, and the request retries.
		unlock, _ := h.lockCache()
		c, err := loadTokenCache(h.cachePath)
		unlock()
		if err == nil && c.Token.Valid() && (h.cache == nil || h.cache.Token == nil || c.Token.AccessToken != h.cache.Token.AccessToken) {
			h.adopt(c)
			h.src = nil
			return nil
		}
		return errNoCachedSession
	}
	if h.cache == nil {
		h.cache = &tokenCache{ServerURL: h.serverURL}
	}
	// The server refused the client itself: signing in with it again can't
	// work, so register a new one.
	if strings.Contains(h.cache.ReauthError, `"invalid_client"`) {
		h.cache.ClientID, h.cache.ClientSecret = "", ""
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
		// creds+endpoints; the token is persisted below. A failure here only
		// costs a fresh client registration if this sign-in is abandoned.
		if err := saveTokenCache(h.cachePath, h.cache); err != nil {
			StateNotSaved.Log(WithServerLog(ctx, h.label), "server", h.label, "what", "the client registration", "err", err)
		}
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
	if ts == nil {
		return nil // a plain 403 signs nothing in: the retried request reports it
	}
	tok, err := ts.Token()
	if err != nil {
		return err
	}
	h.cache.Token = tok
	h.src = nil // drop the memoized source so it rebuilds with the fresh token
	// A new sign-in is saved even past a stuck lock holder.
	unlock, _ := h.lockCache()
	err = saveTokenCache(h.cachePath, h.cache)
	unlock()
	if err != nil {
		return fmt.Errorf("persist token: %w", err)
	}
	SignedIn.Log(WithServerLog(ctx, h.label), "server", h.label)
	return nil
}

func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}
