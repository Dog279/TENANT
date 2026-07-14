package route

import (
	"errors"
	"fmt"

	"tenant/internal/model"
)

var (
	// ErrInvalidClassifierLabel marks a malformed short-prompt verdict. The
	// engine treats it as a classifier failure for circuit-breaker purposes.
	ErrInvalidClassifierLabel = errors.New("route: policy: invalid classifier label")
	// ErrInvalidPolicyInput marks caller-owned policy metadata errors. These
	// degrade to the planner but must not make a healthy classifier look dead.
	ErrInvalidPolicyInput = errors.New("route: policy: invalid input")
)

const (
	// LongPromptTokens is the deterministic threshold above which the
	// classifier is not trusted. Supra's input is intentionally bounded, so a
	// long prompt is routed by policy instead of classifying a truncation.
	LongPromptTokens = 3500

	DefaultNeverCloud          = true
	DefaultMinComplexityForBig = 4
	DefaultStickySession       = true
)

// Tier is the binary routing tier. Tiers map to roles at wiring time; they are
// never profile IDs.
type Tier string

const (
	TierSmall Tier = "small"
	TierBig   Tier = "big"
)

// Override is a one-turn operator clamp carried separately from the prompt.
type Override string

const (
	OverrideNone  Override = ""
	OverrideBig   Override = "big"
	OverrideLocal Override = "local"
)

// PolicyReason is a stable, loggable explanation of the clamp that won.
type PolicyReason string

const (
	ReasonForceProfile      PolicyReason = "force_profile"
	ReasonOperatorBig       PolicyReason = "operator_big"
	ReasonOperatorLocal     PolicyReason = "operator_local"
	ReasonNoLocalTarget     PolicyReason = "no_local_target"
	ReasonNeverCloud        PolicyReason = "never_cloud"
	ReasonStickySession     PolicyReason = "sticky_session"
	ReasonLongPrompt        PolicyReason = "long_prompt"
	ReasonMinComplexity     PolicyReason = "min_complexity_for_big"
	ReasonClassifierLabel   PolicyReason = "classifier_label"
	ReasonContextBudget     PolicyReason = "downswitch_context_budget"
	ReasonClassifierFailure PolicyReason = "classifier_failure"
	ReasonBreakerOpen       PolicyReason = "breaker_open"
)

// Target is one immutable role binding available to the policy. Local must be
// recomputed from the live provider kind for every turn; callers must not cache
// it across /model swaps.
type Target struct {
	Tier              Tier
	Role              model.Role
	ProfileID         string
	Local             bool
	OperationalBudget int
}

// PolicyInput contains every fact the code-owned clamps may use. Signal and
// EstTokens are filled by Engine.DecideWithPolicy. StickyTier is non-empty once
// tool-call syntax has entered the session context; PriorTier is the actual
// tier used by the previous turn (after any role-resolution fallback).
type PolicyInput struct {
	Signal    Signal
	EstTokens int

	Small Target
	Big   Target
	Force *Target

	Override             Override
	NeverCloud           bool
	MinComplexityForBig  int
	StickySession        bool
	StickyTier           Tier
	PriorTier            Tier
	RunningContextTokens int
	RunningContextKnown  bool

	// Candidates is the RESERVED TEN-17 join surface. Apply ignores it today:
	// learned per-(role,profile) evidence blends here once RouteScoringJob
	// ships, without another PolicyInput schema change.
	Candidates []Candidate
}

// PolicyResult is the selected role binding plus the winning explanation.
// Label is retained as a compatibility alias for the selected tier.
type PolicyResult struct {
	Label          string
	Tier           Tier
	Target         Target
	Clamped        bool
	Reason         string
	PrivacyPierced bool
}

// Policy applies the deterministic clamps. It has no mutable state: session
// stickiness is represented explicitly in PolicyInput so concurrent agents do
// not share accidental state.
type Policy struct{}

