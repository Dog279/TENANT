package improve_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"tenant/internal/improve"
)

// --- CronStore tests ---

func TestCronStore_CreateListRoundtrip(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	entry := &improve.CronEntry{
		ID:       "test-1",
		Name:     "test job",
		Schedule: 5 * time.Minute,
		Command:  "run test",
	}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries, err := cs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List count = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.ID != "test-1" {
		t.Fatalf("ID = %q, want test-1", e.ID)
	}
	if e.Name != "test job" {
		t.Fatalf("Name = %q, want test job", e.Name)
	}
	if e.Schedule != 5*time.Minute {
		t.Fatalf("Schedule = %v, want 5m", e.Schedule)
	}
	if e.Command != "run test" {
		t.Fatalf("Command = %q, want run test", e.Command)
	}
	if !e.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if e.LastRun != nil {
		t.Fatal("LastRun should be nil for new entry")
	}
}

func TestCronStore_UpdatePersists(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	entry := &improve.CronEntry{
		ID:       "upd-1",
		Name:     "original",
		Schedule: 10 * time.Minute,
		Command:  "old",
	}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Create mutates entry (sets Enabled=true, NextRun, timestamps),
	// so we update the caller's copy and persist.
	entry.Name = "updated"
	entry.Schedule = 20 * time.Minute
	entry.Command = "new"
	entry.Enabled = false
	if err := cs.Update(entry); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := cs.Get("upd-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "updated" {
		t.Fatalf("Name = %q, want updated", got.Name)
	}
	if got.Schedule != 20*time.Minute {
		t.Fatalf("Schedule = %v, want 20m", got.Schedule)
	}
	if got.Command != "new" {
		t.Fatalf("Command = %q, want new", got.Command)
	}
	if got.Enabled {
		t.Fatal("Enabled = true, want false")
	}
}

func TestCronStore_DeleteRemoves(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	entry := &improve.CronEntry{
		ID:       "del-1",
		Name:     "delete me",
		Schedule: 1 * time.Minute,
		Command:  "cmd",
	}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := cs.Delete("del-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = cs.Get("del-1")
	if err != improve.ErrCronNotFound {
		t.Fatalf("Get after Delete: got %v, want ErrCronNotFound", err)
	}

	entries, err := cs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("List after delete = %d, want 0", len(entries))
	}
}

func TestCronStore_ListDueRespectsSchedule(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	// Very short schedule — should be due on creation since NextRun
	// ≈ now in unix-second terms.
	entry := &improve.CronEntry{
		ID:       "due",
		Name:     "due job",
		Schedule: 1 * time.Nanosecond,
		Command:  "cmd",
	}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	due, err := cs.ListDue()
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("ListDue count = %d, want 1", len(due))
	}
	if due[0].ID != "due" {
		t.Fatalf("ListDue returned wrong entry: %q", due[0].ID)
	}
}

func TestCronStore_ListDueSkipsDisabled(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	// Create enabled entry (default)
	enabled := &improve.CronEntry{
		ID:       "en",
		Name:     "enabled",
		Schedule: 1 * time.Nanosecond,
		Command:  "cmd",
	}
	if err := cs.Create(enabled); err != nil {
		t.Fatalf("Create enabled: %v", err)
	}

	// Create disabled entry — Create sets enabled=true, so disable after.
	disabled := &improve.CronEntry{
		ID:       "dis",
		Name:     "disabled",
		Schedule: 1 * time.Nanosecond,
		Command:  "cmd",
		Enabled:  false,
	}
	if err := cs.Create(disabled); err != nil {
		t.Fatalf("Create disabled: %v", err)
	}
	disabled.Enabled = false
	if err := cs.Update(disabled); err != nil {
		t.Fatalf("Update to disable: %v", err)
	}

	due, err := cs.ListDue()
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("ListDue count = %d, want 1", len(due))
	}
	if due[0].ID != "en" {
		t.Fatal("ListDue returned disabled entry")
	}
}

// --- CronJob tests ---

func TestCronJob_ImplementsJobInterface(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	entry := &improve.CronEntry{
		ID:       "iface",
		Name:     "interface test",
		Schedule: 1 * time.Minute,
		Command:  "cmd",
	}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Compile-time interface check.
	var _ improve.Job = (*improve.CronJob)(nil)

	job := improve.NewCronJob(entry, cs, func(ctx context.Context, e *improve.CronEntry) (improve.JobResult, error) {
		return improve.JobResult{Summary: "ok"}, nil
	})

	if job.Name() != "cron:iface" {
		t.Fatalf("Name() = %q, want cron:iface", job.Name())
	}
}

