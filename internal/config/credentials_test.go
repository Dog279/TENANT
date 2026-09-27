package config

import (
	"testing"

	"tenant/internal/secrets"
)

func TestCredentials_RoundTripAndResolve(t *testing.T) {
	dir := t.TempDir()
	c, _ := LoadCredentials(dir)
	c.Set("openai", "sk-secret")
	if err := c.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	if report := secrets.Probe(CredentialsPath(dir)); !report.Protected {
		t.Fatalf("credentials are not protected: %+v", report)
	}

	// Stored secret resolves.
	if got := ResolveSecret(dir, "openai", Auth{Stored: true}); got != "sk-secret" {
		t.Fatalf("stored secret = %q, want sk-secret", got)
	}
	// Env reference takes priority over stored.
	t.Setenv("MY_KEY", "env-secret")
	if got := ResolveSecret(dir, "openai", Auth{KeyEnv: "MY_KEY", Stored: true}); got != "env-secret" {
		t.Fatalf("env ref = %q, want env-secret", got)
	}
	// Env var set but empty falls through to stored secret.
	t.Setenv("EMPTY_KEY", "")
	if got := ResolveSecret(dir, "openai", Auth{KeyEnv: "EMPTY_KEY", Stored: true}); got != "sk-secret" {
		t.Fatalf("empty env fallthrough = %q, want sk-secret", got)
	}
	// Env var set to nonexistent var falls through to stored secret.
	if got := ResolveSecret(dir, "openai", Auth{KeyEnv: "MISSING_KEY", Stored: true}); got != "sk-secret" {
		t.Fatalf("missing env fallthrough = %q, want sk-secret", got)
	}
	// No auth → empty.
	if got := ResolveSecret(dir, "openai", Auth{}); got != "" {
		t.Fatalf("no-auth secret = %q, want empty", got)
	}
}
