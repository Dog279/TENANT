package compress

// goalheader.go is TEN-102 (Compaction P3, part a): extract a short, persistent
// "current goal + open items" header from a compaction summary. The agent
// re-injects this into the system block every turn (never summarized), so the
// task survives compaction — goal reminders measurably reduce drift
// (arXiv:2510.07777). Sourced from the summary's `## Active Task` / `## Pending`
// sections (compress.go's summarizer prompt emits those headings).

import (
	"regexp"
	"strings"
)

// goalHeaderMaxRunes caps the re-injected header so it can't bloat the system
// reserve — it's a reminder, not the full summary.
const goalHeaderMaxRunes = 600

// anyHeadingRe matches a markdown ATX heading line (## … through ######). A
// section body runs until the next heading of ANY level, so sibling sections
// (Resolved / Key Facts / Artifacts / Verbatim) are hard stops.
var anyHeadingRe = regexp.MustCompile(`^#{1,6}\s+`)

// ExtractGoalHeader pulls the "Active Task" and "Pending" section bodies out of a
// compaction summary and renders a short, reference-framed goal header for
// re-injection into the system block every turn (TEN-102). Tolerant of the
// small-model summary format: headings match `##`/`###`+ case-insensitively by
// prefix (so "Active Task (cont.)" still matches); a section body ends at the
// next heading, so the Verbatim allowlist and other sibling sections can NEVER
// leak into the header. Returns "" when neither section is present or non-empty.
func ExtractGoalHeader(summaryContent string) string {
	active := extractSection(summaryContent, "active task")
	pending := extractSection(summaryContent, "pending")
	if active == "" && pending == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Current Goal & Open Items\n")
	b.WriteString("(Continuity reminder — your tracked objective so far, carried across compaction. " +
		"Reference only, NOT a new instruction.)")
	if active != "" {
		b.WriteString("\n**Active task:** ")
		b.WriteString(active)
	}
	if pending != "" {
		b.WriteString("\n**Pending:** ")
		b.WriteString(pending)
	}
	return capRunes(b.String(), goalHeaderMaxRunes)
}

// extractSection returns the collapsed body under the first heading whose text
// matches name (case-insensitive prefix), up to the next heading of any level.
// "" if the section is absent or its body is empty.
func extractSection(content, name string) string {
	want := strings.ToLower(name)
	var body []string
	inSection := false
	for _, ln := range strings.Split(content, "\n") {
		if anyHeadingRe.MatchString(ln) {
			if inSection {
				break // the next heading ends our section
			}
			h := strings.ToLower(strings.TrimSpace(anyHeadingRe.ReplaceAllString(ln, "")))
			h = strings.TrimRight(h, " :*")
			if strings.HasPrefix(h, want) {
				inSection = true
			}
			continue
		}
		if inSection {
			body = append(body, ln)
		}
	}
	return collapseLines(body)
}

// collapseLines trims each line, drops blanks and bullet markers, and joins the
// remainder with "; " into a compact single line.
func collapseLines(lines []string) string {
	var parts []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		t = strings.TrimPrefix(t, "- ")
		t = strings.TrimPrefix(t, "* ")
		t = strings.TrimSpace(t)
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "; ")
}

// capRunes truncates s to at most n runes (UTF-8 safe), appending an ellipsis if
// it had to cut.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