func TestCronJob_UpdatesLastRunOnExecution(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	entry := &improve.CronEntry{
		ID:       "lr",
		Name:     "last run",
		Schedule: 1 * time.Minute,
		Command:  "cmd",
	}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify LastRun is nil initially.
	got, _ := cs.Get("lr")
	if got.LastRun != nil {
		t.Fatal("Initial LastRun should be nil")
	}

	job := improve.NewCronJob(entry, cs, func(ctx context.Context, e *improve.CronEntry) (improve.JobResult, error) {
		return improve.JobResult{Summary: "ran"}, nil
	})

	_, err = job.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify LastRun is now set in the store.
	got, err = cs.Get("lr")
	if err != nil {
		t.Fatalf("Get after run: %v", err)
	}
	if got.LastRun == nil {
		t.Fatal("LastRun still nil after successful Run")
	}
}

func TestCronJob_ErrorDoesNotPreventNextRun(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	entry := &improve.CronEntry{
		ID:       "err",
		Name:     "error test",
		Schedule: 1 * time.Minute,
		Command:  "cmd",
	}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}

	attempts := 0
	job := improve.NewCronJob(entry, cs, func(ctx context.Context, e *improve.CronEntry) (improve.JobResult, error) {
		attempts++
		if attempts == 1 {
			return improve.JobResult{}, fmt.Errorf("boom")
		}
		return improve.JobResult{Summary: "ok"}, nil
	})

	// First run errors.
	_, err = job.Run(context.Background())
	if err == nil {
		t.Fatal("expected error on first run")
	}

	// Second run should still succeed (job struct not corrupted by prior error).
	res, err := job.Run(context.Background())
	if err != nil {
		t.Fatalf("second run should succeed: %v", err)
	}
	if res.Summary != "ok" {
		t.Fatalf("summary = %q, want ok", res.Summary)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// --- hardening coverage ---

func TestCronStore_UpdateMissingReturnsNotFound(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	err = cs.Update(&improve.CronEntry{ID: "ghost", Name: "n", Command: "c", Schedule: time.Minute})
	if err != improve.ErrCronNotFound {
		t.Fatalf("Update missing id: got %v, want ErrCronNotFound", err)
	}
}

func TestCronStore_GetMissingReturnsNotFound(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	if _, err := cs.Get("nope"); err != improve.ErrCronNotFound {
		t.Fatalf("Get missing id: got %v, want ErrCronNotFound", err)
	}
}

func TestCronStore_CreateValidation(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	cases := []struct {
		name  string
		entry *improve.CronEntry
	}{
		{"nil entry", nil},
		{"empty ID", &improve.CronEntry{Name: "n", Command: "c", Schedule: time.Minute}},
		{"empty Name", &improve.CronEntry{ID: "i", Command: "c", Schedule: time.Minute}},
		{"empty Command", &improve.CronEntry{ID: "i", Name: "n", Schedule: time.Minute}},
		{"zero Schedule", &improve.CronEntry{ID: "i", Name: "n", Command: "c"}},
		{"negative Schedule", &improve.CronEntry{ID: "i", Name: "n", Command: "c", Schedule: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := cs.Create(tc.entry); err == nil {
				t.Fatalf("Create(%s) = nil, want error", tc.name)
			}
		})
	}
}

// A due job that fails must be rescheduled into the future (NextRun += Schedule),
// not left due — otherwise the scheduler hot-loops it every tick.
func TestCronJob_FailingDueJobIsRescheduledNotHotLooped(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	entry := &improve.CronEntry{ID: "loop", Name: "n", Command: "c", Schedule: time.Hour}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Force the job due now.
	entry.NextRun = time.Now().Add(-2 * time.Hour)
	if err := cs.Update(entry); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if due, _ := cs.ListDue(); len(due) != 1 {
		t.Fatalf("precondition: want 1 due, got %d", len(due))
	}

	job := improve.NewCronJob(entry, cs, func(ctx context.Context, e *improve.CronEntry) (improve.JobResult, error) {
		return improve.JobResult{}, fmt.Errorf("boom")
	})
	if _, err := job.Run(context.Background()); err == nil {
		t.Fatal("expected error from failing run")
	}

	if due, _ := cs.ListDue(); len(due) != 0 {
		t.Fatalf("failing job still due after run (hot-loop): %d due", len(due))
	}
	got, err := cs.Get("loop")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRun == nil {
		t.Fatal("LastRun should be set even after a failed run")
	}
}

func TestCronStore_LastRunRoundTrips(t *testing.T) {
	cs, err := improve.OpenCronStore(":memory:")
	if err != nil {
		t.Fatalf("OpenCronStore: %v", err)
	}
	defer cs.Close()

	entry := &improve.CronEntry{ID: "lrt", Name: "n", Command: "c", Schedule: time.Minute}
	if err := cs.Create(entry); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ts := time.Now().Add(-time.Hour).Truncate(time.Second)
	entry.LastRun = &ts
	if err := cs.Update(entry); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := cs.Get("lrt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRun == nil || !got.LastRun.Equal(ts) {
		t.Fatalf("LastRun round-trip = %v, want %v", got.LastRun, ts)
	}
}
