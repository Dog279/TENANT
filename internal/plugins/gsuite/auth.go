// Package gsuite is Tenant's Google Workspace connector (Gmail +
// Calendar + Drive). API calls go through the official
// google.golang.org/api clients (full Workspace coverage); only the
// auth token-minting is hand-rolled here, with stdlib crypto, so the
// three auth paths stay dependency-light and fully unit-testable:
//
//   - Service account + domain-wide delegation: an RS256 JWT-bearer
//     assertion (iss=client_email, sub=the impersonated user) is
//     exchanged for an access token. The Workspace-admin route.
//   - gcloud ADC: shell out to `gcloud auth application-default
//     print-access-token`. Zero setup, no service-account key.
//   - OAuth: a Desktop-App loopback code-grant flow (personal Gmail).
//
// Each path yields a tokenSource; Open wires it onto the official
// clients' transport via a bare oauth2.Transport (see gsuite.go).
package gsuite

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// httpDoer is the seam every Google call goes through (so tests inject
// an httptest server instead of hitting Google).
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// tokenSource yields a bearer access token, refreshing as needed.
type tokenSource interface {
	token(ctx context.Context) (string, error)
}

// minter produces a fresh token + its absolute expiry.
type minter func(ctx context.Context) (string, time.Time, error)

// cachedSource memoises a minter until shortly before expiry. clock is
// injectable so expiry/refresh is unit-testable without sleeping.
type cachedSource struct {
	mint  minter
	clock func() time.Time

	mu  sync.Mutex
	tok string
	exp time.Time
}

func newCached(m minter, clock func() time.Time) *cachedSource {
	if clock == nil {
		clock = time.Now
	}
	return &cachedSource{mint: m, clock: clock}
}

const refreshSkew = 60 * time.Second // refresh a minute early

func (c *cachedSource) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && c.clock().Add(refreshSkew).Before(c.exp) {
		return c.tok, nil
	}
	t, e, err := c.mint(ctx)
	if err != nil {
		return "", err
	}
	c.tok, c.exp = t, e
	return t, nil
}

// --- A. service account + domain-wide delegation ---

// serviceAccount is the slice of the GCP key JSON we need.
type serviceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
	Type        string `json:"type"`
}

type saSource struct {
	sa      serviceAccount
	priv    *rsa.PrivateKey
	subject string // user to impersonate (domain-wide delegation)
	scopes  []string
	http    httpDoer
	clock   func() time.Time
}

func newSASource(keyJSON []byte, subject string, scopes []string, h httpDoer, clock func() time.Time) (*cachedSource, error) {
	var sa serviceAccount
	if err := json.Unmarshal(keyJSON, &sa); err != nil {
		return nil, fmt.Errorf("gsuite: parse service-account JSON: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("gsuite: service-account JSON missing client_email/private_key")
	}
	if sa.Type != "" && sa.Type != "service_account" {
		return nil, fmt.Errorf("gsuite: not a service-account key (type=%q)", sa.Type)
	}
	if subject == "" {
		return nil, fmt.Errorf("gsuite: domain-wide delegation needs --subject (the user to act as)")
	}
	priv, err := parseRSAKey(sa.PrivateKey)
	if err != nil {
		return nil, err
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	if clock == nil {
		clock = time.Now
	}
	s := &saSource{sa: sa, priv: priv, subject: subject, scopes: scopes, http: h, clock: clock}
	return newCached(s.mint, clock), nil
}

func (s *saSource) mint(ctx context.Context) (string, time.Time, error) {
	iat := s.clock()
	exp := iat.Add(time.Hour)
	hdr := b64(`{"alg":"RS256","typ":"JWT"}`)
	claims, _ := json.Marshal(map[string]any{
		"iss":   s.sa.ClientEmail,
		"sub":   s.subject, // impersonate (DWD)
		"scope": strings.Join(s.scopes, " "),
		"aud":   s.sa.TokenURI,
		"iat":   iat.Unix(),
		"exp":   exp.Unix(),
	})
	signing := hdr + "." + b64(string(claims))
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gsuite: sign assertion: %w", err)
	}
	assertion := signing + "." + base64.RawURLEncoding.EncodeToString(sig)

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	return s.exchange(ctx, s.sa.TokenURI, form)
}

func (s *saSource) exchange(ctx context.Context, tokenURI string, form url.Values) (string, time.Time, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gsuite: token endpoint: %w", err)
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", time.Time{}, fmt.Errorf("gsuite: decode token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("gsuite: token exchange failed: %s %s (status %d)",
			tr.Error, tr.ErrorDesc, resp.StatusCode)
	}
	ttl := tr.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	return tr.AccessToken, s.clock().Add(time.Duration(ttl) * time.Second), nil
}

func parseRSAKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("gsuite: private_key is not valid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gsuite: parse RSA key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("gsuite: service-account key is not RSA")
	}
	return rk, nil
}

// --- B. gcloud Application Default Credentials ---

// runner runs an external command (injectable so tests don't need a
// real gcloud install).
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("gsuite: `%s %s` failed: %w (is gcloud installed & `gcloud auth application-default login` done?)",
			name, strings.Join(args, " "), err)
	}
	return out, nil
}

// gcloudTTL: print-access-token doesn't report expiry; ADC tokens last
// ~1h, so refresh well inside that.
const gcloudTTL = 50 * time.Minute

func newGcloudSource(run runner, clock func() time.Time) *cachedSource {
	if run == nil {
		run = execRunner
	}
	if clock == nil {
		clock = time.Now
	}
	mint := func(ctx context.Context) (string, time.Time, error) {
		out, err := run(ctx, "gcloud", "auth", "application-default", "print-access-token")
		if err != nil {
			return "", time.Time{}, err
		}
		tok := strings.TrimSpace(string(out))
		if tok == "" {
			return "", time.Time{}, fmt.Errorf("gsuite: gcloud returned an empty access token")
		}
		return tok, clock().Add(gcloudTTL), nil
	}
	return newCached(mint, clock)
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// --- C. OAuth code-grant flow (TEN-71) ---

// newOAuthSource builds a token source for the OAuth user-auth path.
// Three behaviors based on inputs:
//
//   - saved != nil + saved.Valid()   → use the cached token (refresh
//     automatic via oauth2.TokenSource); no browser
//   - saved != nil + expired         → refresh via the cached
//     RefreshToken; no browser
//   - saved == nil                   → run the loopback flow on first
//     mint(), open the browser, persist via save callback
//
// The save callback fires whenever oauth2 hands us a new token (initial
// flow OR refresh-with-rotation). Caller wires it to credentials.json
// at the skill-config layer.
func newOAuthSource(
	creds []byte,
	saved *oauth2.Token,
	save func(*oauth2.Token) error,
	scopes []string,
	openBrowser func(string) error,
	clock func() time.Time,
) (*cachedSource, error) {
	cfg, err := ParseOAuthClientFile(creds, scopes)
	if err != nil {
		return nil, err
	}
	o := &oauthSource{
		cfg:         cfg,
		saved:       saved,
		save:        save,
		openBrowser: openBrowser,
	}
	if clock == nil {
		clock = time.Now
	}
	return newCached(o.mint, clock), nil
}

type oauthSource struct {
	cfg         *oauth2.Config
	saved       *oauth2.Token
	save        func(*oauth2.Token) error
	openBrowser func(string) error

	mu sync.Mutex
	ts oauth2.TokenSource // lazily built on first mint
}

// mint returns a fresh access token + its expiry, running the browser
// flow if no cached token exists. Honors ctx cancellation.
func (o *oauthSource) mint(ctx context.Context) (string, time.Time, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.ts == nil {
		tok := o.saved
		if tok == nil || (tok.RefreshToken == "" && !tok.Valid()) {
			// No usable cached token — run the loopback flow.
			newTok, err := runLoopbackFlow(ctx, OAuthFlowConfig{
				Config:      o.cfg,
				OpenBrowser: o.openBrowser,
			})
			if err != nil {
				return "", time.Time{}, fmt.Errorf("gsuite: oauth flow: %w", err)
			}
			tok = newTok
			if o.save != nil {
				if err := o.save(tok); err != nil {
					return "", time.Time{}, fmt.Errorf("gsuite: persist oauth token: %w", err)
				}
			}
		}
		o.ts = o.cfg.TokenSource(ctx, tok)
	}

	tok, err := o.ts.Token()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gsuite: oauth refresh: %w", err)
	}
	// oauth2's TokenSource may have rotated the refresh token. Persist
	// the rotated copy so disk stays in sync.
	if o.save != nil && tok != nil && tok.RefreshToken != "" {
		if o.saved == nil || tok.RefreshToken != o.saved.RefreshToken {
			_ = o.save(tok) // best-effort: refresh succeeded, disk save is bonus
		}
		o.saved = tok
	}
	return tok.AccessToken, tok.Expiry, nil
}
