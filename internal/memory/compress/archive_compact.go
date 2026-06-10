package compress

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/memory/assemble"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// KindCompactionSummary marks the working.Message that replaces a run of older
// turns with a structured summary + a verbatim allowlist. Its Meta carries the
// source span (session + unix time bounds + message count) so the raw turns can
// be re-fetched from the archive — compaction is reversible/auditable (TEN-101).
const KindCompactionSummary = "compaction-summary"

// summaryInputBudgetChars bounds the transcript handed to the summarizer so a
// very long raw span can't blow the summarizer's own context. The verbatim
// allowlist is extracted from the FULL span regardless, so identifiers survive
// even when the prose view is truncated.
const summaryInputBudgetChars = 24000

// maxAllowlistItems caps the verbatim allowlist so it can't itself bloat.
const maxAllowlistItems = 48

// CompactWithArchive summarizes the older head of a session and returns
// [summary] + the protected recent tail. When reader != nil and sessionID != ""
// it summarizes the RAW archive span strictly before the tail — full fidelity,
// no fold-forward of a prior lossy summary (the drift fix). Otherwise it
// summarizes the in-memory head. Either way it appends a deterministic
// `## Verbatim` allowlist (identifiers, file paths, artifact URIs, error lines)
// the LLM cannot paraphrase away, and stamps the summary's Kind + Meta source
// tags. changed is false (msgs returned unchanged) when there's nothing worth
// compacting, the summarizer returns empty, or on any error — fail-safe.
func (c *Compressor) CompactWithArchive(ctx context.Context, msgs []working.Message, reader *archive.Reader, sessionID string) ([]working.Message, bool, error) {
	minMsgs := c.MinMessages
	if minMsgs <= 0 {
		minMsgs = 6
	}
	if len(msgs) < minMsgs {
		return msgs, false, nil
	}
	tailBudget := c.TailTokens
	if tailBudget <= 0 {
		tailBudget = defaultTailTokens
	}

	llm, _, err := c.Router.LLMForRole(ctx, c.role())
	if err != nil {
		return msgs, false, fmt.Errorf("compress: resolve summarizer: %w", err)
	}
	tok := func(s string) int {
		if n, cerr := llm.TokenCount(ctx, s); cerr == nil {
			return n
		}
		return len(s) / 4
	}

	// Protect the recent tail as WHOLE exchanges (turn boundaries), never
	// splitting a tool_use from its result, and never collapsing below a floor
	// of recent complete exchanges (TEN-169: stops the "45 → 2 messages" and
	// keeps the live task in-context). The token budget is an upper clamp only.
	tailStartIdx := chooseTailStart(msgs, tok, tailBudget, floorExchanges)
	head := msgs[:tailStartIdx]
	if len(head) < 2 {
		return msgs, false, nil
	}
	tail := msgs[tailStartIdx:]

	// Resolve the summarization source. allowText is the FULL source (untruncated)
	// for verbatim extraction; summaryText is the (possibly bounded) prose view
	// handed to the LLM.
	var (
		summaryText   string
		allowText     string
		srcCount      int
		srcAfterUnix  int64
		srcBeforeUnix int64
		origin        = "working"
	)
	tailStart := earliestStamp(tail)
	if reader != nil && sessionID != "" && !tailStart.IsZero() {
		events := collectSpan(reader, sessionID, tailStart)
		if len(events) > 0 {
			summaryText = renderArchiveSpan(events, summaryInputBudgetChars)
			allowText = joinArchiveContent(events)
			srcCount = len(events)
			srcAfterUnix = events[0].Timestamp.UTC().Unix()
			srcBeforeUnix = tailStart.UTC().Unix()
			origin = "archive"
		}
	}
	if origin == "working" {
		summaryText = renderTranscript(head)
		allowText = joinMessages(head)
		srcCount = len(head)
		if es := earliestStamp(head); !es.IsZero() {
			srcAfterUnix = es.UTC().Unix()
		}
		if !tailStart.IsZero() {
			srcBeforeUnix = tailStart.UTC().Unix()
		}
	}

	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: summaryText},
		},
		Temperature: 0.2,
		MaxTokens:   c.maxSummaryTokens(),
	})
	if err != nil {
		return msgs, false, fmt.Errorf("compress: summarize: %w", err)
	}
	summary := strings.TrimSpace(resp.Text)
	if summary == "" {
		return msgs, false, nil // nothing usable; leave the set intact
	}

	content := SummaryPrefix + "\n\n" + summary
	// Re-emit the most-recent user requests from the summarized head verbatim, so
	// "what the user asked" survives even if the LLM paraphrased it away (TEN-169).
	if ub := userRequestsBlock(head, verbatimUserTurns); ub != "" {
		content += "\n\n" + ub
	}
	if vb := verbatimBlock(extractAllowlist(allowText)); vb != "" {
		content += "\n\n" + vb
	}
	summaryMsg := working.Message{
		Role:      "user",
		Content:   content,
		Timestamp: time.Now().UTC(),
		Kind:      KindCompactionSummary,
		Meta: map[string]any{
			"source_session":   sessionID,
			"source_after":     srcAfterUnix,
			"source_before":    srcBeforeUnix,
			"source_msg_count": srcCount,
			"source_origin":    origin,
		},
	}
	out := make([]working.Message, 0, len(tail)+1)
	out = append(out, summaryMsg)
	out = append(out, tail...)
	return out, true, nil
}

