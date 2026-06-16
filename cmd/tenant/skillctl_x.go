package main

// TEN-67: X / Twitter catalog entry for `/skill configure` — single bearer-token
// field, same shape as discord. Probe hits GET /2/users/me. The bearer is stored
// at skill:x:bearer; the toolmux x activator (toolmux.go) reads it so
// `/configure x <bearer>` → `/enable x` brings the x_* tools live mid-session.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode"
)

// resolveXBearer picks the X bearer token from, in order: an explicit
// --x-bearer flag, $X_BEARER_TOKEN, then the /configure-saved secret in
// credentials.json. Shared by the launch path (toolmux.go `if pf.x`) and the
// /enable activator so both resolve the same token. The flag-vs-creds order
// is moot in practice: when --x is passed the launch path registers "x" and
// the activator's stub is skipped via mux.hasPlugin, so the activator only
// runs when no flag was set (and the freshly-configured creds value wins).
func resolveXBearer(cfgDir, flagBearer string) string {
	if b := strings.TrimSpace(flagBearer); b != "" {
		return b
	}
	if b := strings.TrimSpace(os.Getenv("X_BEARER_TOKEN")); b != "" {
		return b
	}
	if creds, err := loadCredentials(cfgDir); err == nil {
		return strings.TrimSpace(creds.get(skillSecretID("x", "bearer")))
	}
	return ""
}

func xSkillKind() skillKind {
	return skillKind{
		ID:    "x",
		Label: "X / Twitter (app bearer token)",
		Wired: true,
		SetupHint: "Get a bearer token from developer.x.com. No token? Run `tenant x --login` " +
			"for cookie-based auth instead (out-of-band, not /skill).",
		Fields: []skillKindField{
			{
				Key:      "bearer",
				Prompt:   "X app bearer token",
				Secret:   true,
				Required: true,
				Validate: validateXBearer,
			},
		},
		Probe: probeX,
	}
}

func validateXBearer(s string) error {
	s = strings.TrimSpace(s)
	if len(s) < 80 {
		return fmt.Errorf("too short (%d chars; X bearer tokens are 80+) — check you copied the full token", len(s))
	}
	// X bearer tokens are URL-safe base64 plus '%' (URL-encoded chars) and '.'.
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '%' || r == '.') {
			return fmt.Errorf("contains invalid character %q — bearer tokens are URL-safe base64", r)
		}
	}
	return nil
}

// xProbeDeps is the test seam (mirrors discordProbeDeps).
type xProbeDeps struct {
	HTTP interface {
		Do(*http.Request) (*http.Response, error)
	}
	baseURL string // test override; empty = production api.x.com
}

func probeX(ctx context.Context, creds *credentials, _ map[string]string, _ func() error) (string, error) {
	return probeXWith(ctx, creds, xProbeDeps{})
}

func probeXWith(ctx context.Context, creds *credentials, deps xProbeDeps) (string, error) {
	bearer := strings.TrimSpace(creds.get(skillSecretID("x", "bearer")))
	if bearer == "" {
		return "", fmt.Errorf("X bearer token is missing — run `/skill configure x <bearer>`")
	}
	doer := deps.HTTP
	if doer == nil {
		doer = http.DefaultClient
	}
	base := "https://api.x.com"
	if deps.baseURL != "" {
		base = deps.baseURL
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/2/users/me", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach x.com: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 401:
		return "", fmt.Errorf("X returned 401 — bearer token is revoked or invalid")
	case resp.StatusCode == 403:
		return "", fmt.Errorf("X returned 403 — the app is missing read permissions (check developer.x.com)")
	case resp.StatusCode == 429:
		return "", fmt.Errorf("rate limited by X — try again in a few minutes")
	case resp.StatusCode >= 500:
		return "", fmt.Errorf("x.com server error (HTTP %d) — retry shortly", resp.StatusCode)
	case resp.StatusCode >= 400:
		return "", fmt.Errorf("X returned HTTP %d", resp.StatusCode)
	}

	var out struct {
		Data struct {
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("could not parse X response: %w", err)
	}
	if out.Data.Username == "" {
		return "X token valid", nil
	}
	return "@" + out.Data.Username, nil
}
