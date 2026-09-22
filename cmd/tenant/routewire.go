package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/episodic"
	"tenant/internal/model"
	"tenant/internal/route"
)

// routeRuntime is the cmd-layer adapter shared by the agent hook and TUI
// control. It owns the local classifier and decision history; agent only sees
// generic tags and a prompt-free event summary.
type routeRuntime struct {
	mu  sync.RWMutex
	cfg resolvedRouteConfig
	// cfgMu serializes route-config read-modify-write cycles (SetMode, Force,
	// SetBig, SetPolicy) so two route mutations can't interleave their
	// load→mutate→save windows and silently drop each other's writes. It does
	// not cover non-route config writers (/model, credentials watcher) — those
	// serialize on modelControl.mu and can still race any route save, a
	// pre-existing property of the whole-file load→save pattern.
	cfgMu              sync.Mutex
	pendingSuggestions map[string]string
	engine             *route.Engine
	cfgDir             string
	dataDir            string
	router             *model.Router
	log                *slog.Logger
	plannerCloudNotice bool
	configuredSmall    route.Target
	configuredBig      route.Target
	classifier         *swappableClassifier
}

// swappableClassifier is the Engine's classifier indirection. Engine and
// Supra are deliberately immutable after construction, so a live route-config
// change (enabling routing mid-session, a provider edit picked up on the next
// reload) swaps a freshly built Supra in here instead — atomically, without
// blocking an in-flight classify or touching the Engine's locking.
type swappableClassifier struct {
	p atomic.Pointer[route.Supra]
}

func (s *swappableClassifier) store(cfg resolvedRouteConfig) {
	budget := time.Duration(cfg.TimeoutMS) * time.Millisecond
	sup := route.NewSupra(cfg.Endpoint, cfg.Model, budget)
	s.p.Store(sup)
	// Keep the agent's active-routing join budget equal to the classifier's own
	// timeout. Before this, the join was hardcoded at 250ms while timeout_ms fed
	// only the Supra HTTP client — so raising timeout_ms looked like it granted
	// more time and granted none, and any classifier slower than 250ms produced
	// "routed role unavailable; using planner" with zero time left to resolve.
	agent.SetActiveRouteBudget(budget)
	// Pre-load the model off the hot path so the FIRST real classify — and the
	// /route status health probe — do not pay the cold GGUF load cost under the
	// tight per-turn timeout and report "unhealthy (context deadline exceeded)".
	// Best-effort; keep_alive then holds the model resident. Fires on the initial
	// build and whenever a config change rebuilds the classifier.
	if cfg.Endpoint != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			_ = sup.Warm(ctx)
		}()
	}
}

func (s *swappableClassifier) Classify(ctx context.Context, query string) (route.Signal, error) {
	// A nil load is handled by Supra.Classify's own nil-receiver guard.
	return s.p.Load().Classify(ctx, query)
}

// warm synchronously pre-loads the current model using the caller's context
// deadline (not the tight per-turn timeout). The health probe uses it to recover
// from a cold model instead of reporting it unhealthy.
func (s *swappableClassifier) warm(ctx context.Context) error {
	sup := s.p.Load()
	if sup == nil {
		return nil
	}
	return sup.Warm(ctx)
}

// refreshClassifier rebuilds the classifier after a persisted config change
// altered its connection parameters. Called by every Set* after the cfg swap;
// a no-op when endpoint/model/timeout are unchanged.
func (r *routeRuntime) refreshClassifier(prev, cur resolvedRouteConfig) {
	if r.classifier == nil {
		return
	}
	if prev.Endpoint == cur.Endpoint && prev.Model == cur.Model && prev.TimeoutMS == cur.TimeoutMS {
		return
	}
	r.classifier.store(cur)
	if r.log != nil {
		r.log.Info("route: classifier rebuilt", "endpoint", safeDisplayEndpoint(cur.Endpoint), "model", cur.Model)
	}
}

func newRouteRuntime(lc *launchConfig, cfgDir, dataDir string, router *model.Router, log *slog.Logger) (*routeRuntime, error) {
	cfg, err := resolveRouteConfig(lc)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	classifier := &swappableClassifier{}
	classifier.store(cfg)
	rt := &routeRuntime{
		cfg: cfg, cfgDir: cfgDir, dataDir: dataDir, router: router, log: log,
		classifier: classifier,
		engine:     route.NewEngine(classifier, dataDir, 0), pendingSuggestions: make(map[string]string),
	}
	if lc != nil {
		rt.configuredSmall, rt.configuredBig = configuredRouteTargets(lc, cfg)
		if pc := lc.active(); pc != nil {
			pk, known := providerKinds[pc.Kind]
			rt.plannerCloudNotice = !known || !pk.Local
		}
	}
	// Retention (TEN-292): arm daily pruning, and apply it once at wiring
	// time — but only in the LIVE process (router != nil). Headless CLI
	// invocations (status/stats) construct a runtime against the same JSONL a
	// live tenant is appending to; a cross-process read-filter-rename there
	// would silently drop concurrently appended rows.
	rt.engine.SetRetention(cfg.LogRetentionDays)
	if router != nil {
		if _, dropped, err := rt.engine.Prune(); err != nil {
			log.Warn("route: startup retention prune failed", "err", err)
		} else if dropped > 0 {
			log.Info("route: retention pruned decision log", "dropped", dropped, "retention_days", cfg.LogRetentionDays)
		}
	}
	return rt, nil
}

