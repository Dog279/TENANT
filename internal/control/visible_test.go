package control

import "testing"

// The dashboard's own test (TestVisibleText there) covers the table of
// cases; this pins the rule both surfaces share where it lives.
func TestVisibleText(t *testing.T) {
	for in, want := range map[string]string{
		"first words\rsecond words": "first words⟨U+000D⟩second words",
		"bell\x07\x1b[2J":           "bell⟨U+0007⟩⟨U+001B⟩[2J",
		"abc\u202Edef":              "abc⟨U+202E⟩def",
		"a\r\nb":                    "a\nb",
		"top\n\n\n\nhidden":         "top\n⟨3 blank lines⟩\nhidden",
		"tabs\tand 中文 stay":         "tabs\tand 中文 stay",
		"ok\U000E0041 tag letter":   "ok⟨U+E0041⟩ tag letter",
	} {
		if got := VisibleText(in); got != want {
			t.Errorf("VisibleText(%q) = %q, want %q", in, got, want)
		}
	}
	for r, hidden := range map[rune]bool{'\n': false, '\t': false, 'a': false, '\r': true, '\x1b': true, '\u2028': true, '\u200D': true, '\uFE0F': true} {
		if HiddenRune(r) != hidden {
			t.Errorf("HiddenRune(U+%04X) = %v, want %v", r, !hidden, hidden)
		}
	}
}
