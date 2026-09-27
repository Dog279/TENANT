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
	"tenant/internal/plugins/atlassian"
	"tenant/internal/plugins/crm"
	"tenant/internal/plugins/discord"
	githubp "tenant/internal/plugins/github"
	"tenant/internal/plugins/gsuite"
	"tenant/internal/plugins/imessage"
	"tenant/internal/plugins/osys"
	sqlp "tenant/internal/plugins/sql"
	"tenant/internal/plugins/web"
	"tenant/internal/plugins/wiki"
	xp "tenant/internal/plugins/x"
)

// pluginFlags collects the enable/config flags for every plugin. All
// default OFF (memory-only chat). Dangerous actions (write/send/post)
// default OFF even when a plugin is enabled — the operator opts in.
type pluginFlags struct {
	wikiDir string

	sqlDB         string
	sqlAllowWrite bool

	web               bool
	webShow           bool
	webAllowInteract  bool
	webPrivateNetwork []string

	gsuite          bool
	gsuiteAuth      string
	gsuiteSAJSON    string
	gsuiteSubject   string
	gsuiteAllowSend bool

	atlassian           bool
	atlassianSite       string
	atlassianEmail      string
	atlassianProject    string
	atlassianAllowWrite bool
	atlassianClientID   string
	atlassianCallback   string

	github           bool
	githubRepo       string
	githubAllowWrite bool
	githubAPIBase    string

	mcpRemotes          []string // --mcp-remote (repeatable): remote MCP server URLs
	mcpTrustAnnotations bool
	mcpAllowWrite       bool
	mcpCallback         string

	x          bool
	xBearer    string
	xAllowPost bool

	imsg          bool
	imsgURL       string
	imsgPass      string
	imsgPrivate   bool
	imsgAllowSend bool

	discord          bool
	discordToken     string
	discordAllowSend bool

	osEnable     bool
	osAllowExec  bool
	osAllowWrite bool
	osWriteRoots string
	osDenyRoots  string

	crmToolPath    string
	crmAllowMutate bool
}

