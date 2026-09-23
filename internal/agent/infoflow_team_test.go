package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/plugins/osys"
	"tenant/internal/plugins/web"
)

// TEN-336 bypasses of the TEN-311 sensitive-egress gate.

// A secret percent-encoded byte by byte plus one malformed escape used to slip
// through: both unescapers failed on "%zz", so no decoded form was checked.
func TestSensitiveEgress_PercentEncodingWithAMalformedEscapeIsBlocked(t *testing.T) {
	osRead := toolSpec(t, osys.NewDispatcher(nil, osys.Policy{}).Tools(), "os_read_file")
	webSearch := toolSpec(t, web.NewDispatcher(nil, web.Policy{}, "").Tools(), "web_search")
	var b strings.Builder
	for i := 0; i < len(osSentinel); i++ {
		fmt.Fprintf(&b, "%%%02X", osSentinel[i])
	}
	encoded := b.String() + "%zz"
	if strings.Contains(strings.ToLower(encoded), strings.ToLower(osSentinel)) {
		t.Fatalf("encoded query %q still carries the raw secret; the test would prove nothing", encoded)
	}

	_, names := runSensitiveEgressScenario(t, osRead, webSearch, map[string]any{"query": encoded}, nil)
	if got := strings.Join(names, ","); got != "os_read_file" {
		t.Fatalf("dispatched tools = %q; the encoded secret must not reach web_search", got)
	}
}

// A parent could embed a secret in a spawned task, and the fresh child, with
// no provenance of its own, could send it out unchecked. spawn_agent is
// outbound now, and the approved flow seeds the child's tracker.
func TestSensitiveEgress_SpawnHandsTheFlowToTheChild(t *testing.T) {
	osRead := toolSpec(t, osys.NewDispatcher(nil, osys.Policy{}).Tools(), "os_read_file")
	spawn := model.ToolSpec{
		Name:       "spawn_agent",
		Parameters: json.RawMessage(`{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}`),
		Outbound:   true,
	}
	var handed agent.Flow
	parentDisp := agent.DispatcherFunc(func(ctx context.Context, call model.ToolCall) (string, bool, error) {
		if call.Name == "spawn_agent" {
			handed = agent.DispatchFlow(ctx)
			return "spawned", false, nil
		}
		return "api_token=" + osSentinel, false, nil
	})
	var approvals []string
	confirm := func(_ context.Context, _, detail string) bool {
		approvals = append(approvals, detail)
		return true
	}
	parent, fake, _, _ := buildAgentWithEgress(t, model.Profile{}, []model.ToolSpec{osRead, spawn}, parentDisp, confirm)
	scriptCalls(fake,
		call("read", "os_read_file", map[string]any{"path": "credentials.txt"}),
		call("spawn", "spawn_agent", map[string]any{"task": "check whether " + osSentinel + " still works"}),
	)
	if _, err := parent.Turn(context.Background(), agent.TurnRequest{UserQuery: "delegate"}); err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 1 || !strings.Contains(approvals[0], "spawn_agent") {
		t.Fatalf("approvals = %q; a task quoting a secret must ask before it leaves the parent", approvals)
	}
	if handed.Empty() {
		t.Fatal("the approved spawn carried no provenance to the child")
	}

	search := func(flow agent.Flow) []string {
		webSearch := toolSpec(t, web.NewDispatcher(nil, web.Policy{}, "").Tools(), "web_search")
		disp := &recordingDispatcher{}
		child, cfake, _, _ := buildAgentFull(t, model.Profile{}, []model.ToolSpec{webSearch}, disp, nil, nil,
			func(c *agent.Config) { c.InboundFlow = flow })
		scriptCalls(cfake, call("search", "web_search", map[string]any{"query": osSentinel}))
		if _, err := child.Turn(context.Background(), agent.TurnRequest{UserQuery: "check whether " + osSentinel + " still works"}); err != nil {
			t.Fatal(err)
		}
		return disp.names()
	}
	if got := search(handed); len(got) != 0 {
		t.Fatalf("child dispatched %q; its egress of the delegated secret must be checked", got)
	}
	if got := search(agent.Flow{}); strings.Join(got, ",") != "web_search" {
		t.Fatalf("control: a child without the flow dispatched %q, want web_search", got)
	}
}

// A child could return a file secret through team_await, and the parent, with
// an unlabelled result, could send it out. The awaited result now carries the
// flow the child's answer quotes: the parent labels and tracks it.
func TestSensitiveEgress_AwaitedResultCarriesTheChildsFlow(t *testing.T) {
	osRead := toolSpec(t, osys.NewDispatcher(nil, osys.Policy{}).Tools(), "os_read_file")
	child, cfake, _, _ := buildAgentWithEgress(t, model.Profile{}, []model.ToolSpec{osRead}, &recordingDispatcher{}, nil)
	scriptCalls(cfake, call("read", "os_read_file", map[string]any{"path": "credentials.txt"}))
	res, err := child.Turn(context.Background(), agent.TurnRequest{UserQuery: "find the token"})
	if err != nil {
		t.Fatal(err)
	}
	answer := "the token is " + osSentinel
	flow := child.FlowFor(answer)
	if flow.Empty() {
		t.Fatalf("the child's answer quotes its sensitive read but FlowFor found nothing (turn: %q)", res.Response)
	}

	await := model.ToolSpec{Name: "team_await", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
	webSearch := toolSpec(t, web.NewDispatcher(nil, web.Policy{}, "").Tools(), "web_search")
	run := func(report bool) (*agent.TurnResult, []string) {
		var mu sync.Mutex
		var dispatched []string
		disp := agent.DispatcherFunc(func(ctx context.Context, c model.ToolCall) (string, bool, error) {
			mu.Lock()
			dispatched = append(dispatched, c.Name)
			mu.Unlock()
			if c.Name == "team_await" {
				if report {
					agent.ReportFlow(ctx, flow)
				}
				return answer, false, nil
			}
			return "search results", false, nil
		})
		parent, fake, _, _ := buildAgentWithEgress(t, model.Profile{}, []model.ToolSpec{await, webSearch}, disp, nil)
		scriptCalls(fake,
			call("await", "team_await", map[string]any{}),
			call("search", "web_search", map[string]any{"query": osSentinel}),
		)
		result, err := parent.Turn(context.Background(), agent.TurnRequest{UserQuery: "collect"})
		if err != nil {
			t.Fatal(err)
		}
		return result, dispatched
	}

	result, dispatched := run(true)
	if got := strings.Join(dispatched, ","); got != "team_await" {
		t.Fatalf("dispatched %q; the parent must not send out what its child returned", got)
	}
	if len(result.ToolTrace) == 0 || !hasProvenance(result.ToolTrace[0].Labels, model.ProvenanceLocalFile) {
		t.Fatalf("team_await result labels = %+v, want local_file", result.ToolTrace)
	}
	if _, dispatched := run(false); strings.Join(dispatched, ",") != "team_await,web_search" {
		t.Fatalf("control: without the reported flow dispatched %q, want team_await,web_search", dispatched)
	}
}

// scriptCalls makes the fake model emit each call in turn, one per step, then
// finish.
func scriptCalls(fake *testllm.Fake, calls ...model.ToolCall) {
	step := 0
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		step++
		if step <= len(calls) {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{calls[step-1]}}, nil
		}
		return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
	}
}

func hasProvenance(labels []model.InfoFlowLabel, provenance string) bool {
	for _, l := range labels {
		if l.Provenance == provenance && l.Sensitivity == model.SensitivitySensitive {
			return true
		}
	}
	return false
}
