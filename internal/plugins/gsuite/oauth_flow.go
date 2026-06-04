package gsuite

// TEN-71: OAuth code-grant flow for the gsuite plugin.
//
// Mirrors Google's `oauth2l` reference implementation: spin up a
// loopback server on 127.0.0.1:0 (OS picks a free port), open the
// system browser to Google's AuthCodeURL with PKCE + a state nonce,
// the user authorizes in their browser, Google redirects to our
// loopback with `?code=...`, we exchange the code for a token, the
// callback page tells the user they can close the tab, and the main
// goroutine returns the token to the plugin.
//
// We DEPEND on golang.org/x/oauth2 for the OAuth heavy lifting (PKCE
// since v0.18.0, TokenSource refresh, Exchange). NOT on the full
// google.golang.org/api SDK — that pulls in ~30 modules (gRPC, otel,
// protobuf) for no functional gain since we already hand-roll the
// Gmail/Calendar REST callers in this package.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// OAuthClientFile is the shape of the OAuth Client credentials JSON
// the operator downloads from Google Cloud Console (APIs & Services
// → Credentials → OAuth client ID → Desktop App). Google nests the
// values under an "installed" or "web" key; we accept either but
// expect "installed" for the Desktop App type.
type OAuthClientFile struct {
	Installed *oauthClientShape `json:"installed,omitempty"`
	Web       *oauthClientShape `json:"web,omitempty"`
}

type oauthClientShape struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	AuthURI      string   `json:"auth_uri"`
	TokenURI     string   `json:"token_uri"`
	RedirectURIs []string `json:"redirect_uris"`
}

// ParseOAuthClientFile pulls the client_id + endpoint info out of the
// downloaded credentials JSON. Returns the constructed oauth2.Config
// (still missing RedirectURL — runLoopbackFlow fills that in once it
// knows the loopback port).
func ParseOAuthClientFile(b []byte, scopes []string) (*oauth2.Config, error) {
	var f OAuthClientFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("gsuite: oauth client JSON parse: %w", err)
	}
	shape := f.Installed
	if shape == nil {
		shape = f.Web
	}
	if shape == nil {
		return nil, errors.New("gsuite: oauth client JSON missing 'installed' or 'web' block — was this a 'Desktop app' OAuth client?")
	}
	if shape.ClientID == "" {
		return nil, errors.New("gsuite: oauth client JSON missing client_id")
	}
	authURL := shape.AuthURI
	if authURL == "" {
		authURL = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	tokURL := shape.TokenURI
	if tokURL == "" {
		tokURL = "https://oauth2.googleapis.com/token"
	}
	return &oauth2.Config{
		ClientID:     shape.ClientID,
		ClientSecret: shape.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: authURL, TokenURL: tokURL},
		Scopes:       scopes,
	}, nil
}

// OAuthFlowConfig is the per-invocation configuration of runLoopbackFlow.
// Test seams (openBrowser, listenAddr) default to the real things.
type OAuthFlowConfig struct {
	Config        *oauth2.Config
	OpenBrowser   func(url string) error // nil ⇒ openBrowserDefault
	ListenAddr    string                 // nil/"" ⇒ "127.0.0.1:0"
	WaitTimeout   time.Duration          // 0 ⇒ 5 min
	SuccessHTML   string                 // override the "close this tab" page (nil ⇒ default)
}

// runLoopbackFlow runs the full OAuth code-grant dance:
//
//  1. listen on a loopback port (random by default)
//  2. derive RedirectURL from the bound port
//  3. generate PKCE verifier + crypto-random state
//  4. open browser to AuthCodeURL
//  5. wait for callback on the loopback server
//  6. validate state, exchange code for token, return
//
// Honors ctx cancellation throughout. Closes the server on return.
func runLoopbackFlow(ctx context.Context, c OAuthFlowConfig) (*oauth2.Token, error) {
	if c.Config == nil {
		return nil, errors.New("oauth flow: nil oauth2.Config")
	}
	if c.OpenBrowser == nil {
		c.OpenBrowser = openBrowserDefault
	}
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:0"
	}
	if c.WaitTimeout == 0 {
		c.WaitTimeout = 5 * time.Minute
	}
	if c.SuccessHTML == "" {
		c.SuccessHTML = defaultSuccessHTML
	}

	ln, err := net.Listen("tcp", c.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("oauth flow: listen: %w", err)
	}
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("oauth flow: listener addr is not TCP: %T", ln.Addr())
	}
	// Mutate a copy of the config so we don't poison the caller's.
	cfg := *c.Config
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port)

	verifier := oauth2.GenerateVerifier()

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("oauth flow: random state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,    // ⇒ we get a refresh_token
		oauth2.ApprovalForce,        // ⇒ Google re-prompts (don't silently use prior consent — needed if scope set changed)
		oauth2.S256ChallengeOption(verifier),
	)

	type result struct {
		token *oauth2.Token
		err   error
	}
	resultCh := make(chan result, 1)

	var once sync.Once
	send := func(r result) {
		once.Do(func() { resultCh <- r })
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			desc := q.Get("error_description")
			fmt.Fprintf(w, "OAuth error: %s — %s. You can close this window.", e, desc)
			send(result{err: fmt.Errorf("oauth callback error: %s (%s)", e, desc)})
			return
		}
		gotState := q.Get("state")
		if gotState != state {
			http.Error(w, "state mismatch (possible CSRF)", http.StatusBadRequest)
			send(result{err: errors.New("oauth callback: state mismatch")})
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			send(result{err: errors.New("oauth callback: missing code")})
			return
		}
		tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		if err != nil {
			fmt.Fprintf(w, "Exchange failed: %s. You can close this window.", err)
			send(result{err: fmt.Errorf("oauth exchange: %w", err)})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(c.SuccessHTML))
		send(result{token: tok})
	})

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutCtx)
	}()

	if err := c.OpenBrowser(authURL); err != nil {
		// Don't fail — print the URL for manual paste. The token wait
		// loop below still runs.
		fmt.Printf("(could not open browser automatically: %v)\nVisit this URL to authorize:\n  %s\n", err, authURL)
	}

	select {
	case r := <-resultCh:
		return r.token, r.err
	case <-time.After(c.WaitTimeout):
		return nil, fmt.Errorf("oauth flow: timed out after %v waiting for browser authorization", c.WaitTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// openBrowserDefault opens a URL in the OS-default browser. Detached;
// failure is reported but non-fatal (the caller falls back to printing
// the URL for manual paste).
func openBrowserDefault(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}

const defaultSuccessHTML = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Authorization complete</title></head>
<body style="font-family: system-ui, -apple-system, sans-serif; max-width: 480px; margin: 4em auto; padding: 2em; text-align: center; color: #222;">
  <h1 style="font-weight: 400;">✓ Authorized</h1>
  <p>You can close this window and return to your terminal.</p>
</body></html>`
