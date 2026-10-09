package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/config"
	usagestore "tenant/internal/memory/usage"
	"tenant/internal/model"
	"tenant/internal/model/backend/claudecode"
	"tenant/internal/tui"
)

// modelControl implements tui.ModelControl: list configured backends, switch
// the primary live (rebuild a router → swap it into the running agent so the
// next turn routes there), and add a new vLLM backend mid-session. Guarded so
// concurrent /model commands serialize.
type modelControl struct {
	mu      sync.Mutex
	cfgDir  string
	dataDir string
	agentID string
	ag      *agent.Agent
	log     *slog.Logger
	// degraded is the shared echo-fallback gate (nil when the launch was healthy).
	// Set at launch by buildRouterResilient; cleared here on a reachable swap.
	degraded *degradedState
	// reinstallEmbedder, when set, re-resolves the tool-ranking embedder for the
	// now-active provider and reinstalls it on the tool mux after a model swap
	// (TEN-226 step 4). Without it, ranking keeps using the OLD embedder's
	// fingerprint/cache across a /model use — a dim change then makes every cosine
	// 0 (caught by Search's dim guard, which falls back to the full catalog).
	// Set by cmdTUI; nil elsewhere. Run async by the caller so a slow embedder
	// resolve never blocks the swap.
	reinstallEmbedder func(context.Context)
}

func (mc *modelControl) ModelList() []tui.ModelInfo {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return nil
	}
	return modelInfos(lc, mc.degraded)
}

func modelInfos(lc *launchConfig, degraded *degradedState) []tui.ModelInfo {
	names := make([]string, 0, len(lc.Providers))
	for name := range lc.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	deg := degraded.Degraded()
	out := make([]tui.ModelInfo, 0, len(names))
	for _, name := range names {
		p := lc.Providers[name]
		active := name == lc.Provider
		out = append(out, tui.ModelInfo{
			Name: name, Kind: p.Kind, Endpoint: safeDisplayEndpoint(p.Endpoint), Model: p.Model,
			Active: active,
			// The active provider is the one we tried (and failed) to build, so
			// it's the one actually running on echo. Source of truth is the gate,
			// not config (config still names the real provider — we never persist echo).
			Degraded: deg && active,
		})
	}
	return out
}

func (mc *modelControl) UseModel(name, modelOverride string) (string, string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", "", err
	}
	if lc.Providers[name] == nil {
		return "", "", fmt.Errorf("no backend named %q (see /model)", name)
	}
	// Optional model override — operator-friendly path for providers that
	// serve multiple variants (Z.ai's glm-4.6/5/5.1/..., OpenAI's gpt-*).
	// Empty modelOverride preserves the provider's saved Model field.
	if mo := strings.TrimSpace(modelOverride); mo != "" {
		lc.Providers[name].Model = mo
	}
	lc.Provider = name
	if err := lc.Save(mc.cfgDir); err != nil {
		return "", "", err
	}
	// Resolve the now-active provider (loads the config we just saved,
	// auto-detects the served model + context length).
	c2 := &commonFlags{cfgDir: mc.cfgDir, dataDir: mc.dataDir, agent: mc.agentID, embedDim: defaultEmbedDim}
	if err := c2.resolve(); err != nil {
		return "", "", err
	}
	gen, factories, err := genProfiles(c2)
	if err != nil {
		return "", "", err
	}
	// Mutate the SHARED router in place: re-point the generation roles at the
	// new provider. Because the main agent, every spawned sub-agent, and the
	// background jobs all hold this same router, they ALL re-route at once —
	// no more sub-agents calling the old model.
	r := mc.ag.Router()
	if r == nil {
		return "", "", fmt.Errorf("no active router")
	}
	for kind, f := range factories {
		r.RegisterBackend(kind, f)
	}
	if err := r.SetProfiles(gen); err != nil {
		return "", "", err
	}
	// Re-assert the fixed route roles and provider fallback chains. The
	// planner-big preference normally survives SetProfiles by construction;
	// rebuilding here also picks up a model/key change on its named provider
	// and recomputes chains when the active provider changed.
	if err := installRouteBigRole(r, mc.cfgDir, lc, c2.planCeiling, mc.log); err != nil {
		return "", "", err
	}
	installFallbackChain(r, mc.cfgDir, lc, c2.planCeiling, mc.log)
	r.ProbeVision(context.Background(), false) // TEN-493: probe a newly routed model
	// Re-point tool-ranking at the new provider's embedder (TEN-226 step 4).
	// Async: re-resolving + the lazy precompute mustn't block the swap, and it
	// touches the mux's own lock (never mc.mu), so no nesting. Until it lands, the
	// dim guard keeps a stale-embedder turn correct (falls back to full catalog).
	if mc.reinstallEmbedder != nil {
		go mc.reinstallEmbedder(context.Background())
	}
	active := c2.vllmModel
	if active == "" {
		active = name
	}
	model.Switched.Log(context.Background(), "provider", name, "model", active, "backend", c2.backend,
		"endpoint", safeDisplayEndpoint(c2.vllmEndpoint))
	// Probe the new backend NOW so the operator sees connect success/fail in
	// the feed immediately, instead of discovering a 404 on the next message.
	status := probeSwap(name, active, c2)
	// If we launched degraded (echo fallback) and this swap reached a real,
	// REACHABLE model (status leads with ✓), clear the gate so the suppressed
	// background actors (self-improve, cron, relay) resume. A swap to a still-
	// down endpoint returns ⚠ and leaves the gate set — destructive jobs stay
	// suppressed until a real model is actually reachable.
	if mc.degraded.Degraded() && strings.HasPrefix(status, "✓") {
		mc.degraded.clear()
		status += "  (self-improvement / cron resumed)"
	}
	return status, active, nil
}

