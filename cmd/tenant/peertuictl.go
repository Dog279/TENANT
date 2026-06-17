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

type peerTUIControl struct {
	cfgDir string
	// serve (re)starts the in-process peer listener bound to an address and
	// returns the bound address. Injected by cmdTUI (it captures the live
	// stores/broker). nil ⇒ /peer serve is unavailable in this session.
	serve func(addr string) (string, error)
	// reconnect (re)dials every paired peer and brings their shared tools live
	// for the running agent (the same path used at launch). Injected by cmdTUI
	// (it captures the live tool mux). nil ⇒ unavailable.
	reconnect func()
}

// Serve (re)starts the peer listener bound to addr (TEN-239 follow-up: in-TUI
// `/peer serve`). Empty addr ⇒ a reachable default. Returns the bound address.
func (p peerTUIControl) Serve(addr string) (string, error) {
	if p.serve == nil {
		return "", fmt.Errorf("peer serve isn't available in this session")
	}
	return p.serve(addr)
}

// Reconnect re-dials paired peers so their shared tools come live in the running
// agent (without a relaunch). Returns how many dialable peers it's connecting.
func (p peerTUIControl) Reconnect() (int, error) {
	if p.reconnect == nil {
		return 0, fmt.Errorf("peer reconnect isn't available in this session")
	}
	s, err := peering.LoadStore(p.cfgDir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pr := range s.List() {
		if pr.Dial && pr.URL != "" && pr.Token != "" {
			n++
		}
	}
	p.reconnect()
	return n, nil
}

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

func (p peerTUIControl) Rename(old, newName string) error {
	s, err := peering.LoadStore(p.cfgDir)
	if err != nil {
		return err
	}
	return s.Rename(old, newName)
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
		// Bring the new peer's shared tools live in the running agent immediately
		// (no relaunch needed) — dials it + adopts peer_wiki_search/peer_memory_search.
		if p.reconnect != nil {
			p.reconnect()
		}
		return fmt.Sprintf("paired with %s (%s) — connecting to its shared tools (run /tools shortly to see peer_*)", label, pr.Name), nil
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
