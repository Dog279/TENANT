package gsuite

// Embedded OAuth credentials support — the path to zero-Cloud-setup
// gsuite for end users.
//
// THE PROBLEM: Google requires every app accessing Gmail/Calendar to
// register an OAuth client with Google Cloud. Without a published
// client, every operator has to do their own Cloud Console dance —
// painful for non-developers.
//
// THE FIX (two layers, in priority order):
//
//   1. COMPILED-IN (this file's go:embed) — the maintainer drops their
//      OAuth client JSON into embedded_oauth_client.json before `go
//      build`, baking it into the distributed binary. Every end user
//      who downloads that binary gets zero-Cloud-Console UX. The repo
//      ships a placeholder `{}` so vanilla builds still work (just
//      fall through to layer 2).
//
//   2. RUNTIME FILE (<cfgDir>/oauth_client.json) — for cases where
//      the maintainer wants to install credentials AFTER distribution
//      (different OAuth client per environment, dev/prod split, etc.)
//      Installed via `tenant oauth-setup gsuite <path>`.
//
// Standalone @gmail.com accounts work fine — the OAuth client is what
// REGISTERS with Google; the signing-in account can be any Google
// account (Gmail, Workspace, etc.) added as a test user under the
// maintainer's consent screen.
//
// SECURITY NOTE: the "client_secret" for Desktop App OAuth clients is
// NOT actually secret per RFC 8252 (BCP for OAuth on native apps).
// Embedding it in a distributed binary is the documented pattern. The
// user's TOKENS (in credentials.json, 0600) are the real secret.

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
)

// EmbeddedOAuthFilename is the well-known filename for the
// runtime-installed OAuth client JSON, looked up under the operator's
// tenant config directory.
const EmbeddedOAuthFilename = "oauth_client.json"

// compiledInOAuthClientJSON is the OAuth client baked into the binary
// at build time. The maintainer replaces embedded_oauth_client.json
// in source with their real client JSON; vanilla checkouts ship a
// placeholder `{}` which the runtime detects as "no embedded client."
//
//go:embed embedded_oauth_client.json
var compiledInOAuthClientJSON []byte

// EmbeddedOAuthPath returns the absolute path the runtime-file layer
// looks at. Used by `tenant oauth-setup` to know where to install.
func EmbeddedOAuthPath(cfgDir string) string {
	return filepath.Join(cfgDir, EmbeddedOAuthFilename)
}

// HasEmbeddedOAuth returns true when EITHER the compiled-in JSON is a
// valid OAuth client OR the runtime file exists under cfgDir. The
// configure flow uses this to decide whether to hide the "paste your
// JSON" prompt — when true, end users just click sign-in.
func HasEmbeddedOAuth(cfgDir string) bool {
	if compiledInLooksValid() {
		return true
	}
	if cfgDir == "" {
		return false
	}
	info, err := os.Stat(EmbeddedOAuthPath(cfgDir))
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// LoadEmbeddedOAuth returns the OAuth client JSON bytes the plugin
// should use. Resolution order:
//   1. runtime file under cfgDir (lets ops swap creds without rebuild)
//   2. compiled-in via go:embed
//   3. (nil, nil) — caller falls back to operator-supplied JSON
//
// The runtime file wins so an operator can override the distributed
// default per-machine without a rebuild.
func LoadEmbeddedOAuth(cfgDir string) ([]byte, error) {
	// Layer 2 first: runtime file overrides compiled-in.
	if cfgDir != "" {
		path := EmbeddedOAuthPath(cfgDir)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return os.ReadFile(path)
		}
	}
	if compiledInLooksValid() {
		// Return a copy so callers can't mutate the embedded blob.
		out := make([]byte, len(compiledInOAuthClientJSON))
		copy(out, compiledInOAuthClientJSON)
		return out, nil
	}
	return nil, nil
}

// compiledInLooksValid reports whether the go:embed blob parses as a
// real OAuth Desktop App JSON (vs. the `{}` placeholder shipped in
// the repo). Used to detect "maintainer hasn't filled this in yet"
// vs "production binary with real credentials."
func compiledInLooksValid() bool {
	if len(compiledInOAuthClientJSON) == 0 {
		return false
	}
	var probe struct {
		Installed *struct {
			ClientID string `json:"client_id"`
		} `json:"installed,omitempty"`
		Web *struct {
			ClientID string `json:"client_id"`
		} `json:"web,omitempty"`
	}
	if err := json.Unmarshal(compiledInOAuthClientJSON, &probe); err != nil {
		return false
	}
	if probe.Installed != nil && probe.Installed.ClientID != "" {
		return true
	}
	if probe.Web != nil && probe.Web.ClientID != "" {
		return true
	}
	return false
}
