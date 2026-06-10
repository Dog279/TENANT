package main

// TEN-160: Atlassian (Jira) catalog entry for the `/configure` skill-config
// surface — mirrors the gsuite pattern (TEN-65/71). Two auth modes: "oauth"
// (browser sign-in via the Developer Console app — the Probe runs the
// localhost-callback flow and caches the token) and "token" (paste a personal
// API token). The probe reuses the runtime plugin's auth code (atlassian.Open /
// atlassian.Authorize) so /configure hits the exact same paths the agent does.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	refpkg "reflect"
	"strings"
	"time"

	"tenant/internal/plugins/atlassian"
)

// atlassianMCPURL is Atlassian's official hosted Remote MCP Server. The "mcp"
// auth mode connects to it directly (zero app setup) — the recommended path.
const atlassianMCPURL = "https://mcp.atlassian.com/v1/mcp"

func atlassianSkillKind() skillKind {
	showIf := func(mode string) func(map[string]string) bool {
		return func(v map[string]string) bool { return v["auth"] == mode }
	}
	notMCP := func(v map[string]string) bool { return v["auth"] != "" && v["auth"] != "mcp" }
	return skillKind{
		ID:    "atlassian",
		Label: "Atlassian (Jira) — MCP browser sign-in (or native OAuth/token)",
		Wired: true,
		SetupHint: "Three ways to connect:\n" +
			"  • mcp   — RECOMMENDED. Connect to Atlassian's official hosted server (" + atlassianMCPURL + ") via browser sign-in. Zero app setup; survives restarts. Pick this and authorize.\n" +
			"  • oauth — native REST plugin via your OWN Developer Console OAuth app (advanced).\n" +
			"  • token — native REST plugin via a personal API token (id.atlassian.com → Security → API tokens).",
		Fields: []skillKindField{
			{
				Key: "auth", Prompt: "How do you want to connect to Jira?", Required: true, Default: "mcp",
				Options: []string{"mcp", "oauth", "token"},
				OptionLabels: []string{
					"Atlassian MCP — browser sign-in, zero setup (recommended)",
					"OAuth 2.0 — native plugin (your own Developer Console app)",
					"API token — native plugin (personal token)",
				},
				Validate:  validateOneOf("mcp", "oauth", "token"),
				NoteAfter: atlassianAuthNote,
			},
			{Key: "site", Prompt: "Atlassian site URL (e.g. https://you.atlassian.net)", Required: true, Validate: validateAtlassianSite, ShowIf: notMCP},
			{Key: "project", Prompt: "Default Jira project key (e.g. TEN) — optional, Enter to skip", ShowIf: notMCP},
			{Key: "client_id", Prompt: "OAuth app Client ID (Developer Console)", Required: true, ShowIf: showIf("oauth")},
			{Key: "client_secret", Prompt: "OAuth app Client Secret", Secret: true, Required: true, ShowIf: showIf("oauth")},
			{Key: "email", Prompt: "Your Atlassian account email", Required: true, Validate: validateEmailRFC5322, ShowIf: showIf("token")},
			{Key: "api_token", Prompt: "API token (id.atlassian.com → Security → API tokens)", Secret: true, Required: true, ShowIf: showIf("token")},
		},
		Probe:        probeAtlassian,
		ProbeTimeout: 5 * time.Minute, // browser sign-in can take a few minutes
	}
}

func validateAtlassianSite(s string) error {
	if s == "" {
		return nil // framework enforces Required; probe re-checks
	}
	if !strings.HasPrefix(s, "https://") {
		return fmt.Errorf("site must start with https:// (e.g. https://you.atlassian.net)")
	}
	return nil
}

func atlassianAuthNote(value string) (string, bool) {
	switch value {
	case "mcp":
		return "→ Connecting to Atlassian's official hosted server. A browser will open — approve access, then pick your site (two screens; click through once each). Survives restarts; no app to create.", false
	case "oauth":
		return "→ Create a one-time OAuth 2.0 app: developer.atlassian.com → Your apps → Create → OAuth 2.0 integration.\n" +
			"   • Permissions → add the Jira scopes: read:jira-work, write:jira-work, read:jira-user, offline_access.\n" +
			"   • Authorization → Callback URL: http://127.0.0.1:8765/callback\n" +
			"   Then paste the Client ID + Secret. When you finish, a browser opens to authorize.", false
	case "token":
		return "→ Create an API token at id.atlassian.com → Security → Create and manage API tokens, then paste it + your account email.", false
	}
	return "", false
}

