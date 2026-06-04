// Package compaction is a fidelity eval for Tenant's context-window
// compaction (the compressor in internal/memory/compress). It measures
// whether compaction PRESERVES the information a continuing agent needs —
// not how small it makes the context. It is the baseline/regression harness
// for the Context Compaction Upgrade (TEN-98 / TEN-99); see
// docs/compaction-upgrade-plan.md §4.1.
//
// Design: deterministic GIVEN a compactor's output. Each probe plants exact,
// unique marker tokens ("needles") into a synthetic session, runs the
// compactor, and checks whether each marker survives in the post-compaction
// message text. No answerer or judge model is involved — the only model in
// the loop is the compactor's OWN summarizer, which is the thing under test.
//
//   - Run against the real compress.Compressor with a real model for a true
//     baseline (operator command).
//   - Run with a fixture model in `go test` for a deterministic regression
//     gate (the harness's own correctness is asserted that way).
//
// Per docs/compaction-upgrade-plan.md §4.1 we deliberately do NOT use
// perplexity — these are retrieval/continuation/drift probes, which correlate
// with task utility.
package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"tenant/internal/memory/working"
)

// SchemaVersion is the Report JSON schema version, bumped on breaking shape
// changes so external consumers can detect mismatches at parse time.
const SchemaVersion = 1

// Compactor is the unit under test: any "compact this message slice"
// implementation. *compress.Compressor satisfies it, as does agent.Compactor.
type Compactor interface {
	Compact(ctx context.Context, msgs []working.Message) ([]working.Message, bool, error)
}

// TokenCounter measures a string's token length for the tokens-saved metric.
// EstimateTokens is the dependency-free default; pass the model's real
// counter for an exact figure.
type TokenCounter func(string) int

// EstimateTokens is a model-free ~4-chars-per-token estimate.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// Probe kinds.
const (
	KindNeedle       = "needle"       // do planted identifiers survive one compaction?
	KindContinuation = "continuation" // does task state (active task + pending step) survive?
	KindDrift        = "drift"        // do facts survive N SEQUENTIAL compactions?
)

// Options tunes the synthetic sessions. Zero values get sensible defaults via
// withDefaults; defaults guarantee the stock compress.Compressor (1500-token
// protected tail, MinMessages 6) actually triggers a compaction.
type Options struct {
	FillerTurns        int            // filler turns appended after the planted head (default 14)
	FillerCharsPerTurn int            // approx chars per filler turn (default 1600 ≈ 400 tok)
	DriftRounds        int            // sequential compactions in the drift probe (default 4)
	Now                func() time.Time // clock seam for deterministic timestamps
}

func (o Options) withDefaults() Options {
	if o.FillerTurns <= 0 {
		o.FillerTurns = 14
	}
	if o.FillerCharsPerTurn <= 0 {
		o.FillerCharsPerTurn = 1600
	}
	if o.DriftRounds <= 0 {
		o.DriftRounds = 4
	}
	if o.Now == nil {
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		o.Now = func() time.Time { return base }
	}
	return o
}

// Report is the full output of one compaction-fidelity run. Recall /
// Continuation are "higher is better"; DriftRate is "lower is better".
type Report struct {
	SchemaVersion  int           `json:"schema_version"`
	RanAt          time.Time     `json:"ran_at"`
	Recall         float64       `json:"recall_at_compaction"` // [0,1] planted identifiers surviving one compaction
	Continuation   float64       `json:"continuation_success"` // [0,1] task-state survival
	DriftRate      float64       `json:"drift_rate"`           // [0,1] facts LOST across N compactions (lower better)
	TokensBefore   int           `json:"tokens_before"`
	TokensAfter    int           `json:"tokens_after"`
	TokensSavedPct float64       `json:"tokens_saved_pct"`
	AvgLatencyMS   int64         `json:"avg_compaction_latency_ms"`
	Probes         []ProbeResult `json:"probes"`
}

// ProbeResult is one probe's outcome. Score is survival fraction (higher
// better) for every kind — for drift, DriftRate = 1 - Score at the report level.
type ProbeResult struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Planted   int      `json:"planted"`
	Survived  int      `json:"survived"`
	Score     float64  `json:"score"`
	Compacted bool     `json:"compacted"` // did the compactor actually change the set?
	LatencyMS int64    `json:"latency_ms"`
	Lost      []string `json:"lost,omitempty"` // markers that did not survive
	Notes     string   `json:"notes,omitempty"`
}

