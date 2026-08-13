package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"tenant/internal/route"
	"tenant/internal/tui"
)

// Control surface for the /route configure wizard (TUI side:
// internal/tui/routewizard.go). The mutating methods follow SetMode's
// persist-and-apply shape: load config.json fresh from disk (other writers
// exist) → mutate → resolveRouteConfig → live-bind when a router is present →
// save (which re-validates) → swap r.cfg under r.mu.

// routeLadderWord maps a resolved config onto the operator-facing rollout
// ladder vocabulary SetMode accepts: off | shadow | assisted | on | everywhere.
func routeLadderWord(cfg resolvedRouteConfig) string {
	switch {
	case !cfg.Enabled:
		return "off"
	case cfg.Assist:
		return "assisted"
	case cfg.Mode == route.ModeEverywhere:
		return "everywhere"
	case cfg.Mode == route.ModeActive:
		return "on"
	default:
		return "shadow"
	}
}

// ConfigureInfo implements tui.RouteControl: the provider inventory (wired
// kinds only, echo excluded — the offline stub is not a routing candidate)
// plus the current defaults-applied policy.
func (r *routeRuntime) ConfigureInfo() (tui.RouteConfigureInfo, error) {
	if r == nil || r.cfgDir == "" {
		return tui.RouteConfigureInfo{}, fmt.Errorf("config persistence unavailable")
	}
	lc, err := loadLaunchConfig(r.cfgDir)
	if err != nil {
		return tui.RouteConfigureInfo{}, err
	}
	cfg, err := resolveRouteConfig(lc)
	if err != nil {
		return tui.RouteConfigureInfo{}, err
	}
	info := tui.RouteConfigureInfo{
		BigRole:             cfg.BigRole,
		Mode:                routeLadderWord(cfg),
		NeverCloud:          cfg.NeverCloud,
		MinComplexityForBig: cfg.MinComplexityForBig,
		StickySession:       cfg.StickySession,
	}
	names := make([]string, 0, len(lc.Providers))
	for name := range lc.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := lc.Providers[name]
		pk, known := providerKinds[pc.Kind]
		if !known || !pk.Wired {
			continue
		}
		mi := tui.ModelInfo{
			Name: name, Kind: pc.Kind, Endpoint: safeDisplayEndpoint(pc.Endpoint),
			Model: pc.Model, Active: name == lc.Provider,
		}
		if pk.Local {
			if pc.Kind != "echo" {
				info.Local = append(info.Local, mi)
			}
			continue
		}
		if mi.Model == "" {
			mi.Model = pk.DefaultModel // what an empty cloud model resolves to
		}
		info.Cloud = append(info.Cloud, mi)
	}
	return info, nil
}

