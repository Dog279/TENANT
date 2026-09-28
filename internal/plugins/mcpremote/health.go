package mcpremote

// health.go: a remote connector's authorization state as operators see it.
//
// The failure this exists for: a rotating refresh token dies (Atlassian's did on
// 2026-09-18), every tool call from then on fails with an opaque oauth2 error,
// and nothing but a single serve.log Warn says so — the status pages keep showing
// "on, N tools" (the count is frozen from connect time) and the daily brief's
// Jira section just goes quietly empty. Health is persisted in the token cache
// so it survives restarts and can be read offline, surfaced on every status
// page, announced on transition, and stamped into the tool error the model sees.

import (
	"strings"
	"time"

	"tenant/internal/secrets"
)

// Health is a connector's authorization state.
type Health struct {
	NeedsReauth bool
	Err         string    // the refresh error that killed it
	Since       time.Time // when it died (zero when healthy)
}

func (c *tokenCache) health() Health {
	if c == nil || !c.ReauthNeeded {
		return Health{}
	}
	h := Health{NeedsReauth: true, Err: c.ReauthError}
	if c.ReauthSince > 0 {
		h.Since = time.Unix(c.ReauthSince, 0).UTC()
	}
	return h
}

// TokenCachePath is where a server's token (+ auth state) is cached under
// cacheDir — exported so doctor/tests can point at the exact file.
func TokenCachePath(cacheDir, serverURL string) string { return tokenCachePath(cacheDir, serverURL) }

// ReadHealth reports a connector's persisted auth state from its token cache —
// the offline view `tenant doctor` uses (no network, no browser). ok=false when
// there is no cache at all: the server was never connected from this config dir.
func ReadHealth(cacheDir, serverURL string) (h Health, ok bool) {
	c, err := readTokenCache(tokenCachePath(cacheDir, serverURL), secrets.Peek)
	if err != nil || c == nil {
		return Health{}, false
	}
	return c.health(), true
}

// FixCommand is the command that signs a connector in again, for the Tenant
// whose config dir is cfgDir. The --config matters on a server: serve runs as
// SYSTEM with its own config dir, and an administrator's default one holds a
// cache serve never reads.
func FixCommand(cfgDir, serverURL string) string {
	cmd := "tenant mcp connect " + serverURL
	if cfgDir == "" {
		return cmd
	}
	if strings.ContainsAny(cfgDir, " \t") {
		cfgDir = `"` + cfgDir + `"`
	}
	return cmd + " --config " + cfgDir
}
