package route

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	GraduationMinShadowAttempts = 200
	GraduationMaxParseRate      = 0.02
	GraduationMinBigRate        = 0.05
	GraduationMaxBigRate        = 0.40
	GraduationMinFailureRatio   = 2.0
	GraduationMaxP95Latency     = 100 * time.Millisecond

	TurnOutcomeError = "error"
)

const (
	TagDecisionPrefix = "route:decision="
	TagModePrefix     = "route:mode="
	TagLabelPrefix    = "route:label="
	TagApplied        = "route:applied=true"
	TagRoutingError   = "route:routing_error=true"
)

// TurnOutcome is the prompt-free join between one decision and its completed
// episode. Applied and RoutingError are deliberately post-decision facts.
type TurnOutcome struct {
	DecisionID   string
	Mode         string
	Label        Tier
	Outcome      string
	Applied      bool
	RoutingError bool
}

// Tags returns the post-turn route tags that are meaningful for an episode.
// It is intentionally separate from Decision.Tags because application and
// routing-error attribution are not known when the decision row is recorded.
func (o TurnOutcome) Tags() []string {
	tags := make([]string, 0, 5)
	if strings.TrimSpace(o.DecisionID) != "" {
		tags = append(tags, TagDecisionPrefix+strings.TrimSpace(o.DecisionID))
	}
	if o.Mode != "" {
		tags = append(tags, TagModePrefix+o.Mode)
	}
	if o.Label == TierSmall || o.Label == TierBig {
		tags = append(tags, TagLabelPrefix+string(o.Label))
	}
	if o.Applied {
		tags = append(tags, TagApplied)
	}
	if o.RoutingError {
		tags = append(tags, TagRoutingError)
	}
	return tags
}

// TurnOutcomeFromTags converts the route:* tags stored on an episode into the
// graduation input. The bool is false when the episode has no route identity.
func TurnOutcomeFromTags(tags []string, outcome string) (TurnOutcome, bool) {
	got := TurnOutcome{Outcome: strings.TrimSpace(outcome)}
	seen := false
	for _, raw := range tags {
		tag := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(tag, TagDecisionPrefix):
			got.DecisionID = strings.TrimPrefix(tag, TagDecisionPrefix)
			seen = seen || got.DecisionID != ""
		case strings.HasPrefix(tag, TagModePrefix):
			got.Mode = strings.TrimPrefix(tag, TagModePrefix)
			seen = true
		case strings.HasPrefix(tag, TagLabelPrefix):
			label := Tier(strings.TrimPrefix(tag, TagLabelPrefix))
			if label == TierSmall || label == TierBig {
				got.Label = label
				seen = true
			}
		case tag == TagApplied || tag == "route:applied":
			got.Applied = true
			seen = true
		case tag == TagRoutingError || tag == "route:routing_error":
			got.RoutingError = true
			seen = true
		}
	}
	return got, seen
}

// Graduation is the numeric go/no-go gate rendered by /route stats.
type Graduation struct {
	Pass     bool
	Criteria []GraduationCriterion
}

// GraduationCriterion carries both machine-stable identity and operator text.
type GraduationCriterion struct {
	Key       string
	Name      string
	Pass      bool
	Actual    string
	Threshold string
	Detail    string
}

// StatsWithOutcomes preserves all legacy aggregates and additionally evaluates
// graduation from shadow classifier observations plus joined episode outcomes.
func (e *Engine) StatsWithOutcomes(outcomes []TurnOutcome) Stats {
	decisions := e.decisionsForStats()
	stats := statsFromDecisions(decisions)
	stats.Graduation = evaluateGraduation(decisions, outcomes)
	return stats
}

