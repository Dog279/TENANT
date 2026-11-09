package sandbox

import (
	"math/rand"
	"strings"
	"testing"
)

// headTailBuffer must render exactly what clip(whole, max) renders, for every
// cap and every way the bytes were chunked, while never holding more than the
// cap: the host, Seatbelt and Job Object backends keep their output format
// (TEN-438 / W1-4).
func TestHeadTailBuffer_MatchesClip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	alphabet := []byte("abcdefghijklmnopqrstuvwxyz0123456789\n")
	for _, max := range []int{0, 1, 2, 3, 7, 10, 64, 1000} {
		for _, total := range []int{0, 1, 5, 9, 10, 11, 30, 64, 65, 999, 1000, 1001, 5000} {
			whole := make([]byte, total)
			for i := range whole {
				whole[i] = alphabet[r.Intn(len(alphabet))]
			}
			for trial := 0; trial < 3; trial++ {
				b := &headTailBuffer{max: max}
				for off := 0; off < len(whole); {
					n := 1 + r.Intn(17)
					if off+n > len(whole) {
						n = len(whole) - off
					}
					b.Write(whole[off : off+n])
					off += n
				}
				want := clip(string(whole), max)
				if got := b.String(); got != want {
					t.Fatalf("max=%d total=%d: got %q want %q", max, total, got, want)
				}
				if max > 0 && (len(b.head)+len(b.tail)) > max {
					t.Fatalf("max=%d: retained %d bytes", max, len(b.head)+len(b.tail))
				}
				if b.Truncated() != (max > 0 && total > max) {
					t.Fatalf("max=%d total=%d: Truncated=%v", max, total, b.Truncated())
				}
			}
		}
	}
}

func TestHeadTailBuffer_OneHugeWrite(t *testing.T) {
	b := &headTailBuffer{max: 30}
	whole := strings.Repeat("x", 10) + strings.Repeat("y", 10000) + strings.Repeat("z", 10)
	b.Write([]byte(whole))
	if got, want := b.String(), clip(whole, 30); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The first byte over the cap fires onCap exactly once, so the container
// backend kills the producer at the moment the cap is hit, not after the run.
func TestCappedBuffer_OnCapFiresOnce(t *testing.T) {
	fired := 0
	b := &cappedBuffer{max: 5, onCap: func() { fired++ }}
	b.Write([]byte("abc"))
	if fired != 0 {
		t.Fatal("fired before the cap")
	}
	b.Write([]byte("defg"))
	b.Write([]byte("more"))
	b.Write([]byte("and more"))
	if fired != 1 {
		t.Fatalf("onCap fired %d times, want 1", fired)
	}
	if got := b.String(); !strings.HasPrefix(got, "abcde") || !strings.Contains(got, "truncated") {
		t.Fatalf("unexpected output %q", got)
	}
}
