package imessage

import "testing"

// The invisible runes we strip, named for readable test cases. Defined via
// numeric (\u) escapes so the test source file itself never embeds a
// literal BOM (Go rejects a BOM mid-file) or an invisible-but-present
// character a reviewer can't see.
const (
	bom = "\uFEFF" // ZERO WIDTH NO-BREAK SPACE / byte-order mark
	zws = "\u200B" // ZERO WIDTH SPACE
	zwn = "\u200C" // ZERO WIDTH NON-JOINER
	zwj = "\u200D" // ZERO WIDTH JOINER
	wj  = "\u2060" // WORD JOINER
)

func TestSanitizeOutbound(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty string is safe", "", ""},
		{"plain text unchanged", "hello world", "hello world"},
		{"leading BOM stripped", bom + "hello", "hello"},
		{"trailing BOM stripped", "hello" + bom, "hello"},
		{"BOM both ends stripped", bom + "hello" + bom, "hello"},
		{"leading ZWSP stripped", zws + "hi", "hi"},
		{"trailing ZWSP stripped", "hi" + zws, "hi"},
		{"leading ZWNJ stripped", zwn + "hi", "hi"},
		{"leading ZWJ stripped", zwj + "hi", "hi"},
		{"leading word-joiner stripped", wj + "hi", "hi"},
		{"mixed run of edge zero-width stripped", bom + zws + zwj + "ok" + zwn + wj, "ok"},
		// Interior zero-width MUST be preserved: a ZWJ between two
		// codepoints is exactly how composed emoji are built; rewriting it
		// would corrupt the visible glyph.
		{"interior ZWJ preserved", "a" + zwj + "b", "a" + zwj + "b"},
		{"interior BOM preserved", "a" + bom + "b", "a" + bom + "b"},
		{"emoji ZWJ sequence preserved", "\U0001F468" + zwj + "\U0001F469", "\U0001F468" + zwj + "\U0001F469"},
		// Edges trimmed but a load-bearing interior ZWJ kept.
		{"edges trimmed interior kept", bom + "a" + zwj + "b" + bom, "a" + zwj + "b"},
		// Ordinary whitespace is NOT our concern -- it is visible content and
		// must survive (only the invisible send-drop triggers are removed).
		{"surrounding spaces preserved", "  hi  ", "  hi  "},
		{"only zero-width collapses to empty", bom + zws + wj, ""},
		{"newlines preserved", "line1\nline2", "line1\nline2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeOutbound(tt.in)
			if got != tt.want {
				t.Errorf("sanitizeOutbound(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsStripRune(t *testing.T) {
	strip := []rune{'\uFEFF', '\u200B', '\u200C', '\u200D', '\u2060'}
	for _, r := range strip {
		if !isStripRune(r) {
			t.Errorf("isStripRune(U+%04X) = false, want true", r)
		}
	}
	keep := []rune{'a', ' ', '\t', '\n', '0', '\U0001F600'}
	for _, r := range keep {
		if isStripRune(r) {
			t.Errorf("isStripRune(U+%04X) = true, want false", r)
		}
	}
}
