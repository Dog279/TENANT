package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tenant/internal/model"
	"tenant/internal/peering"
	"tenant/internal/plugins/mcpremote"
	"tenant/internal/tui"
)

// peerheartbeat.go (TEN-250): peer liveness for /peer. Two signals, because the
// topology is asymmetric (one side dials, one serves):
//   - OUTBOUND: a background prober dials each dialable peer's peer_hello on a
//     cadence → definitive alive/dead + latency.
//   - INBOUND: the listener's OnAuth hook records when a peer last authenticated
//     a request against us → "alive (inbound Ns ago)".
//
// Each side's outbound probe IS the other side's inbound signal, so liveness is
// mutual: a peer we can't dial still shows alive when it keeps reaching us.

const (
	peerProbeInterval = 30 * time.Second
	peerProbeTimeout  = 8 * time.Second
	// An inbound auth within this window ⇒ the peer is alive even if we don't
	// dial it; beyond it we can't tell (we don't probe inbound peers) ⇒ unknown.
	peerInboundFresh = 2 * time.Minute
)

// peerHealthState holds the raw liveness signals for one peer.
type peerHealthState struct {
	outProbed   bool          // an outbound probe has run at least once
	outAlive    bool          // last outbound probe succeeded
	outLatency  time.Duration // last probe round-trip
	outErr      string        // last probe error (when !outAlive)
	outLastSeen time.Time     // last SUCCESSFUL outbound probe
	inLastSeen  time.Time     // last inbound auth from this peer
}

// peerHealthRegistry is the in-memory liveness store, written by the prober
// (outbound) and the listener OnAuth hook (inbound), read by /peer. Safe for
// concurrent use.
type peerHealthRegistry struct {
	mu sync.Mutex
	m  map[string]*peerHealthState
}

func newPeerHealthRegistry() *peerHealthRegistry {
	return &peerHealthRegistry{m: map[string]*peerHealthState{}}
}

func (r *peerHealthRegistry) entryLocked(name string) *peerHealthState {
	s := r.m[name]
	if s == nil {
		s = &peerHealthState{}
		r.m[name] = s
	}
	return s
}

// markInbound records an inbound auth from name (listener OnAuth; cheap).
func (r *peerHealthRegistry) markInbound(name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	r.entryLocked(name).inLastSeen = time.Now()
	r.mu.Unlock()
}

// recordProbe records an outbound probe result.
func (r *peerHealthRegistry) recordProbe(name string, alive bool, latency time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.entryLocked(name)
	s.outProbed = true
	s.outAlive = alive
	s.outLatency = latency
	if err != nil {
		s.outErr = err.Error()
	} else {
		s.outErr = ""
	}
	if alive {
		s.outLastSeen = time.Now()
	}
}

// tuiHealth derives the displayable liveness view for every peer the registry
// has a signal for. /peer renders missing peers as "unknown". The lock is held
// ONLY to copy the raw states (a value copy per peer); the formatting in
// derivePeerHealth runs lock-free so a /peer read never contends the listener's
// inbound-auth hook or a probe write.
func (r *peerHealthRegistry) tuiHealth() []tui.PeerHealth {
	type namedState struct {
		name string
		s    peerHealthState
	}
	r.mu.Lock()
	snap := make([]namedState, 0, len(r.m))
	for name, s := range r.m {
		snap = append(snap, namedState{name: name, s: *s})
	}
	r.mu.Unlock()

	now := time.Now()
	out := make([]tui.PeerHealth, 0, len(snap))
	for i := range snap {
		out = append(out, derivePeerHealth(snap[i].name, &snap[i].s, now))
	}
	return out
}

