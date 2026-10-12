package main

// toolmux_build.go builds the process's tool mux from flags and config: the
// launch-time plugins, the stub catalog and its activators, remote MCP and
// peers. The mux itself is in toolmux.go.

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"tenant/internal/model"
	"tenant/internal/uicomp"
)

// pluginFlags holds every plugin's launch flags, each plugin's in its own
// struct declared in its plugin_<id>.go (read with flagsOf), plus the
// remote-MCP family's. All default OFF (memory-only chat). Dangerous actions
// (write/send/post) default OFF even when a plugin is enabled — the operator
// opts in.
type pluginFlags struct {
	of map[string]any

	mcpRemotes          []string // --mcp-remote (repeatable): remote MCP server URLs
	mcpTrustAnnotations bool
	mcpAllowWrite       bool
	mcpCallback         string
}

// flagsOf returns plugin id's flag struct. A pluginFlags built by hand (tests)
// starts empty, so a missing struct is created and kept.
func flagsOf[T any](pf *pluginFlags, id string) *T {
	if pf == nil {
		return new(T)
	}
	if v, ok := pf.of[id].(*T); ok {
		return v
	}
	v := new(T)
	if pf.of == nil {
		pf.of = map[string]any{}
	}
	pf.of[id] = v
	return v
}

// bindPluginFlags binds every plugin's launch flags (from its descriptor) and
// the remote-MCP family's.
func bindPluginFlags(fs *flag.FlagSet) *pluginFlags {
	p := &pluginFlags{of: map[string]any{}}
	for _, d := range plugins().All() {
		if d.Flags != nil {
			p.of[d.ID] = d.Flags(fs)
		}
	}
	fs.Func("mcp-remote", "remote MCP server URL to connect as a client (repeatable; OAuth2.1+DCR browser sign-in via `/enable`)", func(s string) error {
		p.mcpRemotes = append(p.mcpRemotes, s)
		return nil
	})
	fs.BoolVar(&p.mcpTrustAnnotations, "mcp-trust-annotations", false, "trust remote servers' read-only tool annotations (relax the deny-by-default gate)")
	fs.BoolVar(&p.mcpAllowWrite, "mcp-allow-write", false, "permit remote MCP write tools without per-action confirm")
	fs.StringVar(&p.mcpCallback, "mcp-callback", "", "localhost OAuth callback for --mcp-remote (default 127.0.0.1:8765)")
	return p
}

// buildToolMux constructs every enabled plugin and returns the merged
// dispatcher + a cleanup func (closes browser/db handles). An enabled
// plugin that fails to construct returns an error — better to tell the
// operator their flag is wrong than to silently drop the tool.
func buildToolMux(ctx context.Context, c *commonFlags, router *model.Router, pf *pluginFlags, confirm func(context.Context, string, string) bool, log *slog.Logger) (*toolMux, *wikiLive, func(), error) {
	mux := newToolMux()
	fail := func(err error) (*toolMux, *wikiLive, func(), error) {
		mux.Close()
		return nil, nil, nil, err
	}
	e := &pluginEnv{c: c, pf: pf, confirm: confirm, log: log, router: router, wiki: &wikiLive{}}

	// Local memory recall tool (TEN-243 Phase B). Enabled by default as of
	// TEN-249: it gives the agent ACTIVE recall of its own long-term memory and,
	// when a memory-sharing peer is connected, federates to that peer — so the
	// knowledge tier is always available and the model isn't pushed to web_search
	// for "what do we know / did we decide" questions. (Relevant memory is still
	// auto-assembled each turn too; this just adds the on-demand search.) The
	// operator can still `/disable memory_search`; a persisted choice wins via
	// restore. Opens its own read-only store handle (SQLite WAL ⇒ safe alongside
	// the live handle); a failure to open just means no memory_search.
	if mst, mclose, merr := openStores(c); merr == nil {
		mux.AddCleanup(mclose)
		hostName, _ := os.Hostname()
		var memEmb model.Embedder
		if router != nil { // nil in some headless/test paths; embedder is optional
			if e, _, eerr := router.EmbedderForRole(ctx, model.RoleEmbedder); eerr == nil {
				memEmb = e
			}
		}
		mux.Add("memory", newMemorySearchDispatcher(hostName, mst.semantic, mst.episodic, memEmb))
		// add() registers enabled by default; no disable-at-launch (TEN-249).
		// memory_history (TEN-255 Phase 2): "as of date X" recall over the
		// bi-temporal validity. Same store handle, enabled by default; additive
		// (no temporal data ⇒ it just returns the current set as-of the date).
		mux.Add("memory", newMemoryHistoryDispatcher(hostName, mst.semantic))
	} else if log != nil {
		log.Debug("memory_search: store open failed; tool unavailable", "err", merr.Error())
	}

	// ui_render (TEN-517): the agent answers with a table or steps and the
	// surfaces draw it from the data. A host tool like memory: nothing to
	// configure, on by default, /disable ui turns it off.
	mux.Add("ui", uicomp.Dispatcher{})

	// Every plugin its launch flags configure, declared in plugin_<id>.go.
	if err := launchPlugins(ctx, mux, e); err != nil {
		return fail(err)
	}

	// Every shared plugin the launch path did not build gets a disabled stub
	// (and its activator), declared in its descriptor (plugin_<id>.go).
	registerPluginStubs(ctx, mux, e)

	// Remote MCP connectors (TEN-162/164): stash the deps so runtime `/mcp add`
	// builds connectors identical (gated, deny-by-default) to launch-time ones,
	// then register one disabled stub + activator per server — from both
	// --mcp-remote flags AND the persisted launchConfig list. The activator
	// connects (OAuth2.1 + DCR + browser) on `/enable` / `/mcp add`.
	deps := mcpRuntimeDeps{
		Ctx:              ctx,
		Callback:         pf.mcpCallback,
		OpenBrowser:      openBrowser,
		TrustAnnotations: pf.mcpTrustAnnotations,
		AllowWrite:       e.gateAllow(pf.mcpAllowWrite),
		Confirm:          confirm,
		Rule:             defaultToolRules.Mode,
		Locked:           offsiteLocked,
		CacheDir:         filepath.Join(c.cfgDir, "mcp"),
		Log:              log,
	}
	if c.lc != nil {
		deps.Trust = c.lc.MCPTrust
	}
	mux.SetMCPDeps(deps)
	mcpURLs := append([]string{}, pf.mcpRemotes...)
	if c.lc != nil {
		mcpURLs = append(mcpURLs, c.lc.MCPRemotes...)
	}
	seenMCP := map[string]bool{}
	for _, url := range mcpURLs {
		if url == "" || seenMCP[mcpLabel(url)] {
			continue
		}
		seenMCP[mcpLabel(url)] = true
		mux.RegisterMCPRemote(url)
		// Auto-reconnect from a cached token (async — no browser, no launch
		// stall). With a valid/refreshable token the tools come live shortly
		// after launch; otherwise the disabled stub remains for /configure.
		go mux.ReconnectMCPSilently(url)
	}
	// TEN-318: local MCP servers, out of process under the execution sandbox,
	// started in the background like the remote connectors above.
	registerMCPServers(mux, c.lc, c.cfgDir, c.dataDir)
	// TEN-186: bring paired peers' shared knowledge tools live for the agent,
	// same silent/non-blocking pattern.
	mux.ReconnectPeersSilently(c.cfgDir)

	return mux, e.wiki, mux.Close, nil
}