// Fallback returns the configured auto-fallback provider chain (TEN-246).
func (mc *modelControl) Fallback() []string {
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return nil
	}
	return lc.Fallbacks
}

// HealthGating describes config.json `health_gating` for /model fallback
// (TEN-358): "off", or what it skips and for how long.
func (mc *modelControl) HealthGating() string {
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "unknown (config.json unreadable)"
	}
	return describeHealthGating(lc.HealthGating.ModelConfig())
}

// SetFallback validates + persists the ordered fallback provider chain and
// re-installs it on the live router (TEN-246) — so it takes effect this session
// with no restart. Each name must be a configured provider; empty clears it.
func (mc *modelControl) SetFallback(names []string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	valid := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if lc.Providers[n] == nil {
			return "", fmt.Errorf("no provider %q (see /model)", n)
		}
		if n == lc.Provider {
			return "", fmt.Errorf("%q is the active model — a fallback must be a DIFFERENT provider", n)
		}
		valid = append(valid, n)
	}
	lc.Fallbacks = valid
	if err := lc.Save(mc.cfgDir); err != nil {
		return "", err
	}
	if r := mc.ag.Router(); r != nil {
		if err := installRouteBigRole(r, mc.cfgDir, lc, lc.PlanLoopCeiling, mc.log); err != nil {
			return "", err
		}
		installFallbackChain(r, mc.cfgDir, lc, lc.PlanLoopCeiling, mc.log)
		r.ProbeVision(context.Background(), false) // TEN-493: probe a newly routed model
	}
	if len(valid) == 0 {
		return "model fallback cleared", nil
	}
	return "model fallback: " + lc.Provider + " → " + strings.Join(valid, " → "), nil
}

// ReloadKeys re-resolves the ACTIVE provider's API key (env → credentials.json)
// and hot-swaps it into the live router, so a key rotated at runtime takes effect
// on the next turn WITHOUT a restart. No-op when no provider is active. It is a
// re-`use` of the current provider — same resolve → SetProfiles → probe path,
// shared by every actor (main agent, cron, relay, dashboard) holding the router,
// and it clears the degraded gate on a reachable result.
func (mc *modelControl) ReloadKeys() (string, error) {
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	active := strings.TrimSpace(lc.Provider)
	if active == "" {
		return "", nil
	}
	status, _, err := mc.UseModel(active, "")
	return status, err
}

