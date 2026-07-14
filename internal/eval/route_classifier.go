package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"tenant/internal/route"
)

// runRouteClassifier executes the complete hand-labeled suite directly
// against the live auxiliary endpoint. It consumes only route.Signal: even an
// adversarial prompt can at worst influence the binary tier, never request an
// agent action or tool call.
func (h *Harness) runRouteClassifier(ctx context.Context, t *Task) TaskResult {
	res := TaskResult{TaskID: t.ID, Category: t.Category, Passed: true, Score: 100}
	metrics := &RouteClassifierMetrics{Samples: len(t.RouteCases)}
	res.RouteMetrics = metrics

	latencies := make([]time.Duration, 0, len(t.RouteCases))
	var parseDiagnostics, agreementDiagnostics []string
	for i, c := range t.RouteCases {
		if ctx.Err() != nil {
			break
		}
		if h.RouteClassifier == nil {
			break
		}
		started := time.Now()
		sig, classifyErr := classifyRouteSafely(h.RouteClassifier, ctx, c.Prompt)
		latency := sig.Latency
		if latency <= 0 {
			latency = time.Since(started)
		}
		latencies = append(latencies, latency)
		metrics.Attempts++

		caseName := c.Name
		if caseName == "" {
			caseName = fmt.Sprintf("case %d", i+1)
		}
		if classifyErr != nil {
			parseDiagnostics = appendDiagnostic(parseDiagnostics,
				fmt.Sprintf("%s: %v", caseName, classifyErr))
			continue
		}
		// Re-parse Raw instead of trusting normalized fields supplied by an
		// adapter. This mechanically enforces Supra's six-field grammar on every
		// sample, including prompts that contain forged Route fields.
		parsed, err := route.ParseSignal(sig.Raw)
		if err != nil {
			parseDiagnostics = appendDiagnostic(parseDiagnostics,
				fmt.Sprintf("%s: %v", caseName, err))
			continue
		}
		metrics.ParseSuccesses++
		if parsed.Label == c.Label {
			metrics.LabelAgreements++
		} else {
			agreementDiagnostics = appendDiagnostic(agreementDiagnostics,
				fmt.Sprintf("%s: got %s, want %s", caseName, parsed.Label, c.Label))
		}
	}

	if metrics.Samples > 0 {
		metrics.ParseSuccessRate = float64(metrics.ParseSuccesses) / float64(metrics.Samples)
		metrics.LabelAgreementRate = float64(metrics.LabelAgreements) / float64(metrics.Samples)
	}
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p95 := percentileDuration(latencies, 0.95)
		metrics.P95LatencyMS = float64(p95) / float64(time.Millisecond)
	}

	exp := t.Expected
	if metrics.ParseSuccessRate < exp.ParseSuccessRateMin {
		res.Failures = append(res.Failures, fmt.Sprintf(
			"parse success: %.1f%% (%d/%d), require >= %.1f%%",
			100*metrics.ParseSuccessRate, metrics.ParseSuccesses, metrics.Samples,
			100*exp.ParseSuccessRateMin))
		if len(parseDiagnostics) > 0 {
			res.Failures = append(res.Failures, "parse diagnostics: "+strings.Join(parseDiagnostics, "; "))
		}
	}
	if metrics.LabelAgreementRate < exp.LabelAgreementRateMin {
		res.Failures = append(res.Failures, fmt.Sprintf(
			"label agreement: %.1f%% (%d/%d), require >= %.1f%%",
			100*metrics.LabelAgreementRate, metrics.LabelAgreements, metrics.Samples,
			100*exp.LabelAgreementRateMin))
		if len(agreementDiagnostics) > 0 {
			res.Failures = append(res.Failures, "agreement diagnostics: "+strings.Join(agreementDiagnostics, "; "))
		}
	}
	latencyMax := float64(exp.P95ClassifyLatencyMSMax)
	if metrics.Attempts == 0 {
		res.Failures = append(res.Failures, fmt.Sprintf(
			"classify latency p95: missing (0/%d attempts), require < %dms",
			metrics.Samples, exp.P95ClassifyLatencyMSMax))
	} else if metrics.P95LatencyMS >= latencyMax {
		res.Failures = append(res.Failures, fmt.Sprintf(
			"classify latency p95: %.1fms, require < %dms",
			metrics.P95LatencyMS, exp.P95ClassifyLatencyMSMax))
	}
	if len(res.Failures) > 0 {
		res.Passed = false
		res.Score = 0
	}
	return res
}

func classifyRouteSafely(classifier route.Classifier, ctx context.Context, prompt string) (sig route.Signal, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("route classifier panic: %v", v)
		}
	}()
	return classifier.Classify(ctx, prompt)
}

func percentileDuration(sorted []time.Duration, percentile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(float64(len(sorted))*percentile)) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// appendDiagnostic caps sample-level detail so a broken endpoint does not
// turn one report row into forty near-identical errors.
func appendDiagnostic(dst []string, message string) []string {
	if len(dst) < 3 {
		return append(dst, message)
	}
	return dst
}
