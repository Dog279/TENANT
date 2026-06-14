package main

// TEN-66: Discord catalog entry for the `/skill configure` surface.
// Single secret field (bot token), probe mirrors checkDiscord (doctor.go).
// Crib the pattern from skillctl_gsuite.go / skillctl_atlassian.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func discordSkillKind() skillKind {
	return skillKind{
		ID:        "discord",
		Label:     "Discord (bot integration)",
		Wired:     true,
		SetupHint: "Create the bot at https://discord.com/developers/applications and invite to a server with the `bot` scope.",
		Fields: []skillKindField{
			{
				Key:      "token",
				Prompt:   "Discord bot token",
				Secret:   true,
				Required: true,
				Validate: validateDiscordToken,
			},
		},
		Probe: probeDiscord,
	}
}

func validateDiscordToken(s string) error {
	s = strings.TrimSpace(s)
	if len(s) < 60 {
		return fmt.Errorf("too short (%d chars; bot tokens are 60+) — check you copied the full token", len(s))
	}
	if strings.ContainsAny(s, " \t\n\r") {
		return fmt.Errorf("contains whitespace — strip surrounding spaces / newlines")
	}
	if strings.HasPrefix(s, `"`) || strings.HasPrefix(s, `'`) {
		return fmt.Errorf("starts with a quote — paste the raw token, no surrounding quotes")
	}
	return nil
}

// discordProbeDeps is the test seam. Production callers pass a zero value
// (real http.DefaultClient). Tests inject an httptest-backed doer.
type discordProbeDeps struct {
	HTTP interface {
		Do(*http.Request) (*http.Response, error)
	}
	baseURL string // test override; empty = production discord.com
}

// probeDiscord hits GET /users/@me with the bot token, mirroring checkDiscord
// (doctor.go:1146) exactly — same endpoint, same auth header, same UA.
func probeDiscord(ctx context.Context, creds *credentials, settings map[string]string, _ func() error) (string, error) {
	return probeDiscordWith(ctx, creds, settings, discordProbeDeps{})
}

// probeDiscordWith is the implementation. Tests call this directly with their
// httptest-backed deps; production callers reach it via probeDiscord with a
// zero-valued deps.
func probeDiscordWith(ctx context.Context, creds *credentials, _ map[string]string, deps discordProbeDeps) (string, error) {
	token := creds.get(skillSecretID("discord", "token"))
	if token == "" {
		return "", fmt.Errorf("discord bot token is missing — run `/skill configure discord <token>`")
	}

	doer := deps.HTTP
	if doer == nil {
		doer = http.DefaultClient
	}

	base := "https://discord.com/api/v10/users/@me"
	if deps.baseURL != "" {
		base = deps.baseURL + "/users/@me"
	}

	req, err := http.NewRequestWithContext(ctx, "GET", base, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/tenant-mcp/tenant, 1.0)")

	resp, err := doer.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach discord.com: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 401:
		return "", fmt.Errorf("discord returned 401 Unauthorized — token is invalid or revoked")
	case resp.StatusCode == 429:
		return "", fmt.Errorf("rate limited by discord — wait a moment and retry `/skill probe discord`")
	case resp.StatusCode >= 500:
		return "", fmt.Errorf("discord server error (HTTP %d) — retry shortly", resp.StatusCode)
	case resp.StatusCode >= 400:
		return "", fmt.Errorf("discord returned HTTP %d", resp.StatusCode)
	}

	// Parse identity from the /users/@me response.
	var user struct {
		Username      string `json:"username"`
		Discriminator string `json:"discriminator"`
		ID            string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", fmt.Errorf("could not parse discord response: %w", err)
	}
	if user.Discriminator != "" && user.Discriminator != "0" {
		return fmt.Sprintf("%s#%s", user.Username, user.Discriminator), nil
	}
	// Modern Discord uses username only (discriminator migration).
	return user.Username, nil
}
