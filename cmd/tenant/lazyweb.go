package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"

	"tenant/internal/agent"
	"tenant/internal/model"
	"tenant/internal/plugins/web"
)

func readerActivityEvent(a web.ReaderActivity) agent.Event {
	type auditArgs struct {
		Provider   string `json:"provider"`
		TargetHost string `json:"target_host"`
	}
	b, _ := json.Marshal(auditArgs{Provider: a.Provider, TargetHost: a.TargetHost})
	return agent.Event{
		Kind:   agent.EventToolCall,
		Tool:   "web_reader_remote",
		Args:   string(b),
		Result: a.Provider + " -> " + a.TargetHost,
	}
}

// webConfig builds the web plugin's Config, resolving optional search/reader
// keys from env or credentials.json. Returned value is safe to share across the
// shared mux and per-agent lazyWeb sessions.
//
// Search backend precedence: Tavily > Brave > DuckDuckGo (keyless default).
// Key precedence per provider: $PRIMARY_ENV > $ALT_ENV > credentials.json > "".
func webConfig(cfgDir string, pf *pluginFlags, lc *launchConfig) web.Config {
	// Resolve keys LAZILY (re-read env→credentials.json on every search/read) so
	// a key added or rotated at runtime — via the settings page or an external
	// edit — is picked up without restarting. credKey is read-only + cheap.
	return web.Config{
		Headless:       !pf.webShow,
		NoSandbox:      lc != nil && lc.BrowserNoSandbox,
		ReaderEnabled:  readerEnabled(lc),
		BraveKeyFunc:   func() string { return braveKey(cfgDir) },
		TavilyKeyFunc:  func() string { return credKey(cfgDir, "tavily", "TAVILY_API_KEY", "TAVILY_KEY") },
		JinaKeyFunc:    func() string { return credKey(cfgDir, "jina", "JINA_API_KEY", "JINA_KEY") },
		PrivateNetwork: append([]string(nil), pf.webPrivateNetwork...),
	}
}

func braveKey(cfgDir string) string {
	if k := credKey(cfgDir, "brave_search", "BRAVE_SEARCH_API_KEY", "BRAVE_API_KEY"); k != "" {
		return k
	}
	// Also honor a key set via `/skill configure web brave_key=…` (TEN-69), which
	// stores under the skill namespace rather than the brave_search credential.
	if creds, err := loadCredentials(cfgDir); err == nil {
		return strings.TrimSpace(creds.Get(skillSecretID("web", "brave_key")))
	}
	return ""
}

// credKey resolves an API key: each environment variable in order, then
// credentials.json[credName], else "".
func credKey(cfgDir, credName string, envVars ...string) string {
	for _, env := range envVars {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	if cfgDir == "" {
		return ""
	}
	if creds, err := loadCredentials(cfgDir); err == nil {
		if v := strings.TrimSpace(creds.Get(credName)); v != "" {
			return v
		}
	}
	return ""
}

// readerEnabled is deliberately false for new/unset config. The setup and
// /configure surfaces persist the opt-in under skills.web.settings.jina_reader;
// TENANT_WEB_READER is an explicit headless/deployment override.
func readerEnabled(lc *launchConfig) bool {
	if raw, ok := os.LookupEnv("TENANT_WEB_READER"); ok && strings.TrimSpace(raw) != "" {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	}
	if lc != nil && lc.Skills != nil {
		if skill := lc.Skills["web"]; skill != nil && skill.Settings != nil {
			switch strings.ToLower(strings.TrimSpace(skill.Settings["jina_reader"])) {
			case "1", "true", "yes", "on", "enabled":
				return true
			}
		}
	}
	return false
}

// lazyWeb is a per-agent web plugin: it advertises the web tool specs but
// only spawns its OWN Chrome session the first time the agent actually
// browses. This gives each orchestration agent an ISOLATED browser, so
// concurrent navigation by different agents can't clobber a shared tab —
// and agents that never browse never start Chrome. One lazyWeb per agent;
// the created session is registered for cleanup at team shutdown.
type lazyWeb struct {
	cfg     web.Config
	policy  web.Policy
	shotDir string
	onNew   func(func())           // register a cleanup for the spawned session
	acquire func() (func(), error) // reserve/release a governed browser slot
	log     *slog.Logger

	mu      sync.Mutex
	disp    *web.Dispatcher
	failed  bool // a hard launch failure (e.g. no Chrome) — don't retry every call
	err     error
	cleanup func()
}

func newLazyWeb(cfg web.Config, policy web.Policy, shotDir string, onNew func(func()), acquire func() (func(), error), log *slog.Logger) *lazyWeb {
	return &lazyWeb{cfg: cfg, policy: policy, shotDir: shotDir, onNew: onNew, acquire: acquire, log: log}
}

// Tools advertises the web specs without a session (NewDispatcher is
// nil-safe for spec listing).
func (l *lazyWeb) Tools() []model.ToolSpec {
	return web.NewDispatcher(nil, l.policy, "").Tools()
}

func (l *lazyWeb) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	// web_search is HTTP-only — bypass the Chrome-launch step so search still
	// works if the browser is unavailable.
	if call.Name == "web_search" {
		return web.HandleSearch(ctx, l.cfg, call.Arguments)
	}
	d, err := l.ensure()
	if err != nil {
		return "browser unavailable: " + err.Error() + " (is Chrome installed?)", true, nil
	}
	return d.Dispatch(ctx, call)
}

// ensure lazily creates this agent's own Chrome session on first use. A
// hard failure is cached so a missing Chrome doesn't re-spawn-and-fail on
// every call within the run.
func (l *lazyWeb) ensure() (*web.Dispatcher, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disp != nil {
		return l.disp, nil
	}
	if l.failed {
		return nil, l.err
	}
	release := func() {}
	if l.acquire != nil {
		var err error
		release, err = l.acquire()
		if err != nil {
			return nil, err
		}
	}
	sess, err := web.NewSession(l.cfg)
	if err != nil {
		release()
		l.failed, l.err = true, err
		return nil, err
	}
	l.disp = web.NewDispatcher(sess, l.policy, l.shotDir)
	var once sync.Once
	l.cleanup = func() { once.Do(func() { _ = sess.Close(); release() }) }
	if l.onNew != nil {
		l.onNew(l.Close)
	}
	if l.log != nil {
		l.log.Info("orchestra: per-agent browser session started")
	}
	return l.disp, nil
}

// Close releases this agent's browser process and its governed slot. It is
// idempotent because sub-agent completion and runtime shutdown may both call it.
func (l *lazyWeb) Close() {
	l.mu.Lock()
	cleanup := l.cleanup
	l.cleanup = nil
	l.disp = nil
	l.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}
