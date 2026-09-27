package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Trust states (TEN-298). Only a trusted skill is offered to the agent. A
// skill imported from a package starts untrusted, and one whose required tools
// widen becomes untrusted, until the operator trusts it.
const (
	TrustTrusted   = "trusted"
	TrustUntrusted = "untrusted"
)

// Origins: how a skill came to be.
const (
	OriginOperator  = "operator"
	OriginAgent     = "agent"
	OriginInduction = "induction"
	OriginSeed      = "seed"
	OriginImport    = "import"
)

// PackageInfo is an imported skill's provenance: the Agent Skills package it
// came from.
type PackageInfo struct {
	Source        string            `json:"source"` // the directory or SKILL.md it was imported from
	Digest        string            `json:"digest"` // sha256 of that SKILL.md, hex
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	// Files lists the package's other files (scripts/, references/, assets/).
	// Tenant installs none of them: it keeps the instructions only.
	Files      []string  `json:"files,omitempty"`
	ImportedAt time.Time `json:"imported_at"`
}

// Eligible reports whether the agent may be offered sk. A skill is eligible
// when it is live, enabled and trusted, and every tool it requires is
// available here (available nil: tools are not checked). reason says why not.
func (sk *Skill) Eligible(available func(tool string) bool) (ok bool, reason string) {
	switch {
	case sk.Status == StatusProposed:
		return false, "proposed: accept it first"
	case sk.Status == StatusTombstoned:
		return false, "removed"
	case sk.Status != StatusLive:
		return false, "status " + sk.Status
	case !sk.Enabled:
		return false, "disabled"
	case sk.Trust != TrustTrusted:
		return false, "untrusted: review it, then trust it"
	}
	if available != nil {
		var missing []string
		for _, t := range sk.RequiresTools {
			if !available(t) {
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			return false, "needs tools not available here: " + strings.Join(missing, ", ")
		}
	}
	return true, ""
}

// SetTrust sets a skill's trust and records the decision in its history, as
// change source "trust" or "distrust" (the snapshot keeps the content it had).
// It reports whether the skill exists; setting the state it already has
// changes nothing.
func (s *Store) SetTrust(ctx context.Context, agentID, name, trust string) (bool, error) {
	if trust != TrustTrusted && trust != TrustUntrusted {
		return false, fmt.Errorf("skills: unknown trust %q", trust)
	}
	cur, err := s.GetByName(ctx, agentID, name)
	if err != nil || cur == nil {
		return false, err
	}
	if cur.Trust == trust {
		return true, nil
	}
	source := "trust"
	if trust == TrustUntrusted {
		source = "distrust"
	}
	cur.Trust = trust
	if _, err := s.UpsertWithSource(ctx, cur, source); err != nil {
		return false, err
	}
	return true, nil
}

// widens reports whether next requires a tool prior did not.
func widens(prior, next []string) bool {
	had := make(map[string]bool, len(prior))
	for _, t := range prior {
		had[t] = true
	}
	for _, t := range next {
		if !had[t] {
			return true
		}
	}
	return false
}

func decodeTools(v sql.NullString) []string {
	if !v.Valid || v.String == "" || v.String == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil
	}
	if out == nil {
		out = []string{}
	}
	return out
}
