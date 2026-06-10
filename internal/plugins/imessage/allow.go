package imessage

import "sort"

// allow.go is the iMessage DRIVE allowlist: the set of handles (phone numbers
// / emails) permitted to drive the agent over iMessage — i.e. let an inbound
// texter invoke the agent's tools (the "MCP framework"). It is build-tag-free
// and dependency-free, so the gate is unit-testable on any OS.
//
// DENY-BY-DEFAULT is the whole point. Unlike the Watcher's OPTIONAL observe
// filter (WatchConfig.AllowFrom, where an empty list means "observe everyone"),
// an empty AllowList permits NOBODY. The two semantics are deliberately kept in
// SEPARATE types so the permissive observe default can never leak into the
// drive gate: a fresh install must not let an arbitrary texter drive the agent.
// This is the safe default the operator's "no unrestricted access" intent
// requires.
//
// Layer 1 (the Watcher) ships today and only observes. The inbound responder
// that actually drives the agent from a texter's message (Layer 2) MUST gate
// every inbound message on AllowList.Allows before invoking any tool — that is
// the real enforcement point. Storing/editing the list (the /imessage TUI
// command) is necessary but not sufficient on its own.

// NormalizeHandle canonicalizes a phone/email handle to the exact form the
// matcher compares against: emails lowercased; phone numbers reduced to a
// leading '+' plus digits. Exported so callers (the TUI allowlist manager)
// store handles in the same canonical form the gate uses — a thin wrapper over
// the internal normalizeHandle keeps a SINGLE source of truth, no duplicated
// normalization logic in cmd/tenant.
func NormalizeHandle(h string) string { return normalizeHandle(h) }

// AllowList is an immutable, normalized, deduplicated set of handles permitted
// to DRIVE the agent over iMessage. The zero value (and a nil *AllowList) is a
// valid deny-all list. Edits (With/Without) return a NEW list, so a value
// shared across goroutines is never mutated in place.
type AllowList struct {
	set   map[string]struct{}
	order []string // sorted, normalized handles — stable display + persistence
}

// NewAllowList builds an AllowList from raw handles, normalizing each, dropping
// blanks/unparseable entries, deduplicating, and sorting. A nil/empty input
// yields a deny-all list (Allows always false).
func NewAllowList(handles []string) *AllowList {
	set := make(map[string]struct{}, len(handles))
	for _, h := range handles {
		if n := normalizeHandle(h); n != "" {
			set[n] = struct{}{}
		}
	}
	return fromSet(set)
}

func fromSet(set map[string]struct{}) *AllowList {
	order := make([]string, 0, len(set))
	for h := range set {
		order = append(order, h)
	}
	sort.Strings(order)
	return &AllowList{set: set, order: order}
}

// Allows reports whether handle may drive the agent. It RE-NORMALIZES the input
// (never trusting a pre-normalized caller) and returns false for an empty list
// (deny-by-default) or a blank/unparseable handle. This is the function the
// inbound responder (Layer 2) must call at the drive gate.
func (a *AllowList) Allows(handle string) bool {
	if a == nil || len(a.set) == 0 {
		return false
	}
	n := normalizeHandle(handle)
	if n == "" {
		return false
	}
	_, ok := a.set[n]
	return ok
}

// Handles returns the normalized handles in sorted order (a defensive copy).
func (a *AllowList) Handles() []string {
	if a == nil || len(a.order) == 0 {
		return nil
	}
	out := make([]string, len(a.order))
	copy(out, a.order)
	return out
}

// Len reports how many handles are on the list.
func (a *AllowList) Len() int {
	if a == nil {
		return 0
	}
	return len(a.set)
}

// With returns a NEW AllowList including handle. added is false (and the
// receiver returned unchanged) when the handle was already present or
// normalizes to empty. normalized is the canonical stored form, returned so
// the UI can echo exactly what was recorded ("" when unparseable).
func (a *AllowList) With(handle string) (list *AllowList, normalized string, added bool) {
	n := normalizeHandle(handle)
	if n == "" {
		return a, "", false
	}
	if a != nil {
		if _, ok := a.set[n]; ok {
			return a, n, false
		}
	}
	set := a.clone()
	set[n] = struct{}{}
	return fromSet(set), n, true
}

// Without returns a NEW AllowList excluding handle. removed is false (receiver
// returned unchanged) when the handle wasn't present or normalizes to empty.
func (a *AllowList) Without(handle string) (list *AllowList, normalized string, removed bool) {
	n := normalizeHandle(handle)
	if n == "" || a == nil {
		return a, n, false
	}
	if _, ok := a.set[n]; !ok {
		return a, n, false
	}
	set := a.clone()
	delete(set, n)
	return fromSet(set), n, true
}

func (a *AllowList) clone() map[string]struct{} {
	set := map[string]struct{}{}
	if a != nil {
		for h := range a.set {
			set[h] = struct{}{}
		}
	}
	return set
}
