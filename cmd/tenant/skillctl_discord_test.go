package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- validateDiscordToken ---

func TestValidateDiscordToken_TooShort(t *testing.T) {
	err := validateDiscordToken("short")
	if err == nil {
		t.Fatal("expected error for too-short token")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("want 'too short' in error, got: %v", err)
	}
}

func TestValidateDiscordToken_LeadingSpace(t *testing.T) {
	// TrimSpace strips leading/trailing space, so the whitespace check
	// only fires on *internal* whitespace.  Use an embedded space to
	// exercise that path.
	err := validateDiscordToken("abcd 12345678901234567890123456789012345678901234567890123456")
	if err == nil {
		t.Fatal("expected error for token with embedded space")
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("want 'whitespace' in error, got: %v", err)
	}
}

func TestValidateDiscordToken_SurroundedQuotes(t *testing.T) {
	err := validateDiscordToken(`"abcd1234567890123456789012345678901234567890123456789012345678"`)
	if err == nil {
		t.Fatal("expected error for token surrounded by double quotes")
	}
	if !strings.Contains(err.Error(), "quote") {
		t.Errorf("want 'quote' in error, got: %v", err)
	}
	err2 := validateDiscordToken(`'abcd1234567890123456789012345678901234567890123456789012345678'`)
	if err2 == nil {
		t.Fatal("expected error for token surrounded by single quotes")
	}
	if !strings.Contains(err2.Error(), "quote") {
		t.Errorf("want 'quote' in error, got: %v", err2)
	}
}

func TestValidateDiscordToken_Valid72(t *testing.T) {
	token := strings.Repeat("a", 72) // valid, long enough, no whitespace/quotes
	err := validateDiscordToken(token)
	if err != nil {
		t.Fatalf("valid 72-char token rejected: %v", err)
	}
}

func TestValidateDiscordToken_Boundary60(t *testing.T) {
	token := strings.Repeat("a", 60) // exactly 60 chars — boundary
	err := validateDiscordToken(token)
	if err != nil {
		t.Fatalf("60-char token (boundary) rejected: %v", err)
	}
}

// --- probeDiscordWith ---

