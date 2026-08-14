// Package mdimport turns operator-authored markdown notes (Hermes-style
// MEMORY.md / USER.md / FOUNDER_PROFILE.md) into a flat list of atomic
// claims suitable for insertion as T3 semantic facts.
//
// It is a deliberately small, stdlib-only "markdown-ish" extractor — NOT a
// CommonMark parser. The job is narrow: pull the human-meaningful sentences
// out of a bulleted index + prose document and drop the scaffolding
// (headings, code fences, blank lines, table-of-contents/index decoration).
// Each surviving bullet or non-heading paragraph line becomes one claim with
// its inline markdown (bold, links, inline code, leading bullet markers)
// stripped down to plain text.
package mdimport

import (
	"regexp"
	"strings"
)

// Claim is one extracted atomic claim plus its normalized form. Text is the
// cleaned, human-readable claim (what gets embedded + stored as the fact).
// Norm is the case-folded, whitespace-collapsed key used ONLY for
// duplicate detection (idempotent re-import) — never stored.
type Claim struct {
	Text string
	Norm string
}

var (
	// bullet markers: -, *, + or "1." / "1)" ordered-list markers, with any
	// leading indentation (nested bullets included).
	reBullet = regexp.MustCompile(`^[ \t]*(?:[-*+]|\d+[.)])\s+`)
	// markdown links [text](url) -> text ; images ![alt](url) -> alt
	reLink = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	// bare autolinks <https://...> -> https://...
	reAutoLink = regexp.MustCompile(`<((?:https?|mailto):[^>]+)>`)
	// emphasis / bold / strikethrough markers around a run of text.
	reBoldDouble   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reBoldUnders   = regexp.MustCompile(`__([^_]+)__`)
	reItalicStar   = regexp.MustCompile(`\*([^*]+)\*`)
	reStrike       = regexp.MustCompile(`~~([^~]+)~~`)
	reInlineCode   = regexp.MustCompile("`([^`]+)`")
	reMultiSpace   = regexp.MustCompile(`\s+`)
	reHeadingHash  = regexp.MustCompile(`^[ \t]*#{1,6}\s`)
	reSetextRule   = regexp.MustCompile(`^[ \t]*(?:[-=]{3,}|\*{3,}|_{3,})[ \t]*$`)
	reBlockquote   = regexp.MustCompile(`^[ \t]*>\s?`)
	reTableDivider = regexp.MustCompile(`^[ \t]*\|?[ \t:-]+\|[ \t:|-]*$`)
	// a pipe-delimited table row (header or body) conventionally starts with
	// a leading "|"; skipped as table scaffolding. Prose with an inline pipe
	// does NOT start with one, so it is unaffected.
	reTableRow = regexp.MustCompile(`^[ \t]*\|`)
)

// Parse extracts claims from markdown source. The rules (see package doc):
//
//   - Fenced code blocks (``` or ~~~) are skipped wholesale.
//   - ATX headings (# .. ######) and setext rules / horizontal rules are skipped.
//   - Blank lines are skipped.
//   - Each bullet / ordered-list item becomes one claim (the marker is stripped).
//   - Each remaining non-heading text line (prose paragraph line) becomes one claim.
//   - Inline markdown (bold/italic/strike/inline-code/links) is reduced to its text.
//   - A claim shorter than minClaimLen runes after cleaning is dropped as noise
//     (e.g. a lone ">" or a stray punctuation line).
//
// Within a single Parse call, claims with the same normalized form are
// de-duplicated (first occurrence wins) so a document that repeats a line does
// not yield two identical facts.
func Parse(src string) []Claim {
	const minClaimLen = 4

	var out []Claim
	seen := map[string]struct{}{}
	inFence := false
	var fenceMarker string

	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		// Fenced code blocks: toggle on the opening fence, skip everything
		// (including the closing fence line) until it closes.
		if fence := fenceMarkerOf(trimmed); fence != "" {
			if !inFence {
				inFence = true
				fenceMarker = fence
				continue
			}
			// Inside a fence: only the SAME marker type closes it.
			if strings.HasPrefix(trimmed, fenceMarker) {
				inFence = false
				fenceMarker = ""
			}
			continue
		}
		if inFence {
			continue
		}

		if trimmed == "" {
			continue
		}
		if reHeadingHash.MatchString(line) {
			continue
		}
		if reSetextRule.MatchString(line) {
			continue // horizontal rule / underline
		}
		if reTableDivider.MatchString(line) {
			continue // markdown table separator row (|---|---|)
		}
		if reTableRow.MatchString(line) {
			continue // markdown table row (| header | header |)
		}

		// Strip a leading bullet / ordered-list / blockquote marker, then
		// clean inline markdown down to plain text.
		body := line
		body = reBullet.ReplaceAllString(body, "")
		body = reBlockquote.ReplaceAllString(body, "")
		text := Clean(body)
		if len([]rune(text)) < minClaimLen {
			continue
		}

		norm := normalize(text)
		if norm == "" {
			continue
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, Claim{Text: text, Norm: norm})
	}
	return out
}

// Clean reduces a single line of inline markdown to plain text: links/images
// to their visible text, bold/italic/strike/inline-code markers removed,
// whitespace collapsed. Exported so the importer can clean a one-off string
// and so it can be unit-tested directly.
func Clean(s string) string {
	s = reAutoLink.ReplaceAllString(s, "$1")
	s = reLink.ReplaceAllString(s, "$1")
	s = reInlineCode.ReplaceAllString(s, "$1")
	s = reBoldDouble.ReplaceAllString(s, "$1")
	s = reBoldUnders.ReplaceAllString(s, "$1")
	s = reStrike.ReplaceAllString(s, "$1")
	// Italic last, so it doesn't eat half of an unmatched bold run.
	s = reItalicStar.ReplaceAllString(s, "$1")
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// normalize is the duplicate-detection key: case-folded, whitespace-collapsed.
// Two claims that differ only by case or runs of whitespace collide here, so
// re-importing the same file inserts nothing new.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = reMultiSpace.ReplaceAllString(s, " ")
	return s
}

// Normalize exposes the duplicate key so the store-facing importer can build
// the same key from an already-stored fact's text.
func Normalize(s string) string { return normalize(s) }

// fenceMarkerOf returns "```" or "~~~" if the (already TrimSpace'd) line opens
// or closes a code fence, else "". A fence is 3+ of the same character.
func fenceMarkerOf(trimmed string) string {
	switch {
	case strings.HasPrefix(trimmed, "```"):
		return "```"
	case strings.HasPrefix(trimmed, "~~~"):
		return "~~~"
	}
	return ""
}
