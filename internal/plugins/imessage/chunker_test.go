package imessage

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Content-loss proofs come in two flavors below. For paragraph-only
// inputs (each paragraph fits) we round-trip exactly by rejoining chunks
// with "\n\n". For word-split / coalesce inputs we can't know which
// joiner produced an adjacent pair, so assertNoWordLoss compares the
// whitespace-normalized word multiset instead. For rune-split (a single
// oversized word) we concatenate the pieces and compare byte-for-byte.

func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func TestChunkParagraphs_Table(t *testing.T) {
	const max = 100
	cases := []struct {
		name      string
		in        string
		max       int
		wantN     int // exact chunk count, 0 = don't assert
		wantAtLst int // minimum chunk count, 0 = don't assert
	}{
		{name: "empty", in: "", max: max, wantN: 1},
		{name: "whitespace only", in: "   \n  ", max: max, wantN: 1},
		{name: "single short paragraph", in: "hello there friend", max: max, wantN: 1},
		{name: "exactly at max", in: strings.Repeat("a", max), max: max, wantN: 1},
		{name: "two small paras coalesce", in: "first para\n\nsecond para", max: max, wantN: 1},
		{
			name:  "two big paras split",
			in:    strings.Repeat("a", 80) + "\n\n" + strings.Repeat("b", 80),
			max:   max,
			wantN: 2,
		},
		{name: "default max when zero", in: "short", max: 0, wantN: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkParagraphs(tc.in, tc.max)
			if len(got) == 0 {
				t.Fatalf("returned no chunks")
			}
			if tc.wantN != 0 && len(got) != tc.wantN {
				t.Fatalf("chunk count = %d, want %d: %q", len(got), tc.wantN, got)
			}
			if tc.wantAtLst != 0 && len(got) < tc.wantAtLst {
				t.Fatalf("chunk count = %d, want >= %d", len(got), tc.wantAtLst)
			}
			// No chunk may exceed max runes (when max>0 and a single word
			// did not force an oversize, which these cases avoid).
			limit := tc.max
			if limit <= 0 {
				limit = maxBubbleSize
			}
			for i, c := range got {
				if n := runeCount(c); n > limit {
					t.Errorf("chunk %d is %d runes > max %d: %q", i, n, limit, c)
				}
			}
		})
	}
}

// A 2000+ char multi-paragraph input must yield a clean multi-bubble
// output: every bubble within max, more than one bubble, no content lost.
func TestChunkParagraphs_LargeMultiParagraph(t *testing.T) {
	const max = 500
	var b strings.Builder
	for i := 0; i < 12; i++ {
		// ~180-char paragraphs of whole words.
		b.WriteString(strings.TrimSpace(strings.Repeat("word ", 36)))
		if i < 11 {
			b.WriteString("\n\n")
		}
	}
	in := b.String()
	if runeCount(in) < 2000 {
		t.Fatalf("test input too small: %d", runeCount(in))
	}
	got := chunkParagraphs(in, max)
	if len(got) < 2 {
		t.Fatalf("expected multiple bubbles, got %d", len(got))
	}
	for i, c := range got {
		if n := runeCount(c); n > max {
			t.Errorf("bubble %d = %d runes > max %d", i, n, max)
		}
	}
	assertNoWordLoss(t, in, got)
}

// A single paragraph longer than max must be word-split: no chunk over
// max, no mid-word break, no content loss.
func TestChunkParagraphs_OversizeSingleParagraph(t *testing.T) {
	const max = 60
	words := []string{}
	for i := 0; i < 40; i++ {
		words = append(words, "alpha")
	}
	in := strings.Join(words, " ") // one paragraph, no blank lines, ~240 chars
	got := chunkParagraphs(in, max)
	if len(got) < 2 {
		t.Fatalf("expected word-split into multiple chunks, got %d", len(got))
	}
	for i, c := range got {
		if n := runeCount(c); n > max {
			t.Errorf("chunk %d = %d runes > max %d: %q", i, n, max, c)
		}
		// Proof of no mid-word break: every space-separated token in every
		// chunk is a complete original word ("alpha").
		for _, tok := range strings.Fields(c) {
			if tok != "alpha" {
				t.Errorf("chunk %d has a broken word token %q", i, tok)
			}
		}
	}
	assertNoWordLoss(t, in, got)
}