// warnRuleCapabilities validates N-way rules against their roles' live
// bindings (TEN-292). Called from BindConfiguredForce — i.e. AFTER
// installRouteBigRole has registered planner-big — so a rule targeting
// planner-big doesn't produce a spurious unbound-role warning on every start.
// Warnings, not errors: capability tags are advisory metadata, and an unbound
// rule role degrades per-turn to the tier decision anyway.
func (r *routeRuntime) warnRuleCapabilities() {
	if r == nil || len(r.cfg.Rules) == 0 {
		return
	}
	for i, rule := range r.cfg.Rules {
		if r.router == nil {
			break
		}
		p, err := r.router.ForRole(rule.Role)
		if err != nil {
			r.log.Warn("route: rule role has no bound profile; rule degrades to the tier decision",
				"rule", i, "role", string(rule.Role), "err", err)
			continue
		}
		if rule.When.Code != nil && *rule.When.Code && !capabilityAtLeastMedium(p.Capabilities, "code_quality") {
			r.log.Warn("route: rule sends code turns to a profile with low code_quality",
				"rule", i, "role", string(rule.Role), "profile", p.ID)
		}
		if rule.When.Math != nil && *rule.When.Math && !capabilityAtLeastMedium(p.Capabilities, "math_quality") {
			r.log.Warn("route: rule sends math turns to a profile with low math_quality",
				"rule", i, "role", string(rule.Role), "profile", p.ID)
		}
	}
}

// capabilityAtLeastMedium interprets the shipped capability-tag vocabulary:
// missing means "unknown, do not warn"; an explicit "low" (or false) warns.
func capabilityAtLeastMedium(caps map[string]any, key string) bool {
	v, ok := caps[key]
	if !ok {
		return true
	}
	switch x := v.(type) {
	case string:
		return !strings.EqualFold(x, "low")
	case bool:
		return x
	default:
		return true
	}
}

func configuredRouteTargets(lc *launchConfig, cfg resolvedRouteConfig) (route.Target, route.Target) {
	small := route.Target{Tier: route.TierSmall, Role: model.RolePlanner}
	big := route.Target{Tier: route.TierBig, Role: plannerBigRole}
	fill := func(target *route.Target, providerName string, namespaced bool) {
		if lc == nil || strings.TrimSpace(providerName) == "" {
			return
		}
		pc := lc.Providers[providerName]
		if pc == nil {
			return
		}
		if ids := generationProfileIDs(pc); len(ids) > 0 {
			target.ProfileID = ids[0]
			if namespaced {
				target.ProfileID = providerName + "/" + target.ProfileID
			}
		}
		kind, known := providerKinds[pc.Kind]
		target.Local = known && kind.Local
	}
	fill(&small, strings.TrimSpace(lc.Provider), false)
	fill(&big, strings.TrimSpace(cfg.BigRole), true)
	return small, big
}

