package secrets

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// The Linux and Windows test files set these two. They are nil where this
// process cannot ask the operating system (macOS).
var (
	// osLocked reports whether the operating system has the page at addr
	// locked.
	osLocked func(addr uintptr) bool
	// osWatch is given a mapping that is about to be handed back. The function
	// it returns reports, afterwards, whether the operating system still has
	// that mapping.
	osWatch func(mem []byte) (stillMapped func() bool)
)

func addrOf(p []byte) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(p))) }

func allZero(p []byte) bool { return len(bytes.Trim(p, "\x00")) == 0 }

// watchFree checks every mapping a buffer gives back while the test runs: its
// bytes are all zero at the moment it is unmapped, the unmap succeeds, and the
// operating system no longer has it afterwards. It returns the count of
// mappings given back.
func watchFree(t *testing.T) *atomic.Int64 {
	t.Helper()
	var freed atomic.Int64
	real := freePages
	t.Cleanup(func() { freePages = real })
	freePages = func(mem []byte, locked bool) error {
		if !allZero(mem) {
			t.Errorf("a %d-byte mapping was not zeroed before it was unmapped", len(mem))
		}
		addr, stillMapped := addrOf(mem), func() bool { return false }
		if osWatch != nil {
			stillMapped = osWatch(mem)
		}
		err := real(mem, locked)
		if err != nil {
			t.Errorf("unmap: %v", err)
		}
		if stillMapped() {
			t.Errorf("the mapping at %#x is still there after Release", addr)
		}
		freed.Add(1)
		return err
	}
	return &freed
}

