package catalog

import (
	"context"
	"errors"
	"strings"
	"testing"

	"tenant/internal/logs/event"
)

type audits []*event.Event

func (a *audits) Emit(context.Context, *event.Event) {}
func (a *audits) Audit(_ context.Context, e *event.Event) error {
	*a = append(*a, e)
	return nil
}

// AuditLogAdmin files one Security record per action on the log book: which
// log, what was done, by whom and from where. With no log book to take it,
// it says so rather than pretending.
func TestAuditLogAdminFilesTheAction(t *testing.T) {
	ctx := context.Background()
	if err := AuditLogAdmin(ctx, LogCleared, "turns", "412 events", "operator", "cli"); !errors.Is(err, event.ErrNoEmitter) {
		t.Fatalf("with no emitter: %v", err)
	}
	var got audits
	event.Install(&got)
	t.Cleanup(func() { event.Install(nil) })
	for _, a := range []struct {
		d            *event.Def
		log, details string
	}{
		{LogCleared, "turns", "412 events"},
		{LogReconfigured, "cron", "max_age_days 30 → 7"},
		{LogExported, "discord", "ndjson, 90 events"},
		{FieldRevealed, "cron", "event 1234, field output"},
		{LogVerified, "security", "7 records"},
		{LogVerifyFailed, "security", "record 5 (logbook.db): hash mismatch"},
	} {
		if err := AuditLogAdmin(ctx, a.d, a.log, a.details, "operator", "dashboard"); err != nil {
			t.Fatal(err)
		}
	}
	var lines []string
	for _, e := range got {
		lines = append(lines, e.Log+" "+strings.Join(e.Keywords, ",")+" "+e.Msg)
	}
	want := []string{
		"security audit.success Log turns cleared: 412 events",
		"security audit.success,config Log cron reconfigured: max_age_days 30 → 7",
		"security audit.success Log discord exported: ndjson, 90 events",
		"security audit.success Sensitive field revealed in cron: event 1234, field output",
		"security audit.success Log security verified: 7 records",
		"security audit.failure Log security failed verification: record 5 (logbook.db): hash mismatch",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// actor and surface are the record's own attributes, not data.
	if e := got[0]; len(e.Data) != 2 || e.Actor != "operator" || e.Surface != "dashboard" {
		t.Errorf("fields %+v, actor %q, surface %q", e.Data, e.Actor, e.Surface)
	}
}
