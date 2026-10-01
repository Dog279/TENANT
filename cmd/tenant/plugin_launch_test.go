package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"tenant/internal/plugins/gsuite"
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

const testOAuthClient = `{"installed":{"client_id":"fake.apps.googleusercontent.com","client_secret":"fake",` +
	`"auth_uri":"https://accounts.google.com/o/oauth2/v2/auth","token_uri":"https://oauth2.googleapis.com/token",` +
	`"redirect_uris":["http://localhost"]}}`

// installTestOAuthClient installs an OAuth client under dir, as
// `tenant oauth-setup gsuite` does.
func installTestOAuthClient(t *testing.T, dir string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(src, []byte(testOAuthClient), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installGSuiteOAuthSilent(dir, src); err != nil {
		t.Fatal(err)
	}
}

// A Google Workspace signed in with OAuth through /configure or `tenant setup`
// runs at launch and on /enable with the client it signed in with and the
// token /configure cached. The launch had no OAuth at all, so a saved OAuth
// setup failed the launch; the activator read only oauth_creds_json, so the
// installed client failed /enable and the cached token was ignored (D1).
func TestGSuiteOAuthRunsWithTheConfiguredSignIn(t *testing.T) {
	dir := t.TempDir()
	installTestOAuthClient(t, dir)
	saveTestSkill(t, dir, "gsuite", map[string]string{"auth": "oauth"})
	tok, _ := json.Marshal(&oauth2.Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)})
	saveTestSecrets(t, dir, map[string]string{skillSecretID("gsuite", "oauth_token"): string(tok)})

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c := bindCommon(fs)
	pf := bindPluginFlags(fs)
	if err := fs.Parse([]string{"--config", dir, "--data", filepath.Join(dir, "data")}); err != nil {
		t.Fatal(err)
	}
	if err := c.resolve(); err != nil {
		t.Fatal(err)
	}
	applyPluginConfig(c, pf)
	if f := gsuiteFlagsOf(pf); !f.on || f.auth != "oauth" {
		t.Fatalf("saved config applied as on=%v auth=%q", f.on, f.auth)
	}
	built, _, err := launchGSuite(context.Background(), &pluginEnv{c: c, pf: pf})
	if err != nil || len(built) != 1 {
		t.Fatalf("launch: built %d, err %v", len(built), err)
	}
	if _, _, err := activateGSuite(context.Background(), &pluginEnv{c: c, pf: testPluginFlags()}); err != nil {
		t.Fatalf("activate: %v", err)
	}
}

// The running plugin signs in with the token /configure cached, and saves a
// refreshed one back without losing a secret stored since it started.
func TestGSuiteRuntimeOAuthKeepsTheToken(t *testing.T) {
	dir := t.TempDir()
	installTestOAuthClient(t, dir)
	tok, _ := json.Marshal(&oauth2.Token{AccessToken: "at", RefreshToken: "rt"})
	saveTestSecrets(t, dir, map[string]string{skillSecretID("gsuite", "oauth_token"): string(tok)})
	var cfg gsuite.Config
	if err := gsuiteRuntimeOAuth(&cfg, dir, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.OAuthCreds) == 0 {
		t.Error("no OAuth client: the installed one was not used")
	}
	if cfg.OAuthToken == nil || cfg.OAuthToken.RefreshToken != "rt" {
		t.Fatalf("token = %+v, want the cached one", cfg.OAuthToken)
	}
	saveTestSecrets(t, dir, map[string]string{"skill:x:bearer": "stored-later"})
	if err := cfg.OAuthSave(&oauth2.Token{AccessToken: "at2", RefreshToken: "rt2"}); err != nil {
		t.Fatal(err)
	}
	creds, err := loadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	var saved oauth2.Token
	if err := json.Unmarshal([]byte(creds.Get(skillSecretID("gsuite", "oauth_token"))), &saved); err != nil || saved.RefreshToken != "rt2" {
		t.Errorf("saved token = %+v, %v", saved, err)
	}
	if creds.Get("skill:x:bearer") != "stored-later" {
		t.Error("saving the refreshed token lost a secret stored after the plugin started")
	}
}

