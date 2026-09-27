package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"tenant/internal/agent"
	"tenant/internal/cron"
	"tenant/internal/dashboard"
	"tenant/internal/improve"
	"tenant/internal/lifecycle"
	"tenant/internal/logs"
	"tenant/internal/memory/compress"
	"tenant/internal/memory/distill"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/sme"
	"tenant/internal/memory/soul"
	usagestore "tenant/internal/memory/usage"
	"tenant/internal/memory/userprofile"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/orchestra"
	cronplugin "tenant/internal/plugins/cron"
	"tenant/internal/plugins/web"
)

// cmdServe runs Tenant as a headless 24/7 hub (TEN-194): the SAME long-term
// brain the TUI drives — full tool surface, memory, self-improvement, cron,
// federation peer listener, comms relays — but with NO terminal UI. The
// operator "hops in" through the web dashboard (always on in serve mode), which
// is the live control + chat + approval surface; an interactive terminal attach
// is a follow-up (TEN-194b).
//
// This assembly intentionally MIRRORS cmdTUI's substrate wiring
// (commands.go) minus the bubbletea seam and the terminal-only adapters. The
// pure DRY extraction of a shared buildHub() both paths call is TEN-194a; until
// then keep the agent.Config, the degraded-suppression decisions, and the
// improve-scheduler registration here in sync with cmdTUI. iMessage responder
// and the tailscale re-assert are TUI-resident today and are picked up by the
// TEN-194a extraction; the Discord relay (opt-in) covers the away-from-desk
// drive case in v1.
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	c := bindCommon(fs)
	pf := bindPluginFlags(fs)
	selfImprove := fs.Bool("self-improve", true, "run background self-improvement (distill/consolidate/profile/skills)")
	distillEvery := fs.Duration("distill-every", 10*time.Minute, "distillation + skill-induction cadence")
	profileEvery := fs.Duration("profile-every", 15*time.Minute, "user-profile re-synthesis cadence (LLM call)")
	evalEvery := fs.Duration("eval-every", 0, "nightly eval cadence (0=off; e.g. 24h); needs --self-improve")
	dashboardOn := fs.Bool("dashboard", true, "serve the web control panel (the hub's control + chat + approval surface)")
	dashboardAddr := fs.String("dashboard-addr", "", "dashboard listen address (default 127.0.0.1:8770)")
	allowNoMemory := fs.Bool("allow-no-memory", false, "start even when semantic memory (embeddings) is down — runs amnesiac (no real vector recall)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	applyPluginConfig(c, pf)
	expandPluginPaths(pf)
	// Semantic memory is a core pillar (TEN-254): a 24/7 hub running amnesiac is
	// worse than not running. Refuse to start when the embedder is down unless the
	// operator opts out (--allow-no-memory, or --backend echo for offline dev).
	if !*allowNoMemory && c.backend != "echo" && !semanticMemoryReady(ctx, c) {
		return memoryDownError(c)
	}

	// Daemon logging goes to stderr (the supervisor — launchd/systemd —
	// captures it); there is no alt-screen to protect. A serve-mode feed sink
	// (notify/sysCh) routes every status line to the log so an operator reading
	// `journalctl`/the log file sees setup, health, fallback, cron, and relay
	// activity exactly as the TUI would surface it.
	// The unified log book (internal/logs): every subsystem's lines are filed
	// under <data>/logs/<source>/<day>.log and retained for the dashboard feed;
	// stderr keeps the TENANT_LOG-filtered stream the supervisor captures,
	// exactly as before (the hub handler tees to it).
	hub, log, herr := openLogHub(c, slog.NewTextHandler(logs.ScrubWriter(os.Stderr), &slog.HandlerOptions{Level: logLevel()}))
	if herr != nil {
		log.Warn("logs: " + herr.Error() + " — logging to memory only")
	}
	scrubLogSecrets(hub, c) // TEN-361: credentials never reach a log line
	defer hub.Close()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	hub.Start(ctx)
	startRetention(ctx, c, log.With("source", "privacy"))

	// Graceful, ordered teardown (TEN-194), in stages (internal/lifecycle):
	// deny pending approvals so no gated turn stays blocked; STOP ACCEPTING NEW
	// WORK so nothing starts a turn or a job mid-teardown; drain in-flight
	// work; then close what it used, last opened first — no goroutine may
	// write to a closed store. Each component registers its stop where it is
	// built; an early return runs whatever is registered so far.
	life := lifecycle.New(log.With("source", "shutdown"))
	defer life.Shutdown()
	// Crash capture and the boot record (TEN-365): its clean stop is the
	// first Release step registered, so it runs last.
	onPanic := startBootRecord(hub, life, "serve")
	defer onPanic()

	// Status feed. Whatever else writes to sysCh (the improve feed, the
	// reconnect monitor) logs its line itself.
	sysCh := make(chan string, 64)
	pushSys := func(s string) { feedLine(hub, sysCh, "system", s) }
	signInNotice = pushSys // a sign-in URL reaches the operator through the feed
	// Status feed → stdout (the supervisor's log). A daemon's setup/health/
	// dashboard/peer/iMessage/fallback/cron lines are the operator's only window
	// into "is it working", so route them at always-visible level rather than the
	// WARN-filtered slog (newLogger defaults to WARN).
	go func() {
		for {
			select {
			case s := <-sysCh:
				fmt.Println("• " + logs.Scrub(s))
			case <-ctx.Done():
				return
			}
		}
	}()

	for _, ln := range ensureSetup(c, pf) {
		pushSys("setup: " + ln)
	}
	for _, ln := range healthCheck(ctx, c) {
		pushSys(ln)
	}

	// Resilient launch: degrade to echo rather than abort if the model can't be
	// built, so the hub still comes up and the operator can recover via the
	// dashboard's model page. `degraded` suppresses autonomous work on echo.
	router, degraded, degradedBanner, err := buildRouterResilient(c, log.With("source", "model"))
	if err != nil {
		return err
	}
	if degraded.Degraded() {
		pushSys(degradedBanner)
		c.backend = "echo"
		pushSys("echo: responses are deterministic stubs — not a real model.")
	}

	st, closeStores, err := openStores(c)
	if err != nil {
		return err
	}
	life.AddFunc(lifecycle.Release, "stores", closeStores)

	usageStore, uerr := usagestore.Open(filepath.Join(c.dataDir, "usage.db"))
	if uerr != nil {
		log.Warn("usage ledger unavailable", "err", uerr)
	} else {
		life.Add(lifecycle.Release, "usage ledger", usageStore.Close)
	}

	// Approval broker: the single decision point for dangerous actions. Seeded
	// from --allow-* flags, overridden by persisted per-category modes. In serve
	// there is no TUI to prompt, so a headless drain (below) routes "ask"-mode
	// requests to the dashboard.
	broker := newApprovalBroker(log.With("source", "approvals"))
	// Deny every still-pending approval first, so no gated turn goroutine
	// stays blocked on its reply during teardown (TEN-203).
	life.AddFunc(lifecycle.Unblock, "pending approvals", broker.DenyAll)
	seedFromFlags(broker, pf)
	stg, serr := loadSettings(c.cfgDir, c.agent)
	if serr != nil {
		pushSys("settings: " + serr.Error() + " — using defaults")
	}
	broker.LoadModes(stg.Permissions)
	var stgMu sync.Mutex
	saveSettings := func() {
		stgMu.Lock()
		defer stgMu.Unlock()
		saveSettingsOrTell(stg, c.cfgDir, c.agent, log, pushSys)
	}
	broker.SetPersist(func(modes map[string]string) {
		stgMu.Lock()
		stg.Permissions = modes
		stgMu.Unlock()
		saveSettings()
	})
	// Per-tool permission rules (internal/approval): loaded from the same file,
	// persisted on every /permissions add|rm, and re-read when the file is
	// edited by hand — so a rule for a tool nobody programmed for is one line
	// in settings.<agent>.json away, with no restart.
	if n, skipped := defaultToolRules.Load(stg.ToolRules); skipped > 0 {
		pushSys(fmt.Sprintf("permissions: %d tool rule(s) loaded, %d invalid skipped", n, skipped))
	}
	defaultToolRules.SetPersist(func(rules map[string]string) {
		stgMu.Lock()
		stg.ToolRules = rules
		stgMu.Unlock()
		saveSettings()
	})
	// Execution-sandbox profiles (execprofiles.go): same file, re-read on edit.
	if n, skipped := defaultExecProfiles.Load(stg.ExecProfiles); n > 0 || len(skipped) > 0 {
		msg := fmt.Sprintf("sandbox: %d exec profile(s) active", n)
		if len(skipped) > 0 {
			msg += fmt.Sprintf(", %d invalid skipped: %s", len(skipped), strings.Join(skipped, "; "))
		}
		pushSys(msg)
	}
	go watchSettings(ctx, c.cfgDir, c.agent, broker, defaultToolRules, &stgMu, stg, pushSys)

	// Web is per-agent (own browser each), kept out of the shared mux.
	teamWeb := webFlagsOf(pf).on
	webCfg := webConfig(c.cfgDir, pf, c.lc)
	webPolicy := web.Policy{AllowInteract: webFlagsOf(pf).allowInteract}
	webFlagsOf(pf).on = false

	egressConfirm := originConfirm(broker.Confirm)
	mux, wikiIx, cleanupMux, err := buildToolMux(ctx, c, router, pf, egressConfirm, log)
	if err != nil {
		return err
	}
	life.AddFunc(lifecycle.Release, "tool mux", cleanupMux)

	skillStore, serr := skills.Open(filepath.Join(c.dataDir, "skills.db"))
	if serr != nil {
		return serr
	}
	life.Add(lifecycle.Release, "skills store", skillStore.Close)

	prof, _ := userprofile.Load(c.dataDir, c.agent)
	profSynth := &userprofile.Synthesizer{Router: router, Semantic: st.semantic, AgentID: c.agent}
	var profMu sync.Mutex
	refreshProfile := func(ctx context.Context) (bool, error) {
		profMu.Lock()
		defer profMu.Unlock()
		md, changed, err := profSynth.Run(ctx, prof)
		if err != nil || !changed {
			return changed, err
		}
		prof.Update(md)
		return true, prof.Save(c.dataDir)
	}
	noteProfile := func(fact string) {
		profMu.Lock()
		defer profMu.Unlock()
		prof.AppendRemembered(fact)
		if err := prof.Save(c.dataDir); err != nil {
			log.Warn("profile note save failed", "err", err)
		}
	}

	skEmb, embProfile, _ := router.EmbedderForRole(ctx, model.RoleEmbedder)
	embedderID := string(embProfile.ID)
	mux.SetEmbedder(embedderID, skEmb)

	// Auto-fallback (TEN-246): wire the configured chain + surface failovers.
	if err := installRouteBigRole(router, c.cfgDir, c.lc, c.planCeiling, log.With("source", "route")); err != nil {
		return err
	}
	installFallbackChain(router, c.cfgDir, c.lc, c.planCeiling, log.With("source", "model"))
	router.SetFailoverObserver(func(ev model.FailoverEvent) {
		from := fallbackLabelToProvider(ev.From, c.lc.Provider)
		to := fallbackLabelToProvider(ev.To, c.lc.Provider)
		msg := fmt.Sprintf("⚠ model fallback: %s %s → using %s", from, ev.Reason, to)
		if errors.Is(ev.Err, model.ErrInsufficientBalance) {
			msg += " (recharge to restore)"
		}
		pushSys(msg)
	})

	for _, note := range mux.Restore(stg.Tools) {
		pushSys("settings: " + note)
	}
	mux.SetOnChange(func(snap map[string]bool) {
		stgMu.Lock()
		stg.Tools = snap
		stgMu.Unlock()
		saveSettings()
	})

	meta, merr := improve.OpenMeta(filepath.Join(c.dataDir, "tenant_meta.db"))
	if merr != nil {
		return merr
	}
	life.Add(lifecycle.Release, "improve meta", meta.Close)
	distiller := &distill.Distiller{Router: router, Episodic: st.episodic, Semantic: st.semantic, AgentID: c.agent, Logger: log.With("source", "improve")}
	distillJob := improve.NewDistillJob(distiller, meta, c.agent)

	soulLive := soul.NewLive(st.soul)
	mainWorking := working.New() // no session-resume seed: a daemon has no "last session"
	memCtl := memControl{
		episodic: st.episodic, semantic: st.semantic, skills: skillStore,
		embedder: skEmb, distill: distillJob, cfgDir: c.cfgDir, agentID: c.agent,
		dataDir: c.dataDir, embedderID: embedderID,
		soulLive:       soulLive,
		profile:        prof,
		profileRefresh: refreshProfile,
		working:        mainWorking,
	}

	budgets := configuredBudgets(c.lc)
	bus := orchestra.NewBusWithHistoryCap(budgets.BusHistoryCap)
	life.AddFunc(lifecycle.Release, "team bus", bus.Close)
	bus.Register(c.agent)
	compressor := &compress.Compressor{Router: router}

	// Event broker + the log book. emit fans every event to BOTH the live
	// broker (dashboard chat/feed) AND the hub, which files it under its source
	// folder (turns / subagents / approvals / cron / …) and retains it for the
	// Activity backfill + the /api/activity projection.
	evBroker := agent.NewBroker(0)
	emit := func(ev agent.Event) {
		evBroker.Publish(ev)
		if e, ok := agent.EntryFor(ev); ok { // denylists token/usage/assistant/memory noise
			hub.Log(e)
		}
	}

	// Sub-agent + bus activity mirrored to the dashboard feed (attributed by
	// agent id). Serve has no terminal team-view channel, so — unlike the TUI's
	// subObserve — it omits EventUsage: there is no per-sub-agent token counter
	// to feed, only the dashboard activity stream.
	subObserve := func(id string, e agent.Event) {
		switch e.Kind {
		case agent.EventToolCall, agent.EventToolResult, agent.EventFinal, agent.EventError, agent.EventTeamBudget:
			ev := e
			ev.Agent = id
			emit(ev)
		}
	}

	_, embProf, _ := router.EmbedderForRole(ctx, model.RoleEmbedder)
	var initialAgentProfiles map[string]*agentProfile
	if lcInit, err := loadLaunchConfig(c.cfgDir); err == nil {
		initialAgentProfiles = effectiveAgents(lcInit)
	} else {
		initialAgentProfiles = effectiveAgents(nil)
	}
	localSave := func(snap map[string]bool) {
		stgMu.Lock()
		if stg.Tools == nil {
			stg.Tools = map[string]bool{}
		}
		for k, v := range snap {
			stg.Tools[k] = v
		}
		stgMu.Unlock()
		saveSettings()
	}
	// routeRT is built later in startup (it needs the fully-wired router); the
	// team only spawns once serve is running, so the late bind is safe. A nil
	// runtime degrades to today's exact spawn behavior.
	var routeRT *routeRuntime
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: mux, Skills: skillStore,
		Embedder: skEmb, EmbedderID: embedderID, Compressor: compressor, Profile: prof,
		OrchID: c.agent, Log: log, Observe: subObserve, Budgets: budgets,
		ConfirmSensitiveEgress: egressConfirm,
		Web:                    teamWeb, WebConfig: webCfg, WebPolicy: webPolicy,
		Shots:         filepath.Join(c.dataDir, "screenshots"),
		AgentProfiles: initialAgentProfiles,
		SpawnRole: func(ctx context.Context, task string) (model.Role, bool) { // TEN-292
			if routeRT == nil {
				return "", false
			}
			return routeRT.SpawnRole(ctx, task)
		},
		CfgDir:        c.cfgDir,
		EmbedProfile:  embProf,
		LocalRestore:  stg.Tools,
		LocalOnChange: localSave,
	})
	life.AddFunc(lifecycle.Release, "team runtime", rt.Close)

	mainLocal := newToolMux()
	mainLocal.Add("orchestrator", spawnTool{rt: rt})
	mainLocal.Add("team", orchestra.CommsTool{Bus: bus, Self: c.agent})
	mainLocal.Add("memory", memoryTool{sem: st.semantic, emb: skEmb, embedderID: embedderID, agentID: c.agent, note: noteProfile})
	mainLocal.Add("skills", skillTool{st: skillStore, emb: skEmb, agentID: c.agent})
	mainLocal.Add("recall", &recallTool{episodic: st.episodic, semantic: st.semantic, archive: st.archive.Reader(), emb: skEmb, embedderID: embedderID, agentID: c.agent})
	rt.addWebTool(mainLocal, c.agent)
	// Quality v2 Phase 1: count every dispatch. One shared stats set across
	// the shared mux and the main agent's local mux; cron's restricted views
	// and sub-agent shared-tool calls funnel through these same instances.
	stats := newToolStats(c.dataDir, "serve")
	mux.EnableStats(stats)
	mainLocal.EnableStats(stats)
	life.AddFunc(lifecycle.Release, "tool stats", stats.StartPersist(time.Minute))
	mainTools := composite{shared: mux, local: mainLocal, budget: rt.budget, agentID: c.agent}

	// Bus traffic → dashboard feed (lossless cursor over the retained log).
	go func() {
		cursor := 0
		for {
			select {
			case _, ok := <-bus.Notify():
				if !ok {
					return
				}
				var msgs []orchestra.Message
				msgs, cursor = bus.Since(cursor)
				for _, m := range msgs {
					to := m.To
					if m.Broadcast() {
						to = "team"
					}
					emit(agent.Event{Kind: agent.EventBus, Agent: m.From, Text: "→ " + to + ": " + m.Content})
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	sysPrompt := serveSystemPrompt(initialAgentProfiles, len(mainTools.All()) > 0)

	// Phase 3 SME (TEN-255): per-project doc store + live cache, injected into
	// the system reserve each turn (mirrors the TUI path). Rendered once now;
	// the ReflectionJob (registered in buildServeImprove) refreshes it.
	var smeStore *sme.Store
	smeLive := sme.NewLive()
	if st.semantic != nil {
		if s, serr := sme.New(st.semantic.DB()); serr == nil {
			smeStore = s
			if rendered, rerr := s.RenderActive(ctx, "", c.agent); rerr == nil && rendered != "" {
				smeLive.Set(rendered)
			}
		} else {
			log.Warn("sme: init failed; project SME disabled", "err", serr.Error())
		}
	}

	routeRT, err = newRouteRuntime(c.lc, c.cfgDir, c.dataDir, router, log.With("source", "route"))
	if err != nil {
		return err
	}
	if err := routeRT.BindConfiguredForce(); err != nil {
		return err
	}
	observeMain := func(e agent.Event) {
		if e.Kind == agent.EventUsage { // the usage ledger, as the TUI keeps it
			recordUsage(ctx, usageStore, c, e.PromptTokens, e.CompletionTokens, e.TraceID)
		}
		rt.ObserveMain(e, emit)
	}
	ag, err := agent.New(agent.Config{
		AgentID:  c.agent,
		Router:   router,
		SoulLive: soulLive,
		Working:  mainWorking,
		Archive:  st.archive,
		Episodic: st.episodic,
		Semantic: st.semantic,
		Runs:     st.runs,
		Tools:    mainTools,
		Dispatcher: &agent.RetryDecorator{
			Inner:      mainTools,
			Eligible:   agent.DefaultEligibleTransient,
			Backoff:    time.Second,
			MaxRetries: 1,
			Observer:   emit,
		},
		ConfirmSensitiveEgress: egressConfirm,
		Logger:                 log.With("source", "turns"),
		Stream:                 true,
		Observer:               observeMain,
		RouteHook:              routeRT.Hook(),
		RouteActive:            routeRT.Active,
		RouteSticky:            routeRT.Sticky,
		LazyToolLoad:           c.lc.LazyTools,
		Skills:                 skillRetriever{st: skillStore, agentID: c.agent, tools: mainTools},
		SystemPrompt:           sysPrompt,
		Compactor:              compressor,
		UserProfile:            prof,
		// Per-project SME doc from the live cache (Phase 3). nil-safe.
		RenderProjectSME: smeLive.String,
	})
	if err != nil {
		return err
	}
	memCtl.expand = ag.ExpandLatestCompaction

	// ONE turn gate over the shared brain (TEN-194): every driver (dashboard
	// now, peer-delegated run later) serializes here so concurrent sources can't
	// race the single working set. It also reports turn liveness to /api/status.
	turnSchedCfg := turnSchedulerConfig{}
	if c.lc != nil {
		turnSchedCfg = c.lc.TurnScheduler
	}
	gated := newTurnGateConfigured(ag, turnSchedCfg)

	// Cron (recurring jobs): dedicated read/comms-safe runner agents on a
	// schedule; cron_* management tools go to the MAIN agent.
	var (
		cronEngine  *cron.Engine
		dashCronCtl dashboard.CronControl
	)
	var cronCatchup bool
	cronLoc := time.Local
	cronExecGate := &execGate{}
	if c.lc != nil {
		cronExecGate.set(c.lc.Cron.AllowExec)
		cronCatchup = c.lc.Cron.Catchup
		if tz := strings.TrimSpace(c.lc.Cron.Timezone); tz != "" {
			if loc, lerr := time.LoadLocation(tz); lerr != nil {
				pushSys("cron: bad timezone " + tz + " — using server local time")
			} else {
				cronLoc = loc
			}
		}
	}
	if cronRunner, cerr := buildCronRunner(cronAgentDeps{
		router:       router,
		soulLive:     soulLive,
		runs:         st.runs,
		skills:       skillRetriever{st: skillStore, agentID: c.agent},
		compactor:    compressor,
		userProfile:  prof,
		fullReg:      mainTools,
		fullDisp:     mainTools,
		sysPrompt:    sysPrompt,
		log:          log.With("source", "cron"),
		routeHook:    routeRT.Hook(),
		routeActive:  routeRT.Active,
		routeSticky:  routeRT.Sticky,
		events:       emit,
		cfgDir:       c.cfgDir,
		dataDir:      c.dataDir,
		allowExec:    cronExecGate,
		authProblems: mcpAuthProblemsFn(mainTools), // banner on every summary while a connector's sign-in is dead
	}); cerr != nil {
		pushSys("cron: disabled (" + cerr.Error() + ")")
	} else {
		var cronDefs []cron.JobDef
		if c.lc != nil {
			for _, j := range c.lc.Cron.Jobs {
				cronDefs = append(cronDefs, cron.JobDef{
					ID: j.ID, Name: j.Name, Spec: j.Spec, Prompt: j.Prompt,
					Enabled: j.Enabled, Kind: j.Kind, Exec: j.Exec, TZ: j.TZ,
				})
			}
		}
		cronEngine = cron.NewEngine(cronDefs, cronRunner, func(defs []cron.JobDef) error {
			if c.lc == nil {
				return nil
			}
			jobs := make([]cronJobConfig, len(defs))
			for i, d := range defs {
				jobs[i] = cronJobConfig{
					ID: d.ID, Name: d.Name, Spec: d.Spec, Prompt: d.Prompt,
					Enabled: d.Enabled, Kind: d.Kind, Exec: d.Exec, TZ: d.TZ,
				}
			}
			return persistLaunchConfig(c.cfgDir, c.lc, func(lc *launchConfig) { lc.Cron.Jobs = jobs })
		})
		cronEngine.SetPaused(degraded.Degraded)
		cronEngine.Prime(cron.PrimeOptions{
			Location: cronLoc,
			Catchup:  cronCatchup,
			PerRun:   cronPerRun(c.lc),
			History:  loadCronHistory(c.dataDir),
			HistoryPersist: func(r cron.RunRecord) error {
				return appendCronHistory(c.dataDir, r)
			},
		})
		cronEngine.SetNotify(pushSys)
		cronMgr := newCronManager(ctx, cronEngine, cronExecGate, func(on bool) error {
			if c.lc == nil {
				return nil
			}
			return persistLaunchConfig(c.cfgDir, c.lc, func(lc *launchConfig) { lc.Cron.AllowExec = on })
		})
		mainLocal.Add("cron", cronplugin.NewDispatcher(cronMgr))
		dashCronCtl = dashCron{cronMgr}
		if serr := cronEngine.Start(ctx); serr != nil {
			pushSys("cron: scheduler did not start (" + serr.Error() + ")")
		}
		life.AddFunc(lifecycle.Drain, "cron", cronEngine.Stop) // drains any in-flight cron run
	}

	// Connector tools: the agent can adopt a tool server it built (mcp_add) —
	// gated by the "mcp" permission category; each connector's own calls then
	// follow its live trust mode (ask / allow / deny). Same control the TUI /mcp
	// and the dashboard's Remote services page drive.
	mcpCtl := newMCPControl(mux, c.cfgDir, c.lc)
	mainLocal.Add("connectors", newMCPToolPlugin(mcpCtl, mux.MCPConfirm()))

	// modelControl powers the dashboard model + key pages and re-points
	// tool-ranking at a new embedder after a /model swap.
	modelCtl := &modelControl{cfgDir: c.cfgDir, dataDir: c.dataDir, agentID: c.agent, ag: ag, log: log.With("source", "model"), degraded: degraded}
	modelCtl.reinstallEmbedder = func(ctx context.Context) {
		emb, prof, err := router.EmbedderForRole(ctx, model.RoleEmbedder)
		if err != nil {
			modelCtl.log.Warn("model: embedder not re-resolved after the swap; tool ranking keeps the old one", "err", err)
			return
		}
		mux.SetEmbedder(string(prof.ID), emb)
		mux.PrecomputeEmbeddings(ctx)
	}
	go watchCredentials(ctx, c.cfgDir, modelCtl, degraded, pushSys)

	// Approval fan-out (TEN-203): the broker itself owns the id-keyed pending
	// registry and IS the dashboard.ApprovalControl. Point its event emitter at
	// the live stream so pending/resolved fan out to the dashboard (and any
	// attach client); no drain goroutine is needed now that the requests channel
	// is a non-blocking notifier. Still-pending requests are denied first on
	// shutdown (the Unblock stage) so no gated turn stays blocked.
	broker.SetEvents(emit)

	dcfg, _ := resolveDashboardConfig(c.lc, true, *dashboardOn, *dashboardAddr)
	dcfg.Auth, err = resolveDashboardAuth(c.cfgDir, dcfg.Auth)
	if err != nil {
		return err
	}
	dashMgr := &dashboardManager{
		base:   ctx,
		cfg:    dcfg,
		runner: gated,
		tools:  dashTools{mainTools},
		mem:    dashMemory{memCtl},
		cron:   dashCronCtl,
		// Watched-repo list for the Integrations page. serve builds its OWN
		// dashboardManager, so anything wired only in cmdTUI is silently absent
		// here — which is exactly how this section came up empty the first time.
		githubRepos: dashGitHubRepos{c: githubTUIControl{cfgDir: c.cfgDir, lc: c.lc}},
		secrets:     dashKeys{cfgDir: c.cfgDir, mc: modelCtl},
		skills:      dashSkill{c: skillControl{st: skillStore, emb: skEmb, agentID: c.agent, cfgDir: c.cfgDir}},
		models:      dashModel{mc: modelCtl},
		approvals:   broker, // the broker is the ApprovalControl (Pending/Decide) — TEN-203
		rules:       dashRules{defaultToolRules},
		broker:      evBroker,
		logs:        hub,
		log:         log.With("source", "dashboard"),
		notify:      pushSys,
		persist: func(enabled bool) error {
			if c.lc == nil {
				return nil
			}
			return persistLaunchConfig(c.cfgDir, c.lc, func(lc *launchConfig) { lc.Dashboard.Enabled = &enabled })
		},
	}
	// Shutdown (not Disable) for the dashboard and the relay: stop WITHOUT
	// persisting an "off" choice, so the operator's enabled=true survives a
	// restart (a SIGTERM is not a /relay off or /dashboard off). Disable
	// persisting on every serve teardown was the bug — twice.
	life.Add(lifecycle.Intake, "dashboard", dashMgr.Shutdown)

	// Discord relay (TEN-114): opt-in, off by default; drives a DEDICATED relay
	// agent (own working set, shared memory) so it never touches the shared
	// brain's turn. The prime away-from-desk drive surface for a 24/7 hub.
	relayMgr := buildServeRelay(ctx, serveRelayDeps{
		c: c, router: router, soulLive: soulLive, stores: st, skills: skillStore,
		compressor: compressor, profile: prof, tools: mainTools, sysPrompt: sysPrompt,
		emit: emit, notify: pushSys, degraded: degraded, log: log.With("source", "discord"), routeHook: routeRT.Hook(),
		routeActive: routeRT.Active, routeSticky: routeRT.Sticky,
	})
	if relayMgr != nil {
		life.AddFunc(lifecycle.Intake, "discord relay", relayMgr.Shutdown) // stop Discord ingest
	}

	// Native iMessage responder (TEN-230): the away-from-desk drive surface a Mac
	// hub answers texts on. Auto-starts when left enabled; needs Full Disk Access
	// for chat.db under a daemon (a failure is a logged note, never fatal).
	imsgAllowMgr := buildServeIMessage(ctx, serveIMessageDeps{
		c: c, router: router, soulLive: soulLive, stores: st, skills: skillStore,
		compressor: compressor, profile: prof, tools: mainTools, sysPrompt: sysPrompt,
		meta: meta, broker: broker, degraded: degraded, emit: emit, notify: pushSys, log: log.With("source", "imessage"),
		routeHook: routeRT.Hook(), routeActive: routeRT.Active, routeSticky: routeRT.Sticky,
	})
	dashMgr.access = dashAccess{im: imsgAllowMgr, relay: relayMgr}

	// Self-improvement scheduler (preserves the old serve's distill+consolidate
	// and adds the full job set). Suspended while degraded.
	evalProg := &evalProgress{}
	sched, evalSched := buildServeImprove(ctx, serveImproveDeps{
		c: c, pf: pf, on: *selfImprove, router: router, distiller: distiller, distillJob: distillJob,
		skills: skillStore, embedder: skEmb, stores: st, sme: smeStore, smeLive: smeLive, refreshProfile: refreshProfile,
		degraded: degraded, feed: sysCh, distillEvery: *distillEvery, profileEvery: *profileEvery,
		evalEvery: *evalEvery, evalEverySet: fsVisited(fs, "eval-every"), log: log.With("source", "improve"),
		events: emit, evalProg: evalProg,
	})
	if sched != nil {
		life.AddFunc(lifecycle.Drain, "improve scheduler", sched.Stop) // drains any in-flight improve job
	}
	dashMgr.eval = dashEval{ev: evalTUIControl{sched: evalSched, cfgDir: c.cfgDir, dataDir: c.dataDir}, judge: judgeCtl{cfgDir: c.cfgDir, planner: c.vllmModel}, prog: evalProg}
	dashMgr.mcp = dashMCP{m: mcpCtl}
	dashMgr.integrations = dashIntegrations{c: newSkillCfgControl(c.cfgDir, skillKinds, mainTools.SetPluginEnabled)}

	dashAddr := ""
	if *dashboardOn {
		if addr, derr := dashMgr.Enable(); derr != nil {
			pushSys("dashboard: " + derr.Error())
		} else {
			dashAddr = addr
			pushSys(dashboardServingLine(addr, c.cfgDir, c.lc))
		}
	}

	// Re-assert a persisted `/tailscale serve` choice (TEN-233) now the dashboard
	// is up — publishes the loopback dashboard onto the tailnet so the operator
	// can reach the hub off-LAN, matching the TUI. Best-effort.
	if c.lc != nil && c.lc.Tailscale.Serve {
		tsMgr := newTailscaleManager(ctx, dashMgr, log.With("source", "dashboard"))
		if url, terr := tsMgr.reassertOnLaunch(); terr != nil {
			pushSys("tailscale: serve not restored — " + terr.Error())
		} else {
			pushSys("tailscale: dashboard republished to your tailnet at " + url)
		}
	}

	// Federation peer listener (TEN-184): paired peers reach this instance over
	// the live stores/broker. Inbound invites route through broker.AskPairing —
	// which, headless, is fronted by the same approval drain (the dashboard).
	peerHostName, _ := os.Hostname()
	if peerHostName == "" {
		peerHostName = "this tenant"
	}
	peerEmb, _, _ := router.EmbedderForRole(ctx, model.RoleEmbedder)
	// Peer liveness heartbeat (TEN-250): inbound OnAuth + background outbound
	// prober. Tracked headless too (the dashboard peer surface can read it later).
	peerHealth := newPeerHealthRegistry()
	go (&peerHealthMonitor{cfgDir: c.cfgDir, reg: peerHealth}).run(ctx)
	peerDeps := peerToolDeps{
		selfName: peerHostName,
		semantic: st.semantic,
		episodic: st.episodic,
		embedder: peerEmb,
		wiki:     wikiIx,
		onAuth:   peerHealth.markInbound,
		onGrant:  peerHealth.markInboundGrant,
	}
	if c.lc != nil && c.lc.Peer.Listen != "" {
		bound, stopFn, perr := startPeerListenerAt(ctx, c, peerDeps, broker.AskPairing, c.lc.Peer.Listen)
		if perr != nil {
			pushSys("peer: listener not started — " + perr.Error())
		} else {
			life.AddFunc(lifecycle.Intake, "peer listener", stopFn) // stop inbound peer turns
			pushSys("peer: federation listener on " + bound)
		}
	}

	reconnectMon := &reconnectMonitor{cfgDir: c.cfgDir, feed: sysCh, ctx: ctx, log: log.With("source", "model")}
	if degraded.Degraded() && degraded.class == degradeReachability {
		reconnectMon.OnGenerationDown()
	}
	_ = reconnectMon

	fmt.Printf("tenant serve — agent=%s backend=%s; headless hub\n", c.agent, c.backend)
	if dashAddr != "" {
		fmt.Printf("dashboard (control + chat + approvals): http://%s\n", dashAddr)
		fmt.Println(dashboardTokenBanner(dcfg.Auth, c.cfgDir, c.lc, stdoutIsTerminal()))
	}
	fmt.Println("running 24/7; Ctrl-C / SIGTERM to stop")

	<-ctx.Done()
	fmt.Println("\nshutting down…")
	life.Shutdown() // each failed step is logged
	return nil
}

// serveSystemPrompt builds the hub agent's system prompt — identical in intent
// to the TUI's, with the named-sub-agent catalog appended.
func serveSystemPrompt(agents map[string]*agentProfile, hasTools bool) string {
	p := "You are the user's personal assistant with long-term memory. Be concise " +
		"and direct. Use the assembled memory context to personalize your answers. " +
		"When the user asks you to remember something, or states a lasting preference or fact " +
		"about themselves or their work, call memory_remember to persist it as one durable " +
		"sentence — don't just acknowledge it. " +
		"For complex or multi-part work, you can spin up concurrent sub-agents with " +
		"spawn_agent(role, task): they run in parallel and talk over the team bus. Make each sub-agent's " +
		"task INDEPENDENT; YOU synthesize their results after team_await (call it ONCE — it BLOCKS until " +
		"the team finishes). " +
		"Never fabricate: ground claims in what you actually gathered; if a tool fails or you can't find " +
		"something, say so plainly rather than inventing facts or sources. " +
		"Your identity/soul is operator-only: you CANNOT change who you are or your operating rules."
	p += renderAgentsForOrchestrator(agents)
	if hasTools {
		p += " You have tools available — use them when they help, and report plainly if an action is blocked by policy."
		p += searchPolicyPrompt() // knowledge-first search-order policy (TEN-249)
	}
	return p
}
