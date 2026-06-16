package main

import (
	"strings"
	"testing"
)

// TEN-185 (2A+5A): the legacy --sse-addr gateway has no auth, so a non-loopback
// bind must be refused unless --insecure-lan is passed. REGRESSION-critical:
// loopback binds must stay allowed (unchanged behavior).
func TestCheckSSEBindPolicy(t *testing.T) {
	cases := []struct {
		addr        string
		insecureLAN bool
		wantErr     bool
	}{
		{"", false, false},                 // stdio mode (no sse) → allowed
		{"127.0.0.1:8765", false, false},   // REGRESSION: loopback unchanged
		{"localhost:8765", false, false},   // loopback name unchanged
		{"[::1]:8765", false, false},       // ipv6 loopback unchanged
		{"0.0.0.0:8765", false, true},      // all-interfaces → refused
		{":8765", false, true},             // empty host = all interfaces → refused
		{"192.168.1.10:8765", false, true}, // LAN → refused
		{"192.168.1.10:8765", true, false}, // ...but allowed with --insecure-lan
		{"0.0.0.0:8765", true, false},      // explicit opt-in allows all-interfaces
	}
	for _, tc := range cases {
		err := checkSSEBindPolicy(tc.addr, tc.insecureLAN)
		if tc.wantErr && err == nil {
			t.Errorf("checkSSEBindPolicy(%q, insecureLAN=%v) = nil, want refusal", tc.addr, tc.insecureLAN)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("checkSSEBindPolicy(%q, insecureLAN=%v) = %v, want ok", tc.addr, tc.insecureLAN, err)
		}
		if tc.wantErr && err != nil && !strings.Contains(err.Error(), "insecure-lan") {
			t.Errorf("refusal for %q should name the --insecure-lan flag; got %q", tc.addr, err.Error())
		}
	}
}
