package github

// auth.go owns the PAT: where it comes from, where it is cached, and the
// guarantee that it never reaches a log, an error string, or the model.
//
// Resolution order is explicit config > environment > on-disk cache. There is
// deliberately NO --github-token flag: a flag lands in shell history, `ps`
// output, and scheduled-task definitions.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// TokenFileName is the cache file inside the config dir.
const TokenFileName = "github-token.json"

// tokenFile is the on-disk shape. A struct (not a bare string) so a future
// GitHub App installation token can add expiry without breaking the format.
type tokenFile struct {
	Token string `json:"token"`
}

// TokenPath is the cache location for a given config dir.
func TokenPath(cfgDir string) string { return filepath.Join(cfgDir, TokenFileName) }

// LoadToken reads the cached PAT. Missing file is not an error — it just means
// "not configured yet", which callers report as a setup hint.
func LoadToken(cfgDir string) (string, error) {
	b, err := os.ReadFile(TokenPath(cfgDir))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return "", errors.New("github: token cache is corrupt; re-run `/configure github`")
	}
	return strings.TrimSpace(tf.Token), nil
}

// SaveToken writes the PAT 0600 into a 0700 dir.
//
// NOTE: on Windows these mode bits do NOT produce an owner-only DACL — Go maps
// them loosely and the file lands 0666-equivalent. That is a known, tracked gap
// (TEN-325, centralize secret storage), not something this plugin can fix
// locally; the same caveat already applies to the Atlassian and MCP token
// stores. Documented here so nobody reads 0600 as a Windows guarantee.
func SaveToken(cfgDir, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("github: refusing to save an empty token")
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(tokenFile{Token: token})
	if err != nil {
		return err
	}
	return os.WriteFile(TokenPath(cfgDir), b, 0o600)
}

// ResolveToken picks the PAT: explicit > $GITHUB_TOKEN > $GITHUB_PAT > cache.
// Returns "" when nothing is configured (callers turn that into a setup hint).
func ResolveToken(explicit, cfgDir string) string {
	if t := strings.TrimSpace(explicit); t != "" {
		return t
	}
	for _, env := range []string{"GITHUB_TOKEN", "GITHUB_PAT"} {
		if t := strings.TrimSpace(os.Getenv(env)); t != "" {
			return t
		}
	}
	if cfgDir != "" {
		if t, err := LoadToken(cfgDir); err == nil {
			return t
		}
	}
	return ""
}

// tokenPattern matches GitHub's documented token prefixes (classic ghp_, fine
// grained github_pat_, app/installation gh[sur]_) plus the x-access-token form
// used in an authenticated clone URL, which is how a token most often leaks:
// echoed inside a `git push https://x-access-token:TOKEN@github.com/...` line.
var tokenPattern = regexp.MustCompile(`(github_pat_[A-Za-z0-9_]+|gh[pousr]_[A-Za-z0-9_]+|x-access-token:[^@\s]+)`)

// Redact removes any GitHub token from a string. Every error this package
// returns to the agent passes through here, because go-github and git echo the
// request URL — credentials included — on failure.
func Redact(s string) string { return tokenPattern.ReplaceAllString(s, "[redacted]") }
