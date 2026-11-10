package control

import (
	"fmt"
	"strings"
	"unicode"
)

// VisibleText shows what a request really says: invisible characters
// (HiddenRune) render as "⟨U+XXXX⟩" so text can't hide or reorder itself,
// and a run of 3 or more blank lines collapses to one "⟨N blank lines⟩" line
// so it can't push the rest out of view. Every surface that asks a person to
// decide on text it did not write (an approval's detail, a command) shows it
// through this, so the dashboard and the terminal agree on what is hidden.
func VisibleText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		if HiddenRune(r) {
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
			continue
		}
		b.WriteRune(r)
	}
	lines := strings.Split(b.String(), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		switch n := j - i; {
		case n >= 3:
			out = append(out, fmt.Sprintf("⟨%d blank lines⟩", n))
			i = j
		case n > 0:
			out = append(out, lines[i:j]...)
			i = j
		default:
			out = append(out, lines[i])
			i++
		}
	}
	return strings.Join(out, "\n")
}

// HiddenRune reports whether r draws nothing, or moves other text, where a
// person reads it: controls, format characters (bidi, zero-width, the tag
// characters that spell text a model reads and a person can't), the other
// default-ignorables (Hangul fillers, U+034F) and variation selectors (which
// can carry bytes too), plus the line and paragraph separators.
func HiddenRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r == '\u2028' || r == '\u2029':
		return true
	}
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector)
}