// derivePeerHealth turns raw signals into a state + human detail. Outbound
// probes are authoritative (we actively tested reachability); otherwise a recent
// inbound auth means alive; a stale/absent signal is unknown (we can't probe a
// peer that only dials us, so it's never reported "dead").
func derivePeerHealth(name string, s *peerHealthState, now time.Time) tui.PeerHealth {
	h := tui.PeerHealth{Name: name}
	switch {
	case s.outProbed && s.outAlive:
		h.State = "alive"
		h.LastSeenUnix = s.outLastSeen.Unix()
		h.Detail = fmt.Sprintf("seen %s ago (%dms)", humanAgo(s.outLastSeen, now), s.outLatency.Milliseconds())
	case s.outProbed && !s.outAlive:
		h.State = "dead"
		h.Detail = "unreachable"
		if s.outErr != "" {
			h.Detail += ": " + shortErr(s.outErr)
		}
		// surface inbound as a hint if the peer still reaches us
		if !s.inLastSeen.IsZero() && now.Sub(s.inLastSeen) < peerInboundFresh {
			h.State = "alive"
			h.LastSeenUnix = s.inLastSeen.Unix()
			h.Detail = "inbound " + humanAgo(s.inLastSeen, now) + " ago (we can't dial it)"
		}
	case !s.inLastSeen.IsZero() && now.Sub(s.inLastSeen) < peerInboundFresh:
		h.State = "alive"
		h.LastSeenUnix = s.inLastSeen.Unix()
		h.Detail = "inbound " + humanAgo(s.inLastSeen, now) + " ago"
	case !s.inLastSeen.IsZero():
		h.State = "unknown"
		h.LastSeenUnix = s.inLastSeen.Unix()
		h.Detail = "last inbound " + humanAgo(s.inLastSeen, now) + " ago"
	default:
		h.State = "unknown"
		h.Detail = "no contact yet"
	}
	return h
}

func humanAgo(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func shortErr(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

// peerHealthMonitor probes every dialable peer's peer_hello on a cadence and
// records the result. Reloads peers.json each round so newly-paired peers are
// picked up live. Liveness is independent of the model, so it runs even while
// degraded.
type peerHealthMonitor struct {
	cfgDir string
	reg    *peerHealthRegistry
	log    *slog.Logger
}

func (m *peerHealthMonitor) run(ctx context.Context) {
	t := time.NewTicker(peerProbeInterval)
	defer t.Stop()
	m.probeAll(ctx) // probe once immediately so /peer is useful right away
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.probeAll(ctx)
		}
	}
}

func (m *peerHealthMonitor) probeAll(ctx context.Context) {
	store, err := peering.LoadStore(m.cfgDir)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, p := range store.List() {
		// Only peers WE dial can be probed outbound; a peer that only dials us is
		// tracked via the inbound (OnAuth) signal instead.
		if !p.Dial || p.URL == "" || p.Token == "" {
			continue
		}
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			lat, perr := probePeerHello(ctx, p)
			m.reg.recordProbe(p.Name, perr == nil, lat, perr)
		}()
	}
	wg.Wait()
}

// probePeerHello dials a peer and calls peer_hello, returning the round-trip
// latency and any error. A bounded timeout keeps a dead peer from stalling the
// round. The dial authenticates against the peer's listener, so it also drives
// the peer's OWN inbound-seen signal (mutual liveness).
func probePeerHello(ctx context.Context, p *peering.Peer) (time.Duration, error) {
	pctx, cancel := context.WithTimeout(ctx, peerProbeTimeout)
	defer cancel()
	start := time.Now()
	d, cleanup, err := mcpremote.OpenStatic(pctx, mcpremote.StaticConfig{
		ServerURL: p.URL,
		Token:     p.Token,
		Label:     "probe:" + p.Name,
		TLS:       peering.PinnedTLSClientConfig(p.Fingerprint),
	}, mcpremote.Policy{})
	if err != nil {
		return time.Since(start), err
	}
	defer cleanup()
	_, isErr, derr := d.Dispatch(pctx, model.ToolCall{Name: "peer_hello", Arguments: []byte("{}")})
	lat := time.Since(start)
	if derr != nil {
		return lat, derr
	}
	if isErr {
		return lat, fmt.Errorf("peer_hello returned an error")
	}
	return lat, nil
}
