package main

import (
	"fmt"
	"sync"

	"tenant/internal/plugins/imessage"
)

// imessageAllowManager implements tui.IMessageControl: it owns the persisted
// iMessage drive-allowlist — the handles permitted to drive the agent over
// iMessage — and edits it live from the /imessage TUI command. It mirrors
// dashboardManager / discordRelayManager: it holds the live policy value plus a
// persist closure that writes through to config.json.
//
// The list is DENY-BY-DEFAULT (see imessage.AllowList): empty permits nobody.
// It is the source of truth the inbound responder (Layer 2, TEN-142) must gate
// on via AllowList.Allows before letting a texter drive the agent. Editing the
// list here is necessary but the enforcement check lives at the drive point.
//
// All methods are safe for concurrent use; the mutex guards the swap of the
// immutable AllowList value (With/Without return new lists, never mutate).
type imessageAllowManager struct {
	mu      sync.Mutex
	list    *imessage.AllowList
	persist func(handles []string) error // write-through to launchConfig (nil = no persist)
}

// newIMessageAllowManager builds a manager seeded from the persisted handles.
func newIMessageAllowManager(initial []string, persist func([]string) error) *imessageAllowManager {
	return &imessageAllowManager{list: imessage.NewAllowList(initial), persist: persist}
}

// AllowList returns the current handles in sorted, normalized form.
func (m *imessageAllowManager) AllowList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list.Handles()
}

// Allow adds a handle. added is false (no error) when it was already present;
// an error is returned only when the handle can't be normalized (not a usable
// phone/email) or persistence fails.
func (m *imessageAllowManager) Allow(handle string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, norm, added := m.list.With(handle)
	if norm == "" {
		return "", false, fmt.Errorf("not a usable handle: %q (expected a phone number or email)", handle)
	}
	if !added {
		return norm, false, nil
	}
	if err := m.commitLocked(next); err != nil {
		return norm, false, err
	}
	return norm, true, nil
}

// Deny removes a handle. removed is false (no error) when it wasn't present.
func (m *imessageAllowManager) Deny(handle string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, norm, removed := m.list.Without(handle)
	if !removed {
		return norm, false, nil
	}
	if err := m.commitLocked(next); err != nil {
		return norm, false, err
	}
	return norm, true, nil
}

// Clear empties the allowlist, returning how many handles were removed.
func (m *imessageAllowManager) Clear() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.list.Len()
	if n == 0 {
		return 0, nil
	}
	if err := m.commitLocked(imessage.NewAllowList(nil)); err != nil {
		return 0, err
	}
	return n, nil
}

// commitLocked persists the new list THEN swaps it in. Persist-first means a
// write failure leaves the in-memory list matching what's actually on disk, so
// the UI never claims a change that wasn't durably recorded.
func (m *imessageAllowManager) commitLocked(next *imessage.AllowList) error {
	if m.persist != nil {
		if err := m.persist(next.Handles()); err != nil {
			return err
		}
	}
	m.list = next
	return nil
}
