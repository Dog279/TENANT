package sandbox

// output.go is the one place a backend's command output is bounded as it
// arrives (TEN-438, folded into W1-4). A command that floods stdout (`yes`,
// `cat /dev/urandom`) must never grow Tenant's memory until a deadline ends it:
// every backend's Run writes into one of the two writers here, which keep at
// most the cap in memory, and nothing buffers the whole stream first.
//
//   - cappedBuffer keeps the HEAD of the output and reports the first byte over
//     the cap. The container backend uses it: hitting the cap is a limit
//     violation and the container is killed at once (onCap).
//   - headTailBuffer keeps clip's shape (a third from the head, the rest from
//     the tail, where a command's errors and summary usually are) in bounded
//     memory. The host, Seatbelt and Job Object backends use it, so their output
//     format and truncation marker are exactly what they were when they
//     buffered everything and clipped afterwards.

import (
	"bytes"
	"strings"
	"sync"
)

// cappedBuffer keeps at most max bytes and records whether more arrived, so a
// run's output can be capped as it streams. max <= 0 means no cap. onCap, when
// set, is called once, from the Write that first crosses the cap, so the writer
// can stop the producer (the container backend kills the container).
type cappedBuffer struct {
	max   int
	onCap func()

	mu        sync.Mutex
	buf       strings.Builder
	total     int64
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.total += int64(len(p))
	fire := false
	switch {
	case c.max <= 0:
		c.buf.Write(p)
	default:
		if room := c.max - c.buf.Len(); room > 0 {
			if len(p) > room {
				c.buf.Write(p[:room])
				fire = !c.truncated
				c.truncated = true
			} else {
				c.buf.Write(p)
			}
		} else if len(p) > 0 {
			fire = !c.truncated
			c.truncated = true
		}
	}
	c.mu.Unlock()
	if fire && c.onCap != nil {
		c.onCap()
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return c.buf.String() + "\n…[truncated]"
	}
	return c.buf.String()
}

func (c *cappedBuffer) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// headTailBuffer is clip() applied as the bytes arrive: at most max bytes are
// ever retained — the first max/3 and a ring of the last max-max/3 — and
// String renders exactly what clip(whole, max) would have. max <= 0 keeps
// everything (no cap), as clip does.
type headTailBuffer struct {
	max int

	mu       sync.Mutex
	head     []byte // the first headCap bytes (everything, when max <= 0)
	tail     []byte // a ring of the last tailCap bytes written after the head
	tailPos  int    // next write position in the ring
	tailFull bool   // the ring has wrapped at least once
	total    int64
}

func (h *headTailBuffer) caps() (headCap, tailCap int) {
	headCap = h.max / 3
	return headCap, h.max - headCap
}

func (h *headTailBuffer) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(p)
	h.total += int64(n)
	if h.max <= 0 {
		h.head = append(h.head, p...)
		return n, nil
	}
	headCap, tailCap := h.caps()
	if room := headCap - len(h.head); room > 0 {
		take := min(room, len(p))
		h.head = append(h.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 || tailCap == 0 {
		return n, nil
	}
	if h.tail == nil {
		h.tail = make([]byte, tailCap)
	}
	if len(p) >= tailCap {
		copy(h.tail, p[len(p)-tailCap:])
		h.tailPos, h.tailFull = 0, true
		return n, nil
	}
	first := copy(h.tail[h.tailPos:], p)
	if first < len(p) {
		copy(h.tail, p[first:])
		h.tailFull = true
	}
	h.tailPos = (h.tailPos + len(p)) % tailCap
	if h.tailPos == 0 {
		h.tailFull = true
	}
	return n, nil
}

// tailBytes returns the ring's contents in write order.
func (h *headTailBuffer) tailBytes() []byte {
	if h.tail == nil {
		return nil
	}
	if !h.tailFull {
		return h.tail[:h.tailPos]
	}
	out := make([]byte, 0, len(h.tail))
	out = append(out, h.tail[h.tailPos:]...)
	return append(out, h.tail[:h.tailPos]...)
}

// String is clip(everything written, max): the whole output when it fit, else
// the head, the marker and the tail.
func (h *headTailBuffer) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.max <= 0 || h.total <= int64(h.max) {
		return string(h.head) + string(h.tailBytes())
	}
	return string(h.head) + "\n…[truncated]…\n" + string(h.tailBytes())
}

// Truncated reports whether more than max bytes arrived.
func (h *headTailBuffer) Truncated() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.max > 0 && h.total > int64(h.max)
}

// refusalWitness scans a run's WHOLE output stream, line by line, for the
// signatures attributeContainer keys on (EROFS, ENOENT, a resource cap, a cut
// network, "not found"). It is the deterministic source for attribution (W1-6).
//
// The display output is capped as it streams and keeps only the head, so
// whether a refusal line survives into Result.Output depends on where the
// kernel happened to merge it into the shared stdout+stderr pipe. The same
// command could therefore be attributed as refused on one run and clean on the
// next: a latent flap, reproduced on purpose by the live acceptance test. The
// witness sees every byte before the cap applies, keeps only the first matching
// LINE per kind and never the stream, so its verdict does not depend on the cap
// or on where a line fell.
//
// A refusal message is one short write(2) to stderr, atomic in the pipe, so
// line assembly is stable; the current line is bounded (witnessLineCap) so a
// newline-free flood cannot grow memory.
type refusalWitness struct {
	mu   sync.Mutex
	line []byte

	erofs, enoent, limit, netCut, notFound bool
	erofsLine, enoentLine                  string
}

// witnessLineCap bounds the in-progress line; a longer run is scanned in
// pieces. Refusal messages are a few hundred bytes at most.
const witnessLineCap = 16 << 10

func (w *refusalWitness) Write(p []byte) (int, error) {
	n := len(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.take(p)
			break
		}
		w.take(p[:i])
		w.evalLine()
		w.line = w.line[:0]
		p = p[i+1:]
	}
	return n, nil
}

// take appends to the current line, scanning and dropping what is there first
// if it would exceed the cap, so memory stays bounded on a newline-free flood.
func (w *refusalWitness) take(b []byte) {
	for len(b) > 0 {
		room := witnessLineCap - len(w.line)
		if room <= 0 {
			w.evalLine()
			w.line = w.line[:0]
			room = witnessLineCap
		}
		n := min(room, len(b))
		w.line = append(w.line, b[:n]...)
		b = b[n:]
	}
}

// flush scans the trailing, unterminated line once the stream has ended.
func (w *refusalWitness) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.evalLine()
	w.line = w.line[:0]
}

// evalLine tests the current line against every signature, recording the
// first matching line per kind in its original case, for path extraction.
func (w *refusalWitness) evalLine() {
	if len(w.line) == 0 {
		return
	}
	orig := string(w.line)
	low := strings.ToLower(orig)
	if !w.erofs && strings.Contains(low, "read-only file system") {
		w.erofs, w.erofsLine = true, orig
	}
	if !w.enoent && strings.Contains(low, "no such file or directory") {
		w.enoent, w.enoentLine = true, orig
	}
	if !w.limit && (strings.Contains(low, "out of memory") || strings.Contains(low, "cannot fork") ||
		strings.Contains(low, "resource temporarily unavailable")) {
		w.limit = true
	}
	if !w.notFound && strings.Contains(low, "not found") {
		w.notFound = true
	}
	if !w.netCut {
		for _, sig := range netCutSigns {
			if strings.Contains(low, sig) {
				w.netCut = true
				break
			}
		}
	}
}
