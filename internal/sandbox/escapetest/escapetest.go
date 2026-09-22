// Package escapetest is the go-test face of the backend-agnostic containment
// suite for sandbox.Backend implementations. The suite itself (case table, the
// work dir with a SECRET file placed OUTSIDE every policy root, the standard
// escape policy) lives in the testing-free internal/sandbox/escapecore, so the
// operator's `tenant sandbox selftest` runs exactly the same cases on a live box.
//
// Harness(t, backend, expectContained) asserts, per case, that the adversarial
// command's containment equals expectContained and that the legitimate twin
// exits 0. Passing expectContained=false against the host backend documents the
// gap (the host backend does not confine the filesystem); passing
// expectContained=true against a real restricted backend proves containment.
//
// Assertions are kept honest: the network case is the only one that needs
// connectivity, and its containment assertion is skipped (with a log line) when
// the host itself has no outbound network — the filesystem cases are never
// skipped or weakened.
package escapetest

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/sandbox"
	"tenant/internal/sandbox/escapecore"
)

// Harness runs the containment suite against b. expectContained is what every
// adversarial case's containment must equal: true for a confining backend,
// false for the unconfined host backend (which the suite then documents). Each
// case is reported as a subtest named after it.
func Harness(t *testing.T, b sandbox.Backend, expectContained bool) {
	t.Helper()
	results, err := escapecore.Evaluate(context.Background(), b)
	for _, r := range results {
		t.Run(r.Name, func(t *testing.T) {
			if r.Skipped {
				t.Log(r.SkipReason)
			} else {
				assertContainment(t, r, expectContained)
			}
			assertLegit(t, r)
		})
	}
	if err != nil {
		t.Fatalf("escape suite could not complete against backend %q: %v", b.Name(), err)
	}
}

func assertContainment(t *testing.T, r escapecore.CaseResult, want bool) {
	t.Helper()
	if want && strings.Contains(r.AdversarialOutput, escapecore.SecretSentinel) {
		t.Fatalf("adversary %q leaked the secret (exit=%d timedOut=%v violations=%v): output=%q",
			r.AdversarialCommand, r.AdversarialExit, r.AdversarialTimedOut, r.AdversarialViolations, r.AdversarialOutput)
	}
	if r.AdversarialContained != want {
		t.Fatalf("adversary %q: Contained()=%v, want %v (exit=%d timedOut=%v violations=%v output=%q)",
			r.AdversarialCommand, r.AdversarialContained, want,
			r.AdversarialExit, r.AdversarialTimedOut, r.AdversarialViolations, r.AdversarialOutput)
	}
}

func assertLegit(t *testing.T, r escapecore.CaseResult) {
	t.Helper()
	if !r.LegitOK {
		t.Fatalf("legit twin %q must exit 0, got exit=%d output=%q", r.LegitCommand, r.LegitExit, r.LegitOutput)
	}
}
