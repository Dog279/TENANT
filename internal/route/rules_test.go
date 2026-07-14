package route

import (
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }

func TestRulesMatchFirstWinsTopDown(t *testing.T) {
	rules := Rules{
		{When: RulePredicate{Code: boolPtr(true), ComplexityGTE: 3}, Role: "coder"},
		{When: RulePredicate{Code: boolPtr(true)}, Role: "coder-lite"},
		{When: RulePredicate{Math: boolPtr(true)}, Role: "mathematician"},
	}
	tests := []struct {
		name     string
		sig      Signal
		wantRole string
		wantOK   bool
	}{
		{"hard code hits first rule", Signal{Code: true, Complexity: 4}, "coder", true},
		{"easy code falls to second rule", Signal{Code: true, Complexity: 2}, "coder-lite", true},
		{"math-only skips code rules", Signal{Math: true, Complexity: 5}, "mathematician", true},
		{"no predicate matches", Signal{Domain: "chit-chat", Complexity: 1}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, ok := rules.Match(tt.sig)
			if ok != tt.wantOK || string(rule.Role) != tt.wantRole {
				t.Fatalf("Match(%+v) = (%q, %t), want (%q, %t)", tt.sig, rule.Role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}

func TestRulePredicateFieldsANDTogether(t *testing.T) {
	p := RulePredicate{Domain: "software", ComplexityGTE: 3, Math: boolPtr(false), Code: boolPtr(true)}
	match := Signal{Domain: "Software", Complexity: 3, Math: false, Code: true}
	if !p.matches(match) {
		t.Fatalf("expected full AND match (domain case-insensitive)")
	}
	for name, sig := range map[string]Signal{
		"wrong domain":       {Domain: "legal", Complexity: 3, Math: false, Code: true},
		"low complexity":     {Domain: "software", Complexity: 2, Math: false, Code: true},
		"math flag mismatch": {Domain: "software", Complexity: 3, Math: true, Code: true},
		"code flag mismatch": {Domain: "software", Complexity: 3, Math: false, Code: false},
	} {
		if p.matches(sig) {
			t.Errorf("%s: predicate unexpectedly matched %+v", name, sig)
		}
	}
}

func TestRulesValidate(t *testing.T) {
	tests := []struct {
		name    string
		rules   Rules
		wantErr string
	}{
		{"valid", Rules{{When: RulePredicate{Code: boolPtr(true)}, Role: "coder"}}, ""},
		{"missing role", Rules{{When: RulePredicate{Code: boolPtr(true)}}}, "role is required"},
		{"empty predicate", Rules{{When: RulePredicate{}, Role: "coder"}}, "at least one predicate"},
		{"complexity out of range", Rules{{When: RulePredicate{ComplexityGTE: 6}, Role: "coder"}}, "between 1 and 5"},
		{"negative complexity", Rules{{When: RulePredicate{ComplexityGTE: -1}, Role: "coder"}}, "between 1 and 5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rules.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestZeroScoresProviderIsInert(t *testing.T) {
	in := []Candidate{{Role: "coder", ProfileID: "p1", Score: 9.5}, {Role: "planner", ProfileID: "p2", Score: -1}}
	out := ZeroScores{}.Scores(Signal{Complexity: 5}, in)
	if len(out) != 2 {
		t.Fatalf("Scores() returned %d candidates, want 2", len(out))
	}
	for i, c := range out {
		if c.Score != 0 {
			t.Errorf("candidate %d score = %v, want 0", i, c.Score)
		}
	}
	if in[0].Score != 9.5 {
		t.Errorf("input slice mutated: %v", in[0].Score)
	}
}

func TestEffortHintFor(t *testing.T) {
	sig5 := Signal{Complexity: 5}
	tests := []struct {
		name              string
		sig               Signal
		supportsReasoning bool
		current           string
		want              string
	}{
		{"complexity 5 on reasoning profile", sig5, true, "", EffortHintHigh},
		{"complexity 4 never hints", Signal{Complexity: 4}, true, "", ""},
		{"non-reasoning profile never hints", sig5, false, "", ""},
		{"already high keeps operator setting", sig5, true, "high", ""},
		{"already xhigh keeps operator setting", sig5, true, "xHigh", ""},
		{"lower operator setting upgraded", sig5, true, "low", EffortHintHigh},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffortHintFor(tt.sig, tt.supportsReasoning, tt.current); got != tt.want {
				t.Fatalf("EffortHintFor() = %q, want %q", got, tt.want)
			}
		})
	}
}
