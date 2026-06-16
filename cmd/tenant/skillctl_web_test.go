package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateBraveKey(t *testing.T) {
	if err := validateBraveKey(""); err != nil {
		t.Errorf("empty key is optional → valid, got %v", err)
	}
	if err := validateBraveKey("   "); err != nil {
		t.Errorf("blank key → valid (trimmed empty), got %v", err)
	}
	if err := validateBraveKey("short"); err == nil {
		t.Error("too-short key should error")
	}
	if err := validateBraveKey("BSA with spaces in it xxxxxxxx"); err == nil {
		t.Error("whitespace key should error")
	}
	if err := validateBraveKey("BSA1234567890abcdefghijklmnop"); err != nil {
		t.Errorf("valid 29-char key should pass, got %v", err)
	}
}

func TestProbeWebBrave(t *testing.T) {
	ctx := context.Background()

	// No key → non-error "fallback active" identity (the only skill where
	// probe-success-with-no-config is valid).
	noKey, _ := loadCredentials(t.TempDir())
	id, err := probeWebBraveWith(ctx, noKey, webProbeDeps{})
	if err != nil || !strings.Contains(id, "DuckDuckGo") {
		t.Fatalf("no-key probe should report fallback, got id=%q err=%v", id, err)
	}

	withKey := func() *credentials {
		c, _ := loadCredentials(t.TempDir())
		c.set(skillSecretID("web", "brave_key"), "BSA1234567890abcdefghijklmnop")
		return c
	}
	cases := []struct {
		code      int
		wantErr   bool
		errSubstr string
	}{
		{200, false, ""},
		{401, true, "rejected"},
		{429, true, "quota"},
		{500, true, "HTTP 500"},
	}
	for _, tc := range cases {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Subscription-Token") == "" {
				t.Error("probe must send the X-Subscription-Token header")
			}
			w.WriteHeader(tc.code)
		}))
		id, err := probeWebBraveWith(ctx, withKey(), webProbeDeps{HTTP: ts.Client(), baseURL: ts.URL})
		ts.Close()
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("HTTP %d: want err containing %q, got id=%q err=%v", tc.code, tc.errSubstr, id, err)
			}
		} else if err != nil || !strings.Contains(id, "valid") {
			t.Errorf("HTTP 200: want valid identity, got id=%q err=%v", id, err)
		}
	}
}

// TEN-69: braveKey() bridges the /skill configure web path — a key stored under
// the skill namespace upgrades web_search even though the cred id differs.
func TestBraveKey_HonorsSkillSecret(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	t.Setenv("BRAVE_API_KEY", "")
	if got := braveKey(dir); got != "" {
		t.Fatalf("no key anywhere → empty, got %q", got)
	}
	c, _ := loadCredentials(dir)
	c.set(skillSecretID("web", "brave_key"), "BSA-skill-configured-key-xxxxxx")
	if err := c.save(dir); err != nil {
		t.Fatal(err)
	}
	if got := braveKey(dir); got != "BSA-skill-configured-key-xxxxxx" {
		t.Fatalf("braveKey should read the skill-configured key, got %q", got)
	}
}
