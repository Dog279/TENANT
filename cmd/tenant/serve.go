package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"tenant/internal/memory/compress"
	"tenant/internal/memory/distill"
	"tenant/internal/memory/skills"
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	applyPluginConfig(c, pf)
	pf.wikiDir = expandPath(pf.wikiDir)
	pf.sqlDB = expandPath(pf.sqlDB)
	pf.gsuiteSAJSON = expandPath(pf.gsuiteSAJSON)

	// Daemon logging goes to stderr (the supervisor — launchd/systemd —
	// captures it); there is no alt-screen to protect. A serve-mode feed sink
	// (notify/sysCh) routes every status line to the log so an operator reading
	// `journalctl`/the log file sees setup, health, fallback, cron, and relay
	// activity exactly as the TUI would surface it.
	log := newLogger()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	sysCh := make(chan string, 64)
	pushSys := func(s string) {
		select {
		case sysCh <- s:
		default:
		}
	}
	// Status feed → stdout (the supervisor's log). A daemon's setup/health/
	// dashboard/peer/iMessage/fallback/cron lines are the operator's only window
	// into "is it working", so route them at always-visible level rather than the
	// WARN-filtered slog (newLogger defaults to WARN). Also mirror to the debug
	// log for structured capture when TENANT_LOG=info|debug.
	go func() {
		for {
			select {
			case s := <-sysCh:
				fmt.Println("• " + s)
				log.Info("serve", "evt", s)
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
	router, degraded, degradedBanner, err := buildRouterResilient(c, log)
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
	defer closeStores()

	usageStore, uerr := usagestore.Open(filepath.Join(c.dataDir, "usage.db"))
	if uerr != nil {
		log.Warn("usage ledger unavailable", "err", uerr)
	} else {
		defer func() { _ = usageStore.Close() }()
	}

	// Approval broker: the single decision point for dangerous actions. Seeded
	// from --allow-* flags, overridden by persisted per-category modes. In serve
	// there is no TUI to prompt, so a headless drain (below) routes "ask"-mode
	// requests to the dashboard.
	broker := newApprovalBroker(log)
	broker.seedFromFlags(pf)
	stg, serr := loadSettings(c.cfgDir, c.agent)
	if serr != nil {
		pushSys("settings: " + serr.Error() + " — using defaults")
	}
	broker.loadModes(stg.Permissions)
	var stgMu sync.Mutex
	saveSettings := func() {
		stgMu.Lock()
		defer stgMu.Unlock()
		if err := stg.save(c.cfgDir, c.agent); err != nil {
			log.Warn("settings save failed", "err", err)
		}
	}
	broker.persist = func(modes map[string]string) {
		stgMu.Lock()
		stg.Permissions = modes
		stgMu.Unlock()
		saveSettings()
	}

	// Web is per-agent (own browser each), kept out of the shared mux.
	teamWeb := pf.web
	webCfg := webConfig(c.cfgDir, pf)
	webPolicy := web.Policy{AllowInteract: pf.webAllowInteract}
	pf.web = false

	mux, wikiIx, cleanupMux, err := buildToolMux(ctx, c, router, pf, originConfirm(broker.Confirm), log)
	if err != nil {
		return err
	}
	defer cleanupMux()

	skillStore, serr := skills.Open(filepath.Join(c.dataDir, "skills.db"))
	if serr != nil {
		return serr
	}
	defer skillStore.Close()

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
	installFallbackChain(router, c.cfgDir, c.lc, c.planCeiling, log)
	router.SetFailoverObserver(func(ev model.FailoverEvent) {
		from := fallbackLabelToProvider(ev.From, c.lc.Provider)
		to := fallbackLabelToProvider(ev.To, c.lc.Provider)
		msg := fmt.Sprintf("⚠ model fallback: %s %s → using %s", from, ev.Reason, to)
		if errors.Is(ev.Err, model.ErrInsufficientBalance) {
			msg += " (recharge to restore)"
		}
		pushSys(msg)
	})

	for _, note := range mux.restore(stg.Tools) {
		pushSys("settings: " + note)
	}
	mux.setOnChange(func(snap map[string]bool) {
		stgMu.Lock()
		stg.Tools = snap
		stgMu.Unlock()
		saveSettings()
	})

	meta, merr := improve.OpenMeta(filepath.Join(c.dataDir, "tenant_meta.db"))
	if merr != nil {
		return merr
	}
	defer meta.Close()
	distiller := &distill.Distiller{Router: router, Episodic: st.episodic, Semantic: st.semantic, AgentID: c.agent, Logger: log}
	distillJob := improve.NewDistillJob(distiller, meta, c.agent)

	soulLive := soul.NewLive(st.soul)
	mainWorking := working.New() // no session-resume seed: a daemon has no "last session"
	memCtl := memControl{
		episodic: st.episodic, semantic: st.semantic, skills: skillStore,
		embedder: skEmb, distill: distillJob, cfgDir: c.cfgDir, agentID: c.agent,
		soulLive:       soulLive,
		profile:        prof,
		profileRefresh: refreshProfile,
		working:        mainWorking,
	}

	bus := orchestra.NewBus()
	defer bus.Close()
	bus.Register(c.agent)
	compressor := &compress.Compressor{Router: router, Logger: log}

	// Event broker + retained log. emit fans every event to BOTH the live
	// broker (dashboard chat/feed) AND the replayable log (backfill + the
	// /api/activity projection).
	evBroker := agent.NewBroker(0)
	evlog := agent.NewEventLog(10000)
	emit := func(ev agent.Event) {
		evBroker.Publish(ev)
		evlog.Append(ev)
	}

	// Sub-agent + bus activity mirrored to the dashboard feed (attributed by
	// agent id). Serve has no terminal team-view channel, so — unlike the TUI's
	// subObserve — it omits EventUsage: there is no per-sub-agent token counter
	// to feed, only the dashboard activity stream.
	subObserve := func(id string, e agent.Event) {
		switch e.Kind {
		case agent.EventToolCall, agent.EventToolResult, agent.EventFinal, agent.EventError:
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
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: mux, Skills: skillStore,
		Embedder: skEmb, EmbedderID: embedderID, Compressor: compressor, Profile: prof,
		OrchID: c.agent, Log: log, Observe: subObserve,
		Web: teamWeb, WebConfig: webCfg, WebPolicy: webPolicy,
		Shots:         filepath.Join(c.dataDir, "screenshots"),
		AgentProfiles: initialAgentProfiles,
		CfgDir:        c.cfgDir,
		EmbedProfile:  embProf,
		LocalRestore:  stg.Tools,
		LocalOnChange: localSave,
	})
	defer rt.Close()

	mainLocal := newToolMux()
	mainLocal.add("orchestrator", spawnTool{rt: rt})
	mainLocal.add("team", orchestra.CommsTool{Bus: bus, Self: c.agent})
	mainLocal.add("memory", memoryTool{sem: st.semantic, emb: skEmb, embedderID: embedderID, agentID: c.agent, note: noteProfile})
	mainLocal.add("skills", skillTool{st: skillStore, emb: skEmb, agentID: c.agent})
	mainLocal.add("recall", &recallTool{episodic: st.episodic, semantic: st.semantic, archive: st.archive.Reader(), emb: skEmb, embedderID: embedderID, agentID: c.agent})
	rt.addWebTool(mainLocal)
	mainTools := composite{shared: mux, local: mainLocal}

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

	ag, err := agent.New(agent.Config{
		AgentID:  c.agent,
		Router:   router,
		SoulLive: soulLive,
		Working:  mainWorking,
		Archive:  st.archive,
		Episodic: st.episodic,
		Semantic: st.semantic,
		Tools:    mainTools,
		Dispatcher: &agent.RetryDecorator{
			Inner:      mainTools,
			Eligible:   agent.DefaultEligibleTransient,
			Backoff:    time.Second,
			MaxRetries: 1,
			Observer:   emit,
		},
		Logger:       log,
		Stream:       true,
		Observer:     emit,
		LazyToolLoad: c.lc.LazyTools,
		Skills:       skillRetriever{st: skillStore, agentID: c.agent},
		SystemPrompt: sysPrompt,
		Compactor:    compressor,
		UserProfile:  prof,
	})
	if err != nil {
		return err
	}
	memCtl.expand = ag.ExpandLatestCompaction

	// ONE turn gate over the shared brain (TEN-194): every driver (dashboard
	// now, peer-delegated run later) serializes here so concurrent sources can't
	// race the single working set. It also reports turn liveness to /api/status.
	gated := newTurnGate(ag)

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
		router:      router,
		soulLive:    soulLive,
		skills:      skillRetriever{st: skillStore, agentID: c.agent},
		compactor:   compressor,
		userProfile: prof,
		fullTools:   mainTools.All(),
		fullDisp:    mainTools,
		sysPrompt:   sysPrompt,
		log:         log,
		cfgDir:      c.cfgDir,
		dataDir:     c.dataDir,
		allowExec:   cronExecGate,
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
			c.lc.Cron.Jobs = jobs
			return c.lc.save(c.cfgDir)
		}, log)
		cronEngine.SetPaused(degraded.Degraded)
		cronEngine.Prime(cron.PrimeOptions{
			Location: cronLoc,
			Catchup:  cronCatchup,
			History:  loadCronHistory(c.dataDir),
			HistoryPersist: func(h []cron.RunRecord) error {
				return saveCronHistory(c.dataDir, h)
			},
		})
		cronEngine.SetNotify(pushSys)
		cronMgr := newCronManager(ctx, cronEngine, cronExecGate, func(on bool) error {
			if c.lc == nil {
				return nil
			}
			c.lc.Cron.AllowExec = on
			return c.lc.save(c.cfgDir)
		})
		mainLocal.add("cron", cronplugin.NewDispatcher(cronMgr))
		dashCronCtl = dashCron{cronMgr}
		if serr := cronEngine.Start(ctx); serr != nil {
			pushSys("cron: scheduler did not start (" + serr.Error() + ")")
		}
	}

	// modelControl powers the dashboard model + key pages and re-points
	// tool-ranking at a new embedder after a /model swap.
	modelCtl := &modelControl{cfgDir: c.cfgDir, dataDir: c.dataDir, agentID: c.agent, ag: ag, log: log, degraded: degraded}
	modelCtl.reinstallEmbedder = func(ctx context.Context) {
		emb, prof, err := router.EmbedderForRole(ctx, model.RoleEmbedder)
		if err != nil {
			return
		}
		mux.SetEmbedder(string(prof.ID), emb)
		mux.precomputeEmbeddings(ctx)
	}
	go watchCredentials(ctx, c.cfgDir, modelCtl, degraded, pushSys)

	// Headless approval drain (TEN-194): consume the broker's request channel so
	// a gated "ask" action surfaces on the dashboard instead of wedging the
	// single-flight brain forever. Denies everything still pending on shutdown.
	approvals := newHeadlessApprovals(broker.Requests(), emit)
	go approvals.run(ctx)

	dcfg, _ := resolveDashboardConfig(c.lc, true, *dashboardOn, *dashboardAddr)
	dashMgr := &dashboardManager{
		base:      ctx,
		cfg:       dcfg,
		runner:    gated,
		tools:     dashTools{mainTools},
		mem:       dashMemory{memCtl},
		cron:      dashCronCtl,
		secrets:   dashKeys{cfgDir: c.cfgDir, mc: modelCtl},
		skills:    dashSkill{c: skillControl{st: skillStore, emb: skEmb, agentID: c.agent, cfgDir: c.cfgDir}},
		models:    dashModel{mc: modelCtl},
		approvals: approvals,
		broker:    evBroker,
		evlog:     evlog,
		log:       log,
		notify:    pushSys,
		persist: func(enabled bool) error {
			if c.lc == nil {
				return nil
			}
			c.lc.Dashboard.Enabled = &enabled
			return c.lc.save(c.cfgDir)
		},
	}

	// Discord relay (TEN-114): opt-in, off by default; drives a DEDICATED relay
	// agent (own working set, shared memory) so it never touches the shared
	// brain's turn. The prime away-from-desk drive surface for a 24/7 hub.
	relayMgr := buildServeRelay(ctx, serveRelayDeps{
		c: c, router: router, soulLive: soulLive, stores: st, skills: skillStore,
		compressor: compressor, profile: prof, tools: mainTools, sysPrompt: sysPrompt,
		emit: emit, notify: pushSys, degraded: degraded, log: log,
	})

	// Native iMessage responder (TEN-230): the away-from-desk drive surface a Mac
	// hub answers texts on. Auto-starts when left enabled; needs Full Disk Access
	// for chat.db under a daemon (a failure is a logged note, never fatal).
	imsgAllowMgr := buildServeIMessage(ctx, serveIMessageDeps{
		c: c, router: router, soulLive: soulLive, stores: st, skills: skillStore,
		compressor: compressor, profile: prof, tools: mainTools, sysPrompt: sysPrompt,
		meta: meta, broker: broker, degraded: degraded, emit: emit, notify: pushSys, log: log,
	})
	dashMgr.access = dashAccess{im: imsgAllowMgr, relay: relayMgr}

	// Self-improvement scheduler (preserves the old serve's distill+consolidate
	// and adds the full job set). Suspended while degraded.
	sched, evalSched := buildServeImprove(ctx, serveImproveDeps{
		c: c, pf: pf, on: *selfImprove, router: router, distiller: distiller, distillJob: distillJob,
		skills: skillStore, embedder: skEmb, stores: st, refreshProfile: refreshProfile,
		degraded: degraded, feed: sysCh, distillEvery: *distillEvery, profileEvery: *profileEvery,
		evalEvery: *evalEvery, evalEverySet: fsVisited(fs, "eval-every"), log: log,
	})
	dashMgr.eval = dashEval{ev: evalTUIControl{sched: evalSched, cfgDir: c.cfgDir, dataDir: c.dataDir}, judge: judgeCtl{cfgDir: c.cfgDir, planner: c.vllmModel}}
	dashMgr.mcp = dashMCP{m: newMCPControl(mux, c.cfgDir, c.lc)}
	dashMgr.integrations = dashIntegrations{c: newSkillCfgControl(c.cfgDir, skillKinds, mainTools.SetPluginEnabled)}

	dashAddr := ""
	if *dashboardOn {
		if addr, derr := dashMgr.Enable(); derr != nil {
			pushSys("dashboard: " + derr.Error())
		} else {
			dashAddr = addr
			pushSys("dashboard: serving on http://" + addr)
		}
	}

	// Re-assert a persisted `/tailscale serve` choice (TEN-233) now the dashboard
	// is up — publishes the loopback dashboard onto the tailnet so the operator
	// can reach the hub off-LAN, matching the TUI. Best-effort.
	if c.lc != nil && c.lc.Tailscale.Serve {
		tsMgr := newTailscaleManager(ctx, dashMgr.Status, log)
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
	go (&peerHealthMonitor{cfgDir: c.cfgDir, reg: peerHealth, log: log}).run(ctx)
	peerDeps := peerToolDeps{
		selfName: peerHostName,
		semantic: st.semantic,
		episodic: st.episodic,
		embedder: peerEmb,
		wiki:     wikiIx,
		onAuth:   peerHealth.markInbound,
	}
	var peerSrvStop func()
	if c.lc != nil && c.lc.Peer.Listen != "" {
		bound, stopFn, perr := startPeerListenerAt(ctx, c, peerDeps, broker.AskPairing, c.lc.Peer.Listen, log)
		if perr != nil {
			pushSys("peer: listener not started — " + perr.Error())
		} else {
			peerSrvStop = stopFn
			pushSys("peer: federation listener on " + bound)
		}
	}

	reconnectMon := &reconnectMonitor{cfgDir: c.cfgDir, feed: sysCh, ctx: ctx, log: log}
	if degraded.Degraded() && degraded.class == degradeReachability {
		reconnectMon.OnGenerationDown()
	}
	_ = reconnectMon

	fmt.Printf("tenant serve — agent=%s backend=%s; headless hub\n", c.agent, c.backend)
	if dashAddr != "" {
		fmt.Printf("dashboard (control + chat + approvals): http://%s\n", dashAddr)
	}
	fmt.Println("running 24/7; Ctrl-C / SIGTERM to stop")

	<-ctx.Done()

	// Graceful, ordered shutdown (TEN-194). STOP ACCEPTING NEW WORK FIRST so
	// nothing starts a turn or a job mid-teardown, drain in-flight work, then
	// close stores LAST (no goroutine may write to a closed store).
	fmt.Println("\nshutting down…")
	if peerSrvStop != nil {
		peerSrvStop() // stop inbound peer turns
	}
	if relayMgr != nil {
		_ = relayMgr.Disable() // stop Discord ingest
	}
	if err := dashMgr.Disable(); err != nil {
		log.Warn("dashboard shutdown", "err", err)
	}
	if cronEngine != nil {
		cronEngine.Stop() // drains any in-flight cron run
	}
	if sched != nil {
		sched.Stop() // drains any in-flight improve job
	}
	// The approval drain + bus/credential goroutines exit on ctx cancel (already
	// fired). closeStores/meta/skills/usage close via the deferred stack AFTER
	// this returns — all writers above are now quiesced.
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