// Hook is stable even while routing is disabled so stage changes take effect
// live. Shadow calls remain asynchronous; assisted !big and active stages use
// the bounded join selected by Active.
func (r *routeRuntime) Hook() agent.RouteHook {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, req agent.RouteRequest) agent.RouteObservation {
		cfg, mode := r.effective(req)
		if !cfg.Enabled || r.engine == nil {
			return agent.RouteObservation{}
		}
		assistedAcceptance := cfg.Assist && mode == route.ModeShadow && req.Active &&
			req.Surface.Interactive() && req.Override == agent.RouteOverrideBig
		if assistedAcceptance {
			r.confirmPendingSuggestion(req.AgentID, req.SessionID)
		} else if cfg.Assist && mode == route.ModeShadow {
			r.clearPendingSuggestion(req.AgentID, req.SessionID)
		}
		// Active eligibility is snapshotted before the goroutine starts. If an
		// operator flips shadow to active concurrently, this already-started turn
		// remains shadow and its log reflects the role it actually used.
		if mode == route.ModeActive && !req.Active {
			mode = route.ModeShadow
		}
		willRun := tierForRole(req.PlannerRole)
		if willRun == "" {
			willRun = route.TierSmall
		}
		small, big, force := r.targets(cfg, req.PlannerRole)
		input := route.PolicyInput{
			Small: small, Big: big, Force: force,
			Override: route.Override(req.Override), NeverCloud: cfg.NeverCloud,
			MinComplexityForBig: cfg.MinComplexityForBig, StickySession: cfg.StickySession,
			PriorTier: tierForRole(req.PreviousRole), StickyTier: tierForRole(req.StickyRole),
			RunningContextTokens: req.ContextTokens, RunningContextKnown: req.ContextKnown,
		}
		d, decideErr := r.engine.DecideWithPolicy(ctx, route.PolicyDecisionRequest{
			AgentID: req.AgentID, SessionID: req.SessionID, Prompt: req.UserQuery,
			Mode: mode, Surface: string(req.Surface),
			Assist: cfg.Assist && !assistedAcceptance, WillRun: willRun, Input: input,
			RefreshInput: func(in *route.PolicyInput) {
				// Re-resolve the bound profiles after classification. Locality is
				// intentionally decision-time state so a concurrent /model swap
				// cannot open a stale never_cloud hole.
				small, big, force := r.targets(cfg, req.PlannerRole)
				in.Small = small
				in.Big = big
				in.Force = force
			},
			AnnotateDecision: func(d *route.Decision) {
				// TEN-292 metadata, stamped before the row is recorded. Rules
				// only annotate when the classifier label was trusted outright —
				// a clamped decision already told the operator why it won.
				if len(cfg.Rules) > 0 && d.ClassifierAttempted && d.PolicyReason == string(route.ReasonClassifierLabel) {
					if rule, ok := cfg.Rules.Match(d.Signal()); ok {
						d.RuleRole = string(rule.Role)
					}
				}
				// The effort hint must describe the role that will actually run:
				// the rule role when one matched (the applied path resolves it
				// first), otherwise the tier selection. The observation path
				// re-derives the hint from the FINAL role after rule fallback.
				effortRole := model.Role(d.SelectedRole)
				if d.RuleRole != "" {
					effortRole = model.Role(d.RuleRole)
				}
				if p, err := r.roleProfile(effortRole); err == nil {
					kind := providerKinds[p.ProviderKind]
					d.EffortHint = route.EffortHintFor(d.Signal(), kind.SupportsReasoning, p.ReasoningEffort)
				}
			},
		})
		obs := agent.RouteObservation{
			Text: d.Summary(), Tags: d.Tags(), StickySession: cfg.StickySession,
		}
		applyDecision := mode == route.ModeActive || assistedAcceptance
		if decideErr == nil && applyDecision && d.SelectedRole != "" {
			role := model.Role(d.SelectedRole)
			allowed, primary, constraintErr := r.profileConstraints(role, cfg, d)
			if constraintErr != nil {
				r.log.Warn("route: selected binding changed; using planner", "role", role, "err", constraintErr)
			} else {
				obs.Role = role
				obs.Applied = true
				obs.ConstrainProfiles = true
				obs.AllowedProfiles = allowed
				obs.ExpectedProfileID = primary.ID
				obs.ExpectedProviderKind = primary.ProviderKind
			}
		}
		// N-way rule application (TEN-292): only on top of an applied, trusted
		// tier decision, and only when the rule role resolves under the same
		// never_cloud constraints. Any failure keeps the tier decision — a rule
		// is a refinement, never a new way to fail or to reach the cloud.
		if obs.Applied && d.RuleRole != "" && d.RuleRole != string(obs.Role) {
			ruleRole := model.Role(d.RuleRole)
			if allowed, primary, ruleErr := r.ruleConstraints(ruleRole, cfg, d); ruleErr != nil {
				r.log.Warn("route: rule role unavailable; keeping tier decision",
					"role", ruleRole, "err", ruleErr)
			} else {
				obs.Role = ruleRole
				obs.AllowedProfiles = allowed
				obs.ExpectedProfileID = primary.ID
				obs.ExpectedProviderKind = primary.ProviderKind
				obs.Tags = append(obs.Tags, "route:rule="+d.RuleRole)
			}
		}
		// Re-derive the effort hint from the role that actually won: if the
		// rule role failed its constraints above, the durable row's estimate
		// (computed for the rule role) must not leak onto the tier profile.
		if obs.Applied && d.EffortHint != "" {
			if p, err := r.roleProfile(obs.Role); err == nil {
				kind := providerKinds[p.ProviderKind]
				if hint := route.EffortHintFor(d.Signal(), kind.SupportsReasoning, p.ReasoningEffort); hint != "" {
					obs.EffortHint = hint
					obs.Tags = append(obs.Tags, "route:effort="+hint)
				}
			}
		}
		if obs.Applied {
			obs.Tags = append(obs.Tags, route.TagApplied)
		}
		if d.AssistedSuggestion && d.SelectedTier == route.TierBig {
			r.rememberSuggestion(req.AgentID, req.SessionID, d.ID)
			obs.Text = "router would send this big — prefix the next prompt with !big to accept"
		}
		if d.PrivacyPierced {
			r.log.Warn("ROUTE PRIVACY OVERRIDE: forced profile selected",
				"agent", req.AgentID, "profile", d.SelectedProfileID, "role", d.SelectedRole)
		}
		return obs
	}
}

func (r *routeRuntime) rememberSuggestion(agentID, sessionID, decisionID string) {
	if r == nil || decisionID == "" {
		return
	}
	r.mu.Lock()
	if r.pendingSuggestions == nil {
		r.pendingSuggestions = make(map[string]string)
	}
	r.pendingSuggestions[agentID+"\x00"+sessionID] = decisionID
	r.mu.Unlock()
}

func (r *routeRuntime) confirmPendingSuggestion(agentID, sessionID string) {
	if r == nil || r.engine == nil {
		return
	}
	key := agentID + "\x00" + sessionID
	r.mu.Lock()
	id := r.pendingSuggestions[key]
	delete(r.pendingSuggestions, key)
	r.mu.Unlock()
	if id != "" {
		go func() {
			if err := r.engine.Confirm(id); err != nil {
				if r.log != nil {
					r.log.Warn("route: persist assisted gold label", "decision", id, "err", err)
				}
			}
		}()
	}
}

func (r *routeRuntime) clearPendingSuggestion(agentID, sessionID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.pendingSuggestions, agentID+"\x00"+sessionID)
	r.mu.Unlock()
}