// Apply evaluates the clamps in safety order. Named force is the sole privacy
// escape hatch. One-turn overrides beat stickiness, while never_cloud beats
// every non-force request for the big target.
func (Policy) Apply(in PolicyInput) (PolicyResult, error) {
	small := withTier(in.Small, TierSmall)
	big := withTier(in.Big, TierBig)

	// Operator overrides are evaluated before session stickiness. !big means
	// biggest ALLOWED: privacy can still clamp a cloud big target to small. A
	// one-shot override also beats a standing named force for this turn.
	switch in.Override {
	case OverrideBig:
		if in.NeverCloud && !big.Local {
			return makePolicyResult(small, in.Signal.Label, ReasonNeverCloud, true, false), nil
		}
		return makePolicyResult(big, in.Signal.Label, ReasonOperatorBig, true, false), nil
	case OverrideLocal:
		if big.Local {
			return makePolicyResult(big, in.Signal.Label, ReasonOperatorLocal, true, false), nil
		}
		if small.Local {
			return makePolicyResult(small, in.Signal.Label, ReasonOperatorLocal, true, false), nil
		}
		// The policy cannot manufacture a local model. Degrade to today's
		// planner path and let the operator-facing status report the standing
		// cloud-planner notice.
		return makePolicyResult(small, in.Signal.Label, ReasonNoLocalTarget, true, false), nil
	case OverrideNone:
		// continue
	default:
		return PolicyResult{}, fmt.Errorf("%w: operator override %q", ErrInvalidPolicyInput, in.Override)
	}

	if in.Force != nil {
		force := *in.Force
		if force.ProfileID == "" || force.Role == "" {
			return PolicyResult{}, fmt.Errorf("%w: force target requires profile and role", ErrInvalidPolicyInput)
		}
		if force.Tier == "" {
			force.Tier = TierBig
		}
		return makePolicyResult(force, in.Signal.Label, ReasonForceProfile, true,
			in.NeverCloud && !force.Local), nil
	}

	// Once tool-call syntax is in context, keep the actual tier that produced
	// it. Privacy remains stronger so a live provider swap cannot open a stale
	// cloud hole.
	if in.StickySession && (in.StickyTier == TierSmall || in.StickyTier == TierBig) {
		target := small
		if in.StickyTier == TierBig {
			target = big
		}
		return finalizeSelection(in, small, big, target, ReasonStickySession, true)
	}

	// Long prompts never depend on a potentially truncated classifier result.
	if in.EstTokens > LongPromptTokens {
		return finalizeSelection(in, small, big, big, ReasonLongPrompt, true)
	}

	minComplexity := in.MinComplexityForBig
	if minComplexity <= 0 {
		minComplexity = DefaultMinComplexityForBig
	}

	var target Target
	var reason PolicyReason
	switch in.Signal.Label {
	case string(TierSmall):
		target, reason = small, ReasonClassifierLabel
	case string(TierBig):
		if in.Signal.Complexity < minComplexity {
			target, reason = small, ReasonMinComplexity
		} else {
			target, reason = big, ReasonClassifierLabel
		}
	default:
		return PolicyResult{}, fmt.Errorf("%w %q", ErrInvalidClassifierLabel, in.Signal.Label)
	}

	// A downswitch is allowed only when the smaller operational budget is
	// known and can hold the running context. Unknown capacity is not proof of
	// fit. Privacy still wins if the only safe-capacity target is cloud.
	return finalizeSelection(in, small, big, target, reason, false)
}

func finalizeSelection(in PolicyInput, small, big, target Target, reason PolicyReason, explicit bool) (PolicyResult, error) {
	if target.Tier == TierSmall && in.PriorTier == TierBig &&
		(!in.RunningContextKnown || small.OperationalBudget <= 0 || in.RunningContextTokens > small.OperationalBudget) {
		if in.NeverCloud && !big.Local {
			return makePolicyResult(small, in.Signal.Label, ReasonNeverCloud, true, false), nil
		}
		return makePolicyResult(big, in.Signal.Label, ReasonContextBudget, true, false), nil
	}
	if target.Tier == TierBig && in.NeverCloud && !target.Local {
		return makePolicyResult(small, in.Signal.Label, ReasonNeverCloud, true, false), nil
	}
	return makePolicyResult(target, in.Signal.Label, reason, explicit, false), nil
}

func withTier(target Target, tier Tier) Target {
	target.Tier = tier
	return target
}

func makePolicyResult(target Target, classifierLabel string, reason PolicyReason, explicit, pierced bool) PolicyResult {
	clamped := explicit || classifierLabel == "" || classifierLabel != string(target.Tier) || reason != ReasonClassifierLabel
	return PolicyResult{
		Label: string(target.Tier), Tier: target.Tier, Target: target,
		Clamped: clamped, Reason: string(reason), PrivacyPierced: pierced,
	}
}
