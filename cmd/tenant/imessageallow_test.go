package main

import (
	"errors"
	"sync"
	"testing"
)

// eqStrings compares two string slices element-wise (nil == empty). Local
// helper because cmd/tenant declares a package-level func named `reflect`,
// which shadows the stdlib reflect package — so we can't import it here.
func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// recordingHandlePersist captures each persisted snapshot of the allowlist so
// tests can assert what was written through to config.
type recordingHandlePersist struct {
	mu    sync.Mutex
	calls [][]string
	err   error // when set, persist fails (to test rollback semantics)
}

func (r *recordingHandlePersist) persist(handles []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, append([]string(nil), handles...))
	return nil
}

func (r *recordingHandlePersist) last() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

func (r *recordingHandlePersist) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// Allow normalizes + persists, dedupes (no-op + no persist on re-add), and
// rejects unparseable handles.
func TestIMessageAllowManager_Allow(t *testing.T) {
	rp := &recordingHandlePersist{}
	m := newIMessageAllowManager(nil, rp.persist)

	norm, added, err := m.Allow("Foo@Example.COM")
	if err != nil || !added || norm != "foo@example.com" {
		t.Fatalf("Allow = (%q, %v, %v), want (foo@example.com, true, nil)", norm, added, err)
	}
	if got, want := m.AllowList(), []string{"foo@example.com"}; !eqStrings(got, want) {
		t.Fatalf("AllowList=%v, want %v", got, want)
	}
	if got, want := rp.last(), []string{"foo@example.com"}; !eqStrings(got, want) {
		t.Fatalf("persisted=%v, want %v", got, want)
	}

	// Re-add (different spelling, same normalized) is a no-op: no extra persist.
	beforeCount := rp.count()
	_, added2, err := m.Allow("FOO@example.com")
	if err != nil || added2 {
		t.Fatalf("re-Allow = (added=%v, err=%v), want (false, nil)", added2, err)
	}
	if rp.count() != beforeCount {
		t.Errorf("re-Allow must not persist again (count %d -> %d)", beforeCount, rp.count())
	}

	// Unparseable handle errors and does not persist.
	if _, _, err := m.Allow("   "); err == nil {
		t.Error("Allow of blank handle should error")
	}
	if rp.count() != beforeCount {
		t.Error("failed Allow must not persist")
	}
}

// Deny removes a present handle (persisting) and is a no-op for an absent one.
func TestIMessageAllowManager_Deny(t *testing.T) {
	rp := &recordingHandlePersist{}
	m := newIMessageAllowManager([]string{"a@x.io", "+1 555 123 4567"}, rp.persist)

	norm, removed, err := m.Deny("A@X.io")
	if err != nil || !removed || norm != "a@x.io" {
		t.Fatalf("Deny = (%q, %v, %v), want (a@x.io, true, nil)", norm, removed, err)
	}
	if got, want := m.AllowList(), []string{"+15551234567"}; !eqStrings(got, want) {
		t.Fatalf("AllowList after Deny=%v, want %v", got, want)
	}

	// Absent handle: no-op, no persist.
	before := rp.count()
	_, removed2, err := m.Deny("ghost@x.io")
	if err != nil || removed2 {
		t.Fatalf("Deny absent = (removed=%v, err=%v), want (false, nil)", removed2, err)
	}
	if rp.count() != before {
		t.Error("no-op Deny must not persist")
	}
}

// Clear empties the list and reports the count removed.
func TestIMessageAllowManager_Clear(t *testing.T) {
	rp := &recordingHandlePersist{}
	m := newIMessageAllowManager([]string{"a@x.io", "b@x.io"}, rp.persist)

	n, err := m.Clear()
	if err != nil || n != 2 {
		t.Fatalf("Clear = (%d, %v), want (2, nil)", n, err)
	}
	if got := m.AllowList(); got != nil {
		t.Errorf("AllowList after Clear=%v, want nil", got)
	}
	if got := rp.last(); got != nil {
		t.Errorf("persisted after Clear=%v, want nil", got)
	}

	// Clearing an already-empty list is a no-op (no persist).
	before := rp.count()
	if n, err := m.Clear(); err != nil || n != 0 {
		t.Fatalf("Clear empty = (%d, %v), want (0, nil)", n, err)
	}
	if rp.count() != before {
		t.Error("Clear of empty list must not persist")
	}
}

// A persist failure leaves the in-memory list unchanged so the UI reflects
// what's actually on disk (commit persists THEN swaps).
func TestIMessageAllowManager_PersistFailureRollsBack(t *testing.T) {
	rp := &recordingHandlePersist{err: errors.New("disk full")}
	m := newIMessageAllowManager([]string{"keep@x.io"}, rp.persist)

	if _, _, err := m.Allow("new@x.io"); err == nil {
		t.Fatal("Allow should surface the persist error")
	}
	if got, want := m.AllowList(), []string{"keep@x.io"}; !eqStrings(got, want) {
		t.Errorf("after failed Allow, list=%v, want unchanged %v", got, want)
	}
}

// A nil persist closure is tolerated (in-memory only).
func TestIMessageAllowManager_NilPersist(t *testing.T) {
	m := newIMessageAllowManager(nil, nil)
	if _, added, err := m.Allow("x@y.io"); err != nil || !added {
		t.Fatalf("Allow with nil persist = (added=%v, err=%v)", added, err)
	}
	if got, want := m.AllowList(), []string{"x@y.io"}; !eqStrings(got, want) {
		t.Errorf("AllowList=%v, want %v", got, want)
	}
}
