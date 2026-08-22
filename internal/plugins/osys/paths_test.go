package osys

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testRoots(t *testing.T) (Roots, string, string) {
	t.Helper()
	base := t.TempDir()
	allowed := filepath.Join(base, "tenant")
	denied := filepath.Join(base, "windows")
	for _, d := range []string{allowed, denied} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return Roots{Deny: []string{denied}, Write: []string{allowed}}, allowed, denied
}

// A denied root is refused for READS too - the point is that the directory is
// invisible, not merely read-only.
func TestRoots_DenyBlocksReadAndWrite(t *testing.T) {
	r, _, denied := testRoots(t)
	target := filepath.Join(denied, "secret.txt")

	for _, write := range []bool{false, true} {
		err := r.Check(target, write)
		if err == nil {
			t.Fatalf("write=%v: denied root must be refused", write)
		}
		if !strings.Contains(err.Error(), "protected directory") {
			t.Errorf("write=%v: unhelpful error: %v", write, err)
		}
		// A hard boundary must say so, otherwise the agent retries via approval.
		if !strings.Contains(err.Error(), "not approvable") {
			t.Errorf("write=%v: refusal should state it is not approvable: %v", write, err)
		}
	}
}

func TestRoots_WriteConfinedButReadsBroad(t *testing.T) {
	r, allowed, _ := testRoots(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")

	if err := r.Check(filepath.Join(allowed, "notes.md"), true); err != nil {
		t.Errorf("write inside an allowed root must pass: %v", err)
	}
	if err := r.Check(outside, true); err == nil {
		t.Error("write outside the write roots must be refused")
	} else if !strings.Contains(err.Error(), "outside the writable roots") {
		t.Errorf("unhelpful error: %v", err)
	}
	// Reads outside Write (but outside Deny too) stay allowed so diagnostics work.
	if err := r.Check(outside, false); err != nil {
		t.Errorf("read outside the write roots should still be allowed: %v", err)
	}
}

// The traversal case: a path that LOOKS like it is inside the allowed root but
// climbs out of it. Refusing this is the whole reason paths are resolved before
// comparison rather than string-prefixed.
func TestRoots_RefusesDotDotEscape(t *testing.T) {
	r, allowed, denied := testRoots(t)
	escape := filepath.Join(allowed, "..", filepath.Base(denied), "pwned.txt")

	if err := r.Check(escape, true); err == nil {
		t.Fatalf("%q climbs into the denied root and must be refused", escape)
	}
}

// A file that does not exist yet must still be checked against its RESOLVED
// parent - otherwise every create bypasses the boundary.
func TestRoots_ChecksNonexistentPathsViaParent(t *testing.T) {
	r, allowed, denied := testRoots(t)

	if err := r.Check(filepath.Join(allowed, "sub", "deep", "new.txt"), true); err != nil {
		t.Errorf("a new file under an allowed root must be permitted: %v", err)
	}
	if err := r.Check(filepath.Join(denied, "sub", "new.txt"), true); err == nil {
		t.Error("a new file under a denied root must be refused")
	}
}

// A symlinked parent pointing out of the allowed tree must not launder a write.
func TestRoots_RefusesSymlinkEscape(t *testing.T) {
	r, allowed, denied := testRoots(t)
	link := filepath.Join(allowed, "backdoor")
	if err := os.Symlink(denied, link); err != nil {
		t.Skipf("symlinks unavailable on this host/user: %v", err)
	}
	if err := r.Check(filepath.Join(link, "pwned.txt"), true); err == nil {
		t.Fatal("a symlink into a denied root must be refused")
	}
}

// Windows paths are case-insensitive; C:\TENANT and c:\tenant are one place.
func TestRoots_CaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only semantics")
	}
	r, _, denied := testRoots(t)
	if err := r.Check(strings.ToUpper(filepath.Join(denied, "x.txt")), false); err == nil {
		t.Error("case variation must not bypass a denied root")
	}
}

// Empty roots preserve the historical unrestricted behavior, so existing
// callers that never configure roots are unaffected.
func TestRoots_EmptyIsUnrestricted(t *testing.T) {
	var r Roots
	if err := r.Check(filepath.Join(t.TempDir(), "anything.txt"), true); err != nil {
		t.Errorf("unconfigured roots must not restrict: %v", err)
	}
}

func TestDefaultDeny_CoversTheDangerousPlaces(t *testing.T) {
	d := DefaultDeny()
	if len(d) == 0 {
		t.Fatal("default deny list is empty")
	}
	joined := strings.ToLower(strings.Join(d, "|"))
	want := "windows"
	if runtime.GOOS != "windows" {
		want = "/etc"
	}
	if !strings.Contains(joined, want) {
		t.Errorf("default deny should cover %q, got %v", want, d)
	}
}
