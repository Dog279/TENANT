package orchestra

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDefaultBudgetsAreBounded(t *testing.T) {
	b := DefaultBudgets()
	if b.MaxConcurrentSubagents <= 0 || b.MaxAgentsPerRun <= 0 ||
		b.MaxToolCallsPerTurn <= 0 || b.MaxToolCallsPerRun <= 0 ||
		b.MaxTokensPerRun <= 0 || b.MaxCostUSD <= 0 || b.MaxWallTime <= 0 ||
		b.MaxBrowserInstances <= 0 || b.BusHistoryCap <= 0 {
		t.Fatalf("every default must be a positive hard cap: %+v", b)
	}
}

func TestGovernorRefusesConcurrentAndTotalAgentCaps(t *testing.T) {
	g := NewGovernor(Budgets{MaxConcurrentSubagents: 1, MaxAgentsPerRun: 2}, nil)
	g.Begin(context.Background())
	_, release1, err := g.AcquireAgent()
	if err != nil {
		t.Fatalf("first agent: %v", err)
	}
	if _, _, err := g.AcquireAgent(); err == nil || !strings.Contains(err.Error(), "concurrent subagents") {
		t.Fatalf("second concurrent spawn should be clearly refused, got %v", err)
	}
	release1()
	_, release2, err := g.AcquireAgent()
	if err != nil {
		t.Fatalf("second total agent: %v", err)
	}
	release2()
	if _, _, err := g.AcquireAgent(); err == nil || !strings.Contains(err.Error(), "agents per run") {
		t.Fatalf("third total spawn should be clearly refused, got %v", err)
	}
}

func TestGovernorEnsureIsAtomicAcrossParallelSpawns(t *testing.T) {
	g := NewGovernor(Budgets{}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Ensure(context.Background())
		}()
	}
	wg.Wait()
	if got := g.Status().RunID; got != 1 {
		t.Fatalf("parallel Ensure started %d runs, want exactly 1", got)
	}
}

func TestOldRunReleaseCannotDecrementNewRun(t *testing.T) {
	g := NewGovernor(Budgets{}, nil)
	g.Begin(context.Background())
	_, releaseOld, err := g.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	g.Begin(context.Background())
	_, releaseNew, err := g.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNew()
	releaseOld()
	if got := g.Status().ActiveSubagents; got != 1 {
		t.Fatalf("old release changed new run accounting: active=%d want 1", got)
	}
}

func TestGovernorCancellationPropagatesToAllChildren(t *testing.T) {
	g := NewGovernor(Budgets{MaxTokensPerRun: 3}, nil)
	g.Begin(context.Background())
	c1, release1, err := g.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	defer release1()
	c2, release2, err := g.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	defer release2()

	if err := g.RecordUsage(2, 2); err == nil {
		t.Fatal("token exhaustion must return a limit error")
	}
	for i, ctx := range []context.Context{c1, c2} {
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatalf("child %d was not cancelled", i)
		}
	}
	st := g.Status()
	if st.TerminationReason == "" || !strings.Contains(st.TerminationReason, "tokens per run") {
		t.Fatalf("termination reason not surfaced: %+v", st)
	}
}

func TestGovernorWallTimeExpiresRun(t *testing.T) {
	g := NewGovernor(Budgets{MaxWallTime: 20 * time.Millisecond}, nil)
	g.Begin(context.Background())
	child, release, err := g.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("wall-time expiry did not cancel child")
	}
	if got := g.Status().TerminationReason; !strings.Contains(got, "wall time limit exceeded") {
		t.Fatalf("termination reason = %q", got)
	}
}

func TestGovernorToolCostAndBrowserCaps(t *testing.T) {
	g := NewGovernor(Budgets{
		MaxToolCallsPerTurn: 1, MaxToolCallsPerRun: 5,
		MaxCostUSD: .000001, InputCostPerMillionUSD: 10, OutputCostPerMillionUSD: 10,
		MaxBrowserInstances: 1,
	}, nil)
	g.Begin(context.Background())
	if err := g.AcquireToolCall("a"); err != nil {
		t.Fatal(err)
	}
	if err := g.AcquireToolCall("a"); err == nil || !strings.Contains(err.Error(), "tool calls per turn") {
		t.Fatalf("per-turn tool cap not enforced: %v", err)
	}

	g.Begin(context.Background())
	release, err := g.AcquireBrowser()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.AcquireBrowser(); err == nil || !strings.Contains(err.Error(), "browser instances") {
		t.Fatalf("browser cap not enforced: %v", err)
	}
	release()
	if err := g.RecordUsage(1, 0); err == nil || !strings.Contains(err.Error(), "estimated cost") {
		t.Fatalf("cost cap not enforced: %v", err)
	}
}