// Tail-selection tuning (TEN-169).
const (
	// floorExchanges is the minimum number of recent COMPLETE exchanges kept
	// verbatim — the live task must never be summarized out from under the agent.
	floorExchanges = 3
	// defaultTailTokens is the upper clamp on the verbatim tail when TailTokens
	// is unset. Raised from the old flat 1500 so tool-heavy turns don't collapse
	// the tail to a single message.
	defaultTailTokens = 6000
	// verbatimUserTurns is how many recent user requests from the summarized head
	// are re-emitted verbatim in the summary.
	verbatimUserTurns = 4
)

// exchangeStartIndices returns the msg indices that begin a new exchange — user
// turns. A tail that begins at one of these never starts mid-tool-pair.
func exchangeStartIndices(msgs []working.Message) []int {
	var idx []int
	for i, m := range msgs {
		// A real user turn — NOT a prior compaction summary (also Role:"user"),
		// which belongs in the head to be re-digested, never treated as a tail
		// boundary (keeps the userRequestsBlock Kind filter consistent).
		if m.Role == "user" && m.Kind == "" {
			idx = append(idx, i)
		}
	}
	return idx
}

// chooseTailStart returns the index where the protected verbatim tail begins:
// always the last `floor` complete exchanges, plus older whole exchanges while
// they fit `budget`. With no clean turn boundary it keeps just the newest
// message (sanitizePairs at the assembler backstops validity).
func chooseTailStart(msgs []working.Message, tok func(string) int, budget, floor int) int {
	starts := exchangeStartIndices(msgs)
	if len(starts) == 0 {
		return len(msgs) - 1
	}
	// Keep at least `floor` recent exchanges, but never the whole set — at least
	// one exchange must remain in the head or there is nothing to summarize.
	minKeep := floor
	if minKeep > len(starts)-1 {
		minKeep = len(starts) - 1
	}
	if minKeep < 1 {
		minKeep = 1
	}
	chosen := starts[len(starts)-1]
	total := 0
	// b >= 1 guarantees starts[0]'s exchange stays in the head.
	for b := len(starts) - 1; b >= 1; b-- {
		start := starts[b]
		end := len(msgs)
		if b+1 < len(starts) {
			end = starts[b+1]
		}
		exTokens := 0
		for j := start; j < end; j++ {
			exTokens += tok(msgs[j].Content)
		}
		included := len(starts) - b
		if included > minKeep && total+exTokens > budget {
			break // past the floor and over the ceiling — stop extending the tail
		}
		total += exTokens
		chosen = start
	}
	return chosen
}

