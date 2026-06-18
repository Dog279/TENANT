package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"tenant/internal/tui"
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
	r.recordProbe("desktop", true, 8*time.Millisecond, []string{"wiki", "memory"}, nil)
	r.recordProbe("offsite", false, 0, nil, errors.New("connection refused"))

	byName := map[string]tui.PeerHealth{}
	for _, h := range r.tuiHealth() {
		byName[h.Name] = h
	}
	if byName["work"].State != "alive" { // fresh inbound
		t.Errorf("work = %q, want alive", byName["work"].State)
	}
	if byName["desktop"].State != "alive" { // successful probe
		t.Errorf("desktop = %q, want alive", byName["desktop"].State)
	}
	if byName["offsite"].State != "dead" { // failed probe, no inbound
		t.Errorf("offsite = %q, want dead", byName["offsite"].State)
	}
	// "their share to us" captured from the probe (TEN-251).
	if d := byName["desktop"]; !d.TheirShareKnown || strings.Join(d.TheirShare, ",") != "wiki,memory" {
		t.Errorf("desktop their-share = %v (known=%v), want wiki,memory", d.TheirShare, d.TheirShareKnown)
	}
	// Inbound-only peer: their grant to us is unknown (we never dialed it).
	if byName["work"].TheirShareKnown {
		t.Errorf("work their-share should be unknown (inbound-only)")
	}
}

// TestRecordProbe_KeepsLastCapsOnFailure: a transient probe failure must not
// blank the last-known grant.
func TestRecordProbe_KeepsLastCapsOnFailure(t *testing.T) {
	r := newPeerHealthRegistry()
	r.recordProbe("p", true, time.Millisecond, []string{"wiki"}, nil)
	r.recordProbe("p", false, 0, nil, errors.New("timeout")) // transient
	var h tui.PeerHealth
	for _, x := range r.tuiHealth() {
		if x.Name == "p" {
			h = x
		}
	}
	if !h.TheirShareKnown || strings.Join(h.TheirShare, ",") != "wiki" {
		t.Fatalf("last-known grant lost on failure: %v (known=%v)", h.TheirShare, h.TheirShareKnown)
	}
	if h.State != "dead" {
		t.Fatalf("state = %q, want dead", h.State)
	}
}

func TestParseHelloCaps(t *testing.T) {
	got := parseHelloCaps(`{"instance_id":"x","capabilities":["wiki","memory","skills"],"you":"me"}`)
	if strings.Join(got, ",") != "wiki,memory,skills" {
		t.Fatalf("caps = %v", got)
	}
	if parseHelloCaps("not json") != nil {
		t.Fatal("garbage should parse to nil (unknown), not panic")
	}
	// DoS guards: oversized raw blob and oversized array are refused (→ nil).
	huge := `{"capabilities":["` + strings.Repeat("x", 9000) + `"]}`
	if parseHelloCaps(huge) != nil {
		t.Fatal("oversized raw stamp should be refused")
	}
	manyEntries := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		manyEntries = append(manyEntries, `"a"`)
	}
	if parseHelloCaps(`{"capabilities":[`+strings.Join(manyEntries, ",")+`]}`) != nil {
		t.Fatal("over-count capabilities array should be refused")
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
			r.recordProbe("b", i%2 == 0, time.Millisecond, []string{"wiki"}, nil)
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = r.tuiHealth()
	}
	<-done
}
