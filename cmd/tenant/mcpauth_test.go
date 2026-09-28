package main

// Tests for "a dead remote-MCP sign-in is loud": status surfaces report it,
// the notifier announces it, the cron summary carries a banner, and `tenant
// doctor` catches it offline. Before this, a dead Atlassian bridge showed as
// "on, N tools" everywhere and the daily brief's Jira section just went empty.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/plugins/mcpremote"
)

// The feed line names the fix; the DM (best-effort) goes to the operator.
func TestMCPAuthNotifier_FeedAndDM(t *testing.T) {
	var feed, dm []string
	fn := mcpAuthNotifier(`C:\Tenant\config`, func(s string) { feed = append(feed, s) }, func(s string) error { dm = append(dm, s); return nil })
	fn("mcp:mcp.atlassian.com", "https://mcp.atlassian.com/v1/mcp", true, "invalid_grant")
	fn("mcp:mcp.atlassian.com", "https://mcp.atlassian.com/v1/mcp", false, "")
	// The fix names serve's config dir: an admin's default one is not the cache serve reads.
	const fix = `tenant mcp connect https://mcp.atlassian.com/v1/mcp --config C:\Tenant\config`
	if len(feed) != 2 || !strings.Contains(feed[0], fix) || !strings.Contains(feed[1], "re-authorized") {
		t.Errorf("feed = %v", feed)
	}
	if len(dm) != 2 || !strings.Contains(dm[0], "invalid_grant") || !strings.Contains(dm[0], fix) || !strings.Contains(dm[1], "re-authorized") {
		t.Errorf("dm = %v", dm)
	}
	// nil hooks are fine.
	mcpAuthNotifier("", nil, nil)("x", "u", true, "e")
}

func TestWithAuthBanner(t *testing.T) {
	if got := withAuthBanner("all good", nil); got != "all good" {
		t.Errorf("no problems must leave the summary untouched, got %q", got)
	}
	got := withAuthBanner("Projects: (nothing)", []string{"mcp:mcp.atlassian.com (tenant mcp connect https://mcp.atlassian.com/v1/mcp)"})
	if !strings.HasPrefix(got, "⚠️ NEEDS RE-AUTHORIZATION") || !strings.Contains(got, "tenant mcp connect") || !strings.HasSuffix(got, "Projects: (nothing)") {
		t.Errorf("banner wrong: %q", got)
	}
}

// doctor reads the persisted state offline — no network, no browser.
func TestDoctor_RemoteMCPAuth(t *testing.T) {
	dir := t.TempDir()
	const url = "https://mcp.atlassian.com/v1/mcp"
	env := func() *doctorEnv {
		return &doctorEnv{c: &commonFlags{cfgDir: dir}, lc: &launchConfig{MCPRemotes: []string{url}}}
	}
	ctx := context.Background()

	if r := checkRemoteMCPAuth(ctx, &doctorEnv{c: &commonFlags{cfgDir: dir}, lc: &launchConfig{}}); r.Status != statusSkip {
		t.Errorf("no connectors configured ⇒ skip, got %+v", r)
	}
	if r := checkRemoteMCPAuth(ctx, env()); r.Status != statusWarn {
		t.Errorf("configured but never connected ⇒ warn, got %+v", r)
	}

	path := mcpremote.TokenCachePath(filepath.Join(dir, "mcp"), url)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	dead := `{"server_url":"` + url + `","reauth_needed":true,"reauth_error":"invalid_grant","reauth_since":1700000000}`
	if err := os.WriteFile(path, []byte(dead), 0o600); err != nil {
		t.Fatal(err)
	}
	r := checkRemoteMCPAuth(ctx, env())
	if r.Status != statusFail || !strings.Contains(r.Detail, "tenant mcp connect "+url+" --config "+dir) || !strings.Contains(r.Detail, "2023-11-14") {
		t.Fatalf("dead sign-in ⇒ FAIL with the reconnect command + since, got %+v", r)
	}

	ok := `{"server_url":"` + url + `","token":{"access_token":"a","refresh_token":"r"}}`
	if err := os.WriteFile(path, []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkRemoteMCPAuth(ctx, env()); r.Status != statusOK {
		t.Errorf("healthy cache ⇒ OK, got %+v", r)
	}
}
