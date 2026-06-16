package main

import (
	"fmt"
	"sync"

	"tenant/internal/plugins/imessage"
	"tenant/internal/tui"
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
	// resp is the live responder lifecycle (TEN-230 Phase 1c), set after
	// construction via setResponder. nil ⇒ responder unavailable (e.g. not
	// macOS), and /imessage on|off reports that cleanly.
	resp *imessageResponderManager
	// perms is the responder's per-category permission broker (TEN-230), exposed
	// to /imessage permissions. nil ⇒ unavailable.
	perms tui.PermissionControl
}

// newIMessageAllowManager builds a manager seeded from the persisted handles.
func newIMessageAllowManager(initial []string, persist func([]string) error) *imessageAllowManager {
	return &imessageAllowManager{list: imessage.NewAllowList(initial), persist: persist}
}

// setResponder wires the live responder manager so /imessage on|off can drive
// it. Call once after both managers are constructed. Locked: resp/perms are now
// also read from the dashboard's HTTP goroutine (TEN-208), not just the TUI's.
func (m *imessageAllowManager) setResponder(r *imessageResponderManager) {
	m.mu.Lock()
	m.resp = r
	m.mu.Unlock()
}

// setPerms wires the responder's permission broker (for /imessage permissions).
func (m *imessageAllowManager) setPerms(p tui.PermissionControl) {
	m.mu.Lock()
	m.perms = p
	m.mu.Unlock()
}

// Perms exposes the responder's per-category permission control (TEN-230);
// nil when the responder is unavailable.
func (m *imessageAllowManager) Perms() tui.PermissionControl {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.perms
}

// ResponderOn reports whether the autonomous responder is running.
func (m *imessageAllowManager) ResponderOn() bool {
	m.mu.Lock()
	r := m.resp
	m.mu.Unlock()
	return r != nil && r.On()
}

// SetResponder turns the autonomous responder on/off live (TEN-230 Phase 1c).
// The lock guards only the m.resp read — Start/Stop run UNLOCKED because Start
// reads the allowlist via AllowList(), which re-locks m.mu (a held lock would
// deadlock; Go mutexes aren't reentrant).
func (m *imessageAllowManager) SetResponder(on bool) (string, error) {
	m.mu.Lock()
	r := m.resp
	m.mu.Unlock()
	if r == nil {
		return "", fmt.Errorf("imessage responder unavailable here (native transport is macOS-only)")
	}
	if on {
		return r.Start()
	}
	return r.Stop()
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
