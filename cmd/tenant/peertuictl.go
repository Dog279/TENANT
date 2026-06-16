package main

// peertuictl.go adapts the federation peer store to tui.PeerControl — the
// `/peer` (list/show/remove) and `/configure peer` (share editor) surfaces
// (TEN-188). It loads peers.json fresh per operation so changes made by the
// `tenant peer` CLI or confirmed mid-session by the listener are reflected; a
// SetShare write lands on the in-process listener via its mtime-cached reload.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

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

// Invite implements TEN-239 push-pairing for the TUI: mint a PIN + return a
// run() that POSTs the pairing request (blocking on the peer's Approve/Deny) and
// stores the peer on success. run is meant to run off the UI goroutine.
func (p peerTUIControl) Invite(label, url string) (string, func(context.Context) (string, error), error) {
	pin, err := peering.GeneratePIN()
	if err != nil {
		return "", nil, err
	}
	lc, err := loadLaunchConfig(p.cfgDir)
	if err != nil {
		return "", nil, err
	}
	id := strings.TrimSpace(lc.InstanceID)
	if id == "" {
		if id, err = peering.NewInstanceID(); err != nil {
			return "", nil, err
		}
		lc.InstanceID = id
		_ = lc.save(p.cfgDir)
	}
	overlay := lc.Peer.Transport == "overlay"
	selfName, _ := os.Hostname()
	if selfName == "" {
		selfName = "tenant"
	}
	run := func(ctx context.Context) (string, error) {
		reqCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		pr, err := peering.RequestPair(reqCtx, url, peering.PairRequest{Name: selfName, InstanceID: id, PIN: pin}, overlay)
		if err != nil {
			return "", err
		}
		s, err := peering.LoadStore(p.cfgDir)
		if err != nil {
			return "", err
		}
		if err := s.Put(&peering.Peer{
			Name:        label,
			InstanceID:  pr.InstanceID,
			URL:         url,
			Dial:        true,
			Token:       pr.Token,
			Fingerprint: pr.Fingerprint,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return "", err
		}
		return fmt.Sprintf("paired with %s (%s)", label, pr.Name), nil
	}
	return peering.FormatPIN(pin), run, nil
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
