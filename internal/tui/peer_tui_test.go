package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakePeerControl is an in-memory PeerControl for handler tests.
type fakePeerControl struct {
	peers       map[string]*PeerInfo
	removed     []string
	served      string
	reconnected int
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
func (f *fakePeerControl) Rename(old, newName string) error {
	p, ok := f.peers[old]
	if !ok {
		return fmt.Errorf("no peer named %q", old)
	}
	if _, exists := f.peers[newName]; exists {
		return fmt.Errorf("peer %q already exists", newName)
	}
	delete(f.peers, old)
	p.Name = newName
	f.peers[newName] = p
	return nil
}
func (f *fakePeerControl) Serve(addr string) (string, error) {
	if addr == "" {
		addr = "0.0.0.0:9100"
	}
	f.served = addr
	return addr, nil
}
func (f *fakePeerControl) Reconnect() (int, error) {
	f.reconnected++
	n := 0
	for _, p := range f.peers {
		if p.Dial {
			n++
		}
	}
	return n, nil
}
func (f *fakePeerControl) Invite(label, url string) (string, func(context.Context) (string, error), error) {
	run := func(context.Context) (string, error) {
		f.peers[label] = &PeerInfo{Name: label, URL: url, Dial: true, TokenState: "set", Share: map[string]bool{}}
		return "paired with " + label, nil
	}
	return "123 456", run, nil
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

func TestHandleConfigurePeer_PermissionsStyle(t *testing.T) {
	f := newFakePeerControl("laptop")
	m := newModel(context.Background(), Config{Peer: f})

	// Show: all five items, all deny by default, permissions-style (lowercase).
	m.handleConfigurePeer([]string{"laptop"})
	out := lastSys(m)
	for _, cap := range PeerShareCaps {
		if !strings.Contains(out, cap) {
			t.Errorf("share view missing item %q: %q", cap, out)
		}
	}
	if !strings.Contains(out, "wiki     deny") || !strings.Contains(out, "memory   deny") {
		t.Errorf("items should default deny in permissions-style rows: %q", out)
	}
	if !strings.Contains(out, "set <item> <mode>") {
		t.Errorf("should show the /permissions-style set hint: %q", out)
	}

	// `set <item> <allow|deny>` — the global-permissions syntax.
	m.handleConfigurePeer([]string{"laptop", "set", "memory", "allow"})
	if !strings.Contains(lastSys(m), "allow") || !f.peers["laptop"].Share["memory"] {
		t.Errorf("set memory allow should apply + show: %q", lastSys(m))
	}

	// Shorthand `<item> <mode>`.
	m.handleConfigurePeer([]string{"laptop", "wiki", "allow"})
	if !f.peers["laptop"].Share["wiki"] {
		t.Error("`wiki allow` shorthand not applied")
	}

	// Legacy `item=mode` still tolerated.
	m.handleConfigurePeer([]string{"laptop", "memory=off"})
	if f.peers["laptop"].Share["memory"] {
		t.Error("legacy `memory=off` should still work")
	}

	// Bad mode → clear error.
	m.handleConfigurePeer([]string{"laptop", "set", "memory", "maybe"})
	if !strings.Contains(lastSys(m), "allow|deny") {
		t.Errorf("bad mode should explain allow|deny: %q", lastSys(m))
	}

	// Unknown peer.
	m.handleConfigurePeer([]string{"ghost"})
	if !strings.Contains(lastSys(m), "no peer named") {
		t.Errorf("unknown peer should error: %q", lastSys(m))
	}
}

func TestHandlePeer_Rename(t *testing.T) {
	f := newFakePeerControl("Dylans-MacBook-Pro-2.local")
	m := newModel(context.Background(), Config{Peer: f})

	m.handlePeer("rename Dylans-MacBook-Pro-2.local mac")
	if !strings.Contains(lastSys(m), "renamed") {
		t.Errorf("rename should confirm: %q", lastSys(m))
	}
	if _, ok := f.peers["mac"]; !ok {
		t.Error("peer should now be under 'mac'")
	}
	if _, ok := f.peers["Dylans-MacBook-Pro-2.local"]; ok {
		t.Error("old name should be gone")
	}
	// alias is the same command.
	m.handlePeer("alias onlyone")
	if !strings.Contains(lastSys(m), "usage") {
		t.Errorf("underspecified rename should show usage: %q", lastSys(m))
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

	// invite: shows the PIN synchronously, returns an async cmd that pairs.
	cmd := m.handlePeer("invite box https://1.2.3.4:9100")
	if !strings.Contains(lastSys(m), "123 456") {
		t.Errorf("invite should display the PIN: %q", lastSys(m))
	}
	if cmd == nil {
		t.Fatal("invite should return an async pairing cmd")
	}
	if msg, ok := cmd().(sysChatMsg); !ok || !strings.Contains(msg.text, "paired with box") {
		t.Errorf("running the invite cmd should report success; got %+v", cmd())
	}
	if _, ok := f.peers["box"]; !ok {
		t.Error("invite cmd should have stored the new peer")
	}

	// invite usage when underspecified.
	m.handlePeer("invite onlyname")
	if !strings.Contains(lastSys(m), "usage") {
		t.Errorf("invite without url should show usage: %q", lastSys(m))
	}
}

func TestHandlePeer_Serve(t *testing.T) {
	f := newFakePeerControl()
	m := newModel(context.Background(), Config{Peer: f})

	// Default address.
	m.handlePeer("serve")
	if f.served != "0.0.0.0:9100" {
		t.Errorf("serve with no addr should use the reachable default, got %q", f.served)
	}
	if !strings.Contains(lastSys(m), "peer listener on") || !strings.Contains(lastSys(m), "prompt here") {
		t.Errorf("serve should confirm the listener + that invites prompt here: %q", lastSys(m))
	}
	// Explicit address.
	m.handlePeer("serve 100.76.238.69:9100")
	if f.served != "100.76.238.69:9100" {
		t.Errorf("serve should bind the given addr, got %q", f.served)
	}
}

func TestHandlePeer_Reconnect(t *testing.T) {
	f := newFakePeerControl("mac") // mac is Dial:true (newFakePeerControl sets Dial)
	m := newModel(context.Background(), Config{Peer: f})
	m.handlePeer("reconnect")
	if f.reconnected != 1 {
		t.Errorf("reconnect should trigger the control once, got %d", f.reconnected)
	}
	if !strings.Contains(lastSys(m), "reconnecting") || !strings.Contains(lastSys(m), "/tools") {
		t.Errorf("reconnect should report + point at /tools: %q", lastSys(m))
	}
	// No dialable peers → guidance.
	empty := newFakePeerControl()
	m2 := newModel(context.Background(), Config{Peer: empty})
	m2.handlePeer("reconnect")
	if !strings.Contains(lastSys(m2), "no dialable peers") {
		t.Errorf("empty reconnect should guide: %q", lastSys(m2))
	}
}

func TestHandlePeer_NilControl(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.handlePeer("")
	if !strings.Contains(lastSys(m), "not available") {
		t.Errorf("nil Peer control should degrade gracefully: %q", lastSys(m))
	}
}
