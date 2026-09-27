package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	neturl "net/url"
	"strings"

	"tenant/internal/model"
	"tenant/internal/route"
)

// RouteRule is one persisted N-way rule. Both levels decode strictly:
// the predicate vocabulary is FROZEN at exactly four fields (domain,
// complexity_gte, math, code) as an anti-scope-creep commitment, so an
// unknown key — including a predicate misplaced at the rule level instead of
// inside "when" — is a config error, not a silently widened rule.
type RouteRule struct {
	When RouteRuleWhen `json:"when"`
	Role string        `json:"role"`
}

// UnmarshalJSON rejects unknown keys at the rule level.
func (r *RouteRule) UnmarshalJSON(data []byte) error {
	type plain RouteRule
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("route rule (allowed keys: when, role): %w", err)
	}
	*r = RouteRule(p)
	return nil
}

// RouteRuleWhen is a rule's predicate.
type RouteRuleWhen struct {
	Domain        string `json:"domain,omitempty"`
	ComplexityGTE int    `json:"complexity_gte,omitempty"`
	Math          *bool  `json:"math,omitempty"`
	Code          *bool  `json:"code,omitempty"`
}

// UnmarshalJSON rejects unknown predicate keys. A plain struct decode would
// ignore them, which reads as "rule works" while the predicate silently
// widened — exactly the failure the 4-field freeze exists to prevent.
func (w *RouteRuleWhen) UnmarshalJSON(data []byte) error {
	type plain RouteRuleWhen
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("route rule predicate (allowed: domain, complexity_gte, math, code): %w", err)
	}
	*w = RouteRuleWhen(p)
	return nil
}

// RouteError distinguishes a router policy rejection from a corrupt
// config file. Startup propagates this error (the unsafe router must not be
// enabled) while retaining the existing warn-and-fallback behavior for JSON
// corruption and unrelated config read failures.
type RouteError struct {
	reason string
}

func (e *RouteError) Error() string { return "route: " + e.reason }

func routeErrorf(format string, args ...any) error {
	return &RouteError{reason: fmt.Sprintf(format, args...)}
}