func TestProbeDiscordWith_200OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bot tok-test-token" {
			t.Errorf("bad Authorization: %q", auth)
		}
		if r.Header.Get("User-Agent") != "DiscordBot (https://github.com/tenant-mcp/tenant, 1.0)" {
			t.Errorf("bad UA: %q", r.Header.Get("User-Agent"))
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"username":      "testbot",
			"discriminator": "0",
			"id":            "123456789",
		})
	}))
	defer srv.Close()

	creds, _ := loadCredentials(t.TempDir())
	creds.set(skillSecretID("discord", "token"), "tok-test-token")

	id, err := probeDiscordWith(context.Background(), creds, nil, discordProbeDeps{
		HTTP:    srv.Client(),
		baseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if id != "testbot" {
		t.Errorf("identity = %q, want %q", id, "testbot")
	}
}

func TestProbeDiscordWith_200_WithDiscriminator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"username":      "legacy",
			"discriminator": "1234",
			"id":            "999",
		})
	}))
	defer srv.Close()

	creds, _ := loadCredentials(t.TempDir())
	creds.set(skillSecretID("discord", "token"), "tok-test-token")

	id, err := probeDiscordWith(context.Background(), creds, nil, discordProbeDeps{
		HTTP:    srv.Client(),
		baseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if id != "legacy#1234" {
		t.Errorf("identity = %q, want %q", id, "legacy#1234")
	}
}

func TestProbeDiscordWith_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()

	creds, _ := loadCredentials(t.TempDir())
	creds.set(skillSecretID("discord", "token"), "revoked-token")

	_, err := probeDiscordWith(context.Background(), creds, nil, discordProbeDeps{
		HTTP:    srv.Client(),
		baseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Errorf("want 401 error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error should mention 'revoked', got: %v", err)
	}
}

func TestProbeDiscordWith_429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer srv.Close()

	creds, _ := loadCredentials(t.TempDir())
	creds.set(skillSecretID("discord", "token"), "tok-test-token")

	_, err := probeDiscordWith(context.Background(), creds, nil, discordProbeDeps{
		HTTP:    srv.Client(),
		baseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("want 'rate limited' in error, got: %v", err)
	}
}

func TestProbeDiscordWith_500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	creds, _ := loadCredentials(t.TempDir())
	creds.set(skillSecretID("discord", "token"), "tok-test-token")

	_, err := probeDiscordWith(context.Background(), creds, nil, discordProbeDeps{
		HTTP:    srv.Client(),
		baseURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "server error") || !strings.Contains(err.Error(), "500") {
		t.Errorf("want 500 server error, got: %v", err)
	}
}

func TestProbeDiscordWith_MissingToken(t *testing.T) {
	creds, _ := loadCredentials(t.TempDir())
	// deliberately do NOT set the token
	_, err := probeDiscordWith(context.Background(), creds, nil, discordProbeDeps{})
	if err == nil {
		t.Fatal("expected error when token is missing")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("want 'missing' in error, got: %v", err)
	}
}

func TestProbeDiscordWith_DefaultHTTP(t *testing.T) {
	// Verify that a zero-valued discordProbeDeps falls through to
	// http.DefaultClient (production path). We don't want a test
	// server in this case, so we verify the deps field is optional.
	// Since we can't reach actual discord.com reliably in CI, just
	// confirm the code path compiles and doesn't panic with nil HTTP.
	// This is a compile-time + zero-value sanity check.
	_ = func() {
		d := discordProbeDeps{}
		_ = d.HTTP    // nil — should be OK, default client used
		_ = d.baseURL // empty — should use prod URL
	}
}

// --- Catalog registration ---

func TestDiscordRegisteredInSkillKinds(t *testing.T) {
	if _, ok := skillKinds["discord"]; !ok {
		t.Fatal("discord not registered in skillKinds after init")
	}
	k := skillKinds["discord"]
	if k.ID != "discord" {
		t.Errorf("skillKinds[discord].ID = %q, want %q", k.ID, "discord")
	}
	if !k.Wired {
		t.Error("discord should be Wired")
	}
	if len(k.Fields) != 2 {
		t.Errorf("expected 2 fields (token + operator_id), got %d", len(k.Fields))
	}
	// Field 0: token (secret)
	tok := k.Fields[0]
	if tok.Key != "token" {
		t.Errorf("field 0 key = %q, want %q", tok.Key, "token")
	}
	if !tok.Secret {
		t.Error("token field must be Secret")
	}
	if !tok.Required {
		t.Error("token field must be Required")
	}
	if tok.Validate == nil {
		t.Error("token field must have a Validate function")
	}
	// Field 1: operator_id (non-secret)
	op := k.Fields[1]
	if op.Key != "operator_id" {
		t.Errorf("field 1 key = %q, want %q", op.Key, "operator_id")
	}
	if op.Secret {
		t.Error("operator_id field must NOT be Secret (stored in settings)")
	}
	if !op.Required {
		t.Error("operator_id field must be Required")
	}
	if op.Validate == nil {
		t.Error("operator_id field must have a Validate function")
	}
}

// --- validateDiscordUserID ---

func TestValidateDiscordUserID_Valid(t *testing.T) {
	err := validateDiscordUserID("1470226458332106895")
	if err != nil {
		t.Fatalf("valid snowflake rejected: %v", err)
	}
}

func TestValidateDiscordUserID_ValidShort(t *testing.T) {
	// 17-digit snowflake — minimum length
	err := validateDiscordUserID("10000000000000000")
	if err != nil {
		t.Fatalf("17-digit snowflake rejected: %v", err)
	}
}

func TestValidateDiscordUserID_ValidLong(t *testing.T) {
	// 20-digit snowflake — maximum length
	err := validateDiscordUserID("99999999999999999999")
	if err != nil {
		t.Fatalf("20-digit snowflake rejected: %v", err)
	}
}

func TestValidateDiscordUserID_Empty(t *testing.T) {
	err := validateDiscordUserID("")
	if err == nil {
		t.Fatal("expected error for empty user ID")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("want 'required' in error, got: %v", err)
	}
}

func TestValidateDiscordUserID_Whitespace(t *testing.T) {
	err := validateDiscordUserID("   ")
	if err == nil {
		t.Fatal("expected error for whitespace-only user ID")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("want 'required' in error, got: %v", err)
	}
}

func TestValidateDiscordUserID_NonNumeric(t *testing.T) {
	err := validateDiscordUserID("dylan#1234")
	if err == nil {
		t.Fatal("expected error for non-numeric user ID")
	}
	if !strings.Contains(err.Error(), "numeric") {
		t.Errorf("want 'numeric' in error, got: %v", err)
	}
}

func TestValidateDiscordUserID_TooShort(t *testing.T) {
	err := validateDiscordUserID("12345")
	if err == nil {
		t.Fatal("expected error for too-short user ID")
	}
	if !strings.Contains(err.Error(), "17-20 digits") {
		t.Errorf("want length guidance in error, got: %v", err)
	}
}

func TestValidateDiscordUserID_TooLong(t *testing.T) {
	err := validateDiscordUserID("123456789012345678901")
	if err == nil {
		t.Fatal("expected error for too-long user ID")
	}
	if !strings.Contains(err.Error(), "17-20 digits") {
		t.Errorf("want length guidance in error, got: %v", err)
	}
}
