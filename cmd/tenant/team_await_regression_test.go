package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

// A team_await TIMEOUT with sub-agents still active must report state and keep
// the run open — it must NOT end the budget run (which cancels the run context
// and executes every still-working sub-agent mid-task: the "terminated: team
// await ended with N subagent(s) still active" kill observed live 2026-07-17).
// The accounting window closes only once all children have joined.
func TestTeamAwait_TimeoutKeepsRunAndWorkersAlive(t *testing.T) {
	rt := newTeamRuntime(TeamConfig{OrchID: "main"})
	ctx := context.Background()
	rt.BeginRun(ctx)

	// Simulate one ACTIVE sub-agent exactly as Spawn does: an acquired budget
	// slot plus a running member whose done channel is still open.
	agCtx, release, err := rt.budget.AcquireAgent()
	if err != nil {
		t.Fatalf("acquire agent: %v", err)
	}
	m := &member{id: "main-worker-1", role: "worker", status: "running", done: make(chan struct{})}
	rt.mu.Lock()
	rt.agents[m.id] = m
	rt.order = append(rt.order, m.id)
	rt.mu.Unlock()

	tool := spawnTool{rt: rt, timeout: 50 * time.Millisecond}
	out, isErr, err := tool.Dispatch(ctx, model.ToolCall{Name: "team_await", Arguments: json.RawMessage(`{}`)})
	if err != nil || isErr {
		t.Fatalf("team_await dispatch: out=%q isErr=%v err=%v", out, isErr, err)
	}

	st := rt.BudgetStatus()
	if !st.Active {
		t.Fatalf("await timeout must keep the budget run OPEN; status=%+v", st)
	}
	if st.TerminationReason != "" {
		t.Fatalf("await timeout must not terminate the run; reason=%q", st.TerminationReason)
	}
	select {
	case <-agCtx.Done():
		t.Fatal("await timeout cancelled the active sub-agent's context (worker executed mid-task)")
	default:
	}

	// Worker finishes: the NEXT await joins it and closes the accounting window.
	rt.mu.Lock()
	m.status = "done"
	m.result = "finished"
	rt.mu.Unlock()
	close(m.done)
	release()

	out2, isErr, err := tool.Dispatch(ctx, model.ToolCall{Name: "team_await", Arguments: json.RawMessage(`{}`)})
	if err != nil || isErr {
		t.Fatalf("second team_await: out=%q isErr=%v err=%v", out2, isErr, err)
	}
	if st := rt.BudgetStatus(); st.Active {
		t.Fatalf("all children joined: the accounting window should be closed; status=%+v", st)
	}
	if !strings.Contains(out2, "main-worker-1") {
		t.Fatalf("await summary should include the joined worker: %q", out2)
	}
}
