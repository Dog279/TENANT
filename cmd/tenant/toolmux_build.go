package main

// toolmux_build.go builds the process's tool mux from flags and config: the
// launch-time plugins, the stub catalog and its activators, remote MCP and
// peers. The mux itself is in toolmux.go.

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/model/backend/claudecode"
	"tenant/internal/peering"
	"tenant/internal/plugins/osys"
	"tenant/internal/plugins/wiki"
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
func buildToolMux(ctx context.Context, c *commonFlags, router *model.Router, pf *pluginFlags, confirm func(context.Context, string, string) bool, log *slog.Logger) (*toolMux, *wiki.Index, func(), error) {
	mux := newToolMux()
	fail := func(err error) (*toolMux, *wiki.Index, func(), error) {
		mux.Close()
		return nil, nil, nil, err
	}
	e := &pluginEnv{c: c, pf: pf, confirm: confirm, log: log, router: router}

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
	// TEN-186: bring paired peers' shared knowledge tools live for the agent,
	// same silent/non-blocking pattern.
	mux.ReconnectPeersSilently(c.cfgDir)

	return mux, e.wikiIndex, mux.Close, nil
}

// buildWikiIndex constructs the wiki index for the TUI. Unlike the
// `tenant wiki` command (where a dead embedder is fatal — wiki is the
// whole point there), here it must NOT crash the TUI: `wiki.New` is
// offline-safe (validates the dir + loads the sidecar, no network), the
// embedder fingerprint falls back to the profile's declared dim if the
// probe fails, and the initial reindex is best-effort (wiki.Search
// auto-reindexes lazily on first use, so it self-heals when the
// embedder comes back). A search while the embedder is down surfaces a
// clean tool error in the feed instead of a startup crash.
func buildWikiIndex(ctx context.Context, c *commonFlags, router *model.Router, dir string, log *slog.Logger) (*wiki.Index, error) {
	emb, embProfile, err := router.EmbedderForRole(ctx, model.RoleEmbedder)
	if err != nil {
		return nil, fmt.Errorf("wiki plugin: resolve embedder: %w", err)
	}
	dim := embProfile.EmbedDim // declared; preferred = actual (probed) when reachable
	if probe, perr := emb.Embed(ctx, []string{"wiki embedder fingerprint probe"}); perr == nil && len(probe) == 1 && len(probe[0]) > 0 {
		dim = len(probe[0])
	} else {
		log.Warn("wiki: embedder probe failed; using declared dim, reindex deferred to first search",
			"declared_dim", dim, "err", perr)
	}
	embedID := embProfile.Model + "/" + strconv.Itoa(dim)
	absVault, _ := filepath.Abs(dir)
	h := fnv.New64a()
	_, _ = h.Write([]byte(absVault))
	sidecar := filepath.Join(c.dataDir, "wiki", fmt.Sprintf("%x.json", h.Sum64()))
	ix, err := wiki.New(dir, sidecar, embedID, emb)
	if err != nil {
		return nil, fmt.Errorf("wiki plugin: %w", err)
	}
	if _, _, rerr := ix.Reindex(ctx); rerr != nil {
		// Embedder down at launch — don't crash; lazy reindex on first search.
		log.Warn("wiki: initial reindex failed; will reindex on first search", "err", rerr)
	}
	return ix, nil
}