// probeSwap verifies the just-swapped backend is reachable and serving the
// expected model, returning a status line that leads with ✓ (good) or ⚠
// (degraded). The router is already swapped either way — a momentarily-down
// server shouldn't block the switch; the next message will retry.
func probeSwap(name, active string, c2 *commonFlags) string {
	switch c2.backend {
	case "echo":
		return fmt.Sprintf("✓ now using %q (echo, offline) — live, no restart", name)
	case "anthropic":
		// Anthropic's API isn't probed via /v1/models; trust the config.
		return fmt.Sprintf("✓ now using %q — %s (Anthropic) — live, no restart", name, active)
	case "claudecode":
		// A local CLI, not an HTTP server — nothing to probe; the next turn shows auth/model errors.
		return fmt.Sprintf("✓ now using %q — %s (Claude Code CLI) — live, no restart", name, active)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ms, err := fetchModels(ctx, c2.vllmEndpoint, c2.genAPIKey)
	if err != nil {
		model.SwapUnready.Log(ctx, "provider", name, "model", active, "endpoint", safeDisplayEndpoint(c2.vllmEndpoint),
			"problem", "its endpoint is unreachable: "+err.Error())
		return fmt.Sprintf("⚠ switched to %q but the endpoint is UNREACHABLE at %s (%v) — start the server, then send a message to retry",
			name, c2.vllmEndpoint, err)
	}
	served := make([]string, 0, len(ms))
	for _, m := range ms {
		served = append(served, m.ID)
		if m.ID == active {
			return fmt.Sprintf("✓ now using %q — connected: %s @ %s (live, no restart)", name, active, c2.vllmEndpoint)
		}
	}
	model.SwapUnready.Log(ctx, "provider", name, "model", active, "endpoint", safeDisplayEndpoint(c2.vllmEndpoint),
		"problem", "it does not serve "+active+" (it serves: "+strings.Join(served, ", ")+")")
	return fmt.Sprintf("⚠ now using %q @ %s, but it does not serve model %q (it serves: %s) — calls may 404; fix with `/model` or set model to \"\" for auto-detect",
		name, c2.vllmEndpoint, active, strings.Join(served, ", "))
}

// LoopCeiling reports the active planner profile's PlanLoopCeiling.
// ListProviderModels resolves the provider's endpoint + auth, then
// queries its live models catalog (/v1/models with /models fallback,
// matching the swap-probe pattern). Empty name = currently-active
// provider. Returns ONLY the model IDs (no max-len, no metadata) —
// for the TUI's `/model models` discovery view.
//
// Best-effort: a slow or unreachable endpoint returns the network
// error to the caller. Auth-required endpoints (Z.ai/OpenAI/etc.)
// must already have their key set up via `/model add-cloud` or
// `tenant setup`; this method reads the saved secret, never accepts
// one inline.
func (mc *modelControl) ListProviderModels(name string) ([]string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(name)
	if target == "" {
		target = lc.Provider
	}
	if target == "" {
		return nil, fmt.Errorf("no active provider — switch with /model use <name> first")
	}
	pc := lc.Providers[target]
	if pc == nil {
		return nil, fmt.Errorf("no backend named %q (see /model)", target)
	}
	// claudecode has no HTTP catalog to query — its endpoint is a binary-
	// discovery sentinel. Offer the Claude CLI's stable aliases so every
	// picker flow (/model pick, /model models, route wizard big-leg) works;
	// full model IDs are also accepted via `/model use claudecode <id>`.
	if pc.Kind == "claudecode" {
		return []string{"fable", "haiku", "opus", "sonnet"}, nil
	}
	endpoint := strings.TrimSpace(pc.Endpoint)
	if endpoint == "" {
		// Fall back to catalog default when the provider's saved config
		// lacks an endpoint (rare — usually only for echo or local kinds).
		if pk, ok := providerKinds[pc.Kind]; ok {
			endpoint = pk.DefaultEndpoint
		}
	}
	if endpoint == "" {
		return nil, fmt.Errorf("provider %q has no endpoint configured", target)
	}
	apiKey := resolveSecret(mc.cfgDir, target, pc.Auth)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ms, err := fetchModels(ctx, endpoint, apiKey)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

func (mc *modelControl) LoopCeiling() int {
	r := mc.ag.Router()
	if r == nil {
		return 0
	}
	if p, err := r.ForRole(model.RolePlanner); err == nil {
		return p.PlanLoopCeiling
	}
	return 0
}

// SetLoopCeiling retunes the loop ceiling live on the shared router (so the
// main agent, sub-agents, and jobs all follow on their next turn) and persists
// it to config so it survives a restart and applies across model swaps.
func (mc *modelControl) SetLoopCeiling(n int) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if n < 1 || n > 200 {
		return "", fmt.Errorf("ceiling must be between 1 and 200")
	}
	r := mc.ag.Router()
	if r == nil {
		return "", fmt.Errorf("no active router")
	}
	updated := r.SetPlanLoopCeiling(n)
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err == nil {
		lc.PlanLoopCeiling = n
		err = lc.Save(mc.cfgDir)
	}
	if err != nil {
		return "", errNotSaved(fmt.Sprintf("loop ceiling %d", n), err)
	}
	return fmt.Sprintf("loop ceiling → %d (updated %d role(s); applies next turn, incl. newly spawned sub-agents)", n, updated), nil
}

// RepeatGuard reports the oscillation guard's live threshold (<= 0 = off).
func (mc *modelControl) RepeatGuard() int { return agent.RepeatGuard() }

// SetRepeatGuard retunes the oscillation guard live for every agent in the
// process and persists it (TEN-506). n < 0 turns it off; 0 restores the default.
func (mc *modelControl) SetRepeatGuard(n int) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if n == 1 || n > 200 {
		return "", fmt.Errorf("repeat guard must be between 2 and 200 (0 = default %d, off = disabled)", agent.DefaultRepeatGuard)
	}
	agent.SetRepeatGuard(n)
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err == nil {
		lc.RepeatGuard = n
		err = lc.Save(mc.cfgDir)
	}
	status := fmt.Sprintf("repeat guard → %d identical rounds (applies next round, all agents)", agent.RepeatGuard())
	if agent.RepeatGuard() <= 0 {
		status = "repeat guard → off (only the loop ceiling ends a turn)"
	}
	if err != nil {
		return "", errNotSaved(status, err)
	}
	return status, nil
}

// Reasoning-effort level sets are PER KIND: Sakana Fugu's API takes high +
// xhigh (with max accepted as an alias for xhigh), while claudecode passes
// --effort straight through to the Claude CLI, whose ladder is the full
// low → max range — and there max is a REAL level, not an alias. "" clears
// either (off, the provider default). The /reasoning picker offers exactly
// the active kind's set via ReasoningLevels().
var (
	fuguReasoningLevels   = []string{"high", "xhigh"}
	claudeReasoningLevels = []string{"low", "medium", "high", "xhigh", "max"}
)

// reasoningLevelsForKind maps a provider kind to its accepted effort values.
func reasoningLevelsForKind(kind string) []string {
	if kind == "claudecode" {
		return claudeReasoningLevels
	}
	return fuguReasoningLevels
}

// ReasoningSupported reports whether the ACTIVE provider's kind accepts a
// reasoning-effort hint (today: Sakana Fugu). The /reasoning command refuses to
// set an effort on a provider that would 400 on the unknown field.
func (mc *modelControl) ReasoningSupported() bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil || lc.Providers[lc.Provider] == nil {
		return false
	}
	return providerKinds[lc.Providers[lc.Provider].Kind].SupportsReasoning
}

