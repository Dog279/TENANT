package config

import (
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/route"
)

func TestResolveRouteConfigRules(t *testing.T) {
	lc := &Launch{Route: &Route{
		Enabled: true, Mode: route.ModeShadow,
		Rules: []RouteRule{
			{When: RouteRuleWhen{Code: rulesBoolPtr(true), ComplexityGTE: 3}, Role: "coder"},
			{When: RouteRuleWhen{Domain: "legal"}, Role: "planner-big"},
		},
	}}
	got, err := ResolveRoute(lc)
	if err != nil {
		t.Fatalf("resolveRouteConfig: %v", err)
	}
	if len(got.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(got.Rules))
	}
	if got.Rules[0].Role != "coder" || got.Rules[0].When.ComplexityGTE != 3 {
		t.Fatalf("rule 0 mangled: %+v", got.Rules[0])
	}
	if got.LogRetentionDays != route.DefaultLogRetentionDays {
		t.Fatalf("retention default = %d, want %d", got.LogRetentionDays, route.DefaultLogRetentionDays)
	}
}

func TestResolveRouteConfigRuleValueValidation(t *testing.T) {
	for name, rules := range map[string][]RouteRule{
		"missing role":    {{When: RouteRuleWhen{Code: rulesBoolPtr(true)}}},
		"empty predicate": {{When: RouteRuleWhen{}, Role: "coder"}},
		"bad complexity":  {{When: RouteRuleWhen{ComplexityGTE: 9}, Role: "coder"}},
	} {
		t.Run(name, func(t *testing.T) {
			lc := &Launch{Route: &Route{Enabled: true, Mode: route.ModeShadow, Rules: rules}}
			if _, err := ResolveRoute(lc); err == nil {
				t.Fatal("resolveRouteConfig accepted an invalid rule")
			}
		})
	}
}

// TestRoutePredicateVocabularyIsFrozen is the 4-field anti-scope-creep
// commitment: any unknown predicate key is a decode error, never a no-op.
func TestRoutePredicateVocabularyIsFrozen(t *testing.T) {
	blob := `{"route":{"enabled":true,"mode":"shadow","rules":[{"when":{"code":true,"language":"go"},"role":"coder"}]}}`
	var lc Launch
	err := json.Unmarshal([]byte(blob), &lc)
	if err == nil {
		t.Fatal("unknown predicate key decoded without error")
	}
	if !strings.Contains(err.Error(), "domain, complexity_gte, math, code") {
		t.Fatalf("rejection does not name the frozen vocabulary: %v", err)
	}
	// The four allowed keys still decode.
	ok := `{"route":{"enabled":true,"mode":"shadow","rules":[{"when":{"domain":"software","complexity_gte":2,"math":false,"code":true},"role":"coder"}]}}`
	if err := json.Unmarshal([]byte(ok), &lc); err != nil {
		t.Fatalf("full frozen vocabulary rejected: %v", err)
	}
}

func TestResolveRouteConfigRetention(t *testing.T) {
	for name, tt := range map[string]struct {
		configured int
		want       int
	}{
		"default 90":       {0, route.DefaultLogRetentionDays},
		"explicit":         {30, 30},
		"explicit disable": {-1, -1},
	} {
		t.Run(name, func(t *testing.T) {
			lc := &Launch{Route: &Route{Enabled: true, Mode: route.ModeShadow, LogRetentionDays: tt.configured}}
			got, err := ResolveRoute(lc)
			if err != nil {
				t.Fatalf("resolveRouteConfig: %v", err)
			}
			if got.LogRetentionDays != tt.want {
				t.Fatalf("retention = %d, want %d", got.LogRetentionDays, tt.want)
			}
		})
	}
}
func rulesBoolPtr(v bool) *bool { return &v }
