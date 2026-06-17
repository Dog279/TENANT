package main

import (
	"context"
	"flag"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/agent"
	"tenant/internal/improve"
	"tenant/internal/memory/compress"
	"tenant/internal/memory/distill"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/model"
	"tenant/internal/plugins/discord"
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
	c          *commonFlags
	router     *model.Router
	soulLive   *soul.Live
	stores     *stores
	skills     *skills.Store
	compressor *compress.Compressor
	profile    *userprofile.Profile
	tools      composite
	sysPrompt  string
	emit       func(agent.Event)
	notify     func(string)
	degraded   *degradedState
	log        *slog.Logger
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

	discordBroker := newDiscordApprovalBroker(d.log)
	discordBroker.persist = func(snap map[string]string) {
		if c.lc == nil {
			return
		}
		c.lc.Relay.Permissions = snap
		_ = c.lc.save(c.cfgDir)
	}
	if c.lc != nil {
		discordBroker.loadModes(c.lc.Relay.Permissions)
	}

	deps := discordAgentDeps{
		router: d.router, soulLive: d.soulLive, archive: d.stores.archive,
		episodic: d.stores.episodic, semantic: d.stores.semantic,
		skills: ret, compactor: d.compressor, userProfile: d.profile,
		fullTools: d.tools, fullDisp: d.tools,
		sysPrompt: d.sysPrompt, log: d.log, gateConfirm: discordBroker.Confirm,
	}

	var (
		relayRunnerAg relayRunner
		relaySvc      *discord.Service
		relayApprover *discordApprover
		relaySender   messageSender
		relayGate     *execGate
	)
	discordToken := resolveSecret(c.cfgDir, skillSecretID("discord", "token"), authCfg{Stored: true})
	if strings.TrimSpace(discordToken) != "" {
		if r, svc, appr, snd, gate, derr := buildDiscordAgent(discordToken, deps); derr != nil {
			d.notify("discord relay: " + derr.Error())
		} else {
			relayRunnerAg, relaySvc, relayApprover, relaySender, relayGate = r, svc, appr, snd, gate
		}
	}
	relayMgr := &discordRelayManager{
		base: ctx, runner: relayRunnerAg, svc: relaySvc, approver: relayApprover,
		sender: relaySender, gate: relayGate, broker: discordBroker, token: discordToken,
		log: d.log, notify: d.notify, ingest: ingest, degraded: d.degraded.Degraded,
		persist: func(enabled bool, opID string, allowExec bool) error {
			if c.lc == nil {
				return nil
			}
			c.lc.Relay.Enabled = enabled
			c.lc.Relay.OperatorID = opID
			c.lc.Relay.AllowExec = allowExec
			return c.lc.save(c.cfgDir)
		},
		buildFn: func(token string) (relayRunner, *discord.Service, *discordApprover, messageSender, *execGate, error) {
			return buildDiscordAgent(token, deps)
		},
	}
	discordBroker.ask = func(cctx context.Context, req tui.ApprovalRequest) tui.ApprovalDecision {
		if relayMgr.askOperator(cctx, req.Action, req.Detail) {
			return tui.ApproveOnce
		}
		return tui.DenyOnce
	}
	if c.lc != nil {
		relayMgr.operatorID = c.lc.Relay.OperatorID
		if c.lc.Relay.AllowExec && relayGate != nil {
			relayMgr.allowExec = true
			relayGate.set(true)
		}
		if c.lc.Relay.Enabled {
			if err := relayMgr.Enable(); err != nil {
				d.notify("discord relay: " + err.Error())
			}
		}
	}
	return relayMgr
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
	refreshProfile func(context.Context) (bool, error)
	degraded       *degradedState
	feed           chan string
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
		select {
		case d.feed <- line:
		default:
		}
	}
	sched := improve.NewScheduler(d.log, 0)
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
	sched.Register(profileJob{refresh: d.refreshProfile}, d.profileEvery)

	evalDue, evalTick, evalDesc := resolveEvalDue(d.evalEverySet, d.evalEvery, improveCfg.EvalEvery, improveCfg.EvalAt, d.log)
	evalCadence := evalTick
	evalSched := newEvalSchedule(evalDue, evalDesc)
	seed := latestTrendTime(filepath.Join(c.dataDir, "eval-artifacts"))
	sched.RegisterDue(newEvalNightlyJob(c, d.pf, d.log), evalSched.DueFunc(), seed)
	if evalDue != nil {
		d.log.Info("nightly eval armed", "schedule", evalDesc, "clock_seed", seed.Format(time.RFC3339))
	}

	soulCadence := resolveEvalCadence(false, 0, improveCfg.SoulNudgeEvery, d.log)
	if soulCadence > 0 {
		sched.Register(&improve.SoulNudgeJob{
			Episodic: d.stores.episodic, AgentID: c.agent, BaseDir: c.cfgDir,
			Proposer: improve.NewLLMSoulProposer(proposerRouter),
			Scorer:   evalSoulScorer{c: c, pf: d.pf, baselinePath: filepath.Join("baselines", "fitness.json"), log: d.log},
			Logger:   d.log,
		}, soulCadence)
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
	if err := sched.Start(ctx, schedulerTick(tick)); err != nil {
		d.log.Warn("self-improve scheduler did not start", "err", err)
		return nil, evalSched
	}
	return sched, evalSched
}