// ReasoningEffort returns the active provider's persisted reasoning-effort hint
// ("" = off / default).
func (mc *modelControl) ReasoningEffort() string {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil || lc.Providers[lc.Provider] == nil {
		return ""
	}
	return lc.Providers[lc.Provider].Reasoning
}

// ReasoningLevels returns the effort values the ACTIVE provider accepts —
// drives the /reasoning picker, so Fugu offers high/xhigh while claudecode
// offers the Claude CLI's full low→max ladder.
func (mc *modelControl) ReasoningLevels() []string {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil || lc.Providers[lc.Provider] == nil {
		return fuguReasoningLevels
	}
	return reasoningLevelsForKind(lc.Providers[lc.Provider].Kind)
}

// SetReasoningEffort retunes the active provider's reasoning effort live on the
// shared router (so the main agent + sub-agents follow next turn) and persists it
// per-provider so it survives a restart. Mirrors UseModel's rebuild path, minus
// the probe/embedder work (endpoint + model are unchanged). effort "" / "off"
// clears it; "max" is accepted as Fugu's alias for "xhigh".
func (mc *modelControl) SetReasoningEffort(effort string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "off" || effort == "none" || effort == "default" {
		effort = ""
	}
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	pc := lc.Providers[lc.Provider]
	if pc == nil {
		return "", fmt.Errorf("no active provider (see /model)")
	}
	if !providerKinds[pc.Kind].SupportsReasoning {
		return "", fmt.Errorf("provider %q (%s) doesn't support reasoning effort — reasoning models (Sakana Fugu) and claudecode do", lc.Provider, pc.Kind)
	}
	levels := reasoningLevelsForKind(pc.Kind)
	// Fugu alias only: its API has no distinct max level. For claudecode,
	// max is a real Claude effort level and passes through untouched.
	if effort == "max" && pc.Kind != "claudecode" {
		effort = "xhigh"
	}
	if effort != "" {
		valid := false
		for _, v := range levels {
			if v == effort {
				valid = true
				break
			}
		}
		if !valid {
			return "", fmt.Errorf("unknown reasoning effort %q (one of: %s, or 'off' to clear)", effort, strings.Join(levels, ", "))
		}
	}
	pc.Reasoning = effort
	if err := lc.Save(mc.cfgDir); err != nil {
		return "", err
	}
	if err := mc.rebuildActiveLocked(lc); err != nil {
		return "", err
	}
	if effort == "" {
		return fmt.Sprintf("reasoning effort cleared for %s (applies next turn)", lc.Provider), nil
	}
	return fmt.Sprintf("reasoning effort → %s for %s (applies next turn, incl. sub-agents)", effort, lc.Provider), nil
}

// rebuildActiveLocked rebuilds the generation profiles for the (unchanged)
// active provider from the saved config and swaps them onto the shared router:
// the same in-place mutation UseModel uses, so every agent re-routes with the
// new setting on its next turn. Callers hold mc.mu.
func (mc *modelControl) rebuildActiveLocked(lc *launchConfig) error {
	c2 := &commonFlags{cfgDir: mc.cfgDir, dataDir: mc.dataDir, agent: mc.agentID, embedDim: defaultEmbedDim}
	if err := c2.resolve(); err != nil {
		return err
	}
	gen, factories, err := genProfiles(c2)
	if err != nil {
		return err
	}
	r := mc.ag.Router()
	if r == nil {
		return fmt.Errorf("no active router")
	}
	for kind, f := range factories {
		r.RegisterBackend(kind, f)
	}
	if err := r.SetProfiles(gen); err != nil {
		return err
	}
	if err := installRouteBigRole(r, mc.cfgDir, lc, c2.planCeiling, mc.log); err != nil {
		return err
	}
	installFallbackChain(r, mc.cfgDir, lc, c2.planCeiling, mc.log)
	r.ProbeVision(context.Background(), false) // TEN-493: probe a newly routed model
	return nil
}

// Vision reports whether the active model can see images (TEN-493): the
// provider's setting (on, off, auto) and, for auto, what the probe found.
func (mc *modelControl) Vision() string {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	setting := "auto"
	if lc, err := loadLaunchConfig(mc.cfgDir); err == nil && lc.Providers[lc.Provider] != nil && lc.Providers[lc.Provider].Vision != "" {
		setting = lc.Providers[lc.Provider].Vision
	}
	r := mc.ag.Router()
	if r == nil {
		return "vision: " + setting
	}
	p, err := r.ForRole(model.RolePlanner)
	if err != nil {
		return "vision: " + setting
	}
	sees := "can't see images"
	if model.CanSee(p) {
		sees = "can see images"
	}
	if setting == "auto" && p.Backend == "vllm" {
		return fmt.Sprintf("vision: auto (probe: %s), %s %s", model.VisionOf(model.VisionKeyOf(p)), p.Model, sees)
	}
	return fmt.Sprintf("vision: %s, %s %s", setting, p.Model, sees)
}

