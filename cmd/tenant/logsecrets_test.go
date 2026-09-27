package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/config"
	"tenant/internal/logs"
	githubp "tenant/internal/plugins/github"
	"tenant/internal/sandbox"
	"tenant/internal/secrets"
)

// TEN-361: no credential Tenant holds reaches any source, in the ring or on
// disk, however it is logged: in a message, a slog attribute, a tool call's
// arguments or result, the sandbox's command, URL-encoded or inside a Basic
// credential, or straddling the value cap or the command clip. The set follows
// a secret written after start-up (an OAuth refresh) and an edit made by
// another process. It covers TEN-360 too: the dashboard token is one of the
// secrets, so even the old start-up line comes out clean.
func TestLogBookScrubsEveryConfiguredSecret(t *testing.T) {
	const leak = 12 // how much of a secret counts as a leak
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	secret := func(name string) string {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		return name + "-" + hex.EncodeToString(b)
	}
	want := map[string]string{} // what the test logged → where it came from
	add := func(from, v string) string { want[v] = from; return v }

	dashToken, err := resolveDashboardAuth(cfgDir, "")
	if err != nil {
		t.Fatal(err)
	}
	add("dashboard token file", dashToken)
	t.Setenv("ANTHROPIC_API_KEY", add("environment", secret("envkey")))
	creds := &credentials{}
	creds.Set("openai", add("credentials.json", secret("storedkey")))
	gtok, _ := json.Marshal(map[string]string{
		"access_token": add("gsuite access token", secret("gsaccess")), "token_type": "Bearer",
		"refresh_token": add("gsuite refresh token", secret("gsrefresh")), "expiry": "2026-09-27T10:00:00Z",
	})
	creds.Set(skillSecretID("gsuite", "oauth_token"), string(gtok))
	if err := creds.Save(cfgDir); err != nil {
		t.Fatal(err)
	}
	if err := githubp.SaveToken(cfgDir, add("github token", secret("ghpat"))); err != nil {
		t.Fatal(err)
	}
	mcpCache := filepath.Join(mcpCacheDir(cfgDir), "mcp-0123456789abcdef.json")
	mcpRefresh := add("mcp refresh token", secret("mcprefresh"))
	writeMCPCache := func(access string) {
		b, _ := json.Marshal(map[string]any{"server_url": "https://mcp.example.com/sse", "client_id": "public-client-id",
			"token": map[string]string{"access_token": access, "token_type": "Bearer", "refresh_token": mcpRefresh}})
		if err := secrets.WriteFile(mcpCache, b); err != nil {
			t.Fatal(err)
		}
	}
	retired := add("mcp access token", secret("mcpaccess"))
	writeMCPCache(retired)
	lc := &launchConfig{MCPServers: []config.MCPServer{{Name: "notes", EnvSet: map[string]string{"NOTES_API_KEY": add("mcp env_set", secret("envset"))}}}}
	c := &commonFlags{cfgDir: cfgDir, dataDir: dataDir, lc: lc, genAPIKey: add("--api-key", secret("flagkey"))}

	root := filepath.Join(dataDir, "logs")
	hub, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() {
		logs.Install(prev)
		secrets.OnWrite(nil)
		logSecretsRefresh.Store(nil)
		_ = hub.Close()
	})
	scrubLogSecrets(hub, c)
	hub.Logf("mcp", slog.LevelInfo, "mcp: connected", "auth", "Bearer "+retired, "raw", retired)

	// An OAuth refresh after start-up is known the moment it is written. The
	// token it replaced leaves the set: it is no longer logged anywhere.
	rotated := add("refreshed mcp access token", secret("mcprotated"))
	writeMCPCache(rotated)
	hub.Logf("mcp", slog.LevelInfo, "mcp: token refreshed", "raw", rotated)
	// Another process edits credentials.json: known on the watcher's tick.
	creds.Set("anthropic", add("hand-edited credentials.json", secret("handkey")))
	b, _ := json.Marshal(creds)
	if err := os.WriteFile(credentialsPath(cfgDir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	refreshLogSecrets()

	for v := range want {
		if v == retired {
			continue
		}
		basic := base64.StdEncoding.EncodeToString([]byte("me:" + v))
		hub.Logf("system", slog.LevelInfo, "dashboard: serving on http://127.0.0.1:8770\ndashboard auth token: "+v)
		logs.For("mcp").Warn("mcp: token refresh failed", "err", "oauth2: "+v, "body", `{"note":"`+v+`"}`)
		hub.Logf("turns", slog.LevelInfo, "fetch", "url", "https://api.example.com/x?q="+url.QueryEscape(v))
		for _, ev := range []agent.Event{
			{Kind: agent.EventToolCall, Tool: "os_exec", Args: `{"command":"curl -H 'Authorization: Basic ` + basic + `' https://x"}`},
			{Kind: agent.EventToolResult, Tool: "web_fetch", Result: strings.Repeat("r", 4096-leak) + v}, // straddles the 4 KB cap
		} {
			if e, ok := agent.EntryFor(ev); ok {
				hub.Log(e)
			}
		}
		// Straddles the sandbox command's 300-character clip.
		logSandboxRun("discord", "restricted", true, sandbox.Command{Shell: strings.Repeat("x", 299-leak) + " " + v}, sandbox.Result{ExitCode: 1, Violations: []sandbox.Violation{{Kind: "filesystem", Detail: "denied"}}}, nil)
	}

	leaks := func(where, text string) {
		t.Helper()
		for v, from := range want {
			if strings.Contains(text, v[:leak]) {
				t.Errorf("%s holds the %s (%s…)", where, from, v[:leak])
			}
		}
	}
	all, _ := hub.Snapshot()
	var markers int
	for _, e := range all {
		leaks("ring entry ["+e.Source+"]", e.Line())
		markers += strings.Count(e.Line(), "«secret:")
	}
	if markers < 5*(len(want)-1) {
		t.Errorf("only %d «secret:…» markers in %d entries; the scrubber did not run", markers, len(all))
	}
	sources := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			leaks(p, string(b))
			sources[filepath.Base(filepath.Dir(p))] = true
		}
		return nil
	})
	for _, s := range []string{"system", "mcp", "turns", "sandbox"} {
		if !sources[s] {
			t.Errorf("no %s folder written; sources: %v", s, sources)
		}
	}
}

