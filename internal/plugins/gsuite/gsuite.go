package gsuite

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	calendar "google.golang.org/api/calendar/v3"
	drive "google.golang.org/api/drive/v3"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Config opens a Service. Auth is one of:
//   - "sa"     — service-account JSON + domain-wide delegation (BUSINESS — primary)
//   - "gcloud" — ADC via the CLI; zero setup, just gcloud login (DEV)
//   - "oauth"  — user-mode OAuth code-grant flow (ADVANCED, personal Gmail)
//
// tenant's primary target is BUSINESS / Workspace deployments where an IT
// admin sets up Domain-Wide Delegation once and impersonates per-user
// subjects. The other two modes are kept for dev work and edge cases.
type Config struct {
	Auth      string // "sa" | "gcloud" | "oauth"
	SAJSON    []byte // service-account key bytes (Auth=="sa")
	Subject   string // user to impersonate via DWD (Auth=="sa")
	AllowSend bool   // selects least-privilege scope set (read-only vs read/write)

	// OAuth fields (Auth=="oauth"):
	OAuthCreds []byte                    // Desktop App OAuth client JSON bytes
	OAuthToken *oauth2.Token             // cached refresh-token blob; nil ⇒ run browser flow
	OAuthSave  func(*oauth2.Token) error // persist callback; called whenever a token is minted/rotated

	HTTP  httpDoer         // transport seam for BOTH SA token-exchange and the API Base (via doerRT); nil ⇒ real network
	Run   runner           // nil ⇒ real exec (Auth=="gcloud")
	Clock func() time.Time // nil ⇒ time.Now (tests inject)
	// OAuthOpenBrowser is the test seam for Auth=="oauth"; nil ⇒ open the
	// system browser.
	OAuthOpenBrowser func(url string) error
}

// Service is the opened Workspace connector. The wrappers hold the
// official google.golang.org/api clients and translate their types into
// tenant's normalized shapes (MsgHdr/Message/Event/File/...).
type Service struct {
	Gmail    *Gmail
	Calendar *Calendar
	Drive    *Drive
}

// Open validates config and wires the chosen auth path onto the official
// Google API clients. No network is touched here — the first real API
// call mints the token.
func Open(cfg Config) (*Service, error) {
	ts, err := tokenSourceFor(cfg)
	if err != nil {
		return nil, err
	}
	// Base transport for the API calls. Production leaves cfg.HTTP nil →
	// real network. Tests inject cfg.HTTP (the same seam the SA token
	// exchange uses) to rewrite *.googleapis.com → an httptest server, so
	// a single injected doer routes BOTH token minting and API traffic.
	var base http.RoundTripper = http.DefaultTransport
	if cfg.HTTP != nil {
		base = doerRT{cfg.HTTP}
	}
	// A bare oauth2.Transport (not oauth2.NewClient) so we don't get a
	// second ReuseTokenSource cache layered on top of cachedSource.
	rt := &oauth2.Transport{Source: bearerShim{ts: ts}, Base: base}
	return newService(&http.Client{Transport: rt})
}

// doerRT adapts an httpDoer to http.RoundTripper. httpDoer.Do and
// RoundTripper.RoundTrip share a signature, so this just lets the
// injected token-exchange seam (cfg.HTTP) double as the API client's
// Base transport in tests.
type doerRT struct{ d httpDoer }

func (r doerRT) RoundTrip(req *http.Request) (*http.Response, error) { return r.d.Do(req) }

// tokenSourceFor builds the bearer-token source for the chosen auth mode.
// Pure validation + construction; no network.
func tokenSourceFor(cfg Config) (tokenSource, error) {
	h := cfg.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	switch cfg.Auth {
	case "", "gcloud":
		return newGcloudSource(cfg.Run, cfg.Clock), nil
	case "sa":
		if len(cfg.SAJSON) == 0 {
			return nil, fmt.Errorf("gsuite: auth=sa needs the service-account JSON (--sa-json)")
		}
		return newSASource(cfg.SAJSON, cfg.Subject, scopesFor(cfg.AllowSend), h, cfg.Clock)
	case "oauth":
		if len(cfg.OAuthCreds) == 0 {
			return nil, fmt.Errorf("gsuite: auth=oauth needs OAuthCreds (path to Desktop App OAuth client JSON)")
		}
		return newOAuthSource(cfg.OAuthCreds, cfg.OAuthToken, cfg.OAuthSave, scopesFor(cfg.AllowSend), cfg.OAuthOpenBrowser, cfg.Clock)
	default:
		return nil, fmt.Errorf("gsuite: unknown auth %q (use \"sa\", \"gcloud\", or \"oauth\")", cfg.Auth)
	}
}

// newService builds the three official Workspace clients over hc. Shared
// by Open (prod: hc's transport carries auth) and tests (hc's transport
// rewrites *.googleapis.com → an httptest server). Passing
// option.WithHTTPClient short-circuits credential resolution, so no
// network is touched at construction.
func newService(hc *http.Client) (*Service, error) {
	ctx := context.Background()
	gm, err := gmail.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("gsuite: gmail client: %w", err)
	}
	cal, err := calendar.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("gsuite: calendar client: %w", err)
	}
	dr, err := drive.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("gsuite: drive client: %w", err)
	}
	return &Service{
		Gmail:    &Gmail{svc: gm},
		Calendar: &Calendar{svc: cal},
		Drive:    &Drive{svc: dr},
	}, nil
}

// bearerShim adapts tenant's tokenSource (which mints + caches bearer
// strings) to oauth2.TokenSource. Driven by a bare oauth2.Transport that
// calls Token() on every request; the cachedSource underneath does the
// real expiry-aware caching, so each call returns a fresh-from-cache
// access token. (Returning a zero Expiry is safe precisely because there
// is no ReuseTokenSource above us inspecting it.)
type bearerShim struct{ ts tokenSource }

func (s bearerShim) Token() (*oauth2.Token, error) {
	tok, err := s.ts.token(context.Background())
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{AccessToken: tok, TokenType: "Bearer"}, nil
}
