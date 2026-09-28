package logs

import (
	"context"
	"fmt"
	"strconv"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// audited reports how a Security record's commit ended (docs/design/logging-v2.md
// §10.3). A failure files a critical system event, raises the operator's
// banner (OnAlert) once until records commit again, and, under
// logs.security.on_failure: deny, is marked event.ErrRefused so the action
// the record gates is refused (event.Def.Gate). The next commit that works
// ends the outage.
func (h *Hub) audited(ctx context.Context, e *event.Event, err error) error {
	if err == nil {
		if n := h.auditFails.Swap(0); n > 0 {
			catalog.AuditRecovered.Log(ctx, "failed", n)
			h.Notify(fmt.Sprintf("✅ security log: records are written again (%d were not; the text mirror has them)", n))
		}
		return nil
	}
	deny := h.opts.AuditDeny != nil && h.opts.AuditDeny()
	policy := "continue"
	if deny {
		policy = "deny"
	}
	catalog.AuditFailed.Log(ctx, "event", e.Provider+"/"+strconv.FormatUint(uint64(e.ID), 10), "policy", policy, "err", err)
	if h.auditFails.Add(1) == 1 {
		effect := "actions go ahead unrecorded"
		if deny {
			effect = "gated actions are refused until records are written again"
		}
		h.Notify(fmt.Sprintf("⚠ security log: an audit record could not be written (%v); %s (logs.security.on_failure: %s)", err, effect, policy))
	}
	if deny {
		return fmt.Errorf("%w: %w", event.ErrRefused, err)
	}
	return err
}
