package imessage

import "strings"

// maxBubbleSize is the soft cap for a single outbound iMessage bubble.
// iMessage has no hard published character limit, but very long sends
// degrade (visual truncation, delivery quirks) so Hermes-style clients
// split long replies on paragraph boundaries. 1000 is a comfortable
// bubble that keeps whole paragraphs intact in the common case.
const maxBubbleSize = 1000

// chunkParagraphs splits text into a sequence of bubbles, each no longer
// than maxSize runes, preferring paragraph boundaries. It is pure-Go and
// tag-free so it compiles on every platform.
//
// Rules (in order):
//   - Split the input on blank-line paragraph boundaries ("\n\n").
//   - Coalesce small adjacent paragraphs back together while the running
//     bubble plus the next paragraph (joined by "\n\n") still fits.
//   - If a single paragraph is itself longer than maxSize, hard-split it
//     on WORD boundaries — never mid-word, never mid-rune. A single word
//     longer than maxSize is rune-split as a last resort (still never
//     mid-rune) so we always make progress.
//
// Content is never dropped or reordered: concatenating the returned
// chunks with the appropriate joiner ("\n\n" between paragraphs, " "
// between word-split fragments of one paragraph) reproduces the original
// semantic content. When the whole text fits, a single-element slice is
// returned. Empty/whitespace-only input returns the text unchanged as a
// single element so the caller's send still fires exactly once.
func chunkParagraphs(text string, maxSize int) []string {
	if maxSize <= 0 {
		maxSize = maxBubbleSize
	}
	// Fast path: fits in one bubble (rune count, not bytes).
	if runeLen(text) <= maxSize {
		return []string{text}
	}

	// Split on blank-line paragraph boundaries. strings.Split on "\n\n"
	// keeps the order and content; we rejoin with the same separator.
	paras := strings.Split(text, "\n\n")

	var chunks []string
	var cur strings.Builder
	curLen := 0

	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
			curLen = 0
		}
	}

	for _, p := range paras {
		pLen := runeLen(p)

		// A paragraph that cannot fit on its own must be hard-split on
		// word boundaries. Flush whatever is buffered first so order is
		// preserved, then emit the word-split pieces directly.
		if pLen > maxSize {
			flush()
			for _, piece := range splitWords(p, maxSize) {
				chunks = append(chunks, piece)
			}
			continue
		}

		// Would appending this paragraph (with a "\n\n" joiner) overflow
		// the current bubble? If so, start a new bubble.
		joinLen := 0
		if curLen > 0 {
			joinLen = 2 // len("\n\n") in runes
		}
		if curLen > 0 && curLen+joinLen+pLen > maxSize {
			flush()
			joinLen = 0
		}
		if curLen > 0 {
			cur.WriteString("\n\n")
			curLen += 2
		}
		cur.WriteString(p)
		curLen += pLen
	}
	flush()

	if len(chunks) == 0 {
		// Only reachable if the input was all separators; return as-is so
		// the caller still sends exactly once.
		return []string{text}
	}
	return chunks
}

// splitWords hard-splits a single oversized paragraph on whitespace word
// boundaries, packing as many words as fit (joined by single spaces) into
// each piece without exceeding maxSize runes. A lone word longer than
// maxSize is rune-split (never mid-rune) as a last resort.
func splitWords(p string, maxSize int) []string {
	words := strings.Fields(p)
	if len(words) == 0 {
		return []string{p}
	}
	var out []string
	var cur strings.Builder
	curLen := 0

	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
			curLen = 0
		}
	}

	for _, w := range words {
		wLen := runeLen(w)
		if wLen > maxSize {
			// A single word longer than a whole bubble. Flush the buffer,
			// then rune-split the word so we never split mid-rune.
			flush()
			for _, frag := range splitRunes(w, maxSize) {
				out = append(out, frag)
			}
			continue
		}
		joinLen := 0
		if curLen > 0 {
			joinLen = 1 // single space
		}
		if curLen > 0 && curLen+joinLen+wLen > maxSize {
			flush()
			joinLen = 0
		}
		if curLen > 0 {
			cur.WriteString(" ")
			curLen++
		}
		cur.WriteString(w)
		curLen += wLen
	}
	flush()
	return out
}

// splitRunes splits s into pieces of at most maxSize runes, never
// breaking a multi-byte rune. Last-resort fallback for a single word
// longer than a whole bubble.
func splitRunes(s string, maxSize int) []string {
	rs := []rune(s)
	if len(rs) <= maxSize {
		return []string{s}
	}
	var out []string
	for i := 0; i < len(rs); i += maxSize {
		end := i + maxSize
		if end > len(rs) {
			end = len(rs)
		}
		out = append(out, string(rs[i:end]))
	}
	return out
}

// runeLen counts runes (visible characters), not bytes, so multi-byte
// (emoji/CJK) text is measured the way a bubble limit should be.
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