// roleProfile is a nil-safe primary-profile lookup used by decision
// annotation, where "no binding" is ordinary (rule roles may be unbound).
func (r *routeRuntime) roleProfile(role model.Role) (model.Profile, error) {
	if r == nil || r.router == nil {
		return model.Profile{}, fmt.Errorf("model router unavailable")
	}
	return r.router.ForRole(role)
}

// ruleConstraints mirrors profileConstraints for an N-way rule role (TEN-292).
// The changed-profile identity check is intentionally absent — a rule role's
// profile legitimately differs from the tier decision's — but never_cloud
// still filters the allowlist, so a rule cannot smuggle a cloud profile in.
func (r *routeRuntime) ruleConstraints(role model.Role, cfg resolvedRouteConfig, d route.Decision) (map[string]string, model.Profile, error) {
	if r == nil || r.router == nil {
		return nil, model.Profile{}, fmt.Errorf("model router unavailable")
	}
	profiles, err := r.router.ProfilesForRole(role)
	if err != nil {
		return nil, model.Profile{}, err
	}
	if len(profiles) == 0 {
		return nil, model.Profile{}, fmt.Errorf("role %q has no profiles", role)
	}
	primary := profiles[0]
	restrictLocal := cfg.NeverCloud && !d.PrivacyPierced
	allowed := make(map[string]string, len(profiles))
	for _, p := range profiles {
		if !restrictLocal || routeProfileLocal(p) {
			allowed[p.ID] = p.ProviderKind
		}
	}
	if _, ok := allowed[primary.ID]; !ok {
		return nil, model.Profile{}, fmt.Errorf("profile %q is not allowed by never_cloud", primary.ID)
	}
	return allowed, primary, nil
}

func (r *routeRuntime) profileConstraints(role model.Role, cfg resolvedRouteConfig, d route.Decision) (map[string]string, model.Profile, error) {
	if r == nil || r.router == nil {
		return nil, model.Profile{}, fmt.Errorf("model router unavailable")
	}
	profiles, err := r.router.ProfilesForRole(role)
	if err != nil {
		return nil, model.Profile{}, err
	}
	if len(profiles) == 0 {
		return nil, model.Profile{}, fmt.Errorf("role %q has no profiles", role)
	}
	primary := profiles[0]
	if role != routeForceRole && d.SelectedProfileID != "" && primary.ID != d.SelectedProfileID {
		return nil, model.Profile{}, fmt.Errorf("profile changed from %q to %q", d.SelectedProfileID, primary.ID)
	}
	restrictLocal := cfg.NeverCloud && !d.PrivacyPierced
	allowed := make(map[string]string, len(profiles))
	for _, p := range profiles {
		if !restrictLocal || routeProfileLocal(p) {
			allowed[p.ID] = p.ProviderKind
		}
	}
	if _, ok := allowed[primary.ID]; !ok {
		return nil, model.Profile{}, fmt.Errorf("profile %q is not allowed by never_cloud", primary.ID)
	}
	return allowed, primary, nil
}

// SpawnRole classifies one sub-agent task at spawn time (TEN-292) and
// suggests an N-way rule role. Spawn is not a turn, so the classifier's own
// bounded timeout is the only latency added, and every failure returns
// (_, false) — the spawn then behaves exactly as before this feature.
// The decision is recorded shadow-mode with surface "spawn" for telemetry.
func (r *routeRuntime) SpawnRole(task string) (model.Role, bool) {
	if r == nil || r.engine == nil {
		return "", false
	}
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	if !cfg.Enabled || len(cfg.Rules) == 0 {
		return "", false
	}
	// Spawn routing rides the active stages only; shadow observation gathers
	// rule telemetry through ordinary turns first.
	if cfg.Mode != route.ModeActive && cfg.Mode != route.ModeEverywhere {
		return "", false
	}
	// Spawns classify without the parent turn's surface/agent context, so the
	// privacy posture is the conservative one: never_cloud is FORCED for spawn
	// rule resolution regardless of the global setting. A relay-fenced parent
	// can therefore never gain cloud access through a rule-routed sub-agent.
	cfg.NeverCloud = true
	small, big, force := r.targets(cfg, model.RolePlanner)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutMS)*time.Millisecond)
	defer cancel()
	d, err := r.engine.DecideWithPolicy(ctx, route.PolicyDecisionRequest{
		AgentID: "team-spawn", Prompt: task,
		Mode: route.ModeShadow, Surface: "spawn",
		Input: route.PolicyInput{
			Small: small, Big: big, Force: force,
			NeverCloud:          cfg.NeverCloud,
			MinComplexityForBig: cfg.MinComplexityForBig,
		},
	})
	// Rules need a real parsed signal: deterministic no-classifier paths
	// (overlong task text, breaker open) must not match rules against a
	// zero-value Signal, where a math=false/code=false predicate would hit.
	if err != nil || d.Status != route.StatusOK || !d.ClassifierAttempted || d.Label == "" {
		return "", false
	}
	rule, ok := cfg.Rules.Match(d.Signal())
	if !ok {
		return "", false
	}
	if _, _, cerr := r.ruleConstraints(rule.Role, cfg, d); cerr != nil {
		return "", false
	}
	return rule.Role, true
}

