package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDerivePeerHealth_States(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name      string
		s         peerHealthState
		wantState string
		detailHas string
	}{
		{
			name:      "outbound alive",
			s:         peerHealthState{outProbed: true, outAlive: true, outLatency: 12 * time.Millisecond, outLastSeen: now.Add(-3 * time.Second)},
			wantState: "alive", detailHas: "seen 3s ago (12ms)",
		},
		{
			name:      "outbound dead",
			s:         peerHealthState{outProbed: true, outAlive: false, outErr: "dial tcp: connection refused"},
			wantState: "dead", detailHas: "unreachable: dial tcp",
		},
		{
			name:      "dead probe but fresh inbound → alive (we can't dial it)",
			s:         peerHealthState{outProbed: true, outAlive: false, outErr: "timeout", inLastSeen: now.Add(-5 * time.Second)},
			wantState: "alive", detailHas: "inbound 5s ago (we can't dial it)",
		},
		{
			name:      "inbound-only fresh → alive",
			s:         peerHealthState{inLastSeen: now.Add(-10 * time.Second)},
			wantState: "alive", detailHas: "inbound 10s ago",
		},
		{
			name:      "inbound stale → unknown",
			s:         peerHealthState{inLastSeen: now.Add(-10 * time.Minute)},
			wantState: "unknown", detailHas: "last inbound 10m ago",
		},
		{
			name:      "no signal → unknown",
			s:         peerHealthState{},
			wantState: "unknown", detailHas: "no contact yet",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			h := derivePeerHealth(tc.name, &s, now)
			if h.State != tc.wantState {
				t.Fatalf("state = %q, want %q", h.State, tc.wantState)
			}
			if !strings.Contains(h.Detail, tc.detailHas) {
				t.Fatalf("detail = %q, want contains %q", h.Detail, tc.detailHas)
			}
		})
	}
}

func TestPeerHealthRegistry_RecordAndDerive(t *testing.T) {
	r := newPeerHealthRegistry()
	r.markInbound("work")
	r.recordProbe("desktop", true, 8*time.Millisecond, nil)
	r.recordProbe("offsite", false, 0, errors.New("connection refused"))

	byName := map[string]string{}
	for _, h := range r.tuiHealth() {
		byName[h.Name] = h.State
	}
	if byName["work"] != "alive" { // fresh inbound
		t.Errorf("work = %q, want alive", byName["work"])
	}
	if byName["desktop"] != "alive" { // successful probe
		t.Errorf("desktop = %q, want alive", byName["desktop"])
	}
	if byName["offsite"] != "dead" { // failed probe, no inbound
		t.Errorf("offsite = %q, want dead", byName["offsite"])
	}
}

// TestPeerHealthRegistry_Concurrent: writers (inbound hook + prober) and the
// reader (/peer) don't race.
func TestPeerHealthRegistry_Concurrent(t *testing.T) {
	r := newPeerHealthRegistry()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			r.markInbound("a")
			r.recordProbe("b", i%2 == 0, time.Millisecond, nil)
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = r.tuiHealth()
	}
	<-done
}
