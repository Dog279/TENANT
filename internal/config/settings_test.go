package config

import (
	"os"
	"testing"
)

func TestSettings_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Missing file => empty, no error (first run).
	s, err := LoadSettings(dir, "main")
	if err != nil || len(s.Tools) != 0 {
		t.Fatalf("first-run load: s=%+v err=%v", s, err)
	}

	s.Tools = map[string]bool{"web_navigate": true, "os_exec": false}
	if err := s.Save(dir, "main"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadSettings(dir, "main")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.Tools["web_navigate"] || got.Tools["os_exec"] {
		t.Fatalf("round-trip wrong: %+v", got.Tools)
	}

	// Per-agent isolation: a different agent has its own file.
	if other, _ := LoadSettings(dir, "other"); len(other.Tools) != 0 {
		t.Fatalf("agent isolation broken: %+v", other.Tools)
	}

	// Corrupt file => error surfaced, defaults returned (not a wipe).
	if err := os.WriteFile(SettingsPath(dir, "main"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, cerr := LoadSettings(dir, "main"); cerr == nil || len(c.Tools) != 0 {
		t.Fatalf("corrupt should error + empty: c=%+v err=%v", c, cerr)
	}
}
