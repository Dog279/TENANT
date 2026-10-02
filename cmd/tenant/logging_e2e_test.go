package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/logs/event"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/model/backend/echo"
	"tenant/internal/model/testllm"
	"tenant/internal/runtrace"
)

// The log book end to end (docs/design/logging-v2.md §14): serve's wiring on
// the echo backend, with the planner scripted to call os_exec. The call asks
// for approval, the operator grants it as the dashboard does, the command
// runs through the sandbox seam on the host backend, and `tenant logs trace`
// then returns the whole story by the turn's trace: turn → tool call →
// approval → Security record → sandbox run, beside the run record. It also
// answers the discovery drill's question 6 (§9.1) from the records: who did
// it, from where, and with whose approval. The Security chain verifies
// afterwards.
func TestLoggingEndToEnd(t *testing.T) {
	keepLogDefaults(t)
	resetExecProfiles(t) // the default profile: the host backend
	data, cfg := t.TempDir(), t.TempDir()
	c := &commonFlags{backend: "echo", agent: "main", dataDir: data, cfgDir: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Serve's log book: logbook.db with the chain key, the process's emitter.
	// It is not Started: the housekeeper would shed the diagnostic tool
	// payloads on a nearly full disk, which would make this test the disk's.
	hub, log, err := openLogHub(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	// The echo backend, with a scripted planner: os_exec, then an answer.
	planner := testllm.New()
	calls := 0
	planner.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		if calls++; calls == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{teamCall("call-1", "os_exec", map[string]any{"command": "printf ok"})}}, nil
		}
		return &model.GenerateResponse{Text: "it printed ok", FinishReason: "stop"}, nil
	}
	reg := model.NewEmptyRegistry()
	for _, p := range echoProfiles() {
		if p.Role == model.RolePlanner {
			p.Backend = "script"
		}
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, log.With("source", "model"))
	router.RegisterBackend("echo", echo.New)
	router.RegisterBackend("script", func(context.Context, model.Profile, *slog.Logger) (any, error) { return planner, nil })

	// Serve's approval broker, its events filed as serve files them. The
	// operator approves the pending card as the dashboard's button does.
	broker := newApprovalBroker(log.With("source", "approvals"))
	emit := func(ev agent.Event) {
		if e, ok := agent.EntryFor(ev); ok {
			hub.Log(e)
		}
		if ev.Kind == agent.EventApprovalPending && ev.Approval != nil {
			if err := broker.Decide(ev.Approval.ID, "approve"); err != nil {
				t.Error(err)
			}
		}
	}
	broker.SetEvents(emit)

	// Serve's tool mux (the OS plugin on, its exec gated by the broker), run
	// records and agent, behind the turn gate; a dashboard turn.
	mux, _, cleanup, err := buildToolMux(ctx, c, nil, testPluginFlags(func(pf *pluginFlags) { osFlagsOf(pf).on = true }),
		originConfirm(broker.Confirm), log)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	runs, err := runtrace.Open(filepath.Join(data, "runs.db"), effectiveRunTraceConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	tools := composite{shared: mux, local: newToolMux(), agentID: c.agent}
	ag, err := agent.New(agent.Config{
		AgentID: c.agent, Router: router, Working: working.New(), Runs: runs, Tools: tools,
		Dispatcher: &agent.RetryDecorator{Inner: tools, Eligible: agent.DefaultEligibleTransient, Backoff: time.Second, MaxRetries: 1, Observer: emit},
		Logger:     log.With("source", "turns"), Observer: emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := newTurnGateConfigured(ag, turnSchedulerConfig{}).Turn(ctx, agent.TurnRequest{UserQuery: "run echo ok", RouteSurface: agent.RouteSurfaceDashboard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolTrace) != 1 || res.ToolTrace[0].IsError || !strings.Contains(res.ToolTrace[0].Result, "ok") {
		t.Fatalf("the os_exec call: %+v", res.ToolTrace)
	}
	trace := res.TraceID
	if err := hub.Flush(); err != nil {
		t.Fatal(err)
	}

	// tenant logs trace <trace>, reading logbook.db here as the command does.
	var out bytes.Buffer
	if err := runLogs(ctx, []string{"trace", trace, "--format", "json", "--data", data, "--config", cfg}, &out); err != nil {
		t.Fatal(err)
	}
	var tl logapi.Timeline
	if err := json.Unmarshal(out.Bytes(), &tl); err != nil {
		t.Fatalf("%v:\n%s", err, out.String())
	}
	var ids []string
	for _, e := range tl.Events {
		ids = append(ids, e2eName(e))
	}
	t.Logf("timeline of %s: %s", trace, strings.Join(ids, ", "))
	out.Reset()
	if err := runLogs(ctx, []string{"trace", trace, "--data", data, "--config", cfg}, &out); err != nil ||
		!strings.HasPrefix(out.String(), "Run "+trace+": success · dashboard") || strings.Count(out.String(), "\n") != len(tl.Events)+1 {
		t.Errorf("the text timeline (%v):\n%s", err, out.String())
	}

	// Each link, in order: the first event of each name after the one before.
	at, last := map[string]event.Event{}, -1
	for _, name := range []string{"turns/1 start", "turns/9001", "security/4101", "security/4102", "sandbox/1 start",
		"sandbox/1001", "security/4201", "sandbox/1 stop", "turns/9002", "turns/1101", "turns/1 stop"} {
		i := slices.IndexFunc(tl.Events[last+1:], func(e event.Event) bool { return e2eName(e) == name })
		if i < 0 {
			t.Fatalf("no %s after the timeline's event %d", name, last)
		}
		last += 1 + i
		at[name] = tl.Events[last]
	}
	approvalID := at["security/4101"].Corr.ApprovalID
	if !strings.HasPrefix(approvalID, "ap-") {
		t.Fatalf("the approval request has no approval ID: %+v", at["security/4101"])
	}
	for name, e := range at {
		if e.Corr.TraceID != trace {
			t.Errorf("%s: trace %q, want the turn's", name, e.Corr.TraceID)
		}
		if strings.HasPrefix(name, "security/") || strings.HasPrefix(name, "sandbox/") {
			if e.Corr.ApprovalID != approvalID {
				t.Errorf("%s: approval %q, want %s", name, e.Corr.ApprovalID, approvalID)
			}
		}
		// The tool call, its approval, its run and their records name the
		// call itself, so two calls in one turn stay apart.
		if name != "turns/1 start" && name != "turns/1101" && name != "turns/1 stop" && e.Corr.ToolCallID != "call-1" {
			t.Errorf("%s: tool call %q, want call-1", name, e.Corr.ToolCallID)
		}
	}
	run := at["sandbox/1 start"]
	if run.Corr.RelatedID != trace || run.Corr.ActivityID == "" || at["security/4201"].Corr.ActivityID != run.Corr.ActivityID {
		t.Errorf("the sandbox run is not the turn's child, or the exec record not the run's: %+v / %+v", run.Corr, at["security/4201"].Corr)
	}
	if r := tl.Run; r == nil || r.TraceID != trace || len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "os_exec" ||
		!slices.ContainsFunc(r.Approvals, func(a runtrace.Approval) bool { return a.ID == approvalID && a.Outcome == "approved" }) {
		t.Errorf("the run record beside the timeline: %+v (%s)", tl.Run, tl.RunNote)
	}

	// Question 6: who did this, from where, and with whose approval?
	exec, grant := at["security/4201"], at["security/4102"]
	if got, want := fmt.Sprintf("%s@%s ran %q on %s under %s, granted %s by %s@%s", exec.Actor, exec.Surface, eventField(&exec, "command").Text(),
		eventField(&exec, "backend").Text(), exec.Corr.ApprovalID, eventField(&grant, "grant").Text(), grant.Actor, grant.Surface),
		fmt.Sprintf(`agent@local ran "printf ok" on host under %s, granted once by operator@dashboard`, approvalID); got != want {
		t.Errorf("question 6:\n got %s\nwant %s", got, want)
	}

	// Nothing the turn wrote is missing from its timeline.
	inTimeline := map[uint64]bool{}
	for _, e := range tl.Events {
		inTimeline[e.Seq] = true
	}
	all, _, _, err := hub.Store().Query(ctx, query.Filter{}, store.Cursor{}, 1000, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if slices.Contains([]string{"turns", "approvals", "sandbox", "security"}, e.Log) && !inTimeline[e.Seq] {
			t.Errorf("%s %q (trace %q) is not in the turn's timeline", e2eName(e), e.Msg, e.Corr.TraceID)
		}
	}

	// The Security chain verifies.
	v, err := hub.Store().Verify(ctx, store.VerifyOptions{})
	if err != nil || v.Break != nil || v.Records < 3 {
		t.Errorf("verify: %+v, %v", v, err)
	}
}

// e2eName is an event's provider/ID, and its opcode when it has one.
func e2eName(e event.Event) string {
	return strings.TrimSpace(fmt.Sprintf("%s/%d %s", e.Provider, e.ID, e.Opcode))
}
