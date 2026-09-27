package store

import (
	"context"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
)

// Clear empties one log (events, full text, rollups, bytes), notes when, and
// leaves the others and the Security log alone; Head is the newest seq.
func TestClearHeadAndLogs(t *testing.T) {
	ctx := context.Background()
	s := open(t, tempDB(t), "")
	if h, err := s.Head(ctx); err != nil || h.Seq != 0 || h.StoreID != s.StoreID() {
		t.Fatalf("empty Head = %v, %v", h, err)
	}
	batch := []event.Event{ev("a", "one"), ev("a", "two"), ev("b", "three"), ev(SecurityLog, "four")}
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.Head(ctx); h.Seq != batch[3].Seq {
		t.Fatalf("Head = %d, want %d", h.Seq, batch[3].Seq)
	}
	if n, err := s.Clear(ctx, "a"); err != nil || n != 2 {
		t.Fatalf("Clear(a) = %d, %v", n, err)
	}
	if _, err := s.Clear(ctx, SecurityLog); err != ErrProtected {
		t.Fatalf("Clear(security) = %v, want ErrProtected", err)
	}
	if n, err := s.Clear(ctx, "nosuchlog"); err != nil || n != 0 {
		t.Fatalf("Clear(nosuchlog) = %d, %v", n, err)
	}
	f, _ := query.Parse(`"one"`)
	if n, _ := s.Count(ctx, f); n != 0 {
		t.Error("a cleared event is still in the full-text index")
	}
	if bs, _ := s.Stats(ctx, query.Filter{}, 0); len(bs) != 2 || bs[0].Log != "b" {
		t.Errorf("rollups after clear = %+v", bs)
	}
	logs, err := s.Logs(ctx)
	if err != nil || len(logs) != 3 {
		t.Fatalf("Logs = %+v, %v", logs, err)
	}
	a, b := logs[0], logs[1]
	if a.Name != "a" || a.Events != 0 || a.Bytes != 0 || a.Cleared.IsZero() || !a.LastWrite.IsZero() {
		t.Errorf("cleared log = %+v", a)
	}
	if b.Name != "b" || b.Events != 1 || b.LastWrite.IsZero() || !b.Cleared.IsZero() {
		t.Errorf("other log = %+v", b)
	}
}

// The debug switch is on until its time, lists only while on, and turns off.
func TestDebugSwitch(t *testing.T) {
	ctx := context.Background()
	s := open(t, tempDB(t), "")
	if err := s.SetDebug(ctx, "discord", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDebug(ctx, "cron", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	on, err := s.Debugging(ctx)
	if _, ok := on["discord"]; err != nil || len(on) != 1 || !ok {
		t.Fatalf("Debugging = %v, %v", on, err)
	}
	if err := s.SetDebug(ctx, "discord", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.Debugging(ctx); len(on) != 0 {
		t.Errorf("after off: %v", on)
	}
	if p, _ := s.Policy(ctx, DefaultBudget(), "discord"); p != DefaultBudget().For("discord") {
		t.Errorf("the switch gave the log a policy of its own: %+v", p)
	}
}

func TestViewsAndIntegrityCheck(t *testing.T) {
	ctx := context.Background()
	s := open(t, tempDB(t), "")
	if err := s.SaveView(ctx, "Discord errors", "log:discord level>=error"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveView(ctx, "Discord errors", "log:discord level>=warning"); err != nil {
		t.Fatal(err)
	}
	vs, err := s.Views(ctx)
	if err != nil || len(vs) != 1 || vs[0].Filter != "log:discord level>=warning" || vs[0].Created.IsZero() {
		t.Fatalf("Views = %+v, %v", vs, err)
	}
	if ok, err := s.DeleteView(ctx, "Discord errors"); !ok || err != nil {
		t.Fatalf("DeleteView = %v, %v", ok, err)
	}
	if ok, _ := s.DeleteView(ctx, "Discord errors"); ok {
		t.Error("deleted a view twice")
	}
	if problems, err := s.IntegrityCheck(ctx); err != nil || problems != nil {
		t.Errorf("IntegrityCheck = %v, %v", problems, err)
	}
}
