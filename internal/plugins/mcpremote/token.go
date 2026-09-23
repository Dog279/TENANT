package mcpremote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
	"tenant/internal/secrets"
)

// tokenCache is persisted with platform-specific owner-only protection. It holds the DCR
// client + discovered endpoints (so a refresh can run without re-registering or
// re-discovering) plus the token itself. No operator secret beyond the
// dynamically-registered client — the OAuth token is the user's own grant.
type tokenCache struct {
	ServerURL    string        `json:"server_url"`
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret,omitempty"`
	AuthURL      string        `json:"auth_url"`
	TokenURL     string        `json:"token_url"`
	Scopes       []string      `json:"scopes,omitempty"`
	Token        *oauth2.Token `json:"token,omitempty"`
	// Re-auth state, persisted beside the token so a restart — and `tenant
	// doctor`, offline — report a dead connector instead of rediscovering it one
	// failing call at a time. Set when the refresh grant fails with no valid
	// access token left; cleared the moment a valid token is seen again.
	ReauthNeeded bool   `json:"reauth_needed,omitempty"`
	ReauthError  string `json:"reauth_error,omitempty"`
	ReauthSince  int64  `json:"reauth_since,omitempty"` // unix seconds
}

// tokenCachePath is the protected cache file for a server URL under dir. The URL is
// hashed so the filename carries no readable host (and avoids path-unsafe chars).
func tokenCachePath(dir, serverURL string) string {
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(serverURL))
	return filepath.Join(dir, "mcp-"+hex.EncodeToString(sum[:8])+".json")
}

func loadTokenCache(path string) (*tokenCache, error) {
	return readTokenCache(path, secrets.ReadFile)
}

// readTokenCache decodes the cache at path with the given reader. ReadHealth
// passes secrets.Peek: it is doctor's offline view, which must not re-protect
// the file it reports on (TEN-333).
func readTokenCache(path string, read func(string) ([]byte, error)) (*tokenCache, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	b, err := read(path)
	if err != nil {
		return nil, err
	}
	var c tokenCache
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func saveTokenCache(path string, c *tokenCache) error {
	if path == "" {
		return nil // persistence disabled (no cache dir) — silently skip
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return secrets.WriteFile(path, b)
}
