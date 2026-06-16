package main

// peertuictl.go adapts the federation peer store to tui.PeerControl — the
// `/peer` (list/show/remove) and `/configure peer` (share editor) surfaces
// (TEN-188). It loads peers.json fresh per operation so changes made by the
// `tenant peer` CLI or confirmed mid-session by the listener are reflected; a
// SetShare write lands on the in-process listener via its mtime-cached reload.

import (
	"tenant/internal/peering"
	"tenant/internal/tui"
)

type peerTUIControl struct{ cfgDir string }

func newPeerTUIControl(cfgDir string) peerTUIControl { return peerTUIControl{cfgDir: cfgDir} }

func (p peerTUIControl) List() []tui.PeerInfo {
	s, err := peering.LoadStore(p.cfgDir)
	if err != nil {
		return nil
	}
	out := make([]tui.PeerInfo, 0)
	for _, pr := range s.List() {
		out = append(out, peerInfoView(pr))
	}
	return out
}

func (p peerTUIControl) Show(name string) (tui.PeerInfo, bool) {
	s, err := peering.LoadStore(p.cfgDir)
	if err != nil {
		return tui.PeerInfo{}, false
	}
	pr, ok := s.Get(name)
	if !ok {
		return tui.PeerInfo{}, false
	}
	return peerInfoView(pr), true
}

func (p peerTUIControl) Remove(name string) (bool, error) {
	s, err := peering.LoadStore(p.cfgDir)
	if err != nil {
		return false, err
	}
	if _, ok := s.Get(name); !ok {
		return false, nil
	}
	return true, s.Remove(name)
}

func (p peerTUIControl) SetShare(name, capability string, allow bool) (tui.PeerInfo, error) {
	s, err := peering.LoadStore(p.cfgDir)
	if err != nil {
		return tui.PeerInfo{}, err
	}
	if err := s.SetShare(name, capability, allow); err != nil {
		return tui.PeerInfo{}, err
	}
	pr, _ := s.Get(name)
	return peerInfoView(pr), nil
}

// peerInfoView projects a stored peer to the TUI view (never exposes the token
// value — only its state).
func peerInfoView(pr *peering.Peer) tui.PeerInfo {
	ts := "set"
	switch {
	case pr.Token == "" && pr.PendingToken == "":
		ts = "revoked"
	case pr.PendingToken != "":
		ts = "rotating"
	}
	return tui.PeerInfo{
		Name:       pr.Name,
		InstanceID: pr.InstanceID,
		URL:        pr.URL,
		Dial:       pr.Dial,
		TokenState: ts,
		Share: map[string]bool{
			"wiki":   pr.Share.Wiki,
			"memory": pr.Share.Memory,
			"skills": pr.Share.Skills,
			"exec":   pr.Share.Exec,
			"llm":    pr.Share.LLM,
		},
	}
}
