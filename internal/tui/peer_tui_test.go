package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakePeerControl is an in-memory PeerControl for handler tests.
type fakePeerControl struct {
	peers   map[string]*PeerInfo
	removed []string
}

func newFakePeerControl(names ...string) *fakePeerControl {
	f := &fakePeerControl{peers: map[string]*PeerInfo{}}
	for _, n := range names {
		f.peers[n] = &PeerInfo{Name: n, URL: "https://" + n + ":9100", Dial: true, TokenState: "set", Share: map[string]bool{}}
	}
	return f
}

func (f *fakePeerControl) List() []PeerInfo {
	out := []PeerInfo{}
	for _, p := range f.peers {
		out = append(out, *p)
	}
	return out
}
func (f *fakePeerControl) Show(name string) (PeerInfo, bool) {
	p, ok := f.peers[name]
	if !ok {
		return PeerInfo{}, false
	}
	return *p, true
}
func (f *fakePeerControl) Remove(name string) (bool, error) {
	if _, ok := f.peers[name]; !ok {
		return false, nil
	}
	delete(f.peers, name)
	f.removed = append(f.removed, name)
	return true, nil
}
func (f *fakePeerControl) SetShare(name, capability string, allow bool) (PeerInfo, error) {
	p, ok := f.peers[name]
	if !ok {
		return PeerInfo{}, fmt.Errorf("no peer named %q", name)
	}
	valid := map[string]bool{"wiki": true, "memory": true, "skills": true, "exec": true, "llm": true}
	if !valid[capability] {
		return PeerInfo{}, fmt.Errorf("unknown capability %q", capability)
	}
	p.Share[capability] = allow
	return *p, nil
}

func lastSys(m *model) string {
	if len(m.msgs) == 0 {
		return ""
	}
	return m.msgs[len(m.msgs)-1].content
}

func TestHandleConfigurePeer_ShowAndToggle(t *testing.T) {
	f := newFakePeerControl("laptop")
	m := newModel(context.Background(), Config{Peer: f})

	// Show: all five sharable caps, all DENY by default.
	m.handleConfigurePeer([]string{"laptop"})
	out := lastSys(m)
	for _, cap := range PeerShareCaps {
		if !strings.Contains(out, cap) {
			t.Errorf("share view missing capability %q: %q", cap, out)
		}
	}
	if !strings.Contains(out, "DENY") || strings.Contains(out, "ALLOW") {
		t.Errorf("all caps should default DENY: %q", out)
	}

	// Toggle memory on → reflected, and the control was actually called.
	m.handleConfigurePeer([]string{"laptop", "memory=on"})
	out = lastSys(m)
	if !strings.Contains(out, "ALLOW") {
		t.Errorf("memory=on should show ALLOW: %q", out)
	}
	if !f.peers["laptop"].Share["memory"] {
		t.Error("SetShare(memory,true) was not applied to the store")
	}

	// Bad toggle value → clear error, no change.
	m.handleConfigurePeer([]string{"laptop", "memory=maybe"})
	if !strings.Contains(lastSys(m), "on|off") {
		t.Errorf("bad toggle should explain on|off: %q", lastSys(m))
	}

	// Unknown peer.
	m.handleConfigurePeer([]string{"ghost"})
	if !strings.Contains(lastSys(m), "no peer named") {
		t.Errorf("unknown peer should error: %q", lastSys(m))
	}
}

func TestHandlePeer_ListShowRemove(t *testing.T) {
	f := newFakePeerControl("hub", "edge")
	m := newModel(context.Background(), Config{Peer: f})

	m.handlePeer("")
	if out := lastSys(m); !strings.Contains(out, "hub") || !strings.Contains(out, "edge") {
		t.Errorf("list should show both peers: %q", out)
	}

	m.handlePeer("show hub")
	if !strings.Contains(lastSys(m), "hub") {
		t.Errorf("show should render the peer: %q", lastSys(m))
	}

	m.handlePeer("remove edge")
	if !strings.Contains(lastSys(m), "removed peer edge") {
		t.Errorf("remove should confirm: %q", lastSys(m))
	}
	if len(f.removed) != 1 || f.removed[0] != "edge" {
		t.Errorf("Remove(edge) not called: %+v", f.removed)
	}

	// invite points at the working flow (part 1 push-pairing is the next build).
	m.handlePeer("invite box 1.2.3.4")
	if !strings.Contains(lastSys(m), "tenant peer") {
		t.Errorf("invite should point at the pairing flow: %q", lastSys(m))
	}
}

func TestHandlePeer_NilControl(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.handlePeer("")
	if !strings.Contains(lastSys(m), "not available") {
		t.Errorf("nil Peer control should degrade gracefully: %q", lastSys(m))
	}
}