// Active is the cheap live gate supplied separately to agent.Config. Keeping
// it separate means a slow shadow classifier never enters the bounded join;
// the everywhere stage can explicitly opt unattended surfaces into it.
func (r *routeRuntime) Active(req agent.RouteRequest) bool {
	if r == nil {
		return false
	}
	cfg, mode := r.effective(req)
	if !cfg.Enabled {
		return false
	}
	if mode == route.ModeActive {
		return true
	}
	return cfg.Assist && mode == route.ModeShadow && req.Surface.Interactive() && req.Override == agent.RouteOverrideBig
}

// Sticky snapshots sticky_session before classification so degraded active
// turns can still pin the planner tier after tool-call syntax appears.
func (r *routeRuntime) Sticky(req agent.RouteRequest) bool {
	if r == nil {
		return false
	}
	cfg, mode := r.effective(req)
	active := mode == route.ModeActive ||
		(cfg.Assist && mode == route.ModeShadow && req.Surface.Interactive() && req.Override == agent.RouteOverrideBig)
	return cfg.Enabled && active && cfg.StickySession
}

func (r *routeRuntime) effective(req agent.RouteRequest) (resolvedRouteConfig, string) {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	mode := cfg.Mode
	if mode == route.ModeEverywhere {
		mode = route.ModeActive
	}
	explicitMode := false
	if override, ok := cfg.Agents[req.AgentID]; ok {
		if override.Mode != "" {
			mode = override.Mode
			explicitMode = true
		}
		if override.NeverCloud != nil {
			cfg.NeverCloud = *override.NeverCloud
		}
	}
	// Active remains the interactive stage. Unattended surfaces need either an
	// explicit per-agent opt-in or the distinct everywhere config stage.
	if !req.Surface.Interactive() && cfg.Mode != route.ModeEverywhere && !explicitMode {
		mode = route.ModeShadow
	}
	if req.Surface == agent.RouteSurfaceRelay {
		if override, ok := cfg.Agents[req.AgentID]; !ok || override.NeverCloud == nil {
			cfg.NeverCloud = true
		}
	}
	return cfg, mode
}

func (r *routeRuntime) targets(cfg resolvedRouteConfig, plannerRole model.Role) (route.Target, route.Target, *route.Target) {
	if plannerRole == "" {
		plannerRole = model.RolePlanner
	}
	small := r.configuredSmall
	big := r.configuredBig
	small.Tier, small.Role = route.TierSmall, plannerRole
	big.Tier, big.Role = route.TierBig, plannerBigRole
	if r.router != nil {
		if p, err := r.router.ForRole(plannerRole); err == nil {
			small.ProfileID = p.ID
			small.Local = routeProfileLocal(p)
			small.OperationalBudget = p.OperationalBudget()
		}
		if p, err := r.router.ForRole(plannerBigRole); err == nil {
			big.ProfileID = p.ID
			big.Local = routeProfileLocal(p)
			big.OperationalBudget = p.OperationalBudget()
		}
	}
	if cfg.ForceProfile == "" || r.router == nil {
		return small, big, nil
	}
	p, err := r.router.ForRole(routeForceRole)
	if err != nil {
		return small, big, nil
	}
	force := route.Target{
		Tier: route.TierBig, Role: routeForceRole, ProfileID: cfg.ForceProfile,
		Local: routeProfileLocal(p), OperationalBudget: p.OperationalBudget(),
	}
	return small, big, &force
}

func routeProfileLocal(p model.Profile) bool {
	if p.ProviderKind == "echo" {
		return true
	}
	kind, known := providerKinds[p.ProviderKind]
	return known && kind.Local
}

func tierForRole(role model.Role) route.Tier {
	switch role {
	case plannerBigRole, routeForceRole:
		return route.TierBig
	case "":
		return ""
	default:
		return route.TierSmall
	}
}

func (r *routeRuntime) bindForce(profileID string) error {
	if r == nil || r.router == nil {
		return fmt.Errorf("model router unavailable")
	}
	p, ok := r.router.ProfileByID(strings.TrimSpace(profileID))
	if !ok {
		return fmt.Errorf("unknown profile %q", profileID)
	}
	if p.Role == model.RoleEmbedder || p.Role == model.RoleJudge {
		return fmt.Errorf("profile %q is not a generation profile", profileID)
	}
	// Clone under a stable ID so forcing the current unnamespaced planner does
	// not silently follow a later `/model use` SetProfiles upsert.
	p.ID = "route-force-binding"
	if err := r.router.AddFallbackProfiles(p); err != nil {
		return fmt.Errorf("register forced profile: %w", err)
	}
	if err := r.router.PinRole(routeForceRole, p.ID); err != nil {
		return fmt.Errorf("pin forced profile: %w", err)
	}
	r.log.Warn("ROUTE PRIVACY OVERRIDE ARMED", "profile", profileID, "binding", p.ID)
	return nil
}

func (r *routeRuntime) ensureForceProfile(lc *launchConfig, profileID string) error {
	if r == nil || r.router == nil {
		return fmt.Errorf("model router unavailable")
	}
	if _, ok := r.router.ProfileByID(profileID); ok {
		return nil
	}
	providerName, _, ok := strings.Cut(profileID, "/")
	if !ok || providerName == "" || lc == nil || lc.Providers[providerName] == nil {
		return fmt.Errorf("unknown profile %q", profileID)
	}
	profiles, factories, err := providerGenProfiles(r.cfgDir, lc, providerName, lc.PlanLoopCeiling)
	if err != nil {
		return fmt.Errorf("build forced provider %q: %w", providerName, err)
	}
	var selected model.Profile
	for _, p := range profiles {
		if p.ID == profileID {
			selected = p
			break
		}
	}
	if selected.ID == "" {
		return fmt.Errorf("provider %q has no generation profile %q", providerName, profileID)
	}
	for kind, factory := range factories {
		r.router.RegisterBackend(kind, factory)
	}
	if err := r.router.AddFallbackProfiles(selected); err != nil {
		return fmt.Errorf("register forced profile %q: %w", profileID, err)
	}
	return nil
}

