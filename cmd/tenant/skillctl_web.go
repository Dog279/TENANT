package main

// TEN-69: web catalog entry for the `/skill configure` surface — the OPTIONAL
// Brave Search API key. The web plugin works without it (web_search falls back
// to DuckDuckGo); a key just upgrades the backend. This is the first skill with
// an all-optional field set, so `/skill configure web` with no args is a valid
// no-op that simply (re)enables the always-on web plugin. The key is stored at
// skill:web:brave_key; braveKey() (lazyweb.go) reads BOTH that and the
// brave_search credential, so a key set via either surface upgrades web_search.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func webSkillKind() skillKind {
	return skillKind{
		ID:    "web",
		Label: "Web browsing (Chrome + optional Brave Search)",
		Wired: true,
		SetupHint: "Brave key is OPTIONAL — without it, web_search uses DuckDuckGo. " +
			"Get a free-tier key at https://api.search.brave.com.",
		Fields: []skillKindField{
			{
				Key:      "brave_key",
				Prompt:   "Brave Search API key (optional)",
				Secret:   true,
				Required: false,
				Validate: validateBraveKey,
			},
		},
		Probe: probeWebBrave,
	}
}

func validateBraveKey(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil // optional — empty is valid (DuckDuckGo fallback)
	}
	if len(s) < 20 {
		return fmt.Errorf("too short (%d chars; Brave keys are 30+) — verify on api.search.brave.com", len(s))
	}
	if strings.ContainsAny(s, " \t\n\r") {
		return errors.New("contains whitespace — strip surrounding spaces / newlines")
	}
	return nil
}

// webProbeDeps is the test seam (mirrors discordProbeDeps). Production passes a
// zero value (real http.DefaultClient, real Brave endpoint).
type webProbeDeps struct {
	HTTP interface {
		Do(*http.Request) (*http.Response, error)
	}
	baseURL string // test override; empty = production api.search.brave.com
}

func probeWebBrave(ctx context.Context, creds *credentials, _ map[string]string, _ func() error) (string, error) {
	return probeWebBraveWith(ctx, creds, webProbeDeps{})
}

// probeWebBraveWith validates the configured Brave key with a 1-result search.
// The "no key" branch returns a non-error identity — for web, probe-success
// with no config is a VALID state (the DuckDuckGo fallback works).
func probeWebBraveWith(ctx context.Context, creds *credentials, deps webProbeDeps) (string, error) {
	key := creds.get(skillSecretID("web", "brave_key"))
	if strings.TrimSpace(key) == "" {
		return "no Brave key configured — web_search uses DuckDuckGo (works without setup)", nil
	}

	doer := deps.HTTP
	if doer == nil {
		doer = http.DefaultClient
	}
	base := "https://api.search.brave.com"
	if deps.baseURL != "" {
		base = deps.baseURL
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/res/v1/web/search?q=test&count=1", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("Accept", "application/json")

	resp, err := doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach Brave Search: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 200:
		return "Brave key valid — web_search upgraded to Brave", nil
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return "", fmt.Errorf("Brave key rejected (HTTP %d) — verify it on api.search.brave.com", resp.StatusCode)
	case resp.StatusCode == 429:
		return "", fmt.Errorf("Brave quota exceeded (HTTP 429) — wait, or upgrade your plan")
	default:
		return "", fmt.Errorf("Brave Search returned HTTP %d", resp.StatusCode)
	}
}
