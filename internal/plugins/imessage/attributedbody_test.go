package imessage

import (
	"strings"
	"testing"
)

func TestDecodeAttributedBody(t *testing.T) {
	t.Run("ascii", func(t *testing.T) {
		if got := decodeAttributedBody(mkAttrBody([]byte("Hello"))); got != "Hello" {
			t.Errorf("ascii: got %q", got)
		}
	})

	t.Run("emoji_utf8", func(t *testing.T) {
		// Multi-byte UTF-8 at the boundary is exactly what the naive
		// byte-grab heuristic corrupts; the varint-aware parser must not.
		want := "wave 👋 done"
		if got := decodeAttributedBody(mkAttrBody([]byte(want))); got != want {
			t.Errorf("emoji: got %q want %q", got, want)
		}
	})

	t.Run("long_2byte_length", func(t *testing.T) {
		// length >= 128 forces the 0x81 two-byte varint path.
		want := strings.Repeat("a", 200)
		if got := decodeAttributedBody(mkAttrBody([]byte(want))); got != want {
			t.Errorf("long: len(got)=%d want %d", len(got), len(want))
		}
	})

	t.Run("utf16_fallback", func(t *testing.T) {
		// Bytes invalid as UTF-8 but valid UTF-16LE must decode via fallback.
		want := "Héllo"
		if got := decodeAttributedBody(mkAttrBody(utf16leBytes(want))); got != want {
			t.Errorf("utf16: got %q want %q", got, want)
		}
	})

	t.Run("garbage_falls_back", func(t *testing.T) {
		if got := decodeAttributedBody([]byte{0x00, 0x01, 0x02, 0x03}); got != unsupportedMessage {
			t.Errorf("garbage: got %q want %q", got, unsupportedMessage)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := decodeAttributedBody(nil); got != unsupportedMessage {
			t.Errorf("empty: got %q", got)
		}
	})
}

func TestReadVarLen(t *testing.T) {
	cases := []struct {
		in   []byte
		val  int
		next int
		ok   bool
	}{
		{[]byte{0x05}, 5, 1, true},
		{[]byte{0x7f}, 127, 1, true},
		{[]byte{0x81, 0xc8, 0x00}, 200, 3, true},
		{[]byte{0x82, 0x01, 0x00, 0x00, 0x00}, 1, 5, true},
		{[]byte{0x81}, 0, 0, false}, // truncated
		{[]byte{}, 0, 0, false},     // empty
	}
	for i, c := range cases {
		val, next, ok := readVarLen(c.in, 0)
		if ok != c.ok || (ok && (val != c.val || next != c.next)) {
			t.Errorf("case %d readVarLen(%v) = (%d,%d,%v) want (%d,%d,%v)",
				i, c.in, val, next, ok, c.val, c.next, c.ok)
		}
	}
}
