package model

import "testing"

func TestRiskLevel_Derivation(t *testing.T) {
	cases := []struct {
		name string
		spec ToolSpec
		want RiskTier
	}{
		{"ungated → read", ToolSpec{Name: "web_search"}, RiskRead},
		{"gated → write (derived)", ToolSpec{Name: "gmail_send", Gated: true}, RiskWrite},
		{"explicit exec wins", ToolSpec{Name: "os_exec", Gated: true, Risk: RiskExec}, RiskExec},
		{"explicit destructive wins", ToolSpec{Name: "x_delete", Gated: true, Risk: RiskDestructive}, RiskDestructive},
	}
	for _, c := range cases {
		if got := c.spec.RiskLevel(); got != c.want {
			t.Errorf("%s: RiskLevel()=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestRiskTier_ParseAndString(t *testing.T) {
	for _, s := range []string{"read", "write", "exec", "destructive"} {
		tier, ok := ParseRiskTier(s)
		if !ok {
			t.Fatalf("ParseRiskTier(%q) should be ok", s)
		}
		if tier.String() != s {
			t.Errorf("round-trip: %q → %v → %q", s, tier, tier.String())
		}
	}
	if _, ok := ParseRiskTier("bogus"); ok {
		t.Error("unknown tier must not parse")
	}
	// ordering: read < write < exec < destructive
	if !(RiskRead < RiskWrite && RiskWrite < RiskExec && RiskExec < RiskDestructive) {
		t.Error("tiers must be ordered read < write < exec < destructive")
	}
}