// BindConfiguredForce retries startup binding after planner-big/fallback
// profiles have been registered. It is wiring-time only, never a turn action.
func (r *routeRuntime) BindConfiguredForce() error {
	if r == nil {
		return nil
	}
	// Rule sanity warnings belong here, after planner-big registration.
	r.warnRuleCapabilities()
	r.mu.RLock()
	profileID := r.cfg.ForceProfile
	r.mu.RUnlock()
	if profileID == "" {
		return nil
	}
	lc, err := loadLaunchConfig(r.cfgDir)
	if err != nil {
		return err
	}
	canonical, canonicalErr := canonicalConfiguredRouteProfileID(lc, profileID)
	if canonicalErr != nil {
		if _, ok := r.router.ProfileByID(profileID); !ok {
			return canonicalErr
		}
		canonical = profileID // compatibility for an already-registered custom profile
	}
	if err := r.ensureForceProfile(lc, canonical); err != nil {
		return err
	}
	if err := r.bindForce(canonical); err != nil {
		return err
	}
	if canonical != profileID {
		lc.Route.ForceProfile = canonical
		if err := lc.save(r.cfgDir); err != nil {
			return fmt.Errorf("persist stable forced profile: %w", err)
		}
		cfg, err := resolveRouteConfig(lc)
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.cfg = cfg
		r.mu.Unlock()
	}
	return nil
}

// Status implements tui.RouteControl. Health means a known prompt completed
// and passed the strict parser; a listening socket by itself is insufficient.
func (r *routeRuntime) Status(ctx context.Context) string {
	if r == nil {
		return "Prompt router\n  enabled: false\n  mode: shadow\n  classifier: unavailable"
	}
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	var b strings.Builder
	b.WriteString("Prompt router\n")
	fmt.Fprintf(&b, "  enabled: %t\n", cfg.Enabled)
	displayMode := cfg.Mode
	modeNote := "shadow; telemetry only"
	if !cfg.Enabled {
		displayMode = route.ModeOff
		modeNote = "disabled; turns use the planner"
	} else if cfg.Assist {
		modeNote = "assisted; suggestions only until !big acceptance"
	} else if cfg.Mode == route.ModeActive {
		modeNote = "active-interactive; relays and cron remain shadow"
	} else if cfg.Mode == route.ModeEverywhere {
		modeNote = "active everywhere, including relays and cron"
	}
	fmt.Fprintf(&b, "  mode: %s (%s)\n", displayMode, modeNote)
	fmt.Fprintf(&b, "  assist: %t\n", cfg.Assist)
	fmt.Fprintf(&b, "  policy: never_cloud=%t min_complexity_for_big=%d sticky_session=%t\n",
		cfg.NeverCloud, cfg.MinComplexityForBig, cfg.StickySession)
	if len(cfg.Rules) > 0 {
		fmt.Fprintf(&b, "  rules: %d N-way rule(s), first match wins, no match = 2-way\n", len(cfg.Rules))
	}
	fmt.Fprintf(&b, "  log retention: %s\n", retentionDisplay(cfg.LogRetentionDays))
	if len(cfg.Agents) > 0 {
		ids := make([]string, 0, len(cfg.Agents))
		for id := range cfg.Agents {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			override := cfg.Agents[id]
			mode := override.Mode
			if mode == "" {
				mode = cfg.Mode
			}
			neverCloud := cfg.NeverCloud
			privacyNote := ""
			if override.NeverCloud != nil {
				neverCloud = *override.NeverCloud
				if !neverCloud {
					privacyNote = " PRIVACY OPT-IN"
				}
			}
			fmt.Fprintf(&b, "  agent %s: mode=%s never_cloud=%t%s\n", id, mode, neverCloud, privacyNote)
		}
	}
	small, big, force := r.targets(cfg, model.RolePlanner)
	fmt.Fprintf(&b, "  small: %s (role %s, local=%t)\n", routeDash(small.ProfileID), small.Role, small.Local)
	fmt.Fprintf(&b, "  big: %s (role %s, local=%t)\n", routeDash(big.ProfileID), big.Role, big.Local)
	if cfg.ForceProfile != "" {
		binding := "resolved at runtime"
		if force != nil {
			binding = string(force.Role)
		}
		fmt.Fprintf(&b, "  FORCE: %s (%s; privacy override; audit-logged)\n", cfg.ForceProfile, binding)
	}
	cloudNotice := r.plannerCloudNotice
	if small.ProfileID != "" {
		cloudNotice = !small.Local
	}
	if cloudNotice {
		b.WriteString("  NOTICE: the default planner is cloud-backed; never_cloud cannot undo the operator's baseline /model selection\n")
	}
	if r.engine != nil {
		fmt.Fprintf(&b, "  breaker: %s\n", r.engine.BreakerState())
	}
	if !cfg.Enabled || r.engine == nil {
		b.WriteString("  classifier: not probed (disabled)")
		return b.String()
	}
	fmt.Fprintf(&b, "  provider: %s @ %s\n", cfg.Model, safeDisplayEndpoint(cfg.Endpoint))
	sig, err := r.engine.Probe(ctx)
	if err != nil && r.classifier != nil {
		// A cold model blows the tight per-turn probe timeout with "context
		// deadline exceeded". Give it one generous warm-load and re-probe before
		// declaring it unhealthy — the model is small (~110ms once resident).
		warmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if werr := r.classifier.warm(warmCtx); werr == nil {
			sig, err = r.engine.Probe(ctx)
		}
		cancel()
	}
	if err != nil {
		fmt.Fprintf(&b, "  classifier: unhealthy (%s)", err)
		return b.String()
	}
	fmt.Fprintf(&b, "  classifier: healthy (parseable; %s, complexity %d, %dms)",
		sig.Label, sig.Complexity, sig.Latency.Milliseconds())
	return b.String()
}

