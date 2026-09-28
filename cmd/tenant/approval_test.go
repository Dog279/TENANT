package main

import (
	"io"
	"log/slog"
	"testing"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCategorize(t *testing.T) {
	cases := map[string]string{
		"os_exec":           catExec,
		"os_exec_dangerous": catDestructive,
		"sql_ddl":           catDestructive,
		"web_transact":      catDestructive,
		"web_interact":      catWeb,
		"gsuite_send":       catSend,
		"imessage_send":     catSend,
		"x_post":            catSend,
		"sensitive_egress":  catEgress,
		"something_unknown": catDestructive, // safest default
		// Every class is declared by its plugin (TEN-321): a tool name is not an
		// action id, and no prefix guesses a category.
		"web_click":    catDestructive,
		"jira_write":   catDestructive,
		"github_write": catDestructive,
		"wiki_save":    catDestructive,
		"crm_ask":      catDestructive,
		"mcp_add":      catMCP,
	}
	for action, want := range cases {
		if got := categorize(action); got != want {
			t.Errorf("categorize(%q)=%q want %q", action, got, want)
		}
	}
}

// The launch allow-flags pre-approve their categories through the plugin
// registry; persisted modes then override them.
func TestBroker_SeedAndLoadModes(t *testing.T) {
	b := newApprovalBroker(testLog())
	seedFromFlags(b, testPluginFlags(func(pf *pluginFlags) { osFlagsOf(pf).allowExec = true; gsuiteFlagsOf(pf).allowSend = true }))
	modes := b.Modes()
	if modes[catExec] != "allow" || modes[catSend] != "allow" {
		t.Fatal("flags should seed allow")
	}
	if modes[catDestructive] != "ask" {
		t.Fatal("destructive must never be flag-seeded to allow")
	}
	// Persisted modes override flags.
	b.LoadModes(map[string]string{"exec": "deny", "web": "allow", "bogus": "allow"}, nil)
	modes = b.Modes()
	if modes[catExec] != "deny" || modes[catWeb] != "allow" {
		t.Fatal("LoadModes should override")
	}
	if _, ok := modes["bogus"]; ok {
		t.Fatal("unknown categories must be ignored")
	}
}
