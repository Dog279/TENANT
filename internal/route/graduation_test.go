package route

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type graduationClassifier func(context.Context, string) (Signal, error)

func (f graduationClassifier) Classify(ctx context.Context, prompt string) (Signal, error) {
	return f(ctx, prompt)
}

func TestAssistedSuggestionRequiresSuccessfulShadowDisagreement(t *testing.T) {
	tests := []struct {
		name    string
		assist  bool
		mode    string
		willRun Tier
		signal  Signal
		err     error
		want    bool
	}{
		{name: "big differs from planner", assist: true, mode: ModeShadow, willRun: TierSmall,
			signal: Signal{Label: "big", Complexity: 5}, want: true},
		{name: "assist off", mode: ModeShadow, willRun: TierSmall,
			signal: Signal{Label: "big", Complexity: 5}},
		{name: "same tier", assist: true, mode: ModeShadow, willRun: TierBig,
			signal: Signal{Label: "big", Complexity: 5}},
		{name: "active is not assisted", assist: true, mode: ModeActive, willRun: TierSmall,
			signal: Signal{Label: "big", Complexity: 5}},
		{name: "classifier failure", assist: true, mode: ModeShadow, willRun: TierSmall,
			signal: Signal{Raw: "bad"}, err: errors.New("route: parse: bad")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine := NewEngine(graduationClassifier(func(context.Context, string) (Signal, error) {
				return tc.signal, tc.err
			}), t.TempDir(), 10)
			d, _ := engine.DecideWithPolicy(context.Background(), PolicyDecisionRequest{
				Prompt: "prompt", Mode: tc.mode, Assist: tc.assist, WillRun: tc.willRun,
				Input: graduationPolicyInput(),
			})
			if d.AssistedSuggestion != tc.want {
				t.Fatalf("AssistedSuggestion = %t, want %t; decision=%+v", d.AssistedSuggestion, tc.want, d)
			}
			if d.ID == "" {
				t.Fatal("decision id is empty")
			}
			if !containsTag(d.Tags(), TagDecisionPrefix+d.ID) {
				t.Fatalf("decision tags = %v, missing stable id", d.Tags())
			}
		})
	}
}

