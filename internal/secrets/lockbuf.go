package secrets

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
)

// ErrBufferReleased is what LockedBuffer.With returns after Release.
var ErrBufferReleased = errors.New("secrets: locked buffer released")

// LockedBuffer holds a secret that stays in memory for a long time, such as a
// key held for the whole run, in memory pages of its own instead of on the Go
// heap.
//
// What it does: the pages are kept out of the swap file (mlock on Linux and
// macOS, VirtualLock on Windows), on Linux they are also left out of core
// dumps (MADV_DONTDUMP), and they are zeroed before they go back to the
// operating system.
//
// What it does not do: it hides nothing from something that can already read
// this process's memory; that is what process protection (TEN-535) is for. On
// Windows and macOS the pages are part of a dump like any others: VirtualLock
// is not a way to exclude memory from one. A hibernation image holds all of
// RAM, locked or not. And it protects its own pages only, not a copy made
// from them.
//
// When the operating system refuses the lock the buffer works all the same,
// and Locked gives the reason.
type LockedBuffer struct {
	mu     sync.Mutex
	mem    []byte // the mapping, at least one byte; nil once released
	n      int    // bytes in use
	locked bool
	reason string
}

// NewLockedBuffer moves src into a new buffer: the bytes are copied into the
// buffer's pages and src is zeroed, so the caller is not left with a second
// copy. It takes bytes and not a string because a string cannot be zeroed. To
// make a secret in place, pass zeroed bytes of the wanted length and fill them
// in With.
//
// The error is a failure to get the pages at all, and src is then untouched; a
// refused lock is not an error (see Locked). Call Release when done. A buffer
// dropped without it is released when the garbage collector finds it, which
// can be much later.
func NewLockedBuffer(src []byte) (*LockedBuffer, error) {
	mem, err := mapPages(max(len(src), 1))
	if err != nil {
		return nil, fmt.Errorf("secrets: locked buffer: %w", err)
	}
	b := &LockedBuffer{mem: mem, n: len(src)}
	b.locked, b.reason = lockPages(mem)
	copy(mem, src)
	clear(src)
	runtime.SetFinalizer(b, func(b *LockedBuffer) { _ = b.Release() })
	return b, nil
}

// With calls fn with the buffer's bytes in place, to read or to write: p is
// the locked memory itself, never a copy, and there is no other way to reach
// it. p is valid only until fn returns. fn must not keep p or a slice of it (a
// later use would crash the process once the pages are gone), and whatever it
// copies out of p, or converts to a string, is no longer protected. One fn
// runs at a time, and Release waits for it; fn must not call a method of the
// same buffer, which would wait for fn itself.
//
// With returns fn's error, or ErrBufferReleased, without calling fn, after
// Release.
func (b *LockedBuffer) With(fn func(p []byte) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mem == nil {
		return ErrBufferReleased
	}
	return fn(b.mem[:b.n:b.n])
}

// Locked reports whether the buffer's pages are locked into RAM. When a
// protection could not be applied, reason names the call that failed and the
// operating system's error, for `tenant doctor` to show; it is empty when
// every protection this system has is in place. After Release it reports
// false and "released".
func (b *LockedBuffer) Locked() (locked bool, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.locked, b.reason
}

// Release zeroes the buffer's pages, then unlocks and unmaps them. A second
// Release does nothing. The error is the operating system refusing to unlock
// or unmap; the bytes are zero by then either way.
func (b *LockedBuffer) Release() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mem == nil {
		return nil
	}
	runtime.SetFinalizer(b, nil)
	mem, locked := b.mem, b.locked
	b.mem, b.n, b.locked, b.reason = nil, 0, false, "released"
	clear(mem)
	return freePages(mem, locked)
}

// Format prints a fixed word for every fmt verb, so that a buffer that ends up
// in a log line or an error message does not print its bytes.
func (*LockedBuffer) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "[locked buffer]") }

// freePages gives a mapping back to the operating system. Tests wrap it to
// read the pages after Release has zeroed them and before they are gone.
var freePages = unmapPages
