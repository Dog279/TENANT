package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/sme"
)

// seedInstall builds a small real install: the stores as Tenant opens them,
// a config, a soul and stored credentials.
func seedInstall(t *testing.T) (cfg, data string) {
	t.Helper()
	root := t.TempDir()
	cfg, data = filepath.Join(root, "config"), filepath.Join(root, "data")
	for _, d := range []string{cfg, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	ep, err := episodic.Open(filepath.Join(data, "episodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ep.Insert(ctx, &episodic.Episode{AgentID: "main", Prompt: "remember I prefer Go", Response: "noted", EmbedderID: "echo/4", Embedding: []float32{0.1, 0.2, 0.3, 0.4}}); err != nil {
		t.Fatal(err)
	}
	ep.Close()
	sem, err := semantic.Open(filepath.Join(data, "facts.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sme.New(sem.DB()); err != nil {
		t.Fatal(err)
	}
	sem.Close()
	sk, err := skills.Open(filepath.Join(data, "skills.db"))
	if err != nil {
		t.Fatal(err)
	}
	sk.Close()
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"schema_version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg, "soul"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "soul", "main.toml"), []byte("[meta]\nversion = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := &credentials{Secrets: map[string]string{"openai": "sk-original"}}
	if err := creds.save(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, data
}

func runBackup(t *testing.T, fn func() error) {
	t.Helper()
	if err := fn(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCommandsRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg, data := seedInstall(t)
	outDir := t.TempDir()
	var out bytes.Buffer
	runBackup(t, func() error {
		return cmdBackupCreate(ctx, []string{"--config", cfg, "--data", data, "--out", outDir}, &out)
	})
	if !strings.Contains(out.String(), "config/credentials.json: secret") {
		t.Fatalf("create output does not say the credentials were left out:\n%s", out.String())
	}
	files, _ := filepath.Glob(filepath.Join(outDir, "tenant-backup-*.tar.gz"))
	if len(files) != 1 {
		t.Fatalf("backup files = %v", files)
	}
	file := files[0]

	out.Reset()
	runBackup(t, func() error { return cmdBackupVerify(ctx, []string{file}, &out) })
	if !strings.Contains(out.String(), "OK:") || strings.Contains(out.String(), "newer than this Tenant") {
		t.Fatalf("verify of a fresh backup:\n%s", out.String())
	}
	if strings.Contains(out.String(), "not a store this Tenant knows") {
		t.Fatalf("a real store was not recognized:\n%s", out.String())
	}

	// Change the install, then restore over it.
	if err := os.WriteFile(filepath.Join(cfg, "soul", "main.toml"), []byte("[meta]\nversion = 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := &credentials{Secrets: map[string]string{"openai": "sk-rotated"}}
	if err := creds.save(cfg); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	runBackup(t, func() error {
		return cmdBackupRestore(ctx, []string{file, "--config", cfg, "--data", data, "--yes"}, strings.NewReader(""), &out)
	})
	if b, _ := os.ReadFile(filepath.Join(cfg, "soul", "main.toml")); !strings.Contains(string(b), "version = 1") {
		t.Fatalf("soul not restored: %s", b)
	}
	got, err := loadCredentials(cfg)
	if err != nil || got.get("openai") != "sk-rotated" {
		t.Fatalf("a plain backup's restore must keep the live credentials: %v %v", got, err)
	}
	if !strings.Contains(out.String(), "previous directory is kept") {
		t.Fatalf("restore output:\n%s", out.String())
	}
	ep, err := episodic.OpenExisting(filepath.Join(data, "episodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	recent, err := ep.Recent(ctx, "main", 5)
	if err != nil || len(recent) != 1 || recent[0].Prompt != "remember I prefer Go" {
		t.Fatalf("restored episodes: %v %v", recent, err)
	}
}

func TestBackupRestoreAsksFirst(t *testing.T) {
	ctx := context.Background()
	cfg, data := seedInstall(t)
	file := filepath.Join(t.TempDir(), "b.tar.gz")
	runBackup(t, func() error {
		return cmdBackupCreate(ctx, []string{"--config", cfg, "--data", data, "--out", file}, &bytes.Buffer{})
	})
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"schema_version":2,"live":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := cmdBackupRestore(ctx, []string{file, "--config", cfg, "--data", data}, strings.NewReader("n\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("declined restore: err = %v", err)
	}
	if b, _ := os.ReadFile(launchConfigPath(cfg)); !strings.Contains(string(b), "live") {
		t.Fatal("a declined restore changed the config")
	}
}

func TestBackupRestoreRefusesWhileAHubAnswers(t *testing.T) {
	ctx := context.Background()
	cfg, data := seedInstall(t)
	file := filepath.Join(t.TempDir(), "b.tar.gz")
	runBackup(t, func() error {
		return cmdBackupCreate(ctx, []string{"--config", cfg, "--data", data, "--out", file}, &bytes.Buffer{})
	})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token required", http.StatusUnauthorized)
	}))
	defer hub.Close()
	addr := strings.TrimPrefix(hub.URL, "http://")
	if err := os.WriteFile(launchConfigPath(cfg), []byte(`{"schema_version":2,"dashboard":{"addr":"`+addr+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmdBackupRestore(ctx, []string{file, "--config", cfg, "--data", data, "--yes"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "hub answers") {
		t.Fatalf("restore under a live hub: err = %v", err)
	}
}

func TestBackupEncryptedWithPassphraseFile(t *testing.T) {
	ctx := context.Background()
	cfg, data := seedInstall(t)
	dir := t.TempDir()
	pass := filepath.Join(dir, "pass.txt")
	if err := os.WriteFile(pass, []byte("correct horse battery\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "b.tar.gz.age")
	var out bytes.Buffer
	runBackup(t, func() error {
		return cmdBackupCreate(ctx, []string{"--config", cfg, "--data", data, "--out", file, "--passphrase-file", pass}, &out)
	})
	if !strings.Contains(out.String(), "encrypted; ") {
		t.Fatalf("create output:\n%s", out.String())
	}
	prev := promptPassphrase
	promptPassphrase = func(io.Writer, bool) (string, error) { return "", errors.New("no terminal in tests") }
	defer func() { promptPassphrase = prev }()
	if err := cmdBackupVerify(ctx, []string{file}, &bytes.Buffer{}); err == nil {
		t.Fatal("verified an encrypted backup without its passphrase")
	}
	out.Reset()
	runBackup(t, func() error { return cmdBackupVerify(ctx, []string{file, "--passphrase-file", pass}, &out) })
	if !strings.Contains(out.String(), "encrypted") || !strings.Contains(out.String(), "OK:") {
		t.Fatalf("verify output:\n%s", out.String())
	}
	creds := &credentials{Secrets: map[string]string{"openai": "sk-rotated"}}
	if err := creds.save(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(passphraseEnv, "correct horse battery")
	runBackup(t, func() error {
		return cmdBackupRestore(ctx, []string{file, "--config", cfg, "--data", data, "--yes"}, strings.NewReader(""), &bytes.Buffer{})
	})
	got, err := loadCredentials(cfg)
	if err != nil || got.get("openai") != "sk-original" {
		t.Fatalf("an encrypted backup restores its own credentials: %v %v", got, err)
	}
}

func TestConfigDirFile(t *testing.T) {
	for rel, want := range map[string]bool{
		"config.json":                 true,
		"credentials.json":            true,
		"settings.main.json":          true,
		"soul/main.toml":              true,
		"soul/proposed/main-1-x.toml": true,
		"mcp/mcp-0123abcd.json":       true,
		"peers.json":                  true,
		"peer-key.pem":                true,
		"dashboard-auth-token":        true,
		"github-token.json":           true,
		"oauth_client.json":           true,
		"claudecode-usage.json":       true,
		"episodes.db":                 false,
		"archive/2026-09/s.jsonl":     false,
		"x-token.json":                false,
		"logs/settings.main.json":     false,
		"soulmate.txt":                false,
	} {
		if got := configDirFile(rel); got != want {
			t.Errorf("configDirFile(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestCheckBackupFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	write := func(s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"schema_version":99}`)
	if newer, _, err := checkBackupFile("config/config.json", p); err != nil || !strings.Contains(newer, "99") {
		t.Fatalf("newer config: %q %v", newer, err)
	}
	write(`{not json`)
	if _, note, err := checkBackupFile("config/config.json", p); err != nil || note == "" {
		t.Fatalf("corrupt config: note %q err %v", note, err)
	}
	write(`{"schema_version":2}`)
	if newer, note, err := checkBackupFile("config/config.json", p); newer != "" || note != "" || err != nil {
		t.Fatalf("current config flagged: %q %q %v", newer, note, err)
	}
	if newer, note, err := checkBackupFile("data/other.json", p); newer != "" || note != "" || err != nil {
		t.Fatal("an unknown file was checked")
	}
}
