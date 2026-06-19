package main

// hub.go (TEN-247): buildHub extracts the substrate assembly that cmdTUI and
// cmdServe both need — router/degraded, stores, usage ledger, approval broker
// (+ persisted modes), the shared tool mux, skills, user profile, embedder,
// the fallback chain, distillation, the live soul, and the compressor.
//
// Before this, cmdServe MIRRORED ~250 lines of cmdTUI's wiring verbatim (kept
// additive in TEN-194 so the daily-driver TUI stayed byte-identical). buildHub
// is that shared block, parameterized only by the per-process `pushSys` feed
// sink (both callers already had an identical `pushSys func(string)`), so the
// two paths can't drift. The DIVERGENT bits stay in each caller: session-resume
// (TUI seeds the working set, the daemon doesn't), the orchestra bus + team
// runtime, the agent, cron, dashboard, relay, peer, and the event broker.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"tenant/internal/memory/distill"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/soul"
	usagestore "tenant/internal/memory/usage"
	"tenant/internal/memory/userprofile"
	"tenant/internal/model"
	"tenant/internal/plugins/web"
	"tenant/internal/plugins/wiki"

	"tenant/internal/improve"
)

// hub is the shared substrate buildHub returns. Field lifetimes are tied to the
// cleanup func it also returns (the caller defers it); unused fields are fine —
// each caller reads the subset it needs.
type hub struct {
	router         *model.Router
	degraded       *degradedState
	degradedBanner string // already surfaced via pushSys inside buildHub
	stores         *stores
	usageStore     *usagestore.Store // nil if the ledger couldn't open
	broker         *approvalBroker
	stg            *settings
	stgMu          *sync.Mutex // guards stg; shared with callers that persist tool/perm snapshots
	saveSettings   func()      // persists stg under stgMu (callers reuse for their own onChange hooks)
	mux            *toolMux
	wikiIx         *wiki.Index
	skillStore     *skills.Store
	prof           *userprofile.Profile
	profSynth      *userprofile.Synthesizer
	profMu         *sync.Mutex
	refreshProfile func(context.Context) (bool, error)
	noteProfile    func(string)
	skEmb          model.Embedder
	embedderID     string
	meta           *improve.Meta
	distiller      *distill.Distiller
	distillJob     *improve.DistillJob
	soulLive       *soul.Live
	teamWeb        bool
	webCfg         web.Config
	webPolicy      web.Policy
	log            *slog.Logger
}

// buildHub assembles the shared substrate. pushSys is the per-process startup/
// feed sink (TUI chat feed or serve's sysCh). On any error it tears down what it
// already opened and returns (nil, nil, err); on success the caller MUST defer
// the returned cleanup (closes meta, skills, mux, usage ledger, stores).
func buildHub(ctx context.Context, c *commonFlags, pf *pluginFlags, pushSys func(string), log *slog.Logger) (*hub, func(), error) {
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (*hub, func(), error) {
		cleanup()
		return nil, nil, err
	}

	// Resilient launch: degrade to echo rather than abort if the model can't be
	// built, so the process still comes up and the operator can recover live.
	router, degraded, degradedBanner, err := buildRouterResilient(c, log)
	if err != nil {
		return nil, nil, err
	}
	if degraded.Degraded() {
		pushSys(degradedBanner)
		c.backend = "echo"
		pushSys("echo: responses are deterministic stubs — not a real model.")
	}

	st, closeStores, err := openStores(c)
	if err != nil {
		return nil, nil, err
	}
	cleanups = append(cleanups, closeStores)

	usageStore, uerr := usagestore.Open(filepath.Join(c.dataDir, "usage.db"))
	if uerr != nil {
		log.Warn("usage ledger unavailable", "err", uerr)
	} else {
		cleanups = append(cleanups, func() { _ = usageStore.Close() })
	}

	// Approval broker: the single decision point for dangerous actions. Seeded
	// from --allow-* flags, overridden by persisted per-category modes.
	broker := newApprovalBroker(log)
	broker.seedFromFlags(pf)
	stg, serr := loadSettings(c.cfgDir, c.agent)
	if serr != nil {
		pushSys("settings: " + serr.Error() + " — using defaults")
	}
	broker.loadModes(stg.Permissions)
	stgMu := &sync.Mutex{}
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
		return fail(err)
	}
	cleanups = append(cleanups, cleanupMux)

	skillStore, serr := skills.Open(filepath.Join(c.dataDir, "skills.db"))
	if serr != nil {
		return fail(serr)
	}
	cleanups = append(cleanups, func() { _ = skillStore.Close() })

	prof, _ := userprofile.Load(c.dataDir, c.agent)
	profSynth := &userprofile.Synthesizer{Router: router, Semantic: st.semantic, AgentID: c.agent}
	profMu := &sync.Mutex{}
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

	// Restore the operator's tool curation over flag defaults, THEN install the
	// save hook (restore must not re-save what it just loaded).
	for _, note := range mux.restore(stg.Tools) {
		pushSys("settings: " + note)
	}
	pushSys("settings: " + settingsPath(c.cfgDir, c.agent))
	mux.setOnChange(func(snap map[string]bool) {
		stgMu.Lock()
		stg.Tools = snap
		stgMu.Unlock()
		saveSettings()
	})

	meta, merr := improve.OpenMeta(filepath.Join(c.dataDir, "tenant_meta.db"))
	if merr != nil {
		return fail(merr)
	}
	cleanups = append(cleanups, func() { _ = meta.Close() })
	distiller := &distill.Distiller{Router: router, Episodic: st.episodic, Semantic: st.semantic, AgentID: c.agent, Logger: log}
	distillJob := improve.NewDistillJob(distiller, meta, c.agent)

	soulLive := soul.NewLive(st.soul)

	return &hub{
		router: router, degraded: degraded, degradedBanner: degradedBanner,
		stores: st, usageStore: usageStore, broker: broker,
		stg: stg, stgMu: stgMu, saveSettings: saveSettings,
		mux: mux, wikiIx: wikiIx, skillStore: skillStore,
		prof: prof, profSynth: profSynth, profMu: profMu,
		refreshProfile: refreshProfile, noteProfile: noteProfile,
		skEmb: skEmb, embedderID: embedderID,
		meta: meta, distiller: distiller, distillJob: distillJob,
		soulLive: soulLive,
		teamWeb:  teamWeb, webCfg: webCfg, webPolicy: webPolicy,
		log: log,
	}, cleanup, nil
}
