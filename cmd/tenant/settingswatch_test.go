package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tenant/internal/tui"
)

// A hand edit to settings.<agent>.json is applied to the live rules and the
// host broker's modes; the process's own state (already equal) is a no-op.
func TestApplySettingsFile_HandEditsApplyLive(t *testing.T) {
	cfg := t.TempDir()
	rules := newToolRules()
	broker := newApprovalBroker(nil)
	broker.SetRules(rules)
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
	if broker.Modes()["exec"] != "allow" {
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

// notADir returns a path under which nothing can be written on any OS: a
// regular file standing where a config directory should be.
func notADir(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// "Approve always" on a prompt a tool rule forced saves an allow rule. When the
// settings file cannot be written the operator is told — the rule is live, but a
// restart would lose it — instead of believing it was saved.
func TestApproveAlways_SaveFailureTellsOperator(t *testing.T) {
	cfgDir := notADir(t)
	var told []string
	stg := &settings{}
	rules := newToolRules()
	rules.Load(map[string]string{"write_note": "ask"})
	rules.SetPersist(func(r map[string]string) {
		stg.ToolRules = r
		saveSettingsOrTell(stg, cfgDir, "main", testLog(), func(s string) { told = append(told, s) })
	})
	broker := newApprovalBroker(nil)
	broker.SetRules(rules)
	go func() {
		req := <-broker.Requests()
		broker.Resolve(req.ID, tui.ApproveAlways)
	}()
	if !broker.Confirm(context.Background(), "write_note", "a note") {
		t.Fatal("approve always must let the action run")
	}
	if len(told) != 1 || !strings.Contains(told[0], "not saved") {
		t.Fatalf("the operator was told %q, want one not-saved notice", told)
	}
	if m, _, _ := rules.Lookup("write_note"); m != "allow" {
		t.Errorf("the rule is still live until restart, got %q", m)
	}
}