// Stats implements tui.RouteControl from the durable decision JSONL.
func (r *routeRuntime) Stats() string {
	if r == nil || r.engine == nil {
		return "Route stats\n  decisions: 0\nGraduation gate: FAIL\n  [FAIL] data: missing (router unavailable)"
	}
	s := r.engine.StatsWithOutcomes(r.turnOutcomes())
	var b strings.Builder
	fmt.Fprintf(&b, "Route stats\n  decisions: %d\n  big rate: %.1f%%\n  shadow disagreement: %.1f%%\n  parse failure: %.1f%%\n  classify latency p50/p95: %s / %s\n  breaker trips: %d",
		s.TotalDecisions, 100*s.BigRate, 100*s.ShadowDisagreementRate,
		100*s.ParseFailureRate, s.P50ClassifyLatency, s.P95ClassifyLatency, s.BreakerTrips)
	verdict := "FAIL"
	if s.Graduation.Pass {
		verdict = "PASS"
	}
	fmt.Fprintf(&b, "\nGraduation gate: %s", verdict)
	for _, criterion := range s.Graduation.Criteria {
		mark := "FAIL"
		if criterion.Pass {
			mark = "PASS"
		}
		fmt.Fprintf(&b, "\n  [%s] %s: %s (threshold %s)", mark, criterion.Name, criterion.Actual, criterion.Threshold)
		if criterion.Detail != "" {
			fmt.Fprintf(&b, " — %s", criterion.Detail)
		}
	}
	return b.String()
}