// atlassianMCPConnector connects to a remote MCP server (browser sign-in),
// persists it, and brings its tools live — returns a human-readable identity.
// Injected at runtime (it needs the live tool mux); nil in tests / cold probes.
type atlassianMCPConnector func(ctx context.Context, url string) (string, error)

// probeAtlassian verifies the configured auth. oauth reuses a cached token (no
// browser) when possible, falling back to the browser sign-in only when needed.
func probeAtlassian(ctx context.Context, creds *credentials, settings map[string]string, _ func() error) (string, error) {
	return probeAtlassianWith(ctx, creds, settings, "", openBrowser, nil)
}

func probeAtlassianWith(ctx context.Context, creds *credentials, settings map[string]string, cfgDir string, openBrowser func(string) error, mcpConnect atlassianMCPConnector) (string, error) {
	auth := settings["auth"]
	if auth == "" {
		auth = "mcp"
	}
	if auth == "mcp" {
		if mcpConnect == nil {
			return "", errors.New("MCP connect unavailable in this session — use `/mcp add " + atlassianMCPURL + "`")
		}
		return mcpConnect(ctx, atlassianMCPURL)
	}

	site := settings["site"]
	if site == "" {
		return "", errors.New("site is required (e.g. https://you.atlassian.net)")
	}
	switch auth {
	case "token":
		email := settings["email"]
		token := creds.get(skillSecretID("atlassian", "api_token"))
		if email == "" || token == "" {
			return "", errors.New("token mode needs email + api_token")
		}
		svc, err := atlassian.Open(ctx, atlassian.Config{SiteURL: site, Email: email, APIToken: token, Project: settings["project"]})
		if err != nil {
			return "", err
		}
		if _, err := svc.Jira.Search(ctx, "ORDER BY created DESC", 1); err != nil {
			return "", fmt.Errorf("Jira probe failed (check site/email/token): %w", err)
		}
		return fmt.Sprintf("%s @ %s", email, site), nil

	case "oauth":
		clientID := settings["client_id"]
		secret := creds.get(skillSecretID("atlassian", "client_secret"))
		if clientID == "" || secret == "" {
			return "", errors.New("oauth mode needs client_id + client_secret (from your Developer Console app)")
		}
		cfg := atlassian.Config{
			SiteURL: site, Project: settings["project"],
			ClientID: clientID, ClientSecret: secret,
			TokenPath:        atlassianTokenPath(cfgDir),
			OAuthOpenBrowser: openBrowser,
		}
		// Reuse a cached token if it still works (no browser re-prompt).
		if svc, err := atlassian.Open(ctx, cfg); err == nil {
			if _, serr := svc.Jira.Search(ctx, "ORDER BY created DESC", 1); serr == nil {
				return fmt.Sprintf("connected (cached token) @ %s", site), nil
			}
		}
		// Otherwise run the browser sign-in, then verify.
		resolved, err := atlassian.Authorize(ctx, cfg)
		if err != nil {
			return "", err
		}
		svc, err := atlassian.Open(ctx, cfg)
		if err != nil {
			return "", fmt.Errorf("post-login open: %w", err)
		}
		if _, err := svc.Jira.Search(ctx, "ORDER BY created DESC", 1); err != nil {
			return "", fmt.Errorf("Jira probe failed after sign-in: %w", err)
		}
		return fmt.Sprintf("OAuth connected — %s", resolved), nil

	default:
		return "", fmt.Errorf("unknown auth mode %q (expected mcp, oauth, or token)", auth)
	}
}

// atlassianTokenPath is the shared 0600 OAuth token cache location — used by the
// probe, the toolmux activator, and `tenant atlassian login` so they all agree.
func atlassianTokenPath(cfgDir string) string {
	return filepath.Join(cfgDir, "atlassian-token.json")
}

// adaptAtlassianForCfgDir injects cfgDir (native OAuth token cache) + the
// runtime MCP connector (for the "mcp" auth mode) into the probe. Mirrors
// adaptGSuiteForCfgDir; leaves a test-installed probe untouched.
func adaptAtlassianForCfgDir(k skillKind, cfgDir string, mcpConnect atlassianMCPConnector) skillKind {
	if isProductionAtlassianProbe(k.Probe) {
		k.Probe = func(ctx context.Context, c *credentials, s map[string]string, _ func() error) (string, error) {
			return probeAtlassianWith(ctx, c, s, cfgDir, openBrowser, mcpConnect)
		}
	}
	return k
}

func isProductionAtlassianProbe(p skillProbe) bool {
	if p == nil {
		return false
	}
	return refpkg.ValueOf(p).Pointer() == refpkg.ValueOf(skillProbe(probeAtlassian)).Pointer()
}
