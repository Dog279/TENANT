package main

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/plugins/osys"

	"tenant/internal/plugins/discord"
)

// The exec auto-approver must hard-deny irreversible actions and writes into the
// config/data dirs (anti self-replication), while allowing benign ones.
func TestCronExecApprover(t *testing.T) {
	ap := cronExecApprover("/cfgdir", "/datadir")
	ctx := context.Background()
	deny := []struct{ action, detail string }{
		{"os_exec_dangerous", "rm -rf /  [DANGER: recursive force remove]"},
		{"sql_ddl", "DROP TABLE x"},
		{"web_transact", "purchase"},
		{"os_write", "OVERWRITE existing file /cfgdir/config.json"},
		{"os_write", "write /datadir/cron-history.json"},
	}
	for _, d := range deny {
		if ap(ctx, d.action, d.detail) {
			t.Errorf("approver must DENY %s %q", d.action, d.detail)
		}
	}
	allow := []struct{ action, detail string }{
		{"os_exec", "ls -la"},
		{"os_write", "write /tmp/report.txt"},
		{"gsuite_send", "email to boss"},
	}
	for _, a := range allow {
		if !ap(ctx, a.action, a.detail) {
			t.Errorf("approver must ALLOW %s %q", a.action, a.detail)
		}
	}
}

func TestScrubbedEnv(t *testing.T) {
	t.Setenv("CRON_TEST_API_KEY", "secret")
	t.Setenv("CRON_TEST_AUTH_TOKEN", "secret")
	t.Setenv("CRON_TEST_PLAIN", "ok")
	env := scrubbedEnv()
	var sawKey, sawToken, sawPlain bool
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "CRON_TEST_API_KEY="):
			sawKey = true
		case strings.HasPrefix(kv, "CRON_TEST_AUTH_TOKEN="):
			sawToken = true
		case strings.HasPrefix(kv, "CRON_TEST_PLAIN="):
			sawPlain = true
		}
	}
	if sawKey || sawToken {
		t.Error("secret-bearing vars must be scrubbed from the shell env")
	}
	if !sawPlain {
		t.Error("non-secret vars must survive")
	}
}

func TestRunCronShellBenign(t *testing.T) {
	out, err := runCronShell(context.Background(), "echo hello-cron", t.TempDir())
	if err != nil {
		t.Fatalf("benign shell command errored: %v", err)
	}
	if !strings.Contains(out, "hello-cron") {
		t.Errorf("expected captured output, got %q", out)
	}
}

func TestRunCronShellRefusesCatastrophic(t *testing.T) {
	// Echo-prefixed so it is INERT even if the classifier (or refusal) fails —
	// the test never risks running a destructive command.
	probe := "echo rm -rf /tmp/cron-probe-not-a-real-path"
	if dangerous, _ := osys.Classify(probe); !dangerous {
		t.Skip("classifier did not flag the probe; refusal path is covered by osys tests")
	}
	_, err := runCronShell(context.Background(), probe, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a classifier-flagged command must be refused, got err=%v", err)
	}
}

// The exec approver's deny-by-default swallowed two delivery-critical families
// because categorize maps unknown action ids to destructive: remote-MCP READS
// ("label:tool" ids the table has never heard of) and Discord delivery (prose
// ids). The 2026-08-22 trial brief lost its Jira section and had to deliver
// over the webhook fallback. The override approves exactly those; everything
// genuinely destructive stays denied.
func TestCronExecApprover_DestructiveOverrides(t *testing.T) {
	approve := cronExecApprover(t.TempDir(), t.TempDir())
	ctx := context.Background()

	allowed := []string{
		"atlassian:searchJiraIssuesUsingJql",
		// the REAL id: the label itself contains a colon
		"mcp:mcp.atlassian.com:searchJiraIssuesUsingJql",
		"mcp:mcp.atlassian.com:getJiraIssue",
		"atlassian:getConfluencePage",
		"atlassian:lookupJiraAccountId",
		"somemcp:listThings",
		discord.ActionSendMessage,
		discord.ActionReact,
	}
	for _, a := range allowed {
		if !approve(ctx, a, "detail") {
			t.Errorf("exec approver denied %q — the brief needs this unattended", a)
		}
	}

	denied := []string{
		"atlassian:createJiraIssue",
		"mcp:mcp.atlassian.com:editJiraIssue",
		"mcp:mcp.atlassian.com:createConfluencePage",
		"atlassian:editJiraIssue",
		"atlassian:transitionJiraIssue",
		"atlassian:deleteEverything",
		"os_exec_dangerous",
		"sql_ddl",
		"web_transact",
		"totally-unknown-action",
		":getNoLabel", // malformed id must not sneak through as a read
	}
	for _, a := range denied {
		if approve(ctx, a, "detail") {
			t.Errorf("exec approver allowed %q — destructive must stay denied unattended", a)
		}
	}

	// The write protection is untouched: config/data writes still refused.
	cfg := t.TempDir()
	approve2 := cronExecApprover(cfg, t.TempDir())
	if approve2(ctx, "os_write", "writing "+cfg+"/config.json") {
		t.Error("exec approver allowed a write into the config dir")
	}
}