// resolveOSRoots builds the file-tool boundary. The write roots default to
// where the agent legitimately works: Tenant's own config and data dirs, the
// operator's Desktop/Documents/Downloads and the temp dir. Never a PARENT of
// the config or data dir: with default dirs that is the OS-wide config root
// (~/.config, %AppData%, ~/Library/Application Support), which holds autostart
// entries and other programs' state, and on a C:\Tenant server it holds the
// binary and the claude CLI's config. Nothing Tenant does needs it: cron, MCP
// and peer changes persist in-process, wiki_save writes the wiki, and memory
// and skills have their own stores.
//
// Deny always includes the platform defaults even when the operator supplies
// their own list: an allowlist typo should not silently expose System32.
// Protect and Secret are always Tenant's own (fileToolGuards), whatever the
// write roots say.
//
// This bounds the FILE tools only. os_exec takes an opaque command string and
// is bounded by the OS account Tenant runs as, not by this list.
func resolveOSRoots(pf *pluginFlags, cfgDir, dataDir string, lc *launchConfig) osys.Roots {
	split := func(v string) []string {
		var out []string
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	deny := append(osys.DefaultDeny(), split(osFlagsOf(pf).denyRoots)...)

	write := split(osFlagsOf(pf).writeRoots)
	if len(write) == 0 {
		seen := map[string]bool{}
		add := func(p string) {
			if p == "" || seen[strings.ToLower(p)] {
				return
			}
			seen[strings.ToLower(p)] = true
			write = append(write, p)
		}
		add(cfgDir)
		add(dataDir)
		if home, err := os.UserHomeDir(); err == nil {
			for _, sub := range []string{"Desktop", "Documents", "Downloads"} {
				add(filepath.Join(home, sub))
			}
		}
		if tmp := os.TempDir(); tmp != "" {
			add(tmp)
		}
	}
	protect, secret := fileToolGuards(cfgDir, dataDir, pf, lc)
	return osys.Roots{Deny: deny, Write: write, Protect: protect, Secret: secret}
}

// fileToolGuards lists what the file tools may never write (protect) and may
// never read or write (secret), on any surface and whatever the write mode.
// Tenant writes all of it in-process, and no agent flow needs the tools for
// any of it.
//
// Protected, because Tenant applies an edit without asking, or runs it:
//   - settings.<agent>.json, for every agent sharing the config dir, and its
//     fixed-name temp file: permission modes, tool rules and exec profiles,
//     re-read within seconds (settingswatch.go);
//   - soul/: the persona in every prompt, which is the operator's alone;
//   - peer-cert.pem: this instance's peer identity;
//   - the claudecode spend ledger: the weekly budget rail;
//   - the Tenant binary;
//   - on Windows, the PowerShell profile and module folders under Documents,
//     which run in the operator's next PowerShell session.
//
// Secret, because a turn could send what they hold anywhere:
//   - config.json: the dashboard's auth token, beside cron jobs, MCP servers,
//     providers and relay and iMessage permissions (persistLaunchConfig folds
//     a hand edit into the next in-process save);
//   - every file on managedSecrets, the authoritative list doctor audits too
//     (TEN-337): credentials.json with every API key and skill secret; the
//     dashboard-auth-token; peers.json and the peer key and cert, who may call
//     in and with what token; the OAuth token files (Atlassian, GitHub, X in
//     the data dir, and the mcp/ caches) and the gsuite OAuth client;
//   - the gsuite service-account key and the dashboard TLS key, when set;
//   - the claude CLI's config ($CLAUDE_CONFIG_DIR, else ~/.claude and
//     ~/.claude.json): its login, and the hooks and MCP servers it runs;
//   - the credential stores and browser profiles the exec sandbox denies to
//     confined commands (credentialStoreRoots).
func fileToolGuards(cfgDir, dataDir string, pf *pluginFlags, lc *launchConfig) (protect, secret []string) {
	add := func(list *[]string, paths ...string) {
		for _, p := range paths {
			if p = strings.TrimSpace(p); p != "" {
				*list = append(*list, expandHome(p))
			}
		}
	}
	if cfgDir != "" {
		settings := settingsPath(cfgDir, "*")
		peerCert, _ := peering.CertPaths(cfgDir)
		add(&protect, settings, settings+".tmp", soul.SoulDir(cfgDir), peerCert)
		// The mcp dir as a whole also covers caches created after this list.
		add(&secret, launchConfigPath(cfgDir), mcpCacheDir(cfgDir))
	}
	// Every file on the authoritative secret list, config and data dirs both
	// (TEN-337). A listing error still leaves every other entry.
	managed, _ := managedSecrets(cfgDir, dataDir)
	for _, m := range managed {
		add(&secret, m.path)
	}
	add(&protect, claudecode.LedgerPath(), exePath())
	home, homeErr := os.UserHomeDir()
	if homeErr == nil && runtime.GOOS == "windows" {
		add(&protect, filepath.Join(home, "Documents", "WindowsPowerShell"), filepath.Join(home, "Documents", "PowerShell"))
	}
	if pf != nil {
		add(&secret, gsuiteFlagsOf(pf).saJSON)
	}
	if lc != nil {
		if s := lc.Skills["gsuite"]; s != nil {
			add(&secret, s.Settings["sa_json"])
		}
		add(&secret, lc.Dashboard.TLSKey)
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		add(&secret, dir)
	} else if homeErr == nil {
		add(&secret, filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json"))
	}
	add(&secret, credentialStoreRoots()...)
	return protect, secret
}