func bindPluginFlags(fs *flag.FlagSet) *pluginFlags {
	p := &pluginFlags{}
	fs.StringVar(&p.wikiDir, "wiki-dir", "", "enable the wiki plugin over this markdown dir")
	fs.StringVar(&p.sqlDB, "sql-db", "", "enable the sql plugin over this SQLite file")
	fs.BoolVar(&p.sqlAllowWrite, "sql-allow-write", false, "permit SQL INSERT/UPDATE/DELETE")
	fs.BoolVar(&p.web, "web", false, "enable the web plugin (drives Chrome)")
	fs.BoolVar(&p.webShow, "web-show", false, "run a visible Chrome window")
	fs.BoolVar(&p.webAllowInteract, "web-allow-interact", false, "permit web click/fill/select")
	fs.Func("web-private-network", "allow an exact named private endpoint (repeatable; explicit SSRF-policy opt-in)", func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return fmt.Errorf("web-private-network hostname cannot be empty")
		}
		p.webPrivateNetwork = append(p.webPrivateNetwork, s)
		return nil
	})
	fs.BoolVar(&p.gsuite, "gsuite", false, "enable Gmail + Calendar")
	fs.StringVar(&p.gsuiteAuth, "gsuite-auth", "gcloud", "gsuite auth: gcloud|sa")
	fs.StringVar(&p.gsuiteSAJSON, "gsuite-sa-json", "", "gsuite service-account JSON (auth=sa)")
	fs.StringVar(&p.gsuiteSubject, "gsuite-subject", "", "gsuite impersonated user (auth=sa)")
	fs.BoolVar(&p.gsuiteAllowSend, "gsuite-allow-send", false, "permit gmail_send/calendar_create")
	fs.BoolVar(&p.atlassian, "atlassian", false, "enable Jira tools (Atlassian)")
	fs.StringVar(&p.atlassianSite, "atlassian-site", "", "Atlassian site URL, e.g. https://you.atlassian.net (Path A)")
	fs.StringVar(&p.atlassianEmail, "atlassian-email", "", "Atlassian account email (Path A; token via $ATLASSIAN_TOKEN)")
	fs.StringVar(&p.atlassianProject, "atlassian-project", "", "default Jira project key (e.g. TEN)")
	fs.BoolVar(&p.atlassianAllowWrite, "atlassian-allow-write", false, "permit jira_create/comment/transition (writes to the board)")
	fs.StringVar(&p.atlassianClientID, "atlassian-client-id", "", "Atlassian OAuth app client id (Path B; secret via $ATLASSIAN_CLIENT_SECRET, login via `tenant atlassian login`)")
	fs.StringVar(&p.atlassianCallback, "atlassian-oauth-callback", "", "OAuth callback bind addr (default 127.0.0.1:8765)")

	fs.BoolVar(&p.github, "github", false, "enable GitHub tools (control plane: branches/PRs/issues/CI)")
	fs.StringVar(&p.githubRepo, "github-repo", "", "GitHub repo this agent may touch, owner/name (single-repo by design)")
	fs.BoolVar(&p.githubAllowWrite, "github-allow-write", false, "permit github_create_branch/issue/comment/pr (writes to the repo)")
	fs.StringVar(&p.githubAPIBase, "github-api-base", "", "GitHub Enterprise API base, e.g. https://ghe.example.com/api/v3/")
	// Deliberately NO --github-token flag: a token passed as a flag lands in
	// shell history, ps output, and the scheduled-task XML. The token comes from
	// $GITHUB_TOKEN, /configure github, or the dashboard keys page.
	fs.Func("mcp-remote", "remote MCP server URL to connect as a client (repeatable; OAuth2.1+DCR browser sign-in via `/enable`)", func(s string) error {
		p.mcpRemotes = append(p.mcpRemotes, s)
		return nil
	})
	fs.BoolVar(&p.mcpTrustAnnotations, "mcp-trust-annotations", false, "trust remote servers' read-only tool annotations (relax the deny-by-default gate)")
	fs.BoolVar(&p.mcpAllowWrite, "mcp-allow-write", false, "permit remote MCP write tools without per-action confirm")
	fs.StringVar(&p.mcpCallback, "mcp-callback", "", "localhost OAuth callback for --mcp-remote (default 127.0.0.1:8765)")
	fs.BoolVar(&p.x, "x", false, "enable X/Twitter")
	fs.StringVar(&p.xBearer, "x-bearer", "", "X app bearer token (or $X_BEARER_TOKEN)")
	fs.BoolVar(&p.xAllowPost, "x-allow-post", false, "permit x_post/x_delete")
	fs.BoolVar(&p.imsg, "imessage", false, "enable iMessage (native chat.db on macOS; --bb-url switches to BlueBubbles)")
	fs.StringVar(&p.imsgURL, "bb-url", "", "BlueBubbles URL (or $BLUEBUBBLES_URL)")
	fs.StringVar(&p.imsgPass, "bb-password", "", "BlueBubbles password (or $BLUEBUBBLES_PASSWORD)")
	fs.BoolVar(&p.imsgPrivate, "bb-private-api", false, "use BlueBubbles private-api send method")
	fs.BoolVar(&p.imsgAllowSend, "imessage-allow-send", false, "permit imessage_send/new_chat")
	fs.BoolVar(&p.discord, "discord", false, "enable the Discord plugin (REST: read/send/react)")
	fs.StringVar(&p.discordToken, "discord-bot-token", "", "Discord bot token (or $DISCORD_BOT_TOKEN)")
	fs.BoolVar(&p.discordAllowSend, "discord-allow-send", false, "permit discord_send_message/discord_react (posts publicly as the bot)")
	fs.BoolVar(&p.osEnable, "os", false, "enable the OS plugin (sysinfo, file read, dir list, processes, gated shell exec)")
	fs.BoolVar(&p.osAllowExec, "os-allow-exec", false, "permit os_exec (run shell commands) — destructive ones still need confirm")
	fs.BoolVar(&p.osAllowWrite, "os-allow-write", false, "permit os_write/os_edit/os_append/os_make_dir (file writes)")
	fs.StringVar(&p.osWriteRoots, "os-write-roots", "", "comma-separated directories the file tools may WRITE to (default: Tenant's config and data dirs, the operator's Desktop/Documents/Downloads, and the temp dir). Tenant's own control and secret files stay off limits either way.")
	fs.StringVar(&p.osDenyRoots, "os-deny-roots", "", "comma-separated directories the file tools may never touch, read OR write (default: Windows/Program Files/ProgramData)")
	fs.StringVar(&p.crmToolPath, "crm-tool-path", "", "path to the external crm-tool binary (or $CRM_TOOL_PATH) — enables the gated crm_* tools")
	fs.BoolVar(&p.crmAllowMutate, "crm-allow-mutate", false, "permit the gated crm subcommands (ask/align/commitments-list) without per-action confirm")
	return p
}

