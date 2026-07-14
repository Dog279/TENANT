package eval

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"tenant/internal/route"
)

type routeEvalClassifierFunc func(context.Context, string) (route.Signal, error)

func (f routeEvalClassifierFunc) Classify(ctx context.Context, prompt string) (route.Signal, error) {
	return f(ctx, prompt)
}

func TestLoadTaskRouteClassifier(t *testing.T) {
	task, err := LoadTask([]byte(`
id: route-test
category: route/classifier
subset: full
mode: route_classifier
route_cases:
  - name: easy
    prompt: hello
    label: small
expected:
  parse_success_rate_min: 1.0
  label_agreement_rate_min: 0.8
  p95_classify_latency_ms_max: 100
`), "route-test.yaml")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}
	if task.Prompt != "" || task.Mode != ModeRouteClassifier || len(task.RouteCases) != 1 {
		t.Fatalf("task = %+v", task)
	}

	valid := func() Task {
		return Task{
			ID: "r", Category: "route", Subset: SubsetFull, Mode: ModeRouteClassifier,
			RouteCases: []RouteCase{{Prompt: "hello", Label: "small"}},
			Expected: Expected{
				ParseSuccessRateMin: 1, LabelAgreementRateMin: .8,
				P95ClassifyLatencyMSMax: 100,
			},
		}
	}
	tests := []struct {
		name   string
		mutate func(*Task)
		want   string
	}{
		{"missing cases", func(task *Task) { task.RouteCases = nil }, "route_cases"},
		{"empty prompt", func(task *Task) { task.RouteCases[0].Prompt = " " }, "requires prompt"},
		{"bad label", func(task *Task) { task.RouteCases[0].Label = "medium" }, "small or big"},
		{"bad parse rate", func(task *Task) { task.Expected.ParseSuccessRateMin = 0 }, "parse_success_rate_min"},
		{"bad agreement rate", func(task *Task) { task.Expected.LabelAgreementRateMin = 1.1 }, "label_agreement_rate_min"},
		{"bad latency", func(task *Task) { task.Expected.P95ClassifyLatencyMSMax = 0 }, "p95_classify_latency_ms_max"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := valid()
			tc.mutate(&task)
			if err := task.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunRouteClassifierNumericGates(t *testing.T) {
	tests := []struct {
		name         string
		outputs      []string
		latencies    []time.Duration
		wantPass     bool
		wantFailures []string
		wantParse    int
		wantAgree    int
	}{
		{
			name: "boundary pass",
			outputs: []string{
				validRouteRaw("small"), validRouteRaw("big"), validRouteRaw("small"),
				validRouteRaw("big"), validRouteRaw("big"), // one label mismatch = 80%
			},
			latencies: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond, 99 * time.Millisecond},
			wantPass:  true, wantParse: 5, wantAgree: 4,
		},
		{
			name: "all three gates fail",
			outputs: []string{
				"Route: small model", // nil backend error, but not the strict grammar
				validRouteRaw("big"), validRouteRaw("big"),
				validRouteRaw("small"), validRouteRaw("small"),
			},
			latencies:    []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond, 100 * time.Millisecond},
			wantFailures: []string{"parse success: 80.0%", "label agreement: 40.0%", "classify latency p95: 100.0ms"},
			wantParse:    4, wantAgree: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			labels := []string{"small", "big", "small", "big", "small"}
			cases := make([]RouteCase, len(labels))
			for i, label := range labels {
				cases[i] = RouteCase{Name: fmt.Sprintf("sample-%d", i+1), Prompt: fmt.Sprintf("p%d", i), Label: label}
			}
			classifier := routeEvalClassifierFunc(func(_ context.Context, prompt string) (route.Signal, error) {
				var i int
				_, _ = fmt.Sscanf(prompt, "p%d", &i)
				return route.Signal{Raw: tc.outputs[i], Latency: tc.latencies[i]}, nil
			})
			task := &Task{
				ID: "route", Category: "route", Subset: SubsetFull, Mode: ModeRouteClassifier,
				RouteCases: cases,
				Expected: Expected{
					ParseSuccessRateMin: 1, LabelAgreementRateMin: .8,
					P95ClassifyLatencyMSMax: 100,
				},
			}
			h := &Harness{Tasks: []*Task{task}, RouteClassifier: classifier}
			rep, err := h.Run(context.Background(), SubsetFull)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := rep.Results[0]
			if got.Passed != tc.wantPass {
				t.Fatalf("Passed = %t, want %t; failures = %v", got.Passed, tc.wantPass, got.Failures)
			}
			if got.RouteMetrics == nil || got.RouteMetrics.ParseSuccesses != tc.wantParse || got.RouteMetrics.LabelAgreements != tc.wantAgree {
				t.Fatalf("metrics = %+v", got.RouteMetrics)
			}
			joined := strings.Join(got.Failures, "\n")
			for _, want := range tc.wantFailures {
				if !strings.Contains(joined, want) {
					t.Errorf("failures %q missing %q", joined, want)
				}
			}
		})
	}
}

