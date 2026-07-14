package route

import (
	"fmt"
	"strings"

	"tenant/internal/model"
)

// RulePredicate is the frozen 4-field condition vocabulary for N-way routing.
// The freeze is an explicit anti-scope-creep commitment (TEN-292): config
// decoding rejects unknown predicate keys, and this struct must not grow
// fields. Set fields AND together; zero values mean "not constrained", so a
// rule must set at least one field to be valid.
type RulePredicate struct {
	// Domain matches Supra's Domain field case-insensitively ("" = any).
	Domain string
	// ComplexityGTE matches Signal.Complexity >= N for N in 1..5 (0 = any).
	ComplexityGTE int
	// Math/Code are tri-state: nil = any, otherwise exact match.
	Math *bool
	Code *bool
}

// Rule maps one predicate to a ROLE. Roles, never profile IDs: the role's
// binding stays under the router's normal wiring-time control, so a rule can
// not smuggle in an unregistered or cloud profile by itself.
type Rule struct {
	When RulePredicate
	Role model.Role
}

// Rules is an ordered top-down rule set; first match wins. An empty set means
// the 2-way small/big behavior from TEN-290 — rules are additive, not a mode.
type Rules []Rule

// Match returns the first rule whose predicate accepts the signal. It is only
// meaningful for a successfully parsed Signal; callers gate on decision status.
func (rs Rules) Match(sig Signal) (Rule, bool) {
	for _, r := range rs {
		if r.When.matches(sig) {
			return r, true
		}
	}
	return Rule{}, false
}

func (p RulePredicate) matches(sig Signal) bool {
	if p.Domain != "" && !strings.EqualFold(p.Domain, sig.Domain) {
		return false
	}
	if p.ComplexityGTE > 0 && sig.Complexity < p.ComplexityGTE {
		return false
	}
	if p.Math != nil && *p.Math != sig.Math {
		return false
	}
	if p.Code != nil && *p.Code != sig.Code {
		return false
	}
	return true
}

// empty reports a predicate with no constraint at all — always-true rules are
// rejected because they silently shadow every later rule and the 2-way default.
func (p RulePredicate) empty() bool {
	return p.Domain == "" && p.ComplexityGTE == 0 && p.Math == nil && p.Code == nil
}

// Validate enforces value-level rule invariants. Key-level freezing (only the
// four predicate names exist) is enforced where config is decoded.
func (rs Rules) Validate() error {
	for i, r := range rs {
		if strings.TrimSpace(string(r.Role)) == "" {
			return fmt.Errorf("route: rules[%d]: role is required", i)
		}
		if r.When.empty() {
			return fmt.Errorf("route: rules[%d]: at least one predicate (domain, complexity_gte, math, code) is required", i)
		}
		if r.When.ComplexityGTE < 0 || r.When.ComplexityGTE > 5 {
			return fmt.Errorf("route: rules[%d]: complexity_gte %d must be between 1 and 5", i, r.When.ComplexityGTE)
		}
	}
	return nil
}