// TEN-483: a launch-built sql plugin picks up a changed db path on
// disable → enable, with no restart; a folder is rejected as the path.
func TestSQLRebuildsFromConfigOnReenable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	saveTestSkill(t, dir, "sql", map[string]string{"db": a})
	pf := &pluginFlags{}
	sqlFlagsOf(pf).db = a
	e := &pluginEnv{c: &commonFlags{cfgDir: dir}, pf: pf}
	built, cleanup, err := launchSQL(ctx, e)
	if err != nil || len(built) != 1 {
		t.Fatalf("launch: %v (%d built)", err, len(built))
	}
	mux := newToolMux()
	mux.Add("sql", built[0])
	mux.AdoptLaunched("sql", activatorFor(ctx, sqlPlugin(), e), cleanup)

	saveTestSkill(t, dir, "sql", map[string]string{"db": b})
	mux.SetEnabled("sql", false)
	if _, _, err := mux.SetEnabled("sql", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatalf("rebuild should open the new db path: %v", err)
	}
	mux.Close()

	if validateSQLiteFile(dir) == nil {
		t.Error("a folder should be rejected as the SQL database path")
	}
	if err := validateSQLiteFile(b); err != nil {
		t.Errorf("a file path should pass: %v", err)
	}
}

// TEN-482: the startup check looks for the claude binary instead of probing
// claudecode://auto over HTTP.
func TestHealthCheckClaudeCode(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &commonFlags{backend: "claudecode", vllmEndpoint: bin}
	if got := healthCheck(context.Background(), c)[0]; got != "generation OK: claude CLI @ "+bin {
		t.Errorf("found binary: %q", got)
	}
	c.vllmEndpoint = bin + "-missing"
	if got := healthCheck(context.Background(), c)[0]; !strings.HasPrefix(got, "GENERATION UNAVAILABLE:") {
		t.Errorf("missing binary: %q", got)
	}
}

// TEN-483 + TEN-478: the wiki rebuilds on a new dir on disable → enable; the
// tools name the new root, and research/peers read the new dir and index.
func TestWikiRebuildsOnNewDir(t *testing.T) {
	ctx := context.Background()
	cfg := t.TempDir()
	a, b := filepath.Join(cfg, "wa"), filepath.Join(cfg, "wb")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	router, err := buildRouter(&commonFlags{backend: "echo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	saveTestSkill(t, cfg, "wiki", map[string]string{"dir": a})
	pf := &pluginFlags{}
	wikiFlagsOf(pf).dir = a
	e := &pluginEnv{c: &commonFlags{cfgDir: cfg, dataDir: cfg}, pf: pf, router: router, wiki: &wikiLive{}}
	built, cleanup, err := launchWiki(ctx, e)
	if err != nil || len(built) != 1 {
		t.Fatalf("launch: %v (%d built)", err, len(built))
	}
	mux := newToolMux()
	mux.Add("wiki", built[0])
	mux.AdoptLaunched("wiki", activatorFor(ctx, wikiPlugin(), e), cleanup)
	desc := func() string {
		for _, ts := range mux.All() {
			if ts.Name == "wiki_list" {
				return ts.Description
			}
		}
		return ""
	}
	if !strings.Contains(desc(), a) {
		t.Fatalf("wiki_list should name the root %s: %q", a, desc())
	}

	saveTestSkill(t, cfg, "wiki", map[string]string{"dir": b})
	mux.SetEnabled("wiki", false)
	if _, _, err := mux.SetEnabled("wiki", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if !strings.Contains(desc(), b) {
		t.Errorf("after a rebuild wiki_list should name %s: %q", b, desc())
	}
	if dir, ix := e.wiki.get(); dir != b || ix == nil || ix.Root() != b {
		t.Errorf("live wiki = %q (index root %v), want %s", dir, ix, b)
	}
	mux.Close()
}