// refusedLock is the part of a refused-lock test that is the same on every
// system: a buffer made from want while the operating system refuses the lock
// says so, with the call that failed first in the reason, and holds and takes
// bytes like any other. The caller releases it.
func refusedLock(t *testing.T, want []byte, call string) (b *LockedBuffer, reason string) {
	t.Helper()
	watchFree(t)
	b, err := NewLockedBuffer(bytes.Clone(want))
	if err != nil {
		t.Fatalf("a refused lock stopped the caller: %v", err)
	}
	locked, reason := b.Locked()
	if locked || !strings.HasPrefix(reason, call+": ") {
		t.Fatalf("Locked() = %v, %q; want false and the %s error", locked, reason, call)
	}
	last := len(want) - 1
	if err := b.With(func(p []byte) error {
		if osLocked(addrOf(p)) {
			t.Error("the operating system reports the page as locked")
		}
		if !bytes.Equal(p, want) {
			t.Error("the bytes read back wrong")
		}
		p[last] ^= 0xff
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b.With(func(p []byte) error {
		if p[last] != want[last]^0xff {
			t.Error("a write through With was lost")
		}
		return nil
	})
	return b, reason
}

func TestLockedBufferHoldsBytesInLockedPages(t *testing.T) {
	freed := watchFree(t)
	src := []byte("the fleet key, 32 bytes long ...")
	want := bytes.Clone(src)
	b, err := NewLockedBuffer(src)
	if err != nil {
		t.Fatal(err)
	}
	if !allZero(src) {
		t.Fatalf("the source bytes were left in place: %q", src)
	}
	if locked, reason := b.Locked(); !locked || reason != "" {
		t.Fatalf("Locked() = %v, %q; want true and no reason", locked, reason)
	}
	var addr uintptr
	if err := b.With(func(p []byte) error {
		addr = addrOf(p)
		if !bytes.Equal(p, want) || cap(p) != len(want) {
			t.Errorf("With saw %q (cap %d), want %q", p, cap(p), want)
		}
		p[0] = 'T'
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if addr%uintptr(os.Getpagesize()) != 0 {
		t.Errorf("the bytes at %#x do not start a page of their own", addr)
	}
	if osLocked != nil && !osLocked(addr) {
		t.Fatal("the operating system does not report the buffer's page as locked")
	}
	failed := errors.New("from fn")
	if err := b.With(func(p []byte) error {
		if p[0] != 'T' {
			t.Errorf("a write through With was lost: %q", p)
		}
		return failed
	}); err != failed {
		t.Fatalf("With returned %v, want fn's error", err)
	}

	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
	if freed.Load() != 1 {
		t.Fatalf("Release gave back %d mappings, want 1", freed.Load())
	}
	if err := b.With(func([]byte) error { t.Error("fn ran after Release"); return nil }); err != ErrBufferReleased {
		t.Fatalf("With after Release = %v, want ErrBufferReleased", err)
	}
	if locked, reason := b.Locked(); locked || reason != "released" {
		t.Fatalf("Locked() after Release = %v, %q", locked, reason)
	}
	if err := b.Release(); err != nil || freed.Load() != 1 {
		t.Fatalf("second Release = %v, %d mappings given back", err, freed.Load())
	}
}

// A buffer nobody released is released by the garbage collector, zeroed
// first (watchFree looks).
func TestLockedBufferDroppedIsReleased(t *testing.T) {
	freed := watchFree(t)
	if _, err := NewLockedBuffer([]byte("dropped without Release")); err != nil {
		t.Fatal(err)
	}
	for i := 0; freed.Load() == 0; i++ {
		if i == 500 {
			t.Fatal("the dropped buffer was never released")
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// Release waits for a With in progress: were it to unmap under one, the read
// below would crash the test binary.
func TestLockedBufferReleaseWaitsForWith(t *testing.T) {
	watchFree(t)
	b, err := NewLockedBuffer(bytes.Repeat([]byte{0x5a}, 3000))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	running := make(chan struct{}, 8)
	for range cap(running) {
		wg.Go(func() {
			for first := true; ; first = false {
				err := b.With(func(p []byte) error {
					if bytes.Count(p, []byte{0x5a}) != 3000 {
						t.Error("With saw bytes that are not the buffer's")
					}
					return nil
				})
				if err != nil {
					return
				}
				if first {
					running <- struct{}{}
				}
			}
		})
	}
	for range cap(running) {
		<-running
	}
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

func TestLockedBufferDoesNotPrintItsBytes(t *testing.T) {
	b, err := NewLockedBuffer([]byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		if got := fmt.Sprintf(verb, b); got != "[locked buffer]" {
			t.Errorf("%s printed %q", verb, got)
		}
	}
}

// FuzzLockedBuffer runs a script of operations over up to eight buffers and
// compares every read with a plain copy kept beside them. Each step is an
// opcode byte followed by its arguments; a script that runs out of bytes reads
// zeros. Nothing in it may panic or fault, and every mapping made is given
// back, zeroed (watchFree looks at each one).
func FuzzLockedBuffer(f *testing.F) {
	page := uint16(os.Getpagesize())
	newBuf := func(size uint16, fill byte) []byte {
		return append(binary.LittleEndian.AppendUint16([]byte{0}, size), fill)
	}
	script := func(steps ...[]byte) []byte { return bytes.Join(steps, nil) }
	f.Add([]byte{})
	f.Add(script(newBuf(0, 1), []byte{1, 0, 0, 0, 9, 9}, []byte{2, 0}, []byte{3, 0}, []byte{3, 0}, []byte{2, 0}))
	f.Add(script(newBuf(1, 2), newBuf(page-1, 3), newBuf(page, 4), newBuf(page+1, 5), []byte{2, 0, 2, 1, 2, 2, 2, 3}, []byte{3, 2, 3, 0, 3, 3, 3, 1}))
	f.Add(script(newBuf(4*page, 7), []byte{1, 0, 0xff, 0x3f, 200, 6}, []byte{2, 0, 4, 0, 3, 0, 1, 0, 0, 0, 1, 1, 4, 0}))
	f.Add(script(newBuf(65535, 8), newBuf(65535, 9), newBuf(65535, 10), newBuf(65535, 11), newBuf(65535, 12), newBuf(65535, 13), newBuf(65535, 14), newBuf(65535, 15), newBuf(1, 1), []byte{2, 7, 3, 7, 3, 0}))

	f.Fuzz(func(t *testing.T, script []byte) {
		freed := watchFree(t)
		next := func() byte {
			if len(script) == 0 {
				return 0
			}
			c := script[0]
			script = script[1:]
			return c
		}
		next16 := func() int { return int(next()) | int(next())<<8 }
		type held struct {
			b      *LockedBuffer
			want   []byte // what the buffer must hold; nil once released
			locked bool
			reason string
		}
		var bufs []*held
		for len(script) > 0 {
			op := next() % 5
			if op == 0 {
				src := make([]byte, next16())
				fill := next()
				if len(bufs) == 8 {
					continue
				}
				for i := range src {
					src[i] = fill + byte(i)
				}
				h := &held{want: bytes.Clone(src)}
				b, err := NewLockedBuffer(src)
				if err != nil {
					t.Fatalf("NewLockedBuffer(%d bytes): %v", len(src), err)
				}
				if !allZero(src) {
					t.Fatal("the source bytes were left in place")
				}
				h.b = b
				h.locked, h.reason = b.Locked()
				if h.locked != (h.reason == "") {
					t.Fatalf("Locked() = %v, %q", h.locked, h.reason)
				}
				bufs = append(bufs, h)
				continue
			}
			if len(bufs) == 0 {
				continue
			}
			h := bufs[int(next())%len(bufs)]
			ran := false
			var err error
			switch op {
			case 1: // write a run of one value
				off, n, val := next16(), int(next()), next()
				err = h.b.With(func(p []byte) error {
					ran = true
					for i := off; i < off+n && i < len(p); i++ {
						p[i], h.want[i] = val, val
					}
					return nil
				})
			case 2: // read it all back
				err = h.b.With(func(p []byte) error {
					ran = true
					if !bytes.Equal(p, h.want) || cap(p) != len(h.want) {
						t.Errorf("a %d-byte buffer read back wrong (len %d, cap %d)", len(h.want), len(p), cap(p))
					}
					if osLocked != nil && osLocked(addrOf(p)) != h.locked {
						t.Errorf("the buffer says locked=%v, the operating system says otherwise", h.locked)
					}
					return nil
				})
			case 3: // release, in whatever order the script says, again and again
				before := freed.Load()
				if err := h.b.Release(); err != nil {
					t.Fatalf("Release: %v", err)
				}
				if gave := freed.Load() - before; (gave == 1) != (h.want != nil) {
					t.Fatalf("Release gave back %d mappings (already released: %v)", gave, h.want == nil)
				}
				h.want, h.locked, h.reason = nil, false, "released"
				continue
			case 4: // the reported state does not drift
				if locked, reason := h.b.Locked(); locked != h.locked || reason != h.reason {
					t.Fatalf("Locked() = %v, %q; was %v, %q", locked, reason, h.locked, h.reason)
				}
				continue
			}
			if released := h.want == nil; released != (err == ErrBufferReleased) || released == ran || (!released && err != nil) {
				t.Fatalf("With on a buffer (released: %v) ran fn: %v, returned %v", released, ran, err)
			}
		}
		for _, h := range bufs {
			if err := h.b.Release(); err != nil {
				t.Fatalf("Release: %v", err)
			}
		}
		if freed.Load() != int64(len(bufs)) {
			t.Fatalf("%d buffers made, %d mappings given back", len(bufs), freed.Load())
		}
	})
}
