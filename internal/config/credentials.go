package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"tenant/internal/secrets"
)

// Credentials stores secrets the operator chose to keep on disk, away from
// the (often shared / committed) config.json. Keyed by an opaque id —
// provider id for model keys, "skill:<name>:<field>" for integration secrets.
type Credentials struct {
	Secrets map[string]string `json:"secrets"`
}

// CredentialsPath is credentials.json in cfgDir.
func CredentialsPath(cfgDir string) string { return filepath.Join(cfgDir, "credentials.json") }

// LoadCredentials reads credentials.json through the secret-storage boundary,
// which hardens its permissions. A missing file is empty credentials.
func LoadCredentials(cfgDir string) (*Credentials, error) {
	return loadCredentialsWith(cfgDir, secrets.ReadFile)
}

// PeekCredentials reads credentials.json without hardening it, for plain
// `tenant doctor`, whose audit must not change what it reports (TEN-333).
func PeekCredentials(cfgDir string) (*Credentials, error) {
	return loadCredentialsWith(cfgDir, secrets.Peek)
}

func loadCredentialsWith(cfgDir string, read func(string) ([]byte, error)) (*Credentials, error) {
	c := &Credentials{Secrets: map[string]string{}}
	b, err := read(CredentialsPath(cfgDir))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return &Credentials{Secrets: map[string]string{}}, fmt.Errorf("credentials.json corrupt: %w", err)
	}
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	return c, nil
}

// Save writes credentials.json through the platform secret-storage boundary.
func (c *Credentials) Save(cfgDir string) error {
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return secrets.WriteFile(CredentialsPath(cfgDir), b)
}

// Get returns the secret stored under id, or "".
func (c *Credentials) Get(id string) string { return c.Secrets[id] }

// Set stores secret under id.
func (c *Credentials) Set(id, secret string) {
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	c.Secrets[id] = secret
}

// ResolveSecret returns the API key for a provider per its auth config:
// an env-var reference takes priority over a stored secret. Empty when the
// provider needs no key or none is configured.
func ResolveSecret(cfgDir, providerID string, auth Auth) string {
	return ResolveSecretWith(LoadCredentials, cfgDir, providerID, auth)
}

// ResolveSecretWith is ResolveSecret with the credentials reader chosen by the
// caller (LoadCredentials, or PeekCredentials for a read-only audit).
func ResolveSecretWith(load func(string) (*Credentials, error), cfgDir, providerID string, auth Auth) string {
	if auth.KeyEnv != "" {
		if v := os.Getenv(auth.KeyEnv); v != "" {
			return v
		}
	}
	if auth.Stored {
		if creds, err := load(cfgDir); err == nil {
			return creds.Get(providerID)
		}
	}
	return ""
}
