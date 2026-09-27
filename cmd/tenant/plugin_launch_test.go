package main

import (
	"context"
	"testing"
)

// saveTestSecrets stores secrets in dir's credentials.json.
func saveTestSecrets(t *testing.T, dir string, kv map[string]string) {
	t.Helper()
	creds, err := loadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		creds.Set(k, v)
	}
	if err := creds.Save(dir); err != nil {
		t.Fatal(err)
	}
}

// saveTestSkill saves plugin id's settings in dir's config.json, enabled.
func saveTestSkill(t *testing.T, dir, id string, settings map[string]string) {
	t.Helper()
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lc.Skills == nil {
		lc.Skills = map[string]*skillConfig{}
	}
	lc.Skills[id] = &skillConfig{Enabled: true, Settings: settings}
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
}

// A GitHub enabled at launch runs with the token /configure stored, as its
// activator does. The launch used to look only at the environment and the
// token cache, so a GitHub that `tenant setup` enabled failed the launch (D2).
func TestGitHubLaunchUsesTheStoredToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_PAT", "")
	dir := t.TempDir()
	saveTestSecrets(t, dir, map[string]string{skillSecretID("github", "token"): "ghp_stored"})
	pf := testPluginFlags(func(pf *pluginFlags) {
		f := githubFlagsOf(pf)
		f.on, f.repo = true, "owner/repo"
	})
	built, _, err := launchGitHub(context.Background(), &pluginEnv{c: &commonFlags{cfgDir: dir}, pf: pf})
	if err != nil || len(built) != 1 {
		t.Fatalf("launch with a stored token: built %d, err %v", len(built), err)
	}
	if got := githubToken(dir); got != "ghp_stored" {
		t.Errorf("githubToken = %q, want the stored token", got)
	}
	t.Setenv("GITHUB_TOKEN", "ghp_env")
	if got := githubToken(dir); got != "ghp_stored" {
		t.Errorf("with $GITHUB_TOKEN set, githubToken = %q; the stored token wins, as in the activator", got)
	}
}

// Eval turns on a Jira that /configure set up with an API token, and the
// launch opens it with the token /configure stored. Eval used to copy a
// client_id secret that does not exist and never the token, so without
// $ATLASSIAN_TOKEN the whole eval failed to build its tools (D14).
func TestEvalLaunchesAConfiguredJira(t *testing.T) {
	t.Setenv("ATLASSIAN_TOKEN", "")
	t.Setenv("ATLASSIAN_CLIENT_SECRET", "")
	dir := t.TempDir()
	saveTestSkill(t, dir, "atlassian", map[string]string{
		"auth": "token", "site": "https://example.atlassian.net", "email": "me@example.com",
	})
	saveTestSecrets(t, dir, map[string]string{skillSecretID("atlassian", "api_token"): "stored-token"})
	c := &commonFlags{cfgDir: dir}
	pf := testPluginFlags()
	if !evalAtlassian(pf, c, map[string]bool{}) {
		t.Fatal("eval did not turn on the configured Jira")
	}
	if f := atlassianFlagsOf(pf); f.clientID != "" {
		t.Errorf("token mode set a client id %q", f.clientID)
	}
	built, _, err := launchAtlassian(context.Background(), &pluginEnv{c: c, pf: pf})
	if err != nil || len(built) != 1 {
		t.Fatalf("launch: built %d, err %v", len(built), err)
	}
}
