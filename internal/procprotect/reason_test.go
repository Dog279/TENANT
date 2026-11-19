package procprotect

import (
	"os"
	"strings"
	"testing"
)

// FAILS ON PURPOSE (TEN-535, found while building the acknowledgement flow;
// the control itself is left as stage 1 wrote it).
//
// Section 8 of the plan records an acknowledgement against the control, the
// reason it could not be applied and the version, and "a different reason
// asks again"; what is remembered is there "so a system where a control can
// never apply does not ask at every start". So the reason a control reports
// has to be the same from one start to the next for the same cause.
//
// windows.thread-dacl's is not: restrictThreads names each thread it could
// not change by its thread id ("thread 1234: Access is denied."), and thread
// ids differ at every start. On a machine where that control fails for a
// thread, Tenant would ask at every start, and the service, which has nobody
// to ask, would never start: the reason acknowledged at the terminal is never
// the reason the service reports.
//
// The cure is in protect_windows.go: a reason without the ids (how many
// threads, and the error), with the ids somewhere that is not matched on.
func TestFailureReasonsAreTheSameAtEveryStart(t *testing.T) {
	src, err := os.ReadFile("protect_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `fmt.Sprintf("thread %d: %v", te.ThreadID, err)`) {
		t.Error("windows.thread-dacl's failure reason carries thread ids (protect_windows.go, restrictThreads), which change at every start: " +
			"an acknowledgement of it never matches again, so Tenant asks at every start and its service never starts (plan section 8)")
	}
}
