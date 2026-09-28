package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/agent"
	"tenant/internal/improve"
	"tenant/internal/memory/compress"
	"tenant/internal/memory/distill"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/sme"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
	"tenant/internal/plugins/imessage"
	"tenant/internal/tui"
)

// fsVisited reports whether flag `name` was explicitly set on fs (mirrors the
// fs.Visit idiom cmdTUI uses to let an explicit flag win over config).
func fsVisited(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// serveRelayDeps bundles what the Discord relay needs. Mirrors cmdTUI's relay
// wiring (commands.go); kept here so the serve assembly stays readable.
type serveRelayDeps struct {
	c           *commonFlags
	router      *model.Router
	soulLive    *soul.Live
	stores      *stores
	skills      *skills.Store
	compressor  *compress.Compressor
	profile     *userprofile.Profile
	tools       composite
	sysPrompt   string
	emit        func(agent.Event)
	notify      func(string)
	degraded    *degradedState
	log         *slog.Logger
	routeHook   agent.RouteHook
	routeActive agent.RouteActive
	routeSticky agent.RouteSticky
}

// buildServeRelay constructs the Discord relay manager (always non-nil so the
// dashboard access page can drive it) and auto-enables it if the operator's
// config has it on. Off by default; OPT-IN — it exposes the agent over a
// third-party network. The relay drives a DEDICATED agent, not the shared
// brain, so it never contends on the hub's turn gate.
func buildServeRelay(ctx context.Context, d serveRelayDeps) *discordRelayManager {
	c := d.c
	ret := skillRetriever{st: d.skills, agentID: c.agent}
	ingest := func(t string) {
		preview := strings.TrimSpace(t)
		if r := []rune(preview); len(r) > 100 {
			preview = string(r[:100]) + "…"
		}
		d.emit(agent.Event{Kind: agent.EventIngest, Text: "Discord: " + preview})
	}

	discordBroker := newDiscordApprovalBroker(d.log.With("source", "approvals"))
	discordBroker.SetEvents(d.emit) // visibility bridge: surface Discord-routed approvals on the feed (TEN-203)
	discordBroker.SetPersist(func(snap map[string]string, known []string) {
		persistChoice(c.cfgDir, c.lc, d.notify, "relay: the permission change", func(lc *launchConfig) {
			lc.Relay.Permissions, lc.Relay.KnownCategories = snap, known
		})
	})
	if c.lc != nil {
		discordBroker.LoadModes(c.lc.Relay.Permissions, c.lc.Relay.KnownCategories)
	}
	if n := newCategoryNotice(discordBroker.NewCategories(), "Review on the dashboard's Access page."); n != "" && d.notify != nil {
		d.notify("Discord conversations: " + n)
	}

	deps := discordAgentDeps{
		router: d.router, soulLive: d.soulLive, archive: d.stores.archive,
		episodic: d.stores.episodic, semantic: d.stores.semantic,
		runs:   d.stores.runs,
		skills: ret, compactor: d.compressor, userProfile: d.profile,
		fullTools: d.tools, fullDisp: d.tools,
		sysPrompt: d.sysPrompt, log: d.log, gateConfirm: discordBroker.Confirm,
		events:    d.emit,
		routeHook: d.routeHook, routeActive: d.routeActive, routeSticky: d.routeSticky,
	}

	var (
		relayRunnerAg relayRunner
		relaySvc      *discord.Service
		relayApprover *discordApprover
		relaySender   messageSender
	)
	discordToken := discordRelayToken(c.cfgDir)
	if strings.TrimSpace(discordToken) != "" {
		if r, svc, appr, snd, derr := buildDiscordAgent(discordToken, deps); derr != nil {
			d.notify("discord relay: " + derr.Error())
		} else {
			relayRunnerAg, relaySvc, relayApprover, relaySender = r, svc, appr, snd
		}
	}
	relayMgr := &discordRelayManager{
		base: ctx, runner: relayRunnerAg, svc: relaySvc, approver: relayApprover,
		sender: relaySender, broker: discordBroker, token: discordToken,
		dataDir: c.dataDir,
		log:     d.log, notify: d.notify, ingest: ingest, degraded: d.degraded.Degraded,
		approvalTimeout: relayApprovalTimeout(c.lc), cardStyle: relayCardStyle(c.lc),
		persistTimeout: func(dur time.Duration) error {
			if c.lc == nil {
				return nil
			}
			return persistLaunchConfig(c.cfgDir, c.lc, func(lc *launchConfig) { lc.Relay.ApprovalTimeout = dur.String() })
		},
		persist: func(enabled bool, opID string, allowExec bool) error {
			if c.lc == nil {
				return nil
			}
			return persistLaunchConfig(c.cfgDir, c.lc, func(lc *launchConfig) {
				lc.Relay.Enabled = enabled
				lc.Relay.OperatorID = opID
				lc.Relay.AllowExec = allowExec
			})
		},
		buildFn: func(token string) (relayRunner, *discord.Service, *discordApprover, messageSender, error) {
			return buildDiscordAgent(token, deps)
		},
	}
	// Feed line + operator DM when a remote connector's sign-in dies / recovers.
	d.tools.SetMCPAuthNotifier(mcpAuthNotifier(d.notify, relayMgr.NotifyOperator))
	setWitnessDM(relayMgr.NotifyOperator) // a "discord" Security log witness
	discordBroker.SetAsk(func(cctx context.Context, req tui.ApprovalRequest) tui.ApprovalDecision {
		return relayMgr.askOperator(cctx, req.Action, req.Detail)
	})
	discordBroker.SetLock(relayMgr.execLocked) // exec mode off ⇒ exec/write/destructive/sandbox/send refused (TEN-338)
	if c.lc != nil {
		relayMgr.operatorID = c.lc.Relay.OperatorID
		relayMgr.allowExec = c.lc.Relay.AllowExec // restored with or without a bot token, as in cmdTUI
		if c.lc.Relay.Enabled {
			if err := relayMgr.Enable(); err != nil {
				d.notify("discord relay: " + err.Error())
			}
		}
	}
	return relayMgr
}

// serveIMessageDeps bundles what the native iMessage responder needs. Mirrors
// cmdTUI's iMessage block (commands.go ~2311-2407) so `tenant serve` answers
// inbound texts exactly like the TUI does.
type serveIMessageDeps struct {
	c           *commonFlags
	router      *model.Router
	soulLive    *soul.Live
	stores      *stores
	skills      *skills.Store
	compressor  *compress.Compressor
	profile     *userprofile.Profile
	tools       composite
	sysPrompt   string
	meta        *improve.Meta
	broker      *approvalBroker
	degraded    *degradedState
	emit        func(agent.Event)
	notify      func(string)
	log         *slog.Logger
	routeHook   agent.RouteHook
	routeActive agent.RouteActive
	routeSticky agent.RouteSticky
}

// buildServeIMessage wires the iMessage drive-allowlist + per-category broker +
// native responder and auto-starts it when the operator left it enabled. Returns
// the allow manager for the dashboard access surface. Native iMessage reads
// chat.db, so under a daemon the serve process needs Full Disk Access — a
// failure here is a logged note, never fatal (the agent + every other channel
// keep running). Returns nil only if there is no config to read.
func buildServeIMessage(ctx context.Context, d serveIMessageDeps) *imessageAllowManager {
	c := d.c
	var allow0 []string
	if c.lc != nil {
		allow0 = c.lc.IMessage.AllowFrom
	}
	allowMgr := newIMessageAllowManager(allow0, func(handles []string) error {
		if c.lc == nil {
			return nil
		}
		return persistLaunchConfig(c.cfgDir, c.lc, func(lc *launchConfig) { lc.IMessage.AllowFrom = handles })
	})

	// Offsite broker: own per-category modes (default DENY) but SHARES the global
	// broker's request channel, so an "ask" lands in the same queue the headless
	// drain surfaces on the dashboard.
	imsgBroker := newOffsiteApprovalBroker(d.log.With("source", "approvals"), d.broker)
	imsgBroker.SetPersist(func(snap map[string]string, known []string) {
		persistChoice(c.cfgDir, c.lc, d.notify, "imessage: the permission change", func(lc *launchConfig) {
			lc.IMessage.Permissions, lc.IMessage.KnownCategories = snap, known
		})
	})
	if c.lc != nil {
		imsgBroker.LoadModes(c.lc.IMessage.Permissions, c.lc.IMessage.KnownCategories)
	}
	allowMgr.setPerms(imsgBroker)

	// Phase-2 text-confirm operator (TEN-267): the handle whose "Y <nonce>" reply
	// approves a gated action over iMessage. Read fresh here; empty ⇒ Phase-1
	// (the broker's "ask" stays deny-all offsite — no out-of-band approval path).
	var imsgOperator string
	if c.lc != nil {
		imsgOperator = c.lc.IMessage.Operator
	}

	ingest := func(t string) {
		preview := strings.TrimSpace(t)
		if r := []rune(preview); len(r) > 100 {
			preview = string(r[:100]) + "…"
		}
		d.emit(agent.Event{Kind: agent.EventIngest, Text: "iMessage: " + preview})
	}

	resp := &imessageResponderManager{
		base:      ctx,
		allowFrom: allowMgr.AllowList,
		persist: func(enabled bool) error {
			if c.lc == nil {
				return nil
			}
			return persistLaunchConfig(c.cfgDir, c.lc, func(lc *launchConfig) { lc.IMessage.Enabled = enabled })
		},
		buildFn: func(allowFrom []string) (responderRunnable, func(), error) {
			nat, err := imessage.OpenNative(imessage.NativeConfig{})
			if err != nil {
				return nil, nil, err
			}
			watcher, err := imessage.NewWatcher(imessage.WatchConfig{
				Source: nat, Store: d.meta, Account: c.agent, AllowFrom: allowFrom,
			})
			if err != nil {
				_ = nat.Close()
				return nil, nil, fmt.Errorf("watcher: %w", err)
			}
			ag, err := buildIMessageAgent(imessageAgentDeps{
				router: d.router, soulLive: d.soulLive, archive: d.stores.archive,
				episodic: d.stores.episodic, semantic: d.stores.semantic,
				runs:      d.stores.runs,
				skills:    skillRetriever{st: d.skills, agentID: c.agent},
				compactor: d.compressor, userProfile: d.profile,
				fullTools: d.tools, fullDisp: d.tools,
				sysPrompt: d.sysPrompt, log: d.log, routeHook: d.routeHook,
				routeActive: d.routeActive, routeSticky: d.routeSticky, events: d.emit,
			})
			if err != nil {
				_ = nat.Close()
				return nil, nil, fmt.Errorf("agent: %w", err)
			}
			responder := &imessageResponder{
				poller: watcher, sender: nat, runner: ag,
				confirm: func(cctx context.Context, action, detail string) bool {
					return imsgBroker.Confirm(cctx, action, "[iMessage] "+detail)
				},
				log: d.log, degraded: d.degraded.Degraded,
				ingest: ingest,
			}
			// Phase-2 (TEN-267): when an operator handle is configured, build the
			// text-confirm approver over the responder's send path and make it the
			// broker's "ask" backend — an "ask"-mode category now texts the operator
			// a nonce handshake instead of failing closed offsite. No operator ⇒
			// approver is nil ⇒ Phase-1 deny-all is unchanged.
			if approver := newIMessageApprover(responder, imsgOperator, d.log); approver != nil {
				responder.approver = approver
				imsgBroker.SetAsk(func(cctx context.Context, req tui.ApprovalRequest) tui.ApprovalDecision {
					return approver.decide(cctx, req.Action, req.Detail)
				})
			}
			return responder, func() { _ = nat.Close() }, nil
		},
	}
	allowMgr.setResponder(resp)
	if c.lc != nil && c.lc.IMessage.Enabled {
		if status, err := resp.Start(); err != nil {
			d.notify("imessage responder: " + err.Error())
		} else {
			d.notify(status)
		}
	}
	return allowMgr
}

// serveImproveDeps bundles the self-improvement scheduler inputs. Mirrors
// cmdTUI's self-improve block (commands.go).
type serveImproveDeps struct {
	c              *commonFlags
	pf             *pluginFlags
	on             bool
	router         *model.Router
	distiller      *distill.Distiller
	distillJob     *improve.DistillJob
	skills         *skills.Store
	embedder       model.Embedder
	stores         *stores
	sme            *sme.Store // Phase 3: SME doc store (nil ⇒ reflection off)
	smeLive        *sme.Live  // Phase 3: live cache the ReflectionJob refreshes
	refreshProfile func(context.Context) (bool, error)
	degraded       *degradedState
	feed           chan string
	events         func(agent.Event) // dashboard feed sink for eval task agents
	evalProg       *evalProgress     // live done/total for the Quality page bar
	distillEvery   time.Duration
	profileEvery   time.Duration
	evalEvery      time.Duration
	evalEverySet   bool
	log            *slog.Logger
}

// buildServeImprove registers and starts the background self-improvement
// scheduler (distill / skill-induction / consolidation / profile / nightly eval
// / soul-nudge). Returns (nil, nil) when self-improve is off. Suspended while
// the model is degraded to echo — the same invariant as the TUI.
func buildServeImprove(ctx context.Context, d serveImproveDeps) (*improve.Scheduler, *evalSchedule) {
	if !d.on {
		return nil, nil
	}
	c := d.c
	feed := func(line string) {
		d.log.Info(line) // filed first: a full feed costs only the stdout copy
		select {
		case d.feed <- line:
		default:
		}
	}
	sched := improve.NewScheduler(0)
	sched.Paused = d.degraded.Degraded
	sched.OnRun = func(rec improve.JobRunRecord) {
		if line, ok := formatSelfImproveFeedLine(rec); ok {
			feed(line)
		}
	}
	sched.OnStart = func(name string) {
		if name == "eval-nightly" {
			feed("improve: eval-nightly started — full live suite; takes minutes, result lands in trend.jsonl")
		}
	}
	sched.Register(d.distillJob, d.distillEvery)

	improveCfg := improveConfig{}
	proposerRouter := d.router
	if x, err := loadLaunchConfig(c.cfgDir); err == nil {
		improveCfg = x.Improve
		embProf, _ := d.router.ForRole(model.RoleEmbedder)
		var pinModel string
		proposerRouter, pinModel = improveProposerRouter(improveCfg.Profile, d.router, effectiveAgents(x), x, c.cfgDir, embProf, d.log)
		if proposerRouter != d.router {
			d.log.Info("improve: reflection jobs pinned to profile", "profile", improveCfg.Profile, "model", pinModel)
		}
	}
	d.distiller.SummarizerRouter = proposerRouter

	sched.Register(&improve.SkillInductionJob{
		Episodic: d.stores.episodic, Skills: d.skills, Embedder: d.embedder, AgentID: c.agent,
		AutoAccept: func() string {
			if x, err := loadLaunchConfig(c.cfgDir); err == nil {
				return x.Improve.AutoAccept
			}
			return ""
		},
		TrustMinAcks: improveCfg.TrustMinAcks,
		TrustWindow:  improveCfg.TrustWindow,
	}, d.distillEvery)
	sched.Register(&improve.ConsolidationJob{
		Semantic: d.stores.semantic, Router: d.router, SummarizerRouter: proposerRouter,
		AgentID: c.agent, Holistic: true, Logger: d.log,
	}, improve.DefaultConsolidateInterval)
	// Feedback-driven protection (TEN-255 Phase 4): ACKed turns → protected
	// facts. Promote-only, no LLM; mirrors the TUI path.
	sched.Register(&improve.FeedbackProtectionJob{
		Semantic: d.stores.semantic, Episodic: d.stores.episodic, AgentID: c.agent,
	}, d.distillEvery)
	sched.Register(profileJob{refresh: d.refreshProfile}, d.profileEvery)

	evalDue, evalTick, evalDesc := resolveEvalDue(d.evalEverySet, d.evalEvery, improveCfg.EvalEvery, improveCfg.EvalAt, d.log)
	evalCadence := evalTick
	evalSched := newEvalSchedule(evalDue, evalDesc)
	seed := latestTrendTime(filepath.Join(c.dataDir, "eval-artifacts"))
	sched.RegisterDue(newEvalNightlyJob(c, d.pf, d.events, d.evalProg), evalSched.DueFunc(), seed)
	if evalDue != nil {
		d.log.Info("nightly eval armed", "schedule", evalDesc, "clock_seed", seed.Format(time.RFC3339))
	}

	soulCadence := resolveEvalCadence(false, 0, improveCfg.SoulNudgeEvery, d.log)
	if soulCadence > 0 {
		sched.Register(&improve.SoulNudgeJob{
			Episodic: d.stores.episodic, AgentID: c.agent, BaseDir: c.cfgDir,
			Proposer: improve.NewLLMSoulProposer(proposerRouter),
			Scorer:   evalSoulScorer{c: c, pf: d.pf, baselinePath: filepath.Join("baselines", "fitness.json"), log: d.log},
		}, soulCadence)
	}

	// Reflection / SME synthesis (TEN-255 Phase 3): OFF by default; refreshes the
	// live SME cache the serve agent injects each turn. Proposer-routed, Paused-
	// gated, fails closed (mirrors the TUI path).
	reflectCadence := resolveEvalCadence(false, 0, improveCfg.ReflectEvery, d.log)
	if reflectCadence > 0 && d.sme != nil {
		sched.Register(&improve.ReflectionJob{
			Semantic: d.stores.semantic, Episodic: d.stores.episodic, SME: d.sme, Live: d.smeLive,
			Router: d.router, SummarizerRouter: proposerRouter, AgentID: c.agent, Logger: d.log,
		}, reflectCadence)
	}

	tick := d.distillEvery
	if d.profileEvery < tick {
		tick = d.profileEvery
	}
	if evalCadence > 0 && evalCadence < tick {
		tick = evalCadence
	}
	if soulCadence > 0 && soulCadence < tick {
		tick = soulCadence
	}
	if reflectCadence > 0 && reflectCadence < tick {
		tick = reflectCadence
	}
	if err := sched.Start(ctx, schedulerTick(tick)); err != nil {
		sysUnavailable.Log(ctx, "component", "self-improve scheduler", "effect", "no background self-improvement", "err", err)
		return nil, evalSched
	}
	return sched, evalSched
}