// A single word longer than a whole bubble is the last-resort case: it is
// rune-split, never mid-rune, and the pieces reconstruct the word.
func TestChunkParagraphs_SingleHugeWord(t *testing.T) {
	const max = 10
	in := strings.Repeat("z", 35) // 35 'z', no spaces
	got := chunkParagraphs(in, max)
	if len(got) < 4 {
		t.Fatalf("expected rune-split into >=4 pieces, got %d: %q", len(got), got)
	}
	var joined strings.Builder
	for i, c := range got {
		if n := runeCount(c); n > max {
			t.Errorf("piece %d = %d runes > max %d", i, n, max)
		}
		if !utf8.ValidString(c) {
			t.Errorf("piece %d is not valid UTF-8: %q", i, c)
		}
		joined.WriteString(c)
	}
	if joined.String() != in {
		t.Errorf("rune-split lost content: got %q want %q", joined.String(), in)
	}
}

// Unicode safety: chunking a long run of multi-byte runes must never
// split mid-rune (every chunk stays valid UTF-8) and must lose nothing.
func TestChunkParagraphs_UnicodeNoMidRune(t *testing.T) {
	const max = 20
	// Mix emoji (4-byte) and CJK (3-byte) so byte- and rune-length diverge.
	one := "🐈漢字🚀"                 // 4 runes, 14 bytes
	in := strings.Repeat(one, 30) // single long "word", 120 runes, no spaces
	got := chunkParagraphs(in, max)
	if len(got) < 2 {
		t.Fatalf("expected multi-piece split, got %d", len(got))
	}
	var joined strings.Builder
	for i, c := range got {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d split mid-rune (invalid UTF-8): %q", i, c)
		}
		if n := runeCount(c); n > max {
			t.Errorf("chunk %d = %d runes > max %d", i, n, max)
		}
		joined.WriteString(c)
	}
	if joined.String() != in {
		t.Errorf("unicode content lost: got %d runes want %d", runeCount(joined.String()), runeCount(in))
	}
}

// Multi-paragraph unicode: bubbles join with "\n\n"; verify exact
// round-trip when paragraphs each fit.
func TestChunkParagraphs_MultiParaUnicodeRoundTrip(t *testing.T) {
	const max = 40
	p1 := "héllo 世界 🌍"
	p2 := "deuxième paragraphe café ☕"
	p3 := "троичный абзац текст"
	in := p1 + "\n\n" + p2 + "\n\n" + p3
	got := chunkParagraphs(in, max)
	// Each paragraph fits and they may coalesce; rejoining with "\n\n"
	// must reproduce the input exactly (no word-splitting happened).
	if rejoined := strings.Join(got, "\n\n"); rejoined != in {
		t.Errorf("paragraph round-trip mismatch:\n got %q\nwant %q", rejoined, in)
	}
}

// assertNoWordLoss proves no content was dropped or duplicated for the
// word-split / coalesce cases: the multiset of word tokens across all
// chunks equals the multiset of word tokens in the input. Robust to the
// joiner ambiguity (space vs "\n\n") because Fields ignores all
// whitespace runs.
func assertNoWordLoss(t *testing.T, in string, chunks []string) {
	t.Helper()
	want := map[string]int{}
	for _, w := range strings.Fields(in) {
		want[w]++
	}
	got := map[string]int{}
	for _, c := range chunks {
		for _, w := range strings.Fields(c) {
			got[w]++
		}
	}
	if len(want) != len(got) {
		t.Fatalf("distinct-word count differs: in=%d chunks=%d", len(want), len(got))
	}
	for w, n := range want {
		if got[w] != n {
			t.Errorf("word %q count: in=%d chunks=%d", w, n, got[w])
		}
	}
}