// buildToolMux constructs every enabled plugin and returns the merged
// dispatcher + a cleanup func (closes browser/db handles). An enabled
// plugin that fails to construct returns an error — better to tell the
// operator their flag is wrong than to silently drop the tool.
func buildToolMux(ctx context.Context, c *commonFlags, router *model.Router, pf *pluginFlags, confirm func(context.Context, string, string) bool, log *slog.Logger) (*toolMux, *wiki.Index, func(), error) {
	mux := newToolMux()
	// wikiIx is captured here so callers (cmdTUI / research auto-deposit)
	// can force a reindex after writeWikiReport without having to chase
	// the dispatcher's unexported `ix` field. nil when --wiki-dir is
	// unset. TEN-44.
	var wikiIx *wiki.Index
	fail := func(err error) (*toolMux, *wiki.Index, func(), error) {
		mux.Close()
		return nil, nil, nil, err
	}
	// When an approval broker is wired (TUI), it is the single decision
	// point: per-action gate flags drop to false so every dangerous action
	// flows through Confirm, and the broker's category modes decide. The
	// auth/scope-level flags (gsuite/x Config) are untouched — they pick
	// OAuth scopes, a separate concern from per-action approval.
	gateAllow := func(flag bool) bool {
		if confirm != nil {
			return false
		}
		return flag
	}

	if pf.sqlDB != "" {
		db, err := sqlp.Open(sqlp.Config{Driver: "sqlite", DSN: pf.sqlDB})
		if err != nil {
			return fail(fmt.Errorf("sql plugin: %w", err))
		}
		mux.AddCleanup(func() { _ = db.Close() })
		mux.Add("sql", sqlp.NewDispatcher(db, sqlp.Policy{AllowWrite: pf.sqlAllowWrite, Confirm: confirm}))
	}

	if pf.wikiDir != "" {
		ix, err := buildWikiIndex(ctx, c, router, pf.wikiDir, log)
		if err != nil {
			return fail(err)
		}
		mux.Add("wiki", wiki.NewDispatcher(ix))
		// Gated local-write counterpart so a fetched peer note (or research) can
		// be ingested into the wiki on the user's direction. (TEN-244)
		mux.Add("wiki", newWikiSaveDispatcher(pf.wikiDir, ix, confirm))
		wikiIx = ix // exported via the return tuple for TEN-44 auto-reindex
	}

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

	if pf.web {
		cfg := webConfig(c.cfgDir, pf, c.lc)
		cfg.ReaderObserver = func(a web.ReaderActivity) {
			if log != nil {
				log.Info("web reader remote request", "provider", a.Provider, "target_host", a.TargetHost)
			}
		}
		sess, err := web.NewSession(cfg)
		if err != nil {
			return fail(fmt.Errorf("web plugin: %w (is Chrome installed?)", err))
		}
		mux.AddCleanup(func() { _ = sess.Close() })
		mux.Add("web", web.NewDispatcher(sess, web.Policy{AllowInteract: gateAllow(pf.webAllowInteract), Confirm: confirm},
			filepath.Join(c.dataDir, "screenshots")))
	}

	if pf.gsuite {
		gcfg := gsuite.Config{Auth: pf.gsuiteAuth, Subject: pf.gsuiteSubject, AllowSend: pf.gsuiteAllowSend}
		if pf.gsuiteAuth == "sa" {
			if pf.gsuiteSAJSON == "" {
				return fail(fmt.Errorf("gsuite plugin: --gsuite-auth sa needs --gsuite-sa-json"))
			}
			b, err := os.ReadFile(pf.gsuiteSAJSON)
			if err != nil {
				return fail(fmt.Errorf("gsuite plugin: read sa json: %w", err))
			}
			gcfg.SAJSON = b
		}
		svc, err := gsuite.Open(gcfg)
		if err != nil {
			return fail(fmt.Errorf("gsuite plugin: %w", err))
		}
		mux.Add("gsuite", gsuite.NewDispatcher(svc, gsuite.Policy{AllowSend: gateAllow(pf.gsuiteAllowSend), Confirm: confirm}))
	}

	if pf.atlassian {
		svc, err := atlassian.Open(ctx, atlassian.Config{
			SiteURL:       pf.atlassianSite,
			Email:         pf.atlassianEmail,
			Project:       pf.atlassianProject,
			ClientID:      pf.atlassianClientID,
			TokenPath:     atlassianTokenPath(c.cfgDir),
			OAuthCallback: pf.atlassianCallback,
		})
		if err != nil {
			return fail(fmt.Errorf("atlassian plugin: %w", err))
		}
		mux.Add("atlassian", atlassian.NewDispatcher(svc, atlassian.Policy{AllowWrite: gateAllow(pf.atlassianAllowWrite), Confirm: confirm}))
	}

	if pf.github {
		repos, rerr := githubp.ParseRepos(pf.githubRepo)
		if rerr != nil {
			return fail(fmt.Errorf("github plugin: %w", rerr))
		}
		svc, err := githubp.Open(githubp.Config{
			Repos:   repos,
			Token:   githubp.ResolveToken("", c.cfgDir),
			APIBase: pf.githubAPIBase,
		})
		if err != nil {
			return fail(fmt.Errorf("github plugin: %w", err))
		}
		mux.Add("github", githubp.NewDispatcher(svc, githubp.Policy{AllowWrite: gateAllow(pf.githubAllowWrite), Confirm: confirm}))
	}

	if pf.x {
		bearer := resolveXBearer(c.cfgDir, pf.xBearer)
		svc, err := xp.Open(xp.Config{Bearer: bearer, TokenPath: xp.TokenPath(c.dataDir), AllowPost: pf.xAllowPost})
		if err != nil {
			return fail(fmt.Errorf("x plugin: %w", err))
		}
		mux.Add("x", xp.NewDispatcher(svc, xp.Policy{AllowPost: gateAllow(pf.xAllowPost), Confirm: confirm}))
	}

	if pf.imsg {
		url, pass := pf.imsgURL, pf.imsgPass
		if url == "" {
			url = os.Getenv("BLUEBUBBLES_URL")
		}
		if pass == "" {
			pass = os.Getenv("BLUEBUBBLES_PASSWORD")
		}
		pol := imessage.Policy{AllowSend: gateAllow(pf.imsgAllowSend), Confirm: confirm}
		// Transport selection: a BlueBubbles URL (flag or env) is an
		// explicit opt-in to the server bridge; otherwise the default is the
		// native macOS transport (chat.db read + AppleScript send, no
		// server). OpenNative returns a "macOS only" error off darwin.
		if url != "" {
			svc, err := imessage.Open(imessage.Config{URL: url, Password: pass, PrivateAPI: pf.imsgPrivate})
			if err != nil {
				return fail(fmt.Errorf("imessage plugin: %w", err))
			}
			mux.Add("imessage", imessage.NewDispatcher(svc, pol))
		} else {
			nat, err := imessage.OpenNative(imessage.NativeConfig{})
			if err != nil {
				return fail(fmt.Errorf("imessage plugin: %w", err))
			}
			mux.AddCleanup(func() { _ = nat.Close() })
			mux.Add("imessage", imessage.NewDispatcher(nat, pol))
		}
	}

	if pf.discord {
		token := pf.discordToken
		if token == "" {
			token = os.Getenv("DISCORD_BOT_TOKEN")
		}
		svc, err := discord.Open(discord.Config{Token: token})
		if err != nil {
			return fail(fmt.Errorf("discord plugin: %w", err))
		}
		mux.Add("discord", discord.NewDispatcher(svc, discord.Policy{AllowSend: gateAllow(pf.discordAllowSend), Confirm: confirm}))
	}

	// OS needs no config, so always register it — it's visible in
	// /tools and `/enable os` activates it instantly at runtime. Starts
	// enabled only if --os was passed (else registered-but-disabled, so
	// it costs nothing until you turn it on).
	// os_exec runs through the execution-backend seam (execprofiles.go): the
	// turn's surface picks its exec profile per call, the default "host" profile
	// is the pre-seam behaviour, and a sandbox refusal escalates through the same
	// origin-routed confirm as every other gated action.
	if svc, err := osys.Open(osys.Config{Runner: sandboxRunner(c.cfgDir, c.dataDir, confirm)}); err == nil {
		mux.Add("os", osys.NewDispatcher(svc, osys.Policy{
			AllowExec:  gateAllow(pf.osAllowExec),
			AllowWrite: gateAllow(pf.osAllowWrite),
			Confirm:    confirm,
			Roots:      resolveOSRoots(pf, c.cfgDir, c.dataDir, c.lc),
		}))
		if !pf.osEnable {
			mux.SetEnabled("os", false)
		}
	}

	// CRM wraps the operator's external crm-tool binary (TEN-269). It needs
	// a configured path (flag or $CRM_TOOL_PATH) — without one we register
	// nothing here and the stub catalog below advertises the tools + how to
	// enable them. Read subcommands are ungated; the heavier set
	// (ask/align/commitments-list) is gated through the confirm broker.
	if crmPath := strings.TrimSpace(firstNonEmpty(pf.crmToolPath, os.Getenv("CRM_TOOL_PATH"))); crmPath != "" {
		svc, err := crm.Open(crm.Config{Path: crmPath})
		if err != nil {
			return fail(fmt.Errorf("crm plugin: %w", err))
		}
		mux.Add("crm", crm.NewDispatcher(svc, crm.Policy{AllowMutate: gateAllow(pf.crmAllowMutate), Confirm: confirm}))
	}

	// Every shared plugin the launch path did not build gets a disabled stub
	// (and its activator), declared in its descriptor (plugin_<id>.go).
	registerPluginStubs(ctx, mux, &pluginEnv{c: c, pf: pf, confirm: confirm, log: log})

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
		AllowWrite:       gateAllow(pf.mcpAllowWrite),
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

	return mux, wikiIx, mux.Close, nil
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

	deny := append(osys.DefaultDeny(), split(pf.osDenyRoots)...)

	write := split(pf.osWriteRoots)
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
		add(&secret, pf.gsuiteSAJSON)
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
