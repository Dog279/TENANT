package osys

import (
	"context"
	"slices"
	"testing"

	"tenant/internal/logs/event"
	"tenant/internal/logs/event/eventtest"
)

// ps (tasklist on Windows) is filed as a child of the os plugin: its start
// and its exit, child=<name> (TEN-364).
func TestProcessesFilesItsChild(t *testing.T) {
	rec := eventtest.Capture(t)
	svc, _ := Open(Config{})
	if _, err := svc.Processes(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The child's own lines, not its component events (internal/lifecycle).
	got := slices.DeleteFunc(rec.Events(), func(e *event.Event) bool { return e.Log == "system" })
	if len(got) != 2 || got[0].Msg != "process started" || got[1].Msg != "process exited" {
		t.Fatalf("filed %d events: %+v", len(got), got)
	}
	for _, e := range got {
		if child := e.Data[0]; e.Log != "plugin.os" || child.Key != "child" || (child.Text() != "ps" && child.Text() != "tasklist") {
			t.Errorf("filed %s %v", e.Log, e.Data)
		}
	}
}