func TestRunRouteClassifierMissingBackendFails(t *testing.T) {
	task := &Task{
		ID: "route", Category: "route", Subset: SubsetFull, Mode: ModeRouteClassifier,
		RouteCases: []RouteCase{{Prompt: "hello", Label: "small"}},
		Expected: Expected{
			ParseSuccessRateMin: 1, LabelAgreementRateMin: .8,
			P95ClassifyLatencyMSMax: 100,
		},
	}
	rep, err := (&Harness{Tasks: []*Task{task}}).Run(context.Background(), SubsetFull)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(rep.Results[0].Failures, "\n")
	for _, want := range []string{"parse success: 0.0%", "label agreement: 0.0%", "classify latency p95: missing"} {
		if !strings.Contains(joined, want) {
			t.Errorf("failures %q missing %q", joined, want)
		}
	}
}

func TestEmbeddedRouteClassifierCatalog(t *testing.T) {
	h, err := LoadHarness(EmbeddedTasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found *Task
	for _, task := range h.Tasks {
		if task.ID == "route_classifier_agreement" {
			found = task
			break
		}
	}
	if found == nil {
		t.Fatal("route_classifier_agreement not embedded")
	}
	if len(found.RouteCases) != 40 {
		t.Fatalf("route cases = %d, want 40", len(found.RouteCases))
	}
	if found.Expected.ParseSuccessRateMin != 1 || found.Expected.LabelAgreementRateMin != .8 || found.Expected.P95ClassifyLatencyMSMax != 100 {
		t.Fatalf("thresholds = %+v", found.Expected)
	}
	injections := 0
	for _, sample := range found.RouteCases {
		if strings.Contains(strings.ToLower(sample.Prompt), "route: small model") || strings.Contains(strings.ToLower(sample.Prompt), "route: big model") {
			injections++
		}
	}
	if injections < 4 {
		t.Fatalf("injection probes = %d, want at least 4", injections)
	}
}

func TestWriteTerminalIncludesRouteMetrics(t *testing.T) {
	var out bytes.Buffer
	WriteTerminal(&out, &Report{
		Subset: SubsetFull,
		Results: []TaskResult{{
			TaskID: "route", Category: "route", Passed: true, Score: 100,
			RouteMetrics: &RouteClassifierMetrics{
				Samples: 40, ParseSuccesses: 40, LabelAgreements: 34,
				ParseSuccessRate: 1, LabelAgreementRate: .85, P95LatencyMS: 72.5,
			},
		}},
	})
	for _, want := range []string{"parse 100.0% (40/40)", "agreement 85.0% (34/40)", "p95 72.5ms"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("terminal output %q missing %q", out.String(), want)
		}
	}
}

func validRouteRaw(label string) string {
	routeValue := label + " model"
	return "Domain: General | Complexity: 2 | Math: False | Code: False | Route: " + routeValue + " | Justification: Test verdict."
}