func evaluateGraduation(decisions []Decision, outcomes []TurnOutcome) Graduation {
	shadow := make([]Decision, 0, len(decisions))
	latencies := make([]int64, 0, len(decisions))
	parseFailures, labels, bigLabels := 0, 0, 0
	for _, d := range decisions {
		if d.Mode != ModeShadow || !classifierWasAttempted(d) {
			continue
		}
		shadow = append(shadow, d)
		latencies = append(latencies, d.LatencyMS)
		if strings.Contains(d.Error, "route: parse:") {
			parseFailures++
		}
		if d.Status == StatusOK && (d.Label == string(TierSmall) || d.Label == string(TierBig)) {
			labels++
			if d.Label == string(TierBig) {
				bigLabels++
			}
		}
	}

	criteria := make([]GraduationCriterion, 0, 6)
	add := func(c GraduationCriterion) { criteria = append(criteria, c) }
	add(GraduationCriterion{
		Key: "shadow_attempts", Name: "shadow/assisted observations",
		Pass:   len(shadow) >= GraduationMinShadowAttempts,
		Actual: fmt.Sprintf("%d", len(shadow)), Threshold: fmt.Sprintf(">= %d", GraduationMinShadowAttempts),
	})

	parse := GraduationCriterion{Key: "parse_failure_rate", Name: "parse failure rate", Threshold: "< 2%"}
	if len(shadow) == 0 {
		parse.Actual, parse.Detail = "missing", "no shadow classifier attempts"
	} else {
		rate := float64(parseFailures) / float64(len(shadow))
		parse.Actual, parse.Pass = percent(rate), rate < GraduationMaxParseRate
	}
	add(parse)

	big := GraduationCriterion{Key: "raw_big_rate", Name: "raw classifier big rate", Threshold: "5% to 40%"}
	if labels == 0 {
		big.Actual, big.Detail = "missing", "no successfully parsed shadow labels"
	} else {
		rate := float64(bigLabels) / float64(labels)
		big.Actual = fmt.Sprintf("%s (%d/%d)", percent(rate), bigLabels, labels)
		big.Pass = rate >= GraduationMinBigRate && rate <= GraduationMaxBigRate
	}
	add(big)

	add(failureRatioCriterion(outcomes))

	latency := GraduationCriterion{Key: "classify_p95", Name: "classify latency p95", Threshold: "< 100ms"}
	if len(latencies) == 0 {
		latency.Actual, latency.Detail = "missing", "no shadow latency observations"
	} else {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p95 := time.Duration(nearestRank(latencies, 0.95)) * time.Millisecond
		latency.Actual, latency.Pass = p95.String(), p95 < GraduationMaxP95Latency
	}
	add(latency)

	applied, routeErrors := 0, 0
	for _, outcome := range outcomes {
		if !outcome.Applied {
			continue
		}
		applied++
		if outcome.RoutingError {
			routeErrors++
		}
	}
	routing := GraduationCriterion{
		Key: "applied_routing_errors", Name: "turn failures attributable to routing", Threshold: "= 0",
	}
	if applied == 0 {
		routing.Actual, routing.Detail = "missing", "no applied turns with routing outcome telemetry"
	} else {
		routing.Actual = fmt.Sprintf("%d across %d applied turns", routeErrors, applied)
		routing.Pass = routeErrors == 0
	}
	add(routing)

	gate := Graduation{Pass: true, Criteria: criteria}
	for _, criterion := range gate.Criteria {
		if !criterion.Pass {
			gate.Pass = false
			break
		}
	}
	return gate
}

func failureRatioCriterion(outcomes []TurnOutcome) GraduationCriterion {
	c := GraduationCriterion{
		Key: "failure_proxy_ratio", Name: "big/small local-failure proxy ratio", Threshold: ">= 2x",
	}
	var smallN, bigN, smallErrors, bigErrors int
	for _, outcome := range outcomes {
		if outcome.Mode != "" && outcome.Mode != ModeShadow {
			continue
		}
		switch outcome.Label {
		case TierSmall:
			smallN++
			if outcome.Outcome == TurnOutcomeError {
				smallErrors++
			}
		case TierBig:
			bigN++
			if outcome.Outcome == TurnOutcomeError {
				bigErrors++
			}
		}
	}
	if smallN == 0 || bigN == 0 {
		c.Actual = "missing"
		c.Detail = fmt.Sprintf("need both label cohorts (small=%d, big=%d)", smallN, bigN)
		return c
	}
	smallRate := float64(smallErrors) / float64(smallN)
	bigRate := float64(bigErrors) / float64(bigN)
	switch {
	case smallRate == 0 && bigRate > 0:
		c.Actual, c.Pass = fmt.Sprintf("infinite (%s vs %s)", percent(bigRate), percent(smallRate)), true
	case smallRate == 0:
		c.Actual = fmt.Sprintf("undefined (%s vs %s)", percent(bigRate), percent(smallRate))
		c.Detail = "neither label cohort has a failure proxy"
	default:
		ratio := bigRate / smallRate
		c.Actual, c.Pass = fmt.Sprintf("%.2fx (%s vs %s)", ratio, percent(bigRate), percent(smallRate)), ratio >= GraduationMinFailureRatio
	}
	return c
}

func percent(rate float64) string {
	if math.IsNaN(rate) || math.IsInf(rate, 0) {
		return "missing"
	}
	return fmt.Sprintf("%.1f%%", 100*rate)
}
