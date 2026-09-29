package agent

import (
	"context"
	"path"
)

// A turn run for a scheduled job has nobody to approve sensitive egress, so
// ConfirmSensitiveEgress fails closed there. A job whose purpose is to move
// such data (a ticket note written from an email, a command copied from a
// runbook) names the tools it may do that with, ahead of time, in config.json
// (the cron job's egress_allow), and the runner carries that list on the
// turn's context.

type egressAllowKey struct{}

// WithEgressAllow returns ctx carrying patterns: path.Match globs on tool
// names ("os_exec", "sdp_*"). An outbound call to a matching tool whose
// arguments carry sensitive data runs without the confirmation; the flow is
// still recorded, and each such call is logged as EgressPreapproved.
func WithEgressAllow(ctx context.Context, patterns []string) context.Context {
	if len(patterns) == 0 {
		return ctx
	}
	return context.WithValue(ctx, egressAllowKey{}, append([]string(nil), patterns...))
}

// egressPreapproved returns the pattern on ctx that approves tool's egress.
func egressPreapproved(ctx context.Context, tool string) (string, bool) {
	patterns, _ := ctx.Value(egressAllowKey{}).([]string)
	for _, p := range patterns {
		if ok, _ := path.Match(p, tool); ok {
			return p, true
		}
	}
	return "", false
}