// SetBig implements tui.RouteControl: persist the big-leg provider
// (route.big_role names a PROVIDER; the model lives on that provider) and
// bind planner-big live so the next routed turn uses it.
func (r *routeRuntime) SetBig(providerName, modelID string) (string, error) {
	if r == nil || r.cfgDir == "" {
		return "", fmt.Errorf("config persistence unavailable")
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return "", fmt.Errorf("provider name is required")
	}
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	lc, err := loadLaunchConfig(r.cfgDir)
	if err != nil {
		return "", err
	}
	pc := lc.Providers[providerName]
	if pc == nil {
		return "", fmt.Errorf("no provider named %q (see /model)", providerName)
	}
	if pk, known := providerKinds[pc.Kind]; !known || !pk.Wired {
		return "", fmt.Errorf("provider %q (kind %q) has no wired backend", providerName, pc.Kind)
	}
	oldBigRole := ""
	if lc.Route != nil {
		oldBigRole = lc.Route.BigRole
	}
	oldModel := pc.Model
	if mo := strings.TrimSpace(modelID); mo != "" {
		pc.Model = mo
	}
	if lc.Route == nil {
		lc.Route = &routeConfig{}
	}
	lc.Route.BigRole = providerName
	cfg, err := resolveRouteConfig(lc)
	if err != nil {
		return "", err
	}
	// Save BEFORE touching the live router (Force's ordering): a failed bind
	// rolls the file back, so disk, r.cfg, and the router can't be left
	// disagreeing in either failure order.
	if err := lc.save(r.cfgDir); err != nil {
		return "", err
	}
	if r.router != nil {
		if err := installRouteBigRole(r.router, r.cfgDir, lc, lc.PlanLoopCeiling, r.log); err != nil {
			lc.Route.BigRole = oldBigRole
			pc.Model = oldModel
			if rerr := lc.save(r.cfgDir); rerr != nil {
				return "", fmt.Errorf("route: install planner-big binding: %v (rolling back config.json also failed: %v)", err, rerr)
			}
			return "", fmt.Errorf("route: install planner-big binding: %w", err)
		}
		installFallbackChain(r.router, r.cfgDir, lc, lc.PlanLoopCeiling, r.log)
	}
	// configuredSmall/Big stay construction-immutable (targets() reads them
	// lock-free): with a live router the binding above is what targets()
	// resolves, and headless runtimes are rebuilt per invocation.
	r.mu.Lock()
	prev := r.cfg
	r.cfg = cfg
	r.mu.Unlock()
	r.refreshClassifier(prev, cfg)
	shownModel := strings.TrimSpace(pc.Model)
	if shownModel == "" {
		shownModel = providerKinds[pc.Kind].DefaultModel
	}
	status := fmt.Sprintf("✓ big leg → %s / %s (route.big_role; bound as planner-big)", providerName, shownModel)
	if cfg.NeverCloud {
		status += "\n  note: never_cloud is on — big-tier selections clamp to local until cloud offload is allowed (policy step)"
	}
	return status, nil
}

// SetPolicy implements tui.RouteControl: persist one policy knob. The pointer
// booleans are written explicitly — an operator decision must stay
// distinguishable from the omitted deny-by-default.
func (r *routeRuntime) SetPolicy(field, value string) (string, error) {
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
	field = strings.ToLower(strings.TrimSpace(field))
	value = strings.ToLower(strings.TrimSpace(value))
	var status string
	switch field {
	case "never_cloud":
		b, perr := strconv.ParseBool(value)
		if perr != nil {
			return "", fmt.Errorf("never_cloud wants true/false, got %q", value)
		}
		lc.Route.NeverCloud = &b
		if b {
			status = "cloud offload blocked (never_cloud=true): big-tier selections clamp to the local model"
		} else {
			status = "cloud offload allowed (never_cloud=false): qualifying turns may route to the big model"
		}
	case "sticky_session":
		b, perr := strconv.ParseBool(value)
		if perr != nil {
			return "", fmt.Errorf("sticky_session wants true/false, got %q", value)
		}
		lc.Route.StickySession = &b
		if b {
			status = "sticky sessions on: a session that went big stays big until a budget-checked downswitch"
		} else {
			status = "sticky sessions off: the tier is re-decided every turn"
		}
	case "min_complexity_for_big":
		// Explicit 1-5 here: resolveRouteConfig tolerates 0 as "unset, use the
		// default", which would make a SetPolicy(…, "0") success status lie
		// about the live threshold.
		n, perr := strconv.Atoi(value)
		if perr != nil || n < 1 || n > 5 {
			return "", fmt.Errorf("min_complexity_for_big wants 1-5, got %q", value)
		}
		lc.Route.MinComplexityForBig = n
		status = fmt.Sprintf("min complexity for big = %d: classifier complexity ≥ %d routes big", n, n)
	default:
		return "", fmt.Errorf("unknown policy field %q (never_cloud|min_complexity_for_big|sticky_session)", field)
	}
	cfg, err := resolveRouteConfig(lc)
	if err != nil {
		return "", err
	}
	if err := lc.save(r.cfgDir); err != nil {
		return "", err
	}
	r.mu.Lock()
	prev := r.cfg
	r.cfg = cfg
	r.mu.Unlock()
	r.refreshClassifier(prev, cfg)
	return status, nil
}