// The collector names each value by file and JSON path, keeps the metadata
// beside a token (token_type, expiry, URLs) out, and walks a JSON document
// stored as a string by its own keys.
func TestAddSecretTextPicksTheSecretFields(t *testing.T) {
	out := map[string]string{}
	addSecretText(out, "credentials.json", `{"secrets":{"openai":"sk-stored","skill:gsuite:sa_json":"{\"client_email\":\"bot@proj.iam\",\"private_key\":\"pem-body\"}"}}`)
	addSecretText(out, "mcp/mcp-1.json", `{"server_url":"https://mcp.example.com","client_secret":"cs","token":{"access_token":"at","token_type":"Bearer","expiry":"2026-09-27T10:00:00Z"},"reauth_error":"refresh failed"}`)
	addSecretText(out, "peers.json", `{"peers":[{"name":"box","token":"pt","fingerprint":"ab12"}]}`)
	addSecretText(out, "dashboard-auth-token", "tok\n")
	want := map[string]string{
		"credentials.json:secrets.openai":                           "sk-stored",
		"credentials.json:secrets.skill:gsuite:sa_json.private_key": "pem-body",
		"mcp/mcp-1.json:client_secret":                              "cs",
		"mcp/mcp-1.json:token.access_token":                         "at",
		"peers.json:peers[0].token":                                 "pt",
		"dashboard-auth-token":                                      "tok",
	}
	if len(out) != len(want) {
		t.Errorf("collected %v", out)
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, out[k], v, out)
		}
	}
}

// A secret file that exists but cannot be read just now keeps the values it
// held; one that is gone drops them.
func TestLogSecretValuesKeepsAFileItCannotRead(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.Mkdir(githubp.TokenPath(cfgDir), 0o700); err != nil { // there, but not a file Peek reads
		t.Fatal(err)
	}
	last := map[string]string{"github-token.json:token": "kept-value", "dashboard-auth-token": "gone-value"}
	got := logSecretValues(cfgDir, "", nil, last)
	if got["github-token.json:token"] != "kept-value" || got["dashboard-auth-token"] != "" {
		t.Errorf("values = %v", got)
	}
}
