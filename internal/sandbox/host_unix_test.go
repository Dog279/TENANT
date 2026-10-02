//go:build !windows

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TEN-490 groundwork (TEN-489): on the host a backgrounded child can't hang
// the call, a /dev/tty reader fails at once (no controlling terminal, so sudo
// can't grab the operator's), and the timeout takes the whole group down.
func TestHostRunDetached(t *testing.T) {
	be := Host()
	for _, tc := range []struct {
		cmd  string
		wall time.Duration
		max  time.Duration
		want string
	}{
		{"sleep 30 & echo started", 0, 5 * time.Second, "started"},
		{"cat /dev/tty || echo no-tty", 0, 5 * time.Second, "no-tty"},
		{"(sleep 30; echo leaked) & sleep 30", 300 * time.Millisecond, 5 * time.Second, ""},
	} {
		start := time.Now()
		res, err := be.Run(context.Background(), Command{Shell: tc.cmd}, Policy{Limits: Limits{Wall: tc.wall}})
		if err != nil {
			t.Fatalf("%q: %v", tc.cmd, err)
		}
		if took := time.Since(start); took > tc.max || !strings.Contains(res.Output, tc.want) || strings.Contains(res.Output, "leaked") {
			t.Errorf("%q: took %s, output %q", tc.cmd, took, res.Output)
		}
	}
}
