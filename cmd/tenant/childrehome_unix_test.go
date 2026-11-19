//go:build !windows

package main

import (
	"os"
	"strings"
	"testing"

	"tenant/internal/fleet"
)

// An account that may read the child's files and not write them (here, the
// directory made read-only to its own owner) is refused before the code is
// read: nothing is changed and the code is not spent.
func TestChildRehomeByAnAccountThatMayNotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a directory whatever its mode says")
	}
	a := newFleetMaster(t)
	dir, digits, _ := enrollChild(t, a, "box", childInstallOpts{})
	mach := a.confirm("box", digits)
	stop := startChild(t, dir)
	a.awaitOnline(mach.ID, true)
	stop()
	before := childSnapshot(t, dir)
	b := newFleetMaster(t)
	code := b.code("box")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := rehome(t, dir, code); exitCode(err) != 2 || !strings.Contains(err.Error(), "may not write the child's files") {
		t.Fatalf("rehome by an account that may not write: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !sameSnapshot(before, childSnapshot(t, dir)) {
		t.Fatal("a refused rehome changed the child's files")
	}
	if child, _ := fleet.LoadChild(fleet.ChildPath(dir)); child.MasterFingerprint != a.id.Fingerprint {
		t.Fatal("a refused rehome changed the master the child trusts")
	}
	if _, err := rehome(t, dir, code); err != nil {
		t.Fatalf("the code the refusal left unspent: %v", err)
	}
}