// userRequestsBlock re-emits the most-recent user turns from the summarized head
// verbatim (skipping prior compaction-summary messages), so the user's actual
// asks survive compaction.
func userRequestsBlock(head []working.Message, k int) string {
	var users []string
	for _, m := range head {
		if m.Role == "user" && m.Kind == "" {
			if s := strings.TrimSpace(m.Content); s != "" {
				users = append(users, s)
			}
		}
	}
	if len(users) == 0 {
		return ""
	}
	if len(users) > k {
		users = users[len(users)-k:]
	}
	var b strings.Builder
	b.WriteString("## User Requests (verbatim)\n")
	for _, u := range users {
		b.WriteString("- ")
		b.WriteString(truncate(u, 800))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// earliestStamp returns the smallest non-zero timestamp among msgs, or zero.
func earliestStamp(msgs []working.Message) time.Time {
	var min time.Time
	for _, m := range msgs {
		if m.Timestamp.IsZero() {
			continue
		}
		if min.IsZero() || m.Timestamp.Before(min) {
			min = m.Timestamp
		}
	}
	return min
}

// collectSpan reads this session's archive events strictly before `before`.
// Best-effort: a malformed/unreadable line is skipped, never fatal.
func collectSpan(reader *archive.Reader, sessionID string, before time.Time) []archive.Event {
	var out []archive.Event
	for ev, err := range reader.Stream(archive.Filter{SessionID: sessionID, Before: before}) {
		if err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// renderArchiveSpan flattens raw events into the summarizer's transcript, with
// full (bounded) tool results rather than the "(called tools: X)" collapse. If
// budgetChars > 0 the OLDEST events are dropped once the budget is hit (recency
// matters most for the prose; the allowlist preserves identifiers from the full
// span). budgetChars == 0 means unbounded.
func renderArchiveSpan(events []archive.Event, budgetChars int) string {
	var pieces []string
	used := 0
	for i := len(events) - 1; i >= 0; i-- { // newest-first so a budget keeps recent
		e := events[i]
		var b strings.Builder
		role := e.Role
		if role == "" {
			role = "user"
		}
		if s := strings.TrimSpace(e.Content); s != "" {
			fmt.Fprintf(&b, "%s: %s\n", role, truncate(s, 2000))
		}
		for _, tc := range e.ToolCalls {
			fmt.Fprintf(&b, "%s: (tool %s %s)\n", role, tc.Name, truncate(string(tc.Arguments), 300))
		}
		if e.ToolResult != nil {
			status := "ok"
			if e.ToolResult.IsError {
				status = "error"
			}
			fmt.Fprintf(&b, "tool[%s]: %s\n", status, truncate(e.ToolResult.Content, 1500))
		}
		piece := b.String()
		if budgetChars > 0 && used+len(piece) > budgetChars && len(pieces) > 0 {
			break
		}
		used += len(piece)
		pieces = append(pieces, piece)
	}
	var out strings.Builder
	for i := len(pieces) - 1; i >= 0; i-- { // back to chronological
		out.WriteString(pieces[i])
	}
	return strings.TrimRight(out.String(), "\n")
}

// joinArchiveContent concatenates ALL event content/args/results untruncated,
// for allowlist extraction (identifiers may hide in a long tool result).
func joinArchiveContent(events []archive.Event) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Content)
		b.WriteByte('\n')
		for _, tc := range e.ToolCalls {
			fmt.Fprintf(&b, "%s %s\n", tc.Name, string(tc.Arguments))
		}
		if e.ToolResult != nil {
			b.WriteString(e.ToolResult.Content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// joinMessages concatenates working-set message contents (+ tool-call descs) for
// allowlist extraction on the working-sourced fallback path.
func joinMessages(msgs []working.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "%s %s\n", tc.Name, string(tc.Arguments))
		}
	}
	return b.String()
}

// --- verbatim allowlist (deterministic salience: keep the heavy hitters) ---

var (
	allowIDRe   = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,}-[A-Z0-9]+\b`)              // TASK-0001, JIRA-12, NDL-0001
	allowPathRe = regexp.MustCompile(`(?:[\w.\-]+[\\/])+[\w.\-]+\.[A-Za-z0-9]{1,6}`) // foo/bar.go, dir\file.txt
	allowErrRe  = regexp.MustCompile(`(?i)\b(error|failed|panic|exception|denied|refused|timeout)\b`)
)

// extractAllowlist pulls high-signal, exact-match tokens out of text that the
// summarizer must not paraphrase: artifact URIs, identifiers, file paths, and
// error lines. Order-preserving, de-duplicated, capped at maxAllowlistItems.
func extractAllowlist(text string) []string {
	seen := make(map[string]bool)
	var out []string
	// add returns false once the cap is reached (caller should stop).
	add := func(s string) bool {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return true
		}
		seen[s] = true
		out = append(out, s)
		return len(out) < maxAllowlistItems
	}
	for _, u := range assemble.ExtractArtifactURIs(text) {
		if !add("artifact: " + u) {
			return out
		}
	}
	for _, m := range allowIDRe.FindAllString(text, -1) {
		if !add("id: " + m) {
			return out
		}
	}
	for _, m := range allowPathRe.FindAllString(text, -1) {
		if !add("path: " + m) {
			return out
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if allowErrRe.MatchString(line) {
			if !add("error: " + truncate(strings.TrimSpace(line), 200)) {
				return out
			}
		}
	}
	return out
}

// verbatimBlock renders the allowlist as a fenced section appended after the
// LLM summary — deterministic, so allowlisted values survive 100% regardless of
// what the summarizer prose drops.
func verbatimBlock(items []string) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Verbatim (exact values preserved from the compacted turns — do not paraphrase)\n")
	for _, it := range items {
		fmt.Fprintf(&b, "- %s\n", it)
	}
	return strings.TrimRight(b.String(), "\n")
}