// Evaluate runs all probes against c and aggregates a Report. count may be nil
// (defaults to EstimateTokens). The compactor's summarizer is the only model
// touched; pass a real one for a baseline, a fixture for a deterministic test.
func Evaluate(ctx context.Context, c Compactor, count TokenCounter, opt Options) (*Report, error) {
	if c == nil {
		return nil, fmt.Errorf("compaction: nil compactor")
	}
	if count == nil {
		count = EstimateTokens
	}
	opt = opt.withDefaults()

	rep := &Report{SchemaVersion: SchemaVersion, RanAt: time.Now().UTC()}

	needle, before, after, err := needleProbe(ctx, c, count, opt)
	if err != nil {
		return nil, fmt.Errorf("compaction: needle probe: %w", err)
	}
	cont, err := continuationProbe(ctx, c, opt)
	if err != nil {
		return nil, fmt.Errorf("compaction: continuation probe: %w", err)
	}
	drift, err := driftProbe(ctx, c, opt)
	if err != nil {
		return nil, fmt.Errorf("compaction: drift probe: %w", err)
	}

	rep.Probes = []ProbeResult{needle, cont, drift}
	rep.Recall = needle.Score
	rep.Continuation = cont.Score
	rep.DriftRate = 1 - drift.Score
	rep.TokensBefore = before
	rep.TokensAfter = after
	if before > 0 {
		rep.TokensSavedPct = float64(before-after) / float64(before) * 100
	}
	rep.AvgLatencyMS = (needle.LatencyMS + cont.LatencyMS + drift.LatencyMS) / 3
	return rep, nil
}

// --- probes ---

func needleProbe(ctx context.Context, c Compactor, count TokenCounter, opt Options) (ProbeResult, int, int, error) {
	s := newSession(opt.Now)
	s.add("user", "Project brief — remember these exact identifiers verbatim for the entire task.")
	facts := []string{
		"The production database password reference is %s.",
		"The deployment target host is %s.",
		"The customer's account number is %s.",
		"The agreed API version pin is %s.",
		"The incident ticket to reference is %s.",
		"The required output file name is %s.",
	}
	markers := make([]string, 0, len(facts))
	for i, tmpl := range facts {
		mk := marker("NDL", i)
		markers = append(markers, mk)
		s.add("assistant", fmt.Sprintf(tmpl, mk))
	}
	s.filler(opt.FillerTurns, opt.FillerCharsPerTurn)

	before := countTokens(count, s.msgs)
	out, changed, lat, err := timedCompact(ctx, c, s.msgs)
	if err != nil {
		return ProbeResult{}, 0, 0, err
	}
	after := countTokens(count, out)
	survived, lost := survival(joinContent(out), markers)
	return ProbeResult{
		Name: "needle-identifiers", Kind: KindNeedle,
		Planted: len(markers), Survived: survived, Score: ratio(survived, len(markers)),
		Compacted: changed, LatencyMS: lat, Lost: lost, Notes: noopNote(changed),
	}, before, after, nil
}

func continuationProbe(ctx context.Context, c Compactor, opt Options) (ProbeResult, error) {
	s := newSession(opt.Now)
	task, dec, pend := marker("TASK", 1), marker("DEC", 1), marker("PEND", 1)
	s.add("user", "We are starting a multi-step task. Keep the state straight across the whole session.")
	s.add("assistant", fmt.Sprintf("Active task %s: migrate the auth module to the new token format.", task))
	s.add("assistant", fmt.Sprintf("Decision %s: keep backward-compatible tokens during the migration window.", dec))
	s.add("assistant", fmt.Sprintf("Pending step %s: run the migration script BEFORE deploying — not done yet.", pend))
	s.filler(opt.FillerTurns, opt.FillerCharsPerTurn)

	markers := []string{task, dec, pend}
	out, changed, lat, err := timedCompact(ctx, c, s.msgs)
	if err != nil {
		return ProbeResult{}, err
	}
	survived, lost := survival(joinContent(out), markers)
	return ProbeResult{
		Name: "continuation-task-state", Kind: KindContinuation,
		Planted: len(markers), Survived: survived, Score: ratio(survived, len(markers)),
		Compacted: changed, LatencyMS: lat, Lost: lost,
		Notes: "active task + decision + pending step must survive so the agent can continue" + noopSuffix(changed),
	}, nil
}

