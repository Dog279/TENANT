package route_test

import (
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/route"
)

func basePolicyInput() route.PolicyInput {
	return route.PolicyInput{
		Signal: route.Signal{Label: "small", Complexity: 2},
		Small: route.Target{
			Role: model.RolePlanner, ProfileID: "small-local", Local: true,
			OperationalBudget: 100,
		},
		Big: route.Target{
			Role: model.Role("planner-big"), ProfileID: "big-local", Local: true,
			OperationalBudget: 1000,
		},
		MinComplexityForBig: 4, RunningContextKnown: true,
	}
}

func TestPolicyClampOrdering(t *testing.T) {
	t.Parallel()
	forceCloud := route.Target{
		Tier: route.TierBig, Role: model.Role("planner-force"),
		ProfileID: "named-cloud", Local: false, OperationalBudget: 2000,
	}
	cases := []struct {
		name        string
		mutate      func(*route.PolicyInput)
		wantTier    route.Tier
		wantReason  route.PolicyReason
		wantPierced bool
	}{
		{
			name:     "operator big beats small label",
			mutate:   func(in *route.PolicyInput) { in.Override = route.OverrideBig },
			wantTier: route.TierBig, wantReason: route.ReasonOperatorBig,
		},
		{
			name: "never cloud beats operator big",
			mutate: func(in *route.PolicyInput) {
				in.Override, in.NeverCloud, in.Big.Local = route.OverrideBig, true, false
			},
			wantTier: route.TierSmall, wantReason: route.ReasonNeverCloud,
		},
		{
			name: "never cloud beats long prompt",
			mutate: func(in *route.PolicyInput) {
				in.EstTokens, in.NeverCloud, in.Big.Local = route.LongPromptTokens+1, true, false
			},
			wantTier: route.TierSmall, wantReason: route.ReasonNeverCloud,
		},
		{
			name: "long prompt beats minimum complexity",
			mutate: func(in *route.PolicyInput) {
				in.EstTokens = route.LongPromptTokens + 1
				in.Signal = route.Signal{Label: "big", Complexity: 1}
			},
			wantTier: route.TierBig, wantReason: route.ReasonLongPrompt,
		},
		{
			name: "minimum complexity beats big label",
			mutate: func(in *route.PolicyInput) {
				in.Signal = route.Signal{Label: "big", Complexity: 3}
			},
			wantTier: route.TierSmall, wantReason: route.ReasonMinComplexity,
		},
		{
			name: "label trusted after clamps",
			mutate: func(in *route.PolicyInput) {
				in.Signal = route.Signal{Label: "big", Complexity: 4}
			},
			wantTier: route.TierBig, wantReason: route.ReasonClassifierLabel,
		},
		{
			name: "sticky small beats long and label",
			mutate: func(in *route.PolicyInput) {
				in.StickySession, in.StickyTier = true, route.TierSmall
				in.EstTokens = route.LongPromptTokens + 1
				in.Signal = route.Signal{Label: "big", Complexity: 5}
			},
			wantTier: route.TierSmall, wantReason: route.ReasonStickySession,
		},
		{
			name: "operator override beats sticky",
			mutate: func(in *route.PolicyInput) {
				in.StickySession, in.StickyTier = true, route.TierSmall
				in.Override = route.OverrideBig
			},
			wantTier: route.TierBig, wantReason: route.ReasonOperatorBig,
		},
		{
			name: "privacy beats sticky big",
			mutate: func(in *route.PolicyInput) {
				in.StickySession, in.StickyTier = true, route.TierBig
				in.NeverCloud, in.Big.Local = true, false
			},
			wantTier: route.TierSmall, wantReason: route.ReasonNeverCloud,
		},
		{
			name: "local override picks biggest local",
			mutate: func(in *route.PolicyInput) {
				in.Override, in.Small.Local = route.OverrideLocal, false
			},
			wantTier: route.TierBig, wantReason: route.ReasonOperatorLocal,
		},
		{
			name: "oversized context blocks downswitch",
			mutate: func(in *route.PolicyInput) {
				in.PriorTier, in.RunningContextTokens = route.TierBig, 101
			},
			wantTier: route.TierBig, wantReason: route.ReasonContextBudget,
		},
		{
			name: "fitting context allows downswitch",
			mutate: func(in *route.PolicyInput) {
				in.PriorTier, in.RunningContextTokens = route.TierBig, 100
			},
			wantTier: route.TierSmall, wantReason: route.ReasonClassifierLabel,
		},
		{
			name: "unknown small budget blocks downswitch",
			mutate: func(in *route.PolicyInput) {
				in.PriorTier, in.Small.OperationalBudget = route.TierBig, 0
			},
			wantTier: route.TierBig, wantReason: route.ReasonContextBudget,
		},
		{
			name: "named force pierces privacy",
			mutate: func(in *route.PolicyInput) {
				in.Force, in.NeverCloud = &forceCloud, true
				in.StickySession, in.StickyTier = true, route.TierSmall
			},
			wantTier: route.TierBig, wantReason: route.ReasonForceProfile, wantPierced: true,
		},
		{
			name: "local override beats named cloud force",
			mutate: func(in *route.PolicyInput) {
				in.Force, in.NeverCloud, in.Override = &forceCloud, true, route.OverrideLocal
			},
			wantTier: route.TierBig, wantReason: route.ReasonOperatorLocal,
		},
		{
			name: "sticky small cannot bypass downswitch budget",
			mutate: func(in *route.PolicyInput) {
				in.StickySession, in.StickyTier = true, route.TierSmall
				in.PriorTier, in.RunningContextTokens = route.TierBig, 101
			},
			wantTier: route.TierBig, wantReason: route.ReasonContextBudget,
		},
		{
			name: "unknown running context blocks downswitch",
			mutate: func(in *route.PolicyInput) {
				in.PriorTier, in.RunningContextKnown = route.TierBig, false
			},
			wantTier: route.TierBig, wantReason: route.ReasonContextBudget,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := basePolicyInput()
			tc.mutate(&in)
			got, err := (route.Policy{}).Apply(in)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got.Tier != tc.wantTier || got.Reason != string(tc.wantReason) {
				t.Fatalf("result = %+v, want tier=%q reason=%q", got, tc.wantTier, tc.wantReason)
			}
			if got.PrivacyPierced != tc.wantPierced {
				t.Fatalf("PrivacyPierced = %t, want %t", got.PrivacyPierced, tc.wantPierced)
			}
		})
	}
}

func TestPolicyRejectsInvalidShortPromptLabel(t *testing.T) {
	t.Parallel()
	in := basePolicyInput()
	in.Signal.Label = ""
	_, err := (route.Policy{}).Apply(in)
	if err == nil || !strings.Contains(err.Error(), "invalid classifier label") {
		t.Fatalf("Apply error = %v", err)
	}
}
