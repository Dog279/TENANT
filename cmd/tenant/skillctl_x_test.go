package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateXBearer(t *testing.T) {
	long := strings.Repeat("A", 90)
	if err := validateXBearer("short"); err == nil {
		t.Error("too-short bearer should error")
	}
	if err := validateXBearer(strings.Repeat("A", 40) + " " + strings.Repeat("B", 40)); err == nil {
		t.Error("whitespace bearer should error")
	}
	if err := validateXBearer(strings.Repeat("A", 79) + "!"); err == nil {
		t.Error("invalid character should error")
	}
	if err := validateXBearer(strings.Repeat("A", 80)); err != nil {
		t.Errorf("exactly 80 valid chars should pass, got %v", err)
	}
	if err := validateXBearer(long + "-_%."); err != nil {
		t.Errorf("URL-safe base64 + %%/. should pass, got %v", err)
	}
}

func TestResolveXBearer(t *testing.T) {
	// flag wins over env + creds.
	t.Setenv("X_BEARER_TOKEN", "env-tok")
	dir := t.TempDir()
	c, _ := loadCredentials(dir)
	c.set(skillSecretID("x", "bearer"), "creds-tok")
	if err := c.save(dir); err != nil {
		t.Fatal(err)
	}
	if got := resolveXBearer(dir, "  flag-tok  "); got != "flag-tok" {
		t.Errorf("flag should win (trimmed), got %q", got)
	}
	// no flag → env wins over creds.
	if got := resolveXBearer(dir, ""); got != "env-tok" {
		t.Errorf("env should win over creds, got %q", got)
	}
	// no flag, no env → creds.
	t.Setenv("X_BEARER_TOKEN", "")
	if got := resolveXBearer(dir, ""); got != "creds-tok" {
		t.Errorf("creds should be the last resort, got %q", got)
	}
	// nothing anywhere → empty.
	if got := resolveXBearer(t.TempDir(), ""); got != "" {
		t.Errorf("no source → empty, got %q", got)
	}
}

func TestProbeX(t *testing.T) {
	ctx := context.Background()
	withBearer := func() *credentials {
		c, _ := loadCredentials(t.TempDir())
		c.set(skillSecretID("x", "bearer"), strings.Repeat("A", 90))
		return c
	}

	// Missing bearer → clear error.
	noKey, _ := loadCredentials(t.TempDir())
	if _, err := probeXWith(ctx, noKey, xProbeDeps{}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("no bearer should error, got %v", err)
	}

	// 200 → @username.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("probe must send a Bearer auth header")
		}
		w.Write([]byte(`{"data":{"username":"jack","name":"Jack"}}`))
	}))
	id, err := probeXWith(ctx, withBearer(), xProbeDeps{HTTP: ts.Client(), baseURL: ts.URL})
	ts.Close()
	if err != nil || id != "@jack" {
		t.Fatalf("200 should yield @jack, got id=%q err=%v", id, err)
	}

	// Error status codes → distinct surfaces.
	for _, tc := range []struct {
		code      int
		errSubstr string
	}{
		{401, "revoked"},
		{403, "permissions"},
		{429, "rate limited"},
		{503, "server error"},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.code) }))
		_, err := probeXWith(ctx, withBearer(), xProbeDeps{HTTP: ts.Client(), baseURL: ts.URL})
		ts.Close()
		if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
			t.Errorf("HTTP %d: want err containing %q, got %v", tc.code, tc.errSubstr, err)
		}
	}
}
