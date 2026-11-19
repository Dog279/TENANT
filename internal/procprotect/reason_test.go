package procprotect

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// An acknowledgement of a control that could not be applied is recorded
// against the control, the reason and the version, and "a different reason
// asks again" (plan section 8). So the reason a control reports has to be
// the same from one start to the next for the same cause: with a process id,
// a thread id or a count in it, Tenant would ask at every start, and its
// service, which has nobody to ask, would never start. What varies goes in
// Control.Detail, which nothing matches on.

// A failure's detail is filed beside the reason, not in it.
func TestAFailuresDetailIsNotInItsReason(t *testing.T) {
	c := apply(Control{ID: "x"}, func() error {
		return &failure{reason: "a thread could not be given the access list: Access is denied.", detail: "thread 1234"}
	})
	if c.OK || c.Reason != "a thread could not be given the access list: Access is denied." || c.Detail != "thread 1234" {
		t.Fatalf("control = %+v", c)
	}
	wrapped := apply(Control{ID: "x"}, func() error { return errors.Join(&failure{reason: "r", detail: "d"}) })
	if wrapped.Detail != "d" {
		t.Fatalf("a wrapped failure lost its detail: %+v", wrapped)
	}
}

// No control, on any system, builds its reason from a number that the system
// hands out anew at each start. Every protect_<os>.go is read, whichever
// system the test runs on: a decimal in an error it builds is such a number
// (an id or a count) until shown otherwise, and belongs in the detail.
func TestFailureReasonsAreTheSameAtEveryStart(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	decimal := regexp.MustCompile(`(Errorf|errors\.New|Sprintf|reason:)[^\n]*%d`)
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, line := range strings.Split(string(src), "\n") {
			if decimal.MatchString(line) && !strings.Contains(line, "detail") {
				t.Errorf("%s:%d builds a message from a number, which a control's reason must not carry (put it in the failure's detail): %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
	if checked < 8 {
		t.Fatalf("read only %d source files; the walk is broken", checked)
	}
}