// SetVision sets the active provider's vision to on, off or auto (probe),
// live on the shared router, and saves it (TEN-493).
func (mc *modelControl) SetVision(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "auto", "":
		v = ""
	case "on", "off":
	default:
		return "", fmt.Errorf("vision must be on, off or auto")
	}
	mc.mu.Lock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		mc.mu.Unlock()
		return "", err
	}
	pc := lc.Providers[lc.Provider]
	if pc == nil {
		mc.mu.Unlock()
		return "", fmt.Errorf("no active provider (see /model)")
	}
	pc.Vision = v
	if err := lc.Save(mc.cfgDir); err != nil {
		mc.mu.Unlock()
		return "", errNotSaved("vision "+firstNonEmpty(v, "auto"), err)
	}
	err = mc.rebuildActiveLocked(lc)
	mc.mu.Unlock()
	if err != nil {
		return "", err
	}
	return mc.Vision() + " (applies next turn)", nil
}

// looksLikeURL and safeDisplayEndpoint live in internal/config (TEN-314),
// which validates endpoints on load.
func looksLikeURL(s string) bool { return config.LooksLikeURL(s) }

func safeDisplayEndpoint(s string) string { return config.SafeDisplayEndpoint(s) }

// clipSecret renders a suspect string for an error message WITHOUT
// echoing the whole thing. If the value happened to be an API key,
// printing it verbatim in an error message leaks it to the operator's
// transcript, scrollback, and any log capture. Show first 6 chars +
// total length so the operator can still recognize what they typed.
func clipSecret(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 8 {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q (%d chars; redacted — looked like a token, not a URL)", s[:6]+"…", len(s))
}

func (mc *modelControl) AddModel(name, endpoint, toolFormat string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	name = strings.TrimSpace(name)
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if name == "" {
		return "", fmt.Errorf("name and endpoint are required")
	}
	if endpoint == "" {
		// Keyless catalog kinds (claudecode today) register straight from the
		// catalog with no endpoint or key: `/model add claudecode`.
		if pk, ok := providerKinds[name]; ok && !pk.NeedsKey && pk.Wired && pk.DefaultEndpoint != "" {
			return mc.addKeylessCatalogProvider(name)
		}
		return "", fmt.Errorf("name and endpoint are required (only keyless catalog kinds like `claudecode` may omit the endpoint)")
	}
	// Reject non-URL endpoints. This catches the very-easy-to-make mistake
	// of typing `/model add zai <api-key>` (which would save the key as the
	// endpoint URL and leak it through the model list). When the name
	// matches a known cloud-provider kind, point the operator at the right
	// command instead of just rejecting. NEVER echo the full bad input —
	// it might be a real secret.
	if !looksLikeURL(endpoint) {
		safe := clipSecret(endpoint)
		if _, isKind := providerKinds[name]; isKind {
			return "", fmt.Errorf("endpoint %s is not a URL — did you mean `/model add-cloud %s <api-key>`? "+
				"`/model add` is for self-hosted vLLM endpoints; cloud providers use `add-cloud`",
				safe, name)
		}
		return "", fmt.Errorf("endpoint must be an http:// or https:// URL; got %s", safe)
	}
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}
	lc.Providers[name] = &providerConfig{Kind: "vllm", Endpoint: endpoint, ToolFmt: strings.TrimSpace(toolFormat)}
	// Make sure an embeddings provider exists so a later `use` builds a full router.
	if lc.Embed == nil {
		lc.Embed = &providerConfig{Kind: "ollama", Endpoint: defaultEmbedEndpoint, Model: defaultEmbedModel, EmbedDim: defaultEmbedDim}
	}
	if err := lc.Save(mc.cfgDir); err != nil {
		return "", err
	}
	return fmt.Sprintf("added vLLM backend %q → %s — switch with `/model use %s`. "+
		"Router bind hint: set `route.big_role` to %q, then use `/route on`.", name, endpoint, name, name), nil
}

// AddCloudModel registers a KEYED cloud provider (zai/openai/grok/anthropic)
// in one shot using the catalog's known endpoint + chat path + default model
// + tool format. The API key is stored in protected credentials.json. The
// provider gets registered under its kind id as the name (so `zai` is its
// own bucket, distinct from any vLLM-with-name-zai an operator might add).
// Idempotent: re-running with a new key overwrites just the secret.
// AddCloudModel is the TUI's /model add-cloud; the Keys and Model pages call
// addCloudModel, naming the dashboard.
func (mc *modelControl) AddCloudModel(kindID, apiKey string) (string, error) {
	return mc.addCloudModel(kindID, apiKey, surfaceTUI)
}

