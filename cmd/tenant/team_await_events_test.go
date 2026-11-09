package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/model"
	"tenant/internal/orchestra"
)

// eventTeam is a runtime with a bus the orchestrator and n running members are
// registered on, as Spawn would leave them, without any model behind them.
func eventTeam(t *testing.T, n int) (*TeamRuntime, *orchestra.Bus, []*member) {
	t.Helper()
	bus := orchestra.NewBus()
	bus.Register("main")
	rt := newTeamRuntime(TeamConfig{OrchID: "main", Bus: bus})
	var ms []*member
	rt.mu.Lock()
	for i := 1; i <= n; i++ {
		id := "main-worker-" + string(rune('0'+i))
		m := &member{id: id, role: "worker", status: "running", done: make(chan struct{}), started: time.Now()}
		rt.agents[id] = m
		rt.order = append(rt.order, id)
		bus.Register(id)
		ms = append(ms, m)
	}
	rt.mu.Unlock()
	return rt, bus, ms
}

func awaitWithin(t *testing.T, rt *TeamRuntime, ceiling, within time.Duration) (string, time.Duration) {
	t.Helper()
	t0 := time.Now()
	out, isErr, err := spawnTool{rt: rt, timeout: ceiling}.Dispatch(context.Background(), model.ToolCall{Name: "team_await", Arguments: json.RawMessage(`{}`)})
	took := time.Since(t0)
	if err != nil || isErr {
		t.Fatalf("team_await: %q isErr=%v err=%v", out, isErr, err)
	}
	if took > within {
		t.Fatalf("team_await took %s, want under %s (it waited for the ceiling, not the event):\n%s", took, within, out)
	}
	return out, took
}

// One of two helpers finishing is news: the await returns with that result and
// says the other is still running, long before the ceiling. The next await
// returns the other, and the first is not repeated.
func TestTeamAwait_ReturnsWhenOneHelperFinishes(t *testing.T) {
	rt, _, ms := eventTeam(t, 2)
	go func() {
		time.Sleep(100 * time.Millisecond)
		rt.finish(ms[0], "done", "the first result", agent.Flow{})
	}()
	out, took := awaitWithin(t, rt, 10*time.Second, 3*time.Second)
	if took < 100*time.Millisecond || !strings.Contains(out, "## main-worker-1 (worker) [done]\nthe first result") {
		t.Fatalf("after %s:\n%s", took, out)
	}
	if !strings.Contains(out, "still running: main-worker-2 (worker") || strings.Contains(out, "timed out") {
		t.Fatalf("the still-running line is missing or the return reads as a timeout:\n%s", out)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		rt.finish(ms[1], "error", "error: it broke", agent.Flow{})
	}()
	out2, _ := awaitWithin(t, rt, 10*time.Second, 3*time.Second)
	if !strings.Contains(out2, "## main-worker-2 (worker) [error]\nerror: it broke") || strings.Contains(out2, "first result") || strings.Contains(out2, "still running") {
		t.Fatalf("second await:\n%s", out2)
	}
}

// A helper writing to the orchestrator mid-task is news too: the await returns
// with the message while the helper keeps running, and the message is drained
// so team_check does not repeat it. The helper's DONE broadcast is not shown
// beside its result section.
func TestTeamAwait_ReturnsOnAMessageAndDrainsIt(t *testing.T) {
	rt, bus, ms := eventTeam(t, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = bus.Send(orchestra.Message{From: "main-worker-1", To: "main", Content: "need a decision: A or B?"})
	}()
	out, _ := awaitWithin(t, rt, 10*time.Second, 3*time.Second)
	if !strings.Contains(out, "## messages to you\n- main-worker-1: need a decision: A or B?") || !strings.Contains(out, "still running: main-worker-1") {
		t.Fatalf("the message should come back with the helper still running:\n%s", out)
	}
	if n := bus.Pending("main"); n != 0 {
		t.Fatalf("the message was not drained: %d pending", n)
	}
	// It finishes the way a real helper does: DONE on the bus, then finish.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = bus.Send(orchestra.Message{From: "main-worker-1", Content: "DONE — chose A"})
		rt.finish(ms[0], "done", "chose A", agent.Flow{})
	}()
	out2, _ := awaitWithin(t, rt, 10*time.Second, 3*time.Second)
	if strings.Count(out2, "chose A") != 1 || strings.Contains(out2, "messages to you") {
		t.Fatalf("one finish is one result, and its DONE broadcast is not a second copy:\n%s", out2)
	}
}

// Nothing happening returns at the ceiling with the timed-out wording and the
// helper still listed; the run stays alive for the next await.
func TestTeamAwait_CeilingReportsStateAndKeepsWaiting(t *testing.T) {
	rt, _, _ := eventTeam(t, 1)
	out, took := awaitWithin(t, rt, 300*time.Millisecond, 3*time.Second)
	if took < 300*time.Millisecond || !strings.HasPrefix(out, "(timed out waiting; reporting current state)") || !strings.Contains(out, "still running: main-worker-1") {
		t.Fatalf("after %s:\n%s", took, out)
	}
}
