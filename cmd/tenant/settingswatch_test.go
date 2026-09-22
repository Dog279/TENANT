package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A hand edit to settings.<agent>.json is applied to the live rules and the
// host broker's modes; the process's own state (already equal) is a no-op.
func TestApplySettingsFile_HandEditsApplyLive(t *testing.T) {
	cfg := t.TempDir()
	rules := newToolRules()
	broker := newApprovalBroker(nil)
	broker.rules = rules
	var mu sync.Mutex
	stg := &settings{Tools: map[string]bool{}}

	// Nothing on disk yet → nothing to apply.
	if got := applySettingsFile(cfg, "main", broker, rules, &mu, stg); got != "" {
		t.Fatalf("empty file should change nothing, got %q", got)
	}

	body := `{"tools":{},"permissions":{"exec":"allow","bogus":"allow"},"tool_rules":{"delete_*":"deny","write_note":"allow","bad one":"allow"}}`
	if err := os.WriteFile(filepath.Join(cfg, "settings.main.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := applySettingsFile(cfg, "main", broker, rules, &mu, stg)
	if got != "2 tool rule(s) (1 invalid skipped), 1 category mode(s)" {
		t.Fatalf("summary = %q", got)
	}
	if m, _, _ := rules.Lookup("mcp:h:1:delete_x"); m != "deny" {
		t.Error("rule from the file should be live")
	}
	if broker.modesPublic()["exec"] != "allow" {
		t.Error("category mode from the file should be live")
	}
	if stg.ToolRules["write_note"] != "allow" || stg.Permissions["exec"] != "allow" {
		t.Errorf("the live settings object must track the file, got rules=%v perms=%v", stg.ToolRules, stg.Permissions)
	}
	// Same content again → no change reported (our own saves never re-trigger).
	if got := applySettingsFile(cfg, "main", broker, rules, &mu, stg); got != "" {
		t.Errorf("unchanged file should be a no-op, got %q", got)
	}
	// A corrupt file is reported, not applied.
	_ = os.WriteFile(filepath.Join(cfg, "settings.main.json"), []byte("{not json"), 0o644)
	if got := applySettingsFile(cfg, "main", broker, rules, &mu, stg); got == "" || rules.Mode("mcp:h:1:delete_x") != "deny" {
		t.Errorf("corrupt file: got %q, rules intact=%v", got, rules.Mode("mcp:h:1:delete_x") == "deny")
	}
}
