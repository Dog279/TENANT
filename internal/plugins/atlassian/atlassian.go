// Package atlassian gives the agent first-class Jira tools (search/get/create/
// comment/transition) over ctreminiom/go-atlassian (pure Go, no CGO). Two auth
// paths, both v1 (TEN-50): Path A = API token + Basic auth (personal Cloud,
// one-line setup); Path B = OAuth 2.0 3LO (browser login + localhost callback +
// auto-refreshing token cache), mirroring the connector flow. Open() picks the
// path from which fields are set. Writes are blast-radius-gated (dispatch.go).
package atlassian

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	v2 "github.com/ctreminiom/go-atlassian/v2/jira/v2"
)

// Config configures the Atlassian service. Path A uses Email+APIToken; Path B
// uses ClientID+ClientSecret (+ the OAuth fields). SiteURL is required for
// Path A (the Cloud base URL); for Path B the effective base is resolved to the
// cloudId-routed api.atlassian.com endpoint after login.
type Config struct {
	SiteURL string // e.g. https://findtime.atlassian.net (Path A)
	Project string // default project key (e.g. TEN); optional

	// Path A
	Email    string
	APIToken string // or env ATLASSIAN_TOKEN

	// Path B (OAuth 3LO)
	ClientID      string
	ClientSecret  string // or env ATLASSIAN_CLIENT_SECRET
	TokenPath     string // refresh-token cache (default <data>/atlassian-token.json, 0600)
	OAuthCallback string // callback bind addr (default 127.0.0.1:8765)
	Scopes        []string
	// OAuthOpenBrowser, if set, is called with the consent URL during Authorize
	// to open the operator's browser (production: openBrowser; tests: a stub).
	// The URL is always also printed, so a failed/absent opener still works.
	OAuthOpenBrowser func(string) error
}

// AuthPath identifies which auth path Open selected (surfaced by the doctor).
type AuthPath string

const (
	PathAPIToken AuthPath = "API token (Path A)"
	PathOAuth    AuthPath = "OAuth (Path B)"
)

// Service is the opened Atlassian client.
type Service struct {
	Jira    *JiraClient
	Auth    AuthPath
	siteURL string
	project string
}

// Open builds the service, selecting the auth path from the configured fields.
// An explicit APIToken (or $ATLASSIAN_TOKEN) takes precedence (Path A); else
// ClientID+ClientSecret select Path B. Errors loudly if neither is configured.
func Open(ctx context.Context, cfg Config) (*Service, error) {
	token := cfg.APIToken
	if token == "" {
		token = os.Getenv("ATLASSIAN_TOKEN")
	}
	switch {
	case token != "":
		if cfg.SiteURL == "" {
			return nil, errors.New("atlassian: Path A (API token) requires SiteURL")
		}
		if cfg.Email == "" {
			return nil, errors.New("atlassian: Path A (API token) requires Email")
		}
		c, err := v2.New(http.DefaultClient, cfg.SiteURL)
		if err != nil {
			return nil, fmt.Errorf("atlassian: build jira client: %w", err)
		}
		c.Auth.SetBasicAuth(cfg.Email, token)
		return &Service{
			Jira:    &JiraClient{c: c, project: cfg.Project},
			Auth:    PathAPIToken,
			siteURL: cfg.SiteURL,
			project: cfg.Project,
		}, nil

	case cfg.ClientID != "" && (cfg.ClientSecret != "" || os.Getenv("ATLASSIAN_CLIENT_SECRET") != ""):
		c, site, err := openOAuthClient(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return &Service{
			Jira:    &JiraClient{c: c, project: cfg.Project},
			Auth:    PathOAuth,
			siteURL: site,
			project: cfg.Project,
		}, nil

	default:
		return nil, errors.New("atlassian: configure Email+APIToken (Path A) or ClientID+ClientSecret (Path B)")
	}
}

// SiteURL returns the effective base URL the service is talking to.
func (s *Service) SiteURL() string { return s.siteURL }

// Project returns the default project key (may be empty).
func (s *Service) Project() string { return s.project }

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
