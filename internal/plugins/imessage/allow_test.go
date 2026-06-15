package imessage

import (
	"reflect"
	"testing"
)

// NormalizeHandle (exported) must be exactly the internal normalizeHandle —
// same canonical form the Watcher matches against. A single source of truth.
func TestNormalizeHandleWrapperMatchesInternal(t *testing.T) {
	cases := []string{
		"Foo@Example.COM", "+1 (555) 123-4567", "555-123-4567",
		"  bar@baz.io ", "+447911123456", "", "   ", "imessage;-;chat",
	}
	for _, in := range cases {
		if got, want := NormalizeHandle(in), normalizeHandle(in); got != want {
			t.Errorf("NormalizeHandle(%q)=%q, internal=%q", in, got, want)
		}
	}
}

// NewAllowList normalizes, drops blanks/unparseable, dedupes, and sorts.
func TestNewAllowListNormalizesDedupesSorts(t *testing.T) {
	a := NewAllowList([]string{
		"Foo@Example.com",   // -> foo@example.com
		"foo@example.com",   // dup after normalize
		"+1 (555) 123-4567", // -> +15551234567
		"   ",               // dropped
		"",                  // dropped
		"bob@b.io",
	})
	got := a.Handles()
	want := []string{"15551234567", "bob@b.io", "foo@example.com"} // phone normalized digits-only (no +)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Handles()=%v, want %v", got, want)
	}
	if a.Len() != 3 {
		t.Errorf("Len()=%d, want 3", a.Len())
	}
}

// DENY-BY-DEFAULT: an empty (or nil) list permits nobody. This is the whole
// point of the type — it must never behave like the Watcher's permissive
// observe filter.
func TestAllowListDenyByDefault(t *testing.T) {
	empty := NewAllowList(nil)
	if empty.Allows("foo@example.com") {
		t.Error("empty list must deny all handles")
	}
	if empty.Allows("+15551234567") {
		t.Error("empty list must deny all phone handles")
	}
	var nilList *AllowList
	if nilList.Allows("foo@example.com") {
		t.Error("nil *AllowList must deny (and not panic)")
	}
	if nilList.Len() != 0 || nilList.Handles() != nil {
		t.Error("nil *AllowList must report empty")
	}
}

// Allows re-normalizes the input (never trusts a pre-normalized caller): a
// member added in one spelling matches the canonical form of any other.
func TestAllowsReNormalizesInput(t *testing.T) {
	a := NewAllowList([]string{"foo@example.com", "+15551234567"})
	if !a.Allows("FOO@example.COM") {
		t.Error("email match must be case-insensitive via re-normalization")
	}
	if !a.Allows("+1 (555) 123-4567") {
		t.Error("phone match must ignore spaces/punctuation via re-normalization")
	}
	if a.Allows("nope@elsewhere.com") {
		t.Error("non-member must be denied")
	}
	if a.Allows("") || a.Allows("   ") {
		t.Error("blank/unparseable handle must be denied")
	}
}

// With/Without are immutable edits returning a new list + change flags.
func TestAllowListWithWithoutImmutable(t *testing.T) {
	base := NewAllowList([]string{"a@x.io"})

	// Add a new handle (normalized form returned).
	next, norm, added := base.With("B@X.io")
	if !added || norm != "b@x.io" {
		t.Fatalf("With new = (added=%v, norm=%q), want (true, b@x.io)", added, norm)
	}
	if base.Len() != 1 {
		t.Error("With must not mutate the receiver")
	}
	if !next.Allows("b@x.io") || !next.Allows("a@x.io") {
		t.Error("new list must contain both handles")
	}

	// Adding an existing handle is a no-op (added=false, receiver unchanged).
	same, norm2, added2 := next.With("a@x.io")
	if added2 || norm2 != "a@x.io" || same.Len() != next.Len() {
		t.Errorf("With existing should be no-op; got added=%v len=%d", added2, same.Len())
	}

	// Unparseable handle: no add, empty normalized.
	if l, n, ok := base.With("   "); ok || n != "" || l.Len() != 1 {
		t.Errorf("With blank should be no-op; got ok=%v n=%q", ok, n)
	}

	// Remove present + absent.
	rem, rnorm, removed := next.Without("A@x.io")
	if !removed || rnorm != "a@x.io" || rem.Allows("a@x.io") {
		t.Errorf("Without present = (removed=%v norm=%q)", removed, rnorm)
	}
	if _, _, removed2 := base.Without("ghost@x.io"); removed2 {
		t.Error("Without absent must report removed=false")
	}
}
