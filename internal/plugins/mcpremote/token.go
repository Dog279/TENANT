package mcpremote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
)

// tokenCache is the persisted per-server OAuth state (0600). It holds the DCR
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
}

// tokenCachePath is the 0600 cache file for a server URL under dir. The URL is
// hashed so the filename carries no readable host (and avoids path-unsafe chars).
func tokenCachePath(dir, serverURL string) string {
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(serverURL))
	return filepath.Join(dir, "mcp-"+hex.EncodeToString(sum[:8])+".json")
}

func loadTokenCache(path string) (*tokenCache, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(path)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
