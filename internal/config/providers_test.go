package config

import (
	"testing"

	"tenant/internal/model/backend/claudecode"
)

// config keeps its own copy of the claudecode discovery sentinel so it need
// not import a model backend; the two must never drift.
func TestClaudeCodeSentinelMatchesTheBackend(t *testing.T) {
	if ClaudeCodeAutoEndpoint != claudecode.AutoEndpoint {
		t.Fatalf("ClaudeCodeAutoEndpoint = %q, backend says %q", ClaudeCodeAutoEndpoint, claudecode.AutoEndpoint)
	}
	if got := SafeDisplayEndpoint(claudecode.AutoEndpoint); got != claudecode.AutoEndpoint {
		t.Fatalf("the sentinel displays as %q; it is not a secret", got)
	}
	if got := ProviderKinds["claudecode"].DefaultEndpoint; got != claudecode.AutoEndpoint {
		t.Fatalf("the claudecode kind's endpoint is %q", got)
	}
}

func TestDashboardIsEnabledByDefault(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		enabled *bool
		want    bool
	}{{nil, true}, {&on, true}, {&off, false}} {
		if got := (Dashboard{Enabled: tc.enabled}).IsEnabled(); got != tc.want {
			t.Errorf("IsEnabled with %v = %v, want %v", tc.enabled, got, tc.want)
		}
	}
}