func TestEngineConfirmReplacesAssistedRowWithoutInflatingStats(t *testing.T) {
	dir := t.TempDir()
	engine := NewEngine(graduationClassifier(func(context.Context, string) (Signal, error) {
		return Signal{Label: "big", Complexity: 5, Latency: time.Millisecond}, nil
	}), dir, 10)
	suggested, err := engine.DecideWithPolicy(context.Background(), PolicyDecisionRequest{
		Prompt: "first", Mode: ModeShadow, Assist: true, WillRun: TierSmall,
		Input: graduationPolicyInput(),
	})
	if err != nil || !suggested.AssistedSuggestion {
		t.Fatalf("suggested decision/error = %+v / %v", suggested, err)
	}
	ordinary, err := engine.DecideWithPolicy(context.Background(), PolicyDecisionRequest{
		Prompt: "second", Mode: ModeShadow, Input: graduationPolicyInput(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Confirm(suggested.ID); err != nil {
		t.Fatal(err)
	}
	if err := engine.Confirm(suggested.ID); err != nil {
		t.Fatalf("idempotent Confirm: %v", err)
	}
	if err := engine.Confirm(ordinary.ID); err == nil || !strings.Contains(err.Error(), "not an assisted suggestion") {
		t.Fatalf("Confirm ordinary decision error = %v", err)
	}
	if got := engine.Stats().TotalDecisions; got != 2 {
		t.Fatalf("decision count after confirmation = %d, want 2", got)
	}

	raw, err := os.ReadFile(engine.Path())
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	rows := map[string]Decision{}
	for decoder.More() {
		var d Decision
		if err := decoder.Decode(&d); err != nil {
			t.Fatal(err)
		}
		rows[d.ID] = d
	}
	if !rows[suggested.ID].OperatorConfirmed || rows[ordinary.ID].OperatorConfirmed {
		t.Fatalf("confirmed rows = %+v", rows)
	}
	recent, ok := engine.NthRecent(2)
	if !ok || recent.ID != suggested.ID || !recent.OperatorConfirmed {
		t.Fatalf("in-memory confirmed decision = %+v, %t", recent, ok)
	}
}

func TestStatsWithOutcomesGraduationGate(t *testing.T) {
	baseDecisions := passingGraduationDecisions()
	baseOutcomes := passingGraduationOutcomes()
	tests := []struct {
		name      string
		mutate    func(*[]Decision, *[]TurnOutcome)
		wantPass  bool
		failedKey string
	}{
		{name: "pass", wantPass: true},
		{name: "insufficient observations", failedKey: "shadow_attempts", mutate: func(d *[]Decision, _ *[]TurnOutcome) {
			*d = (*d)[:199]
		}},
		{name: "parse rate is strict", failedKey: "parse_failure_rate", mutate: func(d *[]Decision, _ *[]TurnOutcome) {
			for i := 0; i < 4; i++ {
				(*d)[i].Status = StatusError
				(*d)[i].Error = "route: parse: malformed"
				(*d)[i].Label = ""
			}
		}},
		{name: "raw big rate below band", failedKey: "raw_big_rate", mutate: func(d *[]Decision, _ *[]TurnOutcome) {
			for i := range *d {
				(*d)[i].Label = string(TierSmall)
			}
			for i := 0; i < 9; i++ {
				(*d)[i].Label = string(TierBig)
			}
		}},
		{name: "failure proxy below two x", failedKey: "failure_proxy_ratio", mutate: func(_ *[]Decision, o *[]TurnOutcome) {
			seen := 0
			for i := range *o {
				if (*o)[i].Label == TierBig && (*o)[i].Outcome == TurnOutcomeError {
					seen++
					if seen == 10 {
						(*o)[i].Outcome = "success"
					}
				}
			}
		}},
		{name: "latency is strict", failedKey: "classify_p95", mutate: func(d *[]Decision, _ *[]TurnOutcome) {
			for i := 0; i < 11; i++ {
				(*d)[i].LatencyMS = 100
			}
		}},
		{name: "applied route error", failedKey: "applied_routing_errors", mutate: func(_ *[]Decision, o *[]TurnOutcome) {
			(*o)[0].RoutingError = true
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decisions := append([]Decision(nil), baseDecisions...)
			outcomes := append([]TurnOutcome(nil), baseOutcomes...)
			if tc.mutate != nil {
				tc.mutate(&decisions, &outcomes)
			}
			engine := &Engine{history: decisions, limit: len(decisions), now: time.Now}
			stats := engine.StatsWithOutcomes(outcomes)
			if stats.Graduation.Pass != tc.wantPass {
				t.Fatalf("graduation pass = %t, want %t: %+v", stats.Graduation.Pass, tc.wantPass, stats.Graduation)
			}
			if tc.failedKey != "" && criterion(t, stats.Graduation, tc.failedKey).Pass {
				t.Fatalf("criterion %s unexpectedly passed: %+v", tc.failedKey, stats.Graduation)
			}
			if len(engine.Stats().Graduation.Criteria) != 0 {
				t.Fatal("legacy Stats unexpectedly populated graduation")
			}
		})
	}
}

func TestStatsWithOutcomesMissingDataFailsWithExplanations(t *testing.T) {
	stats := (&Engine{}).StatsWithOutcomes(nil)
	if stats.Graduation.Pass || len(stats.Graduation.Criteria) != 6 {
		t.Fatalf("empty graduation = %+v", stats.Graduation)
	}
	for _, key := range []string{"parse_failure_rate", "raw_big_rate", "failure_proxy_ratio", "classify_p95", "applied_routing_errors"} {
		c := criterion(t, stats.Graduation, key)
		if c.Pass || c.Actual != "missing" || c.Detail == "" {
			t.Errorf("missing criterion %s = %+v", key, c)
		}
	}
}

func TestTurnOutcomeFromTags(t *testing.T) {
	want := TurnOutcome{
		DecisionID: "d-1", Mode: ModeShadow, Label: TierBig, Outcome: TurnOutcomeError,
		Applied: true, RoutingError: true,
	}
	tags := want.Tags()
	got, ok := TurnOutcomeFromTags(tags, TurnOutcomeError)
	if !ok || got.DecisionID != "d-1" || got.Mode != ModeShadow || got.Label != TierBig ||
		got.Outcome != TurnOutcomeError || !got.Applied || !got.RoutingError {
		t.Fatalf("TurnOutcomeFromTags = %+v, %t", got, ok)
	}
	for _, required := range []string{TagDecisionPrefix + "d-1", TagApplied, TagRoutingError} {
		if !containsTag(tags, required) {
			t.Errorf("TurnOutcome.Tags = %v, missing %q", tags, required)
		}
	}
	if _, ok := TurnOutcomeFromTags([]string{"unrelated"}, "success"); ok {
		t.Fatal("unrelated tags produced a route outcome")
	}
}

func graduationPolicyInput() PolicyInput {
	return PolicyInput{
		Small:               Target{ProfileID: "small", Local: true, OperationalBudget: 100},
		Big:                 Target{ProfileID: "big", Local: true, OperationalBudget: 1000},
		MinComplexityForBig: 4, RunningContextKnown: true,
	}
}

func passingGraduationDecisions() []Decision {
	out := make([]Decision, 200)
	for i := range out {
		label := TierSmall
		if i < 40 {
			label = TierBig
		}
		out[i] = Decision{
			Mode: ModeShadow, Status: StatusOK, ClassifierAttempted: true,
			Label: string(label), SelectedTier: label, LatencyMS: 50,
		}
	}
	return out
}

func passingGraduationOutcomes() []TurnOutcome {
	out := make([]TurnOutcome, 0, 150)
	for i := 0; i < 100; i++ {
		outcome := "success"
		if i < 10 {
			outcome = TurnOutcomeError
		}
		out = append(out, TurnOutcome{Mode: ModeShadow, Label: TierSmall, Outcome: outcome, Applied: i == 0})
	}
	for i := 0; i < 50; i++ {
		outcome := "success"
		if i < 10 {
			outcome = TurnOutcomeError
		}
		out = append(out, TurnOutcome{Mode: ModeShadow, Label: TierBig, Outcome: outcome})
	}
	return out
}

func criterion(t *testing.T, gate Graduation, key string) GraduationCriterion {
	t.Helper()
	for _, c := range gate.Criteria {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("criterion %q not found in %+v", key, gate)
	return GraduationCriterion{}
}

func containsTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