func (r *routeRuntime) turnOutcomes() []route.TurnOutcome {
	if r == nil || r.dataDir == "" {
		return nil
	}
	path := filepath.Join(r.dataDir, "episodes.db")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	store, err := episodic.Open(path)
	if err != nil {
		return nil
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	episodes, err := store.List(ctx, episodic.ListFilter{})
	if err != nil {
		return nil
	}
	out := make([]route.TurnOutcome, 0, len(episodes))
	for _, episode := range episodes {
		if joined, ok := route.TurnOutcomeFromTags(episode.Tags, episode.Outcome); ok {
			out = append(out, joined)
		}
	}
	return out
}

// SetMode persists and applies the explicit rollout ladder.
func (r *routeRuntime) SetMode(mode string) (string, error) {
	if r == nil || r.cfgDir == "" {
		return "", fmt.Errorf("config persistence unavailable")
	}
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	lc, err := loadLaunchConfig(r.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Route == nil {
		lc.Route = &routeConfig{}
	}
	normalized := strings.ToLower(strings.TrimSpace(mode))
	switch normalized {
	case "on":
		lc.Route.Enabled = true
		lc.Route.Mode = route.ModeActive
		lc.Route.Assist = false
		lc.Route.ForceProfile = ""
	case "off":
		lc.Route.Enabled = false
		lc.Route.Assist = false
		lc.Route.ForceProfile = ""
	case "shadow":
		lc.Route.Enabled = true
		lc.Route.Mode = route.ModeShadow
		lc.Route.Assist = false
		lc.Route.ForceProfile = ""
	case "assisted":
		lc.Route.Enabled = true
		lc.Route.Mode = route.ModeShadow
		lc.Route.Assist = true
		lc.Route.ForceProfile = ""
	case "everywhere":
		if r.engine == nil || !r.engine.StatsWithOutcomes(r.turnOutcomes()).Graduation.Pass {
			return "", fmt.Errorf("graduation gate FAIL; run /route stats and satisfy every criterion before enabling active-everywhere")
		}
		lc.Route.Enabled = true
		lc.Route.Mode = route.ModeEverywhere
		lc.Route.Assist = false
		lc.Route.ForceProfile = ""
	default:
		return "", fmt.Errorf("unknown mode %q", mode)
	}
	cfg, err := resolveRouteConfig(lc)
	if err != nil {
		return "", err
	}
	if cfg.Enabled && r.router != nil {
		if err := installRouteBigRole(r.router, r.cfgDir, lc, lc.PlanLoopCeiling, r.log); err != nil {
			return "", fmt.Errorf("route: install planner-big binding: %w", err)
		}
		installFallbackChain(r.router, r.cfgDir, lc, lc.PlanLoopCeiling, r.log)
	}
	if err := lc.save(r.cfgDir); err != nil {
		return "", err
	}
	r.mu.Lock()
	prev := r.cfg
	r.cfg = cfg
	r.mu.Unlock()
	r.refreshClassifier(prev, cfg)
	if !cfg.Enabled {
		return "prompt routing off (persisted; next turns use the planner)", nil
	}
	if cfg.Mode == route.ModeShadow {
		if cfg.Assist {
			return "prompt routing assisted (persisted; suggestions only; prefix the next prompt with !big to accept)", nil
		}
		return "prompt routing shadow (persisted; model selection unchanged)", nil
	}
	if cfg.Mode == route.ModeEverywhere {
		return "prompt routing active everywhere (persisted; TUI, dashboard, relays, and cron)", nil
	}
	return "prompt routing active for TUI/dashboard (persisted; relays/cron remain shadow)", nil
}

// Force binds, persists, and loudly reports the sole never_cloud escape hatch.
func (r *routeRuntime) Force(profileID string) (string, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return "", fmt.Errorf("profile id is required")
	}
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	lc, err := loadLaunchConfig(r.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Route == nil {
		lc.Route = &routeConfig{}
	}
	canonical, err := canonicalConfiguredRouteProfileID(lc, profileID)
	if err != nil {
		if _, ok := r.router.ProfileByID(profileID); !ok {
			return "", err
		}
		canonical = profileID
	}
	if err := r.ensureForceProfile(lc, canonical); err != nil {
		return "", err
	}
	oldEnabled, oldMode, oldForce := lc.Route.Enabled, lc.Route.Mode, lc.Route.ForceProfile
	lc.Route.Enabled = true
	lc.Route.Mode = route.ModeActive
	lc.Route.Assist = false
	lc.Route.ForceProfile = canonical
	if err := lc.save(r.cfgDir); err != nil {
		return "", err
	}
	if err := r.bindForce(canonical); err != nil {
		lc.Route.Enabled, lc.Route.Mode, lc.Route.ForceProfile = oldEnabled, oldMode, oldForce
		_ = lc.save(r.cfgDir)
		return "", err
	}
	cfg, err := resolveRouteConfig(lc)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	prev := r.cfg
	r.cfg = cfg
	r.mu.Unlock()
	r.refreshClassifier(prev, cfg)
	return fmt.Sprintf("WARNING: route forced to profile %q; this explicitly pierces never_cloud and is audit-logged", canonical), nil
}

// Why implements tui.RouteControl. It never renders the prompt, only its
// SHA-256 identity. Failed parses include the exact classifier completion.
func (r *routeRuntime) Why(n int) string {
	if n < 1 {
		n = 1
	}
	if r == nil || r.engine == nil {
		return "route: no decisions (shadow routing is disabled)"
	}
	d, ok := r.engine.NthRecent(n)
	if !ok {
		return fmt.Sprintf("route: decision #%d is not available", n)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Route decision #%d (most recent = 1)\n", n)
	fmt.Fprintf(&b, "  time: %s\n", d.Timestamp.Local().Format("2006-01-02 15:04:05.000 MST"))
	fmt.Fprintf(&b, "  agent/session: %s / %s\n", routeDash(d.AgentID), routeDash(d.SessionID))
	fmt.Fprintf(&b, "  prompt hash: %s\n", d.PromptHash)
	fmt.Fprintf(&b, "  estimated tokens: %d\n", d.EstTokens)
	fmt.Fprintf(&b, "  mode/status: %s / %s\n", d.Mode, d.Status)
	if d.Error != "" {
		fmt.Fprintf(&b, "  error: %s\n", d.Error)
	}
	if d.Domain != "" || d.Label != "" {
		fmt.Fprintf(&b, "  domain: %s\n", routeDash(d.Domain))
		fmt.Fprintf(&b, "  complexity: %d\n", d.Complexity)
		fmt.Fprintf(&b, "  math/code: %t / %t\n", d.Math, d.Code)
		fmt.Fprintf(&b, "  label: %s\n", routeDash(d.Label))
		fmt.Fprintf(&b, "  latency: %dms\n", d.LatencyMS)
		fmt.Fprintf(&b, "  justification: %s\n", routeDash(d.Justification))
	}
	if d.SelectedTier != "" {
		fmt.Fprintf(&b, "  selected: %s via role %s / profile %s\n",
			d.SelectedTier, routeDash(d.SelectedRole), routeDash(d.SelectedProfileID))
		fmt.Fprintf(&b, "  policy: %s (clamped=%t)\n", routeDash(d.PolicyReason), d.PolicyClamped)
	}
	if d.PrivacyPierced {
		b.WriteString("  WARNING: explicit force pierced never_cloud\n")
	}
	if d.BreakerTripped || d.BreakerState != "" {
		fmt.Fprintf(&b, "  breaker: %s (tripped=%t)\n", routeDash(string(d.BreakerState)), d.BreakerTripped)
	}
	if d.Status == route.StatusError && d.Raw != "" {
		fmt.Fprintf(&b, "  raw output: %s\n", d.Raw)
	}
	fmt.Fprintf(&b, "  outcome hint: %s", routeDash(d.OutcomeHint))
	return b.String()
}

func routeDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func retentionDisplay(days int) string {
	if days <= 0 {
		return "disabled"
	}
	return fmt.Sprintf("%d days", days)
}
