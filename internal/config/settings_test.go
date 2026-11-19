package config

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
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

// A settings file from before the fleet loads, and saves again, without the
// fleet's keys. With them, each machine's entry comes back as it was written,
// whether or not it is a scheme: reading it is internal/approval's.
func TestSettings_FleetKeys(t *testing.T) {
	dir := t.TempDir()
	old := `{"tools":{"os_exec":true},"permissions":{"exec":"allow"},"tool_rules":{"delete_*":"deny"}}`
	if err := os.WriteFile(SettingsPath(dir, "main"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(dir, "main")
	if err != nil || s.FleetSchemes != nil || s.FleetRules != nil || s.Permissions["exec"] != "allow" || s.ToolRules["delete_*"] != "deny" {
		t.Fatalf("an old file: %+v, %v", s, err)
	}
	if err := s.Save(dir, "main"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(SettingsPath(dir, "main")); strings.Contains(string(b), "fleet") {
		t.Fatalf("saving an old file added the fleet's keys:\n%s", b)
	}

	withFleet := `{"tools":{},"tool_rules":{"delete_*":"deny"},
		"fleet_schemes":{"m1":{"permissions":{"exec":"deny"},"tool_rules":{"os_exec(df *)":"allow"}},"m2":"not a scheme","m3":null},
		"fleet_rules":["not","rules"]}`
	if err := os.WriteFile(SettingsPath(dir, "main"), []byte(withFleet), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = LoadSettings(dir, "main")
	if err != nil || len(s.FleetSchemes) != 3 || s.ToolRules["delete_*"] != "deny" {
		t.Fatalf("a file with the fleet's keys: %+v, %v", s, err)
	}
	if err := s.Save(dir, "main"); err != nil {
		t.Fatal(err)
	}
	again, err := LoadSettings(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	compact := func(raw json.RawMessage) string {
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for id, want := range map[string]string{
		"m1": `{"permissions":{"exec":"deny"},"tool_rules":{"os_exec(df *)":"allow"}}`,
		"m2": `"not a scheme"`,
		"m3": `null`,
	} {
		if got := compact(again.FleetSchemes[id]); got != want {
			t.Errorf("%s came back as %s, want %s", id, got, want)
		}
	}
	if got := compact(again.FleetRules); got != `["not","rules"]` {
		t.Errorf("fleet_rules came back as %s", got)
	}
}
