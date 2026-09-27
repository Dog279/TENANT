package osys

import (
	"context"
	"testing"

	"tenant/internal/logs/event"
)

type filed []*event.Event

// Emit keeps the child's own lines, not its component events (internal/lifecycle).
func (f *filed) Emit(_ context.Context, e *event.Event) {
	if e.Log != "system" {
		*f = append(*f, e)
	}
}
func (f *filed) Audit(ctx context.Context, e *event.Event) error { f.Emit(ctx, e); return nil }

// ps (tasklist on Windows) is filed as a child of the os plugin: its start
// and its exit, child=<name> (TEN-364).
func TestProcessesFilesItsChild(t *testing.T) {
	var got filed
	event.Install(&got)
	t.Cleanup(func() { event.Install(nil) })
	svc, _ := Open(Config{})
	if _, err := svc.Processes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Msg != "process started" || got[1].Msg != "process exited" {
		t.Fatalf("filed %d events: %+v", len(got), got)
	}
	for _, e := range got {
		if child := e.Data[0]; e.Log != "plugin.os" || child.Key != "child" || (child.Text() != "ps" && child.Text() != "tasklist") {
			t.Errorf("filed %s %v", e.Log, e.Data)
		}
	}
}