func (mc *modelControl) addCloudModel(kindID, apiKey, surface string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	kindID = strings.TrimSpace(kindID)
	apiKey = strings.TrimSpace(apiKey)
	pk, ok := providerKinds[kindID]
	if !ok {
		return "", fmt.Errorf("unknown provider kind %q (known: %s)", kindID, strings.Join(providerOrder, ", "))
	}
	if !pk.NeedsKey {
		// Keyless kind typed into add-cloud (easy mistake — the operator
		// doesn't care which command family a kind belongs to): do the
		// keyless add instead of scolding. A mistakenly-pasted key is NOT
		// stored anywhere.
		status, err := mc.addKeylessCatalogProvider(kindID)
		if err == nil && apiKey != "" {
			status += " (note: this provider takes no API key — the value you passed was not stored)"
		}
		return status, err
	}
	if !pk.Wired {
		return "", fmt.Errorf("%s is in the catalog but its backend is not yet implemented", pk.Label)
	}
	if apiKey == "" {
		return "", fmt.Errorf("an API key is required for %s (passed as the second arg)", pk.Label)
	}
	if pk.DefaultEndpoint == "" {
		return "", fmt.Errorf("internal: %s has no DefaultEndpoint in the catalog", pk.Label)
	}
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}
	// Build the provider entry from the catalog. Preserve any pre-existing
	// model override (don't blow away a custom DefaultModel the operator set).
	existing := lc.Providers[kindID]
	model := pk.DefaultModel
	if existing != nil && existing.Model != "" {
		model = existing.Model
	}
	lc.Providers[kindID] = &providerConfig{
		Kind: kindID, Endpoint: pk.DefaultEndpoint, Model: model,
		ToolFmt: pk.DefaultToolFmt,
		Auth:    authCfg{Mode: "apikey", Stored: true},
	}
	// Make sure an embeddings provider exists so a later `use` builds a full
	// router. Cloud LLM providers don't ship embeddings (yet), so default to
	// the local Ollama + nomic-embed-text stack.
	if lc.Embed == nil {
		lc.Embed = &providerConfig{Kind: "ollama", Endpoint: defaultEmbedEndpoint, Model: defaultEmbedModel, EmbedDim: defaultEmbedDim}
	}
	if err := lc.Save(mc.cfgDir); err != nil {
		return "", err
	}
	// Store the key in protected credentials.json.
	creds, _ := loadCredentials(mc.cfgDir)
	creds.Set(kindID, apiKey)
	if err := saveCredentials(mc.cfgDir, creds, surface); err != nil {
		return "", fmt.Errorf("save credentials: %w", err)
	}
	return fmt.Sprintf("added %s backend %q → %s (key stored with owner-only protection). Switch with `/model use %s`. "+
		"Router bind hint: set `route.big_role` to %q, then use `/route on`.",
		pk.Label, kindID, pk.DefaultEndpoint, kindID, kindID), nil
}

// addKeylessCatalogProvider registers a catalog kind that authenticates
// WITHOUT an API key — today that's claudecode (Claude Code subscription
// login). Endpoint, default model, and tool format all come from the
// catalog. Caller must hold mc.mu. For claudecode the binary is probed at
// add time so a missing/uninstalled CLI is reported here, with an
// actionable hint, instead of surfacing as a failure on the first
// escalated turn.
func (mc *modelControl) addKeylessCatalogProvider(kindID string) (string, error) {
	pk, ok := providerKinds[kindID]
	if !ok {
		return "", fmt.Errorf("unknown provider kind %q (known: %s)", kindID, strings.Join(providerOrder, ", "))
	}
	if pk.NeedsKey {
		return "", fmt.Errorf("%s requires an API key — use `/model add-cloud %s <api-key>`", pk.Label, kindID)
	}
	if !pk.Wired {
		return "", fmt.Errorf("%s is in the catalog but its backend is not yet implemented", pk.Label)
	}
	if pk.DefaultEndpoint == "" {
		// vllm/echo land here: keyless but endpoint-less in the catalog, so
		// they can't be one-shot added. Keep the original operator-facing
		// wording (tests + muscle memory rely on it).
		return "", fmt.Errorf("%s does not use an API key — use `/model add %s <endpoint>` for self-hosted backends", pk.Label, kindID)
	}
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}
	// Preserve a pre-existing model override, same as AddCloudModel.
	existing := lc.Providers[kindID]
	model := pk.DefaultModel
	if existing != nil && existing.Model != "" {
		model = existing.Model
	}
	lc.Providers[kindID] = &providerConfig{
		Kind: kindID, Endpoint: pk.DefaultEndpoint, Model: model,
		ToolFmt: pk.DefaultToolFmt,
	}
	if lc.Embed == nil {
		lc.Embed = &providerConfig{Kind: "ollama", Endpoint: defaultEmbedEndpoint, Model: defaultEmbedModel, EmbedDim: defaultEmbedDim}
	}
	if err := lc.Save(mc.cfgDir); err != nil {
		return "", err
	}
	note := ""
	if pk.Backend == "claudecode" {
		if bin, derr := claudecode.DiscoverBinary(pk.DefaultEndpoint); derr == nil {
			note = fmt.Sprintf(" Claude CLI: %s (billing: your subscription).", bin)
		} else {
			note = " ⚠ Claude CLI not found — install it, run `claude` → /login once, or set TENANT_CLAUDE_BIN to the binary."
		}
	}
	return fmt.Sprintf("added %s as %q (model %s, no API key).%s Switch with `/model use %s`. "+
		"Router bind hint: set the big leg to %q via /route configure.",
		pk.Label, kindID, model, note, kindID, kindID), nil
}

// RemoveModel deletes a configured backend (and its stored credential).
// Refuses to remove the currently-active model — switch first. RemoveModel
// is the TUI's /model remove; the Model page calls removeModel.
func (mc *modelControl) RemoveModel(name string) (string, error) {
	return mc.removeModel(name, surfaceTUI)
}

