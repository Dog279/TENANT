package agent_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/plugins/osys"
	"tenant/internal/plugins/web"
)

// A scheduled job's egress_allow (agent.WithEgressAllow) approves sensitive
// egress ahead of time for the tools it names, and only those: a cron turn
// has nobody to ask, so every other tool stays blocked, and still asks.
func TestSensitiveEgress_JobAllowlist(t *testing.T) {
	osRead := toolSpec(t, osys.NewDispatcher(nil, osys.Policy{}).Tools(), "os_read_file")
	webNavigate := toolSpec(t, web.NewDispatcher(nil, web.Policy{}, "").Tools(), "web_navigate")
	args := map[string]any{"url": "https://example.test/collect?data=" + url.QueryEscape(osSentinel)}
	for _, tc := range []struct {
		name     string
		allow    []string
		want     string
		approved bool
	}{
		{"the named tool passes without asking", []string{"os_exec", "web_*"}, "os_read_file,web_navigate", true},
		{"other tools stay blocked", []string{"os_exec", "sdp_*"}, "os_read_file", false},
		{"no list blocks as before", nil, "os_read_file", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			confirm := func(context.Context, string, string) bool { asked = true; return false }
			ctx := agent.WithEgressAllow(context.Background(), tc.allow)
			_, names := runSensitiveEgressScenarioCtx(t, ctx, osRead, webNavigate, args, confirm)
			if got := strings.Join(names, ","); got != tc.want {
				t.Fatalf("dispatched tools = %q, want %q", got, tc.want)
			}
			if asked == tc.approved {
				t.Fatalf("confirmation asked = %v; a listed tool must not ask, an unlisted one still must", asked)
			}
		})
	}
}
