//go:build linux

package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

// doneByDoctorItself: no control a doctor's own start applies is one a
// running Tenant reads back from a shared place here (Windows only).
const doneByDoctorItself = ""

// undoFromOutside is how another program of the user undoes one control of a
// running Tenant on Linux, as the tester did: prlimit raises the soft core
// limit of any process of the same user, up to its hard limit.
func undoFromOutside(t *testing.T, _ string) (env []string, undo func(t *testing.T, pid int), found string) {
	var own unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &own); err != nil || own.Max == 0 {
		return nil, nil, "" // a hard limit of 0, which the serve inherits: nobody can raise the soft one
	}
	return nil, func(t *testing.T, pid int) {
		var lim unix.Rlimit
		if err := unix.Prlimit(pid, unix.RLIMIT_CORE, nil, &lim); err != nil || lim.Cur != 0 {
			t.Fatalf("the serve's soft core limit before: %d, %v", lim.Cur, err)
		}
		lim.Cur = lim.Max
		if err := unix.Prlimit(pid, unix.RLIMIT_CORE, &lim, nil); err != nil {
			t.Fatalf("prlimit on the serve: %v", err)
		}
	}, "the soft core limit is not 0"
}