func (mc *modelControl) removeModel(name, surface string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Providers[name] == nil {
		return "", fmt.Errorf("no backend named %q (see /model)", name)
	}
	if lc.Provider == name {
		return "", fmt.Errorf("%q is the active model — `/model use <other>` first, then remove it", name)
	}
	delete(lc.Providers, name)
	if err := forgetProviderSecret(mc.cfgDir, name, surface); err != nil {
		return "", err
	}
	if err := lc.Save(mc.cfgDir); err != nil {
		return "", err
	}
	return fmt.Sprintf("removed backend %q", name), nil
}

// forgetProviderSecret drops any stored API key keyed by the provider name.
// An error means the key is still on disk, which the operator removing it
// must hear about.
func forgetProviderSecret(cfgDir, name, surface string) error {
	creds, err := loadCredentials(cfgDir)
	if err != nil {
		return fmt.Errorf("forget the %s key: %w", name, err)
	}
	if _, ok := creds.Secrets[name]; !ok {
		return nil
	}
	delete(creds.Secrets, name)
	if err := saveCredentials(cfgDir, creds, surface); err != nil {
		return fmt.Errorf("forget the %s key: %w", name, err)
	}
	return nil
}

// cmdModel is the `tenant model` CLI: manage model backends in config.json.
//
//	tenant model list
//	tenant model show
//	tenant model use <name>
//	tenant model add <name> --endpoint URL [--kind vllm] [--tool-format qwen] [--api-key K]
//	tenant model remove <name>
func cmdModel(_ context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	// Pull leading positional(s) before parsing — Go's flag parser stops
	// at the first non-flag token, so `model add NAME --endpoint X` would
	// otherwise drop the flags. We support up to 2 leading positionals:
	// for `use`, that's [name, optional model variant].
	var name, posModel string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		posModel, rest = rest[0], rest[1:]
	}
	fs := flag.NewFlagSet("model "+sub, flag.ContinueOnError)
	c := bindCommon(fs)
	// Note: --api-key + --vllm-* are already registered by bindCommon; reuse
	// them (re-registering would panic). For `add` we read c.genAPIKey.
	var endpoint, kind, mdl, toolFmt string
	if sub == "add" {
		fs.StringVar(&endpoint, "endpoint", "", "backend base URL (no /v1 suffix)")
		fs.StringVar(&kind, "kind", "vllm", "backend kind: "+strings.Join(providerOrder, "|"))
		fs.StringVar(&mdl, "model", "", "model id (blank = auto-detect at launch)")
		fs.StringVar(&toolFmt, "tool-format", "", "qwen|gemma|llama|mistral|openai")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	c.cfgDir = expandPath(c.cfgDir)
	if err := c.resolveDirs(); err != nil {
		return err
	}
	defer fileEvents(c)() // the Security records of the change
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		return err
	}
	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}
	if name == "" && fs.NArg() > 0 { // flags-first form: `model use --foo bar NAME`
		name = fs.Arg(0)
	}

	switch sub {
	case "list", "ls":
		printModelList(lc)
		return nil
	case "show":
		p := lc.Active()
		if p == nil {
			fmt.Println("no active model backend (run `tenant setup` or `tenant model add`)")
			return nil
		}
		fmt.Printf("active: %s\n  kind:     %s\n  endpoint: %s\n  model:    %s\n  tool fmt: %s\n",
			lc.Provider, p.Kind, orDash(safeDisplayEndpoint(p.Endpoint)), orDash(p.Model), orDash(p.ToolFmt))
		return nil
	case "use":
		if name == "" {
			return fmt.Errorf("usage: tenant model use <name> [<model>]   (model optional — pins a variant, e.g. `tenant model use zai glm-5.1`)")
		}
		if lc.Providers[name] == nil {
			return fmt.Errorf("no backend named %q (see `tenant model list`)", name)
		}
		// Optional model override — second positional. Mirrors the TUI's
		// `/model use <name> [<model>]` shape.
		if posModel != "" {
			lc.Providers[name].Model = posModel
		}
		lc.Provider = name
		if err := lc.Save(c.cfgDir); err != nil {
			return err
		}
		extra := ""
		if posModel != "" {
			extra = " (model pinned: " + posModel + ")"
		}
		fmt.Printf("primary model → %q%s (restart tenant, or use `/model use %s` in the TUI for a live swap)\n", name, extra, name)
		return nil
	case "models", "model-list", "list-models":
		// `tenant model models [<name>]` — live fetch of the model
		// catalog served by the provider's endpoint. Name optional;
		// defaults to active.
		target := name
		if target == "" {
			target = lc.Provider
		}
		if target == "" {
			return fmt.Errorf("no active provider — switch with `tenant model use <name>` first")
		}
		pc := lc.Providers[target]
		if pc == nil {
			return fmt.Errorf("no backend named %q (see `tenant model list`)", target)
		}
		if pc.Kind == "claudecode" {
			// No HTTP catalog behind the sentinel endpoint — print the CLI's
			// stable aliases (full model IDs also work with `model use`).
			fmt.Printf("4 model alias(es) for %q (claude -p; full Claude model IDs also accepted):\n", target)
			for _, id := range []string{"fable", "haiku", "opus", "sonnet"} {
				fmt.Printf("  · %s\n", id)
			}
			return nil
		}
		endpoint := strings.TrimSpace(pc.Endpoint)
		if endpoint == "" {
			if pk, ok := providerKinds[pc.Kind]; ok {
				endpoint = pk.DefaultEndpoint
			}
		}
		if endpoint == "" {
			return fmt.Errorf("provider %q has no endpoint configured", target)
		}
		apiKey := resolveSecret(c.cfgDir, target, pc.Auth)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ms, err := fetchModels(ctx, endpoint, apiKey)
		if err != nil {
			return fmt.Errorf("list models from %s: %w", endpoint, err)
		}
		fmt.Printf("%d model(s) served by %q (%s):\n", len(ms), target, endpoint)
		ids := make([]string, 0, len(ms))
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Printf("  · %s\n", id)
		}
		if posModel != "" {
			// `tenant model models zai glm-5.1` is a noop-ish form;
			// most operators probably meant `use`. Hint, don't error.
			fmt.Fprintf(os.Stderr, "\n(hint: to switch + pin a variant, use: tenant model use %s %s)\n", target, posModel)
		}
		return nil
	case "add":
		if name == "" {
			return fmt.Errorf("usage: tenant model add <name> --endpoint URL [--kind ...] [--tool-format ...]   (keyless: tenant model add claudecode)")
		}
		// Bare `tenant model add claudecode`: when the name IS a keyless,
		// non-OpenAI-compat catalog kind and --kind was left at its vllm
		// default, infer the kind from the name so no flags are needed.
		if kind == "vllm" && name != "vllm" {
			if nk, isKind := providerKinds[name]; isKind && !nk.NeedsKey && nk.Backend != "vllm" && nk.DefaultEndpoint != "" {
				kind = name
			}
		}
		pk, ok := providerKinds[kind]
		if !ok {
			return fmt.Errorf("unknown kind %q (one of %s)", kind, strings.Join(providerOrder, ", "))
		}
		if endpoint == "" {
			endpoint = pk.DefaultEndpoint
		}
		if endpoint == "" {
			return fmt.Errorf("--endpoint is required for kind %q", kind)
		}
		// Don't silently default the tool format to gemma (TEN-138): when it
		// wasn't passed, prompt on a TTY. selectOne returns the supplied default
		// on a non-TTY, so non-interactive use keeps working.
		if pk.Backend == "vllm" && toolFmt == "" {
			toolFmt = selectOne("Tool format for "+name, toolFormatOpts(), firstNonEmpty(pk.DefaultToolFmt, defaultVLLMToolFmt))
		}
		pc := &providerConfig{Kind: kind, Endpoint: strings.TrimRight(endpoint, "/"), Model: mdl, ToolFmt: toolFmt}
		if c.genAPIKey != "" {
			creds, _ := loadCredentials(c.cfgDir)
			creds.Set(name, c.genAPIKey)
			if err := saveCredentials(c.cfgDir, creds, surfaceCLI); err != nil {
				return err
			}
			pc.Auth = authCfg{Mode: "apikey", Stored: true}
		}
		lc.Providers[name] = pc
		if lc.Embed == nil {
			lc.Embed = &providerConfig{Kind: "ollama", Endpoint: defaultEmbedEndpoint, Model: defaultEmbedModel, EmbedDim: defaultEmbedDim}
		}
		if err := lc.Save(c.cfgDir); err != nil {
			return err
		}
		fmt.Printf("added backend %q (kind=%s endpoint=%s) — `tenant model use %s` to make it primary; "+
			"set route.big_role to %q to use it as the prompt-router big target\n",
			name, kind, safeDisplayEndpoint(pc.Endpoint), name, name)
		return nil
	case "remove", "rm", "delete":
		if name == "" {
			return fmt.Errorf("usage: tenant model remove <name>")
		}
		if lc.Providers[name] == nil {
			return fmt.Errorf("no backend named %q", name)
		}
		if lc.Provider == name {
			return fmt.Errorf("%q is the active model — `tenant model use <other>` first, then remove it", name)
		}
		delete(lc.Providers, name)
		if err := forgetProviderSecret(c.cfgDir, name, surfaceCLI); err != nil {
			return err
		}
		if err := lc.Save(c.cfgDir); err != nil {
			return err
		}
		fmt.Printf("removed backend %q\n", name)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (list|show|use|add|remove)", sub)
	}
}

func printModelList(lc *launchConfig) {
	infos := modelInfos(lc, nil) // headless CLI: never degraded
	if len(infos) == 0 {
		fmt.Println("no model backends configured — `tenant model add <name> --endpoint URL` or run `tenant setup`")
		return
	}
	for _, mi := range infos {
		mark := " "
		if mi.Active {
			mark = "*"
		}
		model := mi.Model
		if model == "" {
			model = "(auto)"
		}
		fmt.Printf("%s %-16s %-10s %-22s %s\n", mark, mi.Name, mi.Kind, model, mi.Endpoint)
	}
}

// recordUsage files one main-agent model call in the usage ledger (TEN-167),
// under the launch's model. A failed write is logged, never returned: the
// ledger must not block a turn. s may be nil (no ledger).
func recordUsage(ctx context.Context, s *usagestore.Store, c *commonFlags, in, out int, traceID string) {
	name := c.vllmModel
	if c.backend == "echo" || name == "" {
		name = c.backend
	}
	if err := s.Record(ctx, c.agent, name, in, out, traceID); err != nil {
		model.UsageUnrecorded.Log(ctx, "model", name, "err", err)
	}
}
