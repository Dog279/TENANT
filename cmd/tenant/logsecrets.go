package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"tenant/internal/fleet"
	"tenant/internal/logs"
	"tenant/internal/secrets"
)

// The log book's scrubber (internal/logs, TEN-361) replaces every credential
// this process holds wherever one turns up in a log line. The log package
// knows neither the secrets layer nor the launch config, so the values are
// collected here and handed to the hub.

// scrubLogSecrets loads hub's scrubber, then keeps it current: after every
// secret file this process writes (a stored key, an OAuth refresh, a new peer
// token), and on the credential watcher's tick, which catches edits made by
// hand or by another Tenant process.
func scrubLogSecrets(hub *logs.Hub, c *commonFlags) {
	fixed := fixedLogSecrets(c)
	var mu sync.Mutex
	var last map[string]string
	refresh := func() {
		mu.Lock()
		defer mu.Unlock()
		last = logSecretValues(c.cfgDir, c.dataDir, fixed, last)
		hub.SetSecrets(last)
	}
	refresh()
	logSecretsRefresh.Store(&refresh)
	secrets.OnWrite(func(string) { refresh() })
}

// logSecretsRefresh re-collects the values; nil until scrubLogSecrets runs.
var logSecretsRefresh atomic.Pointer[func()]

// refreshLogSecrets is the credential watcher's hook: re-read every secret
// file (the scrubber skips the rebuild when nothing changed).
func refreshLogSecrets() {
	if f := logSecretsRefresh.Load(); f != nil {
		(*f)()
	}
}

// fixedLogSecrets are the credentials outside the secret files, fixed while
// Tenant runs: the dashboard token and MCP env_set values from config.json,
// and API keys given as flags or environment variables.
func fixedLogSecrets(c *commonFlags) map[string]string {
	out := map[string]string{"generation-api-key": c.genAPIKey, "embedding-api-key": c.embedAPIKey}
	for _, spec := range keyCatalog() {
		for _, ev := range spec.EnvVars {
			out["$"+ev] = os.Getenv(ev)
		}
	}
	if c.lc != nil {
		out["dashboard.auth"] = c.lc.Dashboard.Auth
		for _, p := range c.lc.Providers {
			if p != nil && p.Auth.KeyEnv != "" {
				out["$"+p.Auth.KeyEnv] = os.Getenv(p.Auth.KeyEnv)
			}
		}
		for _, s := range c.lc.MCPServers {
			for k, v := range s.EnvSet {
				out["mcp_servers."+s.Name+".env_set."+k] = v
			}
		}
	}
	return out
}

// logSecretValues is fixed plus the values in every secret file Tenant keeps
// (managedSecrets), each named <file>:<JSON path>, or <file> for a file that
// is one secret. A file that exists but cannot be read just now (a writer
// mid-rename on Windows) keeps the values it held in last, the previous
// result, so a passing error never unmasks a secret.
func logSecretValues(cfgDir, dataDir string, fixed, last map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, fixed)
	files, listErr := managedSecrets(cfgDir, dataDir)
	if listErr != nil {
		maps.Copy(out, last) // an MCP cache went unlisted
	}
	for _, f := range files {
		if f.path == fleet.KeyPath(cfgDir) {
			// The fleet key (TEN-532) is only ever read into locked memory:
			// as text here it would sit on the heap for the whole run. It is
			// never written as text either, so no log line can hold it.
			continue
		}
		b, err := secrets.Peek(f.path)
		switch {
		case err == nil:
			addSecretText(out, f.label, string(b))
		case !errors.Is(err, fs.ErrNotExist):
			for k, v := range last {
				if k == f.label || strings.HasPrefix(k, f.label+":") {
					out[k] = v
				}
			}
		}
	}
	return out
}

// secretJSONKey names a JSON field that holds a secret, and makes everything
// under it secret; notSecretJSONKey is the metadata kept beside a token.
var (
	secretJSONKey    = regexp.MustCompile(`(?i)secret|token|passw|api_?key|private_?key|credential`)
	notSecretJSONKey = regexp.MustCompile(`(?i)(_type|_url|_uri|expir\w*|_at|_since|_error|_needed)$`)
)

// addSecretText adds one secret file: the secret fields of a JSON document,
// else the whole text (the dashboard token, a PEM key).
func addSecretText(out map[string]string, name, text string) {
	text = strings.TrimSpace(text)
	var doc any
	if (strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[")) && json.Unmarshal([]byte(text), &doc) == nil {
		addSecretJSON(out, name, "", doc, false)
		return
	}
	out[name] = text
}

// addSecretJSON walks the JSON value v at path. A string is secret when its
// key or a key above it names a secret. A string that holds a JSON document
// (the gsuite OAuth token or service account in credentials.json) is walked in
// turn, by its own keys: its client_email and project_id are not secrets.
func addSecretJSON(out map[string]string, name, path string, v any, secret bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			p := k
			if path != "" {
				p = path + "." + k
			}
			addSecretJSON(out, name, p, e, (secret || secretJSONKey.MatchString(k)) && !notSecretJSONKey.MatchString(k))
		}
	case []any:
		for i, e := range x {
			addSecretJSON(out, name, path+"["+strconv.Itoa(i)+"]", e, secret)
		}
	case string:
		var doc any
		if t := strings.TrimSpace(x); strings.HasPrefix(t, "{") && json.Unmarshal([]byte(t), &doc) == nil {
			addSecretJSON(out, name, path, doc, false)
		} else if secret {
			out[name+":"+path] = x
		}
	}
}
