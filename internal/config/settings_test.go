package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The acknowledgement of a missing process protection (TEN-535): it matches
// on the control, the reason and the version together; a file from before the
// key loads as it did and is written back without it; the list is bounded.
func TestSettings_ProtectionAcknowledged(t *testing.T) {
	dir := t.TempDir()
	old := `{"tools":{"os_exec":false},"permissions":{"exec":"ask"},"tool_rules":{"web_*":"deny"}}`
	if err := os.WriteFile(SettingsPath(dir, "main"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(dir, "main")
	if err != nil || len(s.ProtectionAcknowledged) != 0 || s.Permissions["exec"] != "ask" || s.ToolRules["web_*"] != "deny" {
		t.Fatalf("a file from before the key: %+v, %v", s, err)
	}
	if err := s.Save(dir, "main"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(SettingsPath(dir, "main")); strings.Contains(string(b), "protection_acknowledged") {
		t.Errorf("nothing acknowledged, yet the key was written:\n%s", b)
	}

	at := time.Date(2026, 11, 18, 19, 2, 11, 500, time.FixedZone("PST", -8*3600))
	s.Acknowledge("linux.dumpable", "operation not permitted", "1.4.0", at)
	s.Acknowledge("linux.dumpable", "operation not permitted", "1.4.0", at.Add(time.Hour)) // already there: keeps its time
	if err := s.Save(dir, "main"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(dir, "main")
	if err != nil || len(got.ProtectionAcknowledged) != 1 {
		t.Fatalf("reload: %+v, %v", got, err)
	}
	a, ok := got.Acknowledged("linux.dumpable", "operation not permitted", "1.4.0")
	if !ok || !a.At.Equal(at.Truncate(time.Second)) || a.At.Location() != time.UTC {
		t.Errorf("acknowledged = %+v, %v; want it, at %s in UTC", a, ok, at.UTC().Truncate(time.Second))
	}
	for name, q := range map[string][3]string{
		"another control": {"linux.core-limit", "operation not permitted", "1.4.0"},
		"another reason":  {"linux.dumpable", "permission denied", "1.4.0"},
		"another version": {"linux.dumpable", "operation not permitted", "1.4.1"},
	} {
		if _, ok := got.Acknowledged(q[0], q[1], q[2]); ok {
			t.Errorf("%s counts as acknowledged", name)
		}
	}
	if b, _ := os.ReadFile(SettingsPath(dir, "main")); !strings.Contains(string(b), `"control": "linux.dumpable"`) || !strings.Contains(string(b), `"at": "2026-11-19T03:02:11Z"`) {
		t.Errorf("the file:\n%s", b)
	}

	for i := range maxProtectionAcks + 3 {
		got.Acknowledge("linux.dumpable", "operation not permitted", fmt.Sprintf("2.0.%d", i), at)
	}
	if n := len(got.ProtectionAcknowledged); n != maxProtectionAcks {
		t.Errorf("%d kept, want the newest %d", n, maxProtectionAcks)
	}
	if _, ok := got.Acknowledged("linux.dumpable", "operation not permitted", "1.4.0"); ok {
		t.Error("the oldest entry was kept past the bound")
	}
	if _, ok := got.Acknowledged("linux.dumpable", "operation not permitted", fmt.Sprintf("2.0.%d", maxProtectionAcks+2)); !ok {
		t.Error("the newest entry was dropped")
	}
}

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
