package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"

	"tenant/internal/model"
	"tenant/internal/plugins/web"
)

// webConfig builds the web plugin's Config, resolving optional search/reader
// keys from env or credentials.json. Returned value is safe to share across the
// shared mux and per-agent lazyWeb sessions.
//
// Search backend precedence: Tavily > Brave > DuckDuckGo (keyless default).
// Key precedence per provider: $PRIMARY_ENV > $ALT_ENV > credentials.json > "".
func webConfig(cfgDir string, pf *pluginFlags) web.Config {
	// Resolve keys LAZILY (re-read env→credentials.json on every search/read) so
	// a key added or rotated at runtime — via the settings page or an external
	// edit — is picked up without restarting. credKey is read-only + cheap.
	return web.Config{
		Headless:       !pf.webShow,
		BraveKeyFunc:   func() string { return braveKey(cfgDir) },
		TavilyKeyFunc:  func() string { return credKey(cfgDir, "tavily", "TAVILY_API_KEY", "TAVILY_KEY") },
		JinaKeyFunc:    func() string { return credKey(cfgDir, "jina", "JINA_API_KEY", "JINA_KEY") },
		ReaderDisabled: readerDisabled(),
	}
}

func braveKey(cfgDir string) string {
	return credKey(cfgDir, "brave_search", "BRAVE_SEARCH_API_KEY", "BRAVE_API_KEY")
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
		if v := strings.TrimSpace(creds.get(credName)); v != "" {
			return v
		}
	}
	return ""
}

// readerDisabled lets an operator turn OFF the r.jina.ai reader fallback (so no
// URL is ever sent to a third party) via TENANT_WEB_NO_READER=1.
func readerDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TENANT_WEB_NO_READER"))) {
	case "1", "true", "yes":
		return true
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
	onNew   func(func()) // register a cleanup for the spawned session
	log     *slog.Logger

	mu     sync.Mutex
	disp   *web.Dispatcher
	failed bool // a hard launch failure (e.g. no Chrome) — don't retry every call
	err    error
}

func newLazyWeb(cfg web.Config, policy web.Policy, shotDir string, onNew func(func()), log *slog.Logger) *lazyWeb {
	return &lazyWeb{cfg: cfg, policy: policy, shotDir: shotDir, onNew: onNew, log: log}
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
	sess, err := web.NewSession(l.cfg)
	if err != nil {
		l.failed, l.err = true, err
		return nil, err
	}
	l.disp = web.NewDispatcher(sess, l.policy, l.shotDir)
	if l.onNew != nil {
		l.onNew(func() { _ = sess.Close() })
	}
	if l.log != nil {
		l.log.Info("orchestra: per-agent browser session started")
	}
	return l.disp, nil
}
