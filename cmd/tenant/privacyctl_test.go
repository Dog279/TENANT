package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	xp "tenant/internal/plugins/x"
	"tenant/internal/secrets"
)

func privacyArgs(cfg, data string, rest ...string) []string {
	return append([]string{"--config", cfg, "--data", data}, rest...)
}

func TestPrivacyStatusShowsEveryClass(t *testing.T) {
	cfg, data := seedInstall(t)
	var out bytes.Buffer
	if err := cmdPrivacyStatus(context.Background(), privacyArgs(cfg, data), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"episodes", "forever", "relay_inbox", "30 days", "runs", "90 days", "route_log", "logs", "1 turns", "tenant privacy forget"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
}

func TestPrivacyRetentionSet(t *testing.T) {
	cfg, data := seedInstall(t)
	ctx := context.Background()
	set := func(class, value string) error {
		return cmdPrivacyRetention(ctx, append([]string{"set", class, value}, privacyArgs(cfg, data)...), io.Discard)
	}
	for _, c := range [][2]string{{"episodes", "30"}, {"relay_inbox", "forever"}, {"runs", "forever"}, {"logs", "7"}, {"route_log", "10d"}} {
		if err := set(c[0], c[1]); err != nil {
			t.Fatalf("set %s %s: %v", c[0], c[1], err)
		}
	}
	lc, err := loadLaunchConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if lc.Privacy.RetentionDays["episodes"] != 30 || lc.Privacy.RetentionDays["relay_inbox"] != -1 ||
		lc.Runs.MaxAgeDays != -1 || lc.Logs.RetentionDays != 7 || lc.Route == nil || lc.Route.LogRetentionDays != 10 {
		t.Fatalf("config after set: privacy=%v runs=%d logs=%d route=%+v", lc.Privacy, lc.Runs.MaxAgeDays, lc.Logs.RetentionDays, lc.Route)
	}
	if got := relayInboxKeep(cfg); got != 0 {
		t.Fatalf("relay inbox set to forever keeps %v", got)
	}
	if err := set("episodes", "forever"); err != nil {
		t.Fatal(err)
	}
	if lc, _ := loadLaunchConfig(cfg); lc.Privacy.RetentionDays["episodes"] != 0 {
		t.Fatal("forever did not clear the episodes period")
	}
	for _, bad := range [][2]string{{"episodes", "-3"}, {"episodes", "soon"}, {"secrets", "30"}, {"route_log", "4000"}} {
		if err := set(bad[0], bad[1]); err == nil {
			t.Errorf("set %s %s accepted", bad[0], bad[1])
		}
	}
}

func TestRelayInboxKeepDefaults(t *testing.T) {
	cfg := t.TempDir()
	if got := relayInboxKeep(cfg); got != relayInboxRetention {
		t.Fatalf("default relay retention = %v, want %v", got, relayInboxRetention)
	}
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"schema_version":2,"privacy":{"retention_days":{"relay_inbox":3}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := relayInboxKeep(cfg); got != 3*24*time.Hour {
		t.Fatalf("configured relay retention = %v", got)
	}
}

func TestPrivacyForgetAsksThenForgets(t *testing.T) {
	cfg, data := seedInstall(t)
	ctx := context.Background()
	var out bytes.Buffer
	err := cmdPrivacyForget(ctx, privacyArgs(cfg, data, "--match", "prefer go"), strings.NewReader("n\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("declined forget: %v", err)
	}
	if !strings.Contains(out.String(), "episodes (conversation turns)") || !strings.Contains(out.String(), "remember I prefer Go") {
		t.Fatalf("preview:\n%s", out.String())
	}
	countEps := func() int {
		st, err := episodic.OpenExisting(filepath.Join(data, "episodes.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		n, err := st.Count(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if countEps() != 1 {
		t.Fatal("a declined forget deleted something")
	}
	out.Reset()
	if err := cmdPrivacyForget(ctx, privacyArgs(cfg, data, "--match", "prefer go", "--yes"), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if countEps() != 0 {
		t.Fatalf("forget --yes left the episode:\n%s", out.String())
	}
	out.Reset()
	if err := cmdPrivacyForget(ctx, privacyArgs(cfg, data, "--match", "prefer go", "--yes"), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nothing matches") {
		t.Fatalf("second forget:\n%s", out.String())
	}
	if err := cmdPrivacyForget(ctx, privacyArgs(cfg, data), strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("a forget with no selector ran")
	}
}

func TestPrivacyForgetConnector(t *testing.T) {
	cfg, data := seedInstall(t)
	ctx := context.Background()
	creds, err := loadCredentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	creds.Set(skillSecretID("x", "bearer"), "bearer-value")
	if err := creds.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := secrets.WriteFile(xp.TokenPath(data), []byte(`{"access_token":"t"}`)); err != nil {
		t.Fatal(err)
	}
	conns := storedConnectors(cfg, data)
	if len(conns) != 1 || conns[0].Name != "x" {
		t.Fatalf("stored connectors = %+v", conns)
	}
	for _, s := range conns[0].Stored {
		if strings.Contains(s, "bearer-value") {
			t.Fatal("a secret value leaked into the connector summary")
		}
	}
	var out bytes.Buffer
	if err := cmdPrivacyForget(ctx, privacyArgs(cfg, data, "--connector", "x", "--yes"), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(xp.TokenPath(data)); !os.IsNotExist(err) {
		t.Fatal("the X token cache survived")
	}
	after, err := loadCredentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if after.Get(skillSecretID("x", "bearer")) != "" || after.Get("openai") != "sk-original" {
		t.Fatalf("credentials after forgetting x: %v", after.Secrets)
	}
	if err := cmdPrivacyForget(ctx, privacyArgs(cfg, data, "--connector", "myspace"), strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("an unknown connector was accepted")
	}
}

func TestPrivacySessionsAndExport(t *testing.T) {
	cfg, data := seedInstall(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := cmdPrivacySessions(ctx, privacyArgs(cfg, data), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No sessions yet") {
		// The seeded episode has no session; the listing only shows sessions.
		t.Fatalf("sessions:\n%s", out.String())
	}
	dir := filepath.Join(t.TempDir(), "export")
	out.Reset()
	if err := cmdPrivacyExport(ctx, privacyArgs(cfg, data, "--out", dir), &out); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "episodes.jsonl")); err != nil || !strings.Contains(string(b), "remember I prefer Go") {
		t.Fatalf("export episodes: %s %v", b, err)
	}
	if err := cmdPrivacyExport(ctx, privacyArgs(cfg, data), io.Discard); err == nil {
		t.Fatal("export without --out ran")
	}
}

func TestRunRetentionAppliesConfiguredPeriods(t *testing.T) {
	cfg, data := seedInstall(t)
	shots := filepath.Join(data, "screenshots")
	if err := os.MkdirAll(shots, 0o700); err != nil {
		t.Fatal(err)
	}
	old, fresh := filepath.Join(shots, "web-1.png"), filepath.Join(shots, "web-2.png")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	c := &commonFlags{cfgDir: cfg, dataDir: data}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runRetention(context.Background(), c, log) // nothing configured: nothing pruned
	if _, err := os.Stat(old); err != nil {
		t.Fatal("retention ran with no period configured")
	}
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"schema_version":2,"privacy":{"retention_days":{"screenshots":7}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runRetention(context.Background(), c, log)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("an old screenshot survived its retention")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a recent screenshot was pruned")
	}
}
