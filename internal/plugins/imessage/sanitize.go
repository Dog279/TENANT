package imessage

import "strings"

// Dedup layer 3 (outbound): strip leading/trailing BOM and zero-width
// characters from text we are about to send.
//
// Messages.app silently DROPS a send when the body begins (or, observed
// less often, ends) with an invisible byte such as a UTF-8 BOM (U+FEFF) or
// a zero-width space/joiner. The text "looks" non-empty to us but the send
// never lands and never errors -- a Hermes-class send-drop. LLM output and
// clipboard round-trips are common sources of a stray leading U+FEFF.
//
// We strip ONLY at the edges. A zero-width joiner in the INTERIOR is
// load-bearing -- it is exactly how multi-codepoint emoji (e.g. family or
// profession sequences) and some scripts are composed, so rewriting
// interior content would corrupt legitimate messages. Trimming the leading
// and trailing runs is sufficient to clear the send-drop trigger while
// leaving the visible/meaningful body byte-for-byte intact.
//
// This is a pure function with no build tag so it compiles (and is tested)
// on every platform; only the darwin/BlueBubbles send call sites that
// invoke it are platform-specific.
func sanitizeOutbound(text string) string {
	return strings.TrimFunc(text, isStripRune)
}

// isStripRune reports whether r is an invisible formatting rune we strip
// from the edges of outbound text:
//
//	U+FEFF  ZERO WIDTH NO-BREAK SPACE / byte-order mark (the prime offender)
//	U+200B  ZERO WIDTH SPACE
//	U+200C  ZERO WIDTH NON-JOINER
//	U+200D  ZERO WIDTH JOINER
//	U+2060  WORD JOINER (the invisible-glue sibling of U+FEFF)
//
// Ordinary whitespace (spaces, tabs, newlines) is intentionally NOT
// stripped -- trimming it would change the user-visible message; only the
// invisible send-drop triggers are removed. The runes are written as
// numeric escapes (not literals) so this source file never embeds an
// invisible byte -- Go rejects a literal BOM mid-file, and an unseen
// literal would be unreviewable.
func isStripRune(r rune) bool {
	switch r {
	case '\uFEFF', // ZERO WIDTH NO-BREAK SPACE / byte-order mark
		'\u200B', // ZERO WIDTH SPACE
		'\u200C', // ZERO WIDTH NON-JOINER
		'\u200D', // ZERO WIDTH JOINER
		'\u2060': // WORD JOINER
		return true
	default:
		return false
	}
}