func driftProbe(ctx context.Context, c Compactor, opt Options) (ProbeResult, error) {
	s := newSession(opt.Now)
	factText := []string{
		"the project ships on SQLite with FTS5, not Postgres",
		"the primary user is in the America/New_York timezone",
		"the agreed code style is tabs, width 4",
		"the release train is every other Thursday",
		"the on-call escalation path is pager then phone",
	}
	markers := make([]string, 0, len(factText))
	s.add("user", "Record these established facts; they must remain stable for the entire project.")
	for i, ft := range factText {
		mk := marker("FACT", i)
		markers = append(markers, mk)
		s.add("assistant", fmt.Sprintf("Established fact %s: %s.", mk, ft))
	}

	cur := s.msgs
	t := s.t
	var totalLat int64
	anyChanged := false
	for r := 0; r < opt.DriftRounds; r++ {
		fill, nt := fillerMsgs(t, opt.FillerTurns, opt.FillerCharsPerTurn)
		t = nt
		cur = append(cur, fill...)
		out, changed, lat, err := timedCompact(ctx, c, cur)
		if err != nil {
			return ProbeResult{}, err
		}
		totalLat += lat
		anyChanged = anyChanged || changed
		cur = out
	}
	survived, lost := survival(joinContent(cur), markers)
	return ProbeResult{
		Name: "drift-established-facts", Kind: KindDrift,
		Planted: len(markers), Survived: survived, Score: ratio(survived, len(markers)),
		Compacted: anyChanged, LatencyMS: totalLat / int64(maxInt(opt.DriftRounds, 1)),
		Lost: lost,
		Notes: fmt.Sprintf("fact-marker survival after %d sequential compactions; report drift_rate = 1 - score%s",
			opt.DriftRounds, noopSuffix(anyChanged)),
	}, nil
}

// --- synthetic session helpers ---

// session accumulates synthetic turns with strictly increasing timestamps.
type session struct {
	now  func() time.Time
	t    time.Time
	msgs []working.Message
}

func newSession(now func() time.Time) *session { return &session{now: now, t: now()} }

func (s *session) add(role, content string) {
	s.t = s.t.Add(time.Second)
	s.msgs = append(s.msgs, working.Message{Role: role, Content: content, Timestamp: s.t})
}

func (s *session) filler(n, chars int) {
	more, nt := fillerMsgs(s.t, n, chars)
	s.msgs = append(s.msgs, more...)
	s.t = nt
}

// fillerMsgs builds n user/assistant filler pairs (~chars each) with
// timestamps continuing from start. Returns the messages and the last stamp.
func fillerMsgs(start time.Time, n, chars int) ([]working.Message, time.Time) {
	body := fillerBody(chars)
	t := start
	out := make([]working.Message, 0, n*2)
	for i := 0; i < n; i++ {
		t = t.Add(time.Second)
		out = append(out, working.Message{Role: "user", Content: "Please continue with the next step. " + body, Timestamp: t})
		t = t.Add(time.Second)
		out = append(out, working.Message{Role: "assistant", Content: "Continuing the work as instructed. " + body, Timestamp: t})
	}
	return out, t
}

func fillerBody(chars int) string {
	const unit = "lorem ipsum dolor sit amet consectetur adipiscing elit "
	if chars < len(unit) {
		chars = len(unit)
	}
	return strings.Repeat(unit, chars/len(unit)+1)
}

// --- scoring helpers ---

func timedCompact(ctx context.Context, c Compactor, msgs []working.Message) (out []working.Message, changed bool, latencyMS int64, err error) {
	start := time.Now()
	res, ch, e := c.Compact(ctx, msgs)
	latencyMS = time.Since(start).Milliseconds()
	if e != nil {
		return nil, false, latencyMS, e
	}
	if !ch {
		// Fail-safe / below-threshold: the compactor left the set untouched.
		return msgs, false, latencyMS, nil
	}
	return res, true, latencyMS, nil
}

func joinContent(msgs []working.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

func countTokens(count TokenCounter, msgs []working.Message) int {
	total := 0
	for _, m := range msgs {
		total += count(m.Content)
	}
	return total
}

func survival(text string, markers []string) (survived int, lost []string) {
	for _, m := range markers {
		if strings.Contains(text, m) {
			survived++
		} else {
			lost = append(lost, m)
		}
	}
	return survived, lost
}

func marker(prefix string, i int) string { return fmt.Sprintf("%s-%04d", prefix, i) }

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const noopMsg = "compactor made NO change (session below threshold or summarizer no-op) — survival is trivial here; enlarge the session for a real signal"

func noopNote(changed bool) string {
	if changed {
		return ""
	}
	return noopMsg
}

func noopSuffix(changed bool) string {
	if changed {
		return ""
	}
	return " — NOTE: " + noopMsg
}

// --- output ---

// WriteJSON emits rep as indented JSON (stable external consumption).
func WriteJSON(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// WriteTerminal prints a compact human summary.
func WriteTerminal(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "compaction fidelity — recall %.0f%% · continuation %.0f%% · drift %.0f%% · tokens saved %.0f%% · avg %dms\n",
		rep.Recall*100, rep.Continuation*100, rep.DriftRate*100, rep.TokensSavedPct, rep.AvgLatencyMS)
	for _, p := range rep.Probes {
		flag := ""
		if !p.Compacted {
			flag = " [no-op]"
		}
		fmt.Fprintf(w, "  %-24s %-12s %d/%d survived (%.0f%%)%s\n",
			p.Name, p.Kind, p.Survived, p.Planted, p.Score*100, flag)
		if len(p.Lost) > 0 {
			fmt.Fprintf(w, "      lost: %s\n", strings.Join(p.Lost, ", "))
		}
	}
}