// ResolveRoute applies prompt-router defaults and validates both active
// role selection and the classifier's local-only transport. A nil/missing
// block resolves to disabled with the deny-by-default policy intact.
func ResolveRoute(lc *Launch) (ResolvedRoute, error) {
	out := ResolvedRoute{
		Mode:                DefaultRouteMode,
		Model:               DefaultRouteModel,
		TimeoutMS:           DefaultRouteTimeoutMS,
		NeverCloud:          DefaultRouteNeverCloud,
		MinComplexityForBig: DefaultRouteMinComplexityForBig,
		StickySession:       DefaultRouteStickySession,
		LogRetentionDays:    route.DefaultLogRetentionDays,
	}
	if lc == nil || lc.Route == nil {
		// Resolve the classifier endpoint even for an absent route block: the
		// dormant runtime is constructed from this resolution, and a later
		// live enable (SetMode / the configure wizard) must not inherit an
		// empty endpoint from launch — the classifier would stay dead until
		// restart.
		out.Endpoint, _ = inheritedRouteEndpoint(lc)
		return out, nil
	}

	rc := lc.Route
	out.Enabled = rc.Enabled
	out.Assist = rc.Assist
	if rc.Mode != "" {
		out.Mode = strings.TrimSpace(rc.Mode)
	}
	if !validRouteMode(out.Mode) {
		return ResolvedRoute{}, routeErrorf("mode %q must be %q, %q, or %q", out.Mode, route.ModeShadow, route.ModeActive, route.ModeEverywhere)
	}
	if out.Assist && (!out.Enabled || out.Mode != route.ModeShadow) {
		return ResolvedRoute{}, routeErrorf("assist is only valid when routing is enabled in shadow mode")
	}
	if rc.TimeoutMS < 0 {
		return ResolvedRoute{}, routeErrorf("timeout_ms must be non-negative")
	}
	if rc.TimeoutMS > 0 {
		out.TimeoutMS = rc.TimeoutMS
	}
	if rc.NeverCloud != nil {
		out.NeverCloud = *rc.NeverCloud
	}
	if rc.MinComplexityForBig < 0 || rc.MinComplexityForBig > 5 {
		return ResolvedRoute{}, routeErrorf("min_complexity_for_big must be between 1 and 5")
	}
	if rc.MinComplexityForBig > 0 {
		out.MinComplexityForBig = rc.MinComplexityForBig
	}
	if rc.StickySession != nil {
		out.StickySession = *rc.StickySession
	}
	out.BigRole = strings.TrimSpace(rc.BigRole)
	out.ForceProfile = strings.TrimSpace(rc.ForceProfile)

	// Retention (TEN-292): 0 = default (set above), negative = explicit off.
	// The upper bound keeps a typo'd value from meaning "practically never
	// prune" while staying far above any plausible observation window.
	if rc.LogRetentionDays != 0 {
		if rc.LogRetentionDays > 3650 {
			return ResolvedRoute{}, routeErrorf("log_retention_days %d must be 3650 or less (or negative to disable pruning)", rc.LogRetentionDays)
		}
		out.LogRetentionDays = rc.LogRetentionDays
	}

	// N-way rules (TEN-292). Key-level freezing already happened at JSON
	// decode (RouteRuleWhen rejects unknown predicate keys); this validates
	// values and converts to the route package's evaluated form.
	if len(rc.Rules) > 0 {
		rules := make(route.Rules, 0, len(rc.Rules))
		for _, rr := range rc.Rules {
			rules = append(rules, route.Rule{
				When: route.RulePredicate{
					Domain:        strings.TrimSpace(rr.When.Domain),
					ComplexityGTE: rr.When.ComplexityGTE,
					Math:          rr.When.Math,
					Code:          rr.When.Code,
				},
				Role: model.Role(strings.TrimSpace(rr.Role)),
			})
		}
		if err := rules.Validate(); err != nil {
			return ResolvedRoute{}, routeErrorf("rules: %v", err)
		}
		out.Rules = rules
	}

	active := out.Mode == route.ModeActive || out.Mode == route.ModeEverywhere
	if len(rc.Agents) > 0 {
		out.Agents = make(map[string]RouteAgent, len(rc.Agents))
		for rawID, agentCfg := range rc.Agents {
			agentID := strings.TrimSpace(rawID)
			if agentID == "" {
				return ResolvedRoute{}, routeErrorf("agents contains an empty agent id")
			}
			if _, exists := out.Agents[agentID]; exists {
				return ResolvedRoute{}, routeErrorf("agents contains duplicate agent id %q after trimming", agentID)
			}
			agentCfg.Mode = strings.TrimSpace(agentCfg.Mode)
			if agentCfg.Mode != "" && agentCfg.Mode != route.ModeShadow && agentCfg.Mode != route.ModeActive {
				return ResolvedRoute{}, routeErrorf(
					"agents[%q].mode %q must be %q or %q", agentID, agentCfg.Mode, "shadow", "active",
				)
			}
			if agentCfg.Mode == route.ModeActive && out.Assist {
				return ResolvedRoute{}, routeErrorf(
					"agents[%q].mode cannot be active while assist is enabled; advance the global stage to active first", agentID,
				)
			}
			if agentCfg.Mode == "active" {
				active = true
			}
			if agentCfg.NeverCloud != nil {
				neverCloud := *agentCfg.NeverCloud
				agentCfg.NeverCloud = &neverCloud
			}
			out.Agents[agentID] = agentCfg
		}
	}

	if active && out.BigRole == "" {
		return ResolvedRoute{}, routeErrorf(
			"active mode requires route.big_role to name a configured provider; add one with /model add-cloud <kind> <key>, then bind its provider name",
		)
	}
	if out.BigRole != "" {
		provider := (*Provider)(nil)
		if lc != nil && lc.Providers != nil {
			provider = lc.Providers[out.BigRole]
		}
		if provider == nil {
			return ResolvedRoute{}, routeErrorf(
				"big_role %q is not a configured provider; add it with /model add-cloud <kind> <key> or choose a name from /model",
				out.BigRole,
			)
		}
		kind, known := ProviderKinds[provider.Kind]
		if !known || !kind.Wired {
			return ResolvedRoute{}, routeErrorf(
				"big_role provider %q uses unavailable kind %q; choose a wired provider from /model",
				out.BigRole, provider.Kind,
			)
		}
	}

	localKind := false
	if rc.Provider != nil {
		out.Endpoint = strings.TrimSpace(rc.Provider.Endpoint)
		if model := strings.TrimSpace(rc.Provider.Model); model != "" {
			out.Model = model
		}
	} else {
		// Only the endpoint is inherited. The embeddings model cannot serve the
		// completion-style Supra classifier; route wiring supplies/checks the
		// classifier model independently.
		out.Endpoint, localKind = inheritedRouteEndpoint(lc)
	}

	if !out.Enabled {
		return out, nil
	}
	if out.Endpoint == "" {
		return ResolvedRoute{}, routeErrorf("classifier endpoint is required when enabled")
	}
	if !localKind && !RouteEndpointIsLoopback(out.Endpoint) {
		return ResolvedRoute{}, routeErrorf(
			"classifier endpoint %s is not local; use a provider kind marked local or a localhost/loopback URL",
			SafeDisplayEndpoint(out.Endpoint),
		)
	}
	return out, nil
}

func validRouteMode(mode string) bool {
	return mode == route.ModeShadow || mode == route.ModeActive || mode == route.ModeEverywhere
}

// RouteBlockPresent reports whether the operator has a route section in
// config.json at all — the discriminator between "routing unconfigured"
// (ResolveRoute still fills an inherited classifier endpoint so a live
// enable works) and "routing configured but disabled".
func RouteBlockPresent(lc *Launch) bool {
	return lc != nil && lc.Route != nil
}

// inheritedRouteEndpoint resolves the classifier endpoint when route.provider
// is absent: the embed provider's endpoint, then its kind's default, then the
// packaged default — never empty. The bool reports whether the source kind is
// local, which exempts the endpoint from the loopback requirement (mirrors
// the embed provider's own trust level).
func inheritedRouteEndpoint(lc *Launch) (string, bool) {
	if lc == nil || lc.Embed == nil {
		return DefaultEmbedEndpoint, false
	}
	pk, known := ProviderKinds[lc.Embed.Kind]
	return firstNonEmpty(
		strings.TrimSpace(lc.Embed.Endpoint),
		pk.DefaultEndpoint,
		DefaultEmbedEndpoint,
	), known && pk.Local
}

// RouteEndpointIsLoopback accepts only an HTTP(S) URL whose hostname is the
// loopback name or address. In particular, wildcard and private-LAN addresses
// are not treated as localhost merely because they are non-public.
func RouteEndpointIsLoopback(endpoint string) bool {
	u, err := neturl.Parse(strings.TrimSpace(endpoint))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
