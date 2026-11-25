package main

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// rankedFake is a stand-in for the live composite: Search returns tools in
// RELEVANCE order (which the fake fixes explicitly), not registration order.
type rankedFake struct {
	byRank []model.ToolSpec // most-relevant first
}

func (f *rankedFake) Get(name string) (model.ToolSpec, bool) {
	for _, sp := range f.byRank {
		if sp.Name == name {
			return sp, true
		}
	}
	return model.ToolSpec{}, false
}
func (f *rankedFake) All() []model.ToolSpec { return f.byRank }
func (f *rankedFake) Search(_ context.Context, _ []float32, k int) ([]model.ToolSpec, error) {
	if k > len(f.byRank) {
		k = len(f.byRank)
	}
	return f.byRank[:k], nil
}

var _ agent.ToolRegistry = (*rankedFake)(nil)

func spec(name string, gated bool) model.ToolSpec {
	return model.ToolSpec{Name: name, Description: name, Gated: gated}
}

// The daily brief failed for five straight days because the cron surface was a
// StaticRegistry snapshot: Search ignored the query and returned the first K
// tools in registration order — always the local set — so the model never SAW
// discord_send_message/money_*/gmail_* even though the dispatcher would have
// allowed them. The restricted surface must (a) preserve the inner registry's
// relevance order, (b) not let disallowed tools consume result slots, and
// (c) never re-truncate to k after filtering, which would resurrect the
// original starvation via the composite's local-tools-first ordering.
func TestRestrictedView_SearchKeepsRankedAllowedTools(t *testing.T) {
	inner := &rankedFake{byRank: []model.ToolSpec{
		// Disallowed tools deliberately outrank everything the surface allows.
		spec("team_await", false),
		spec("cron_add", false),
		spec("spawn_agent", false),
		spec("discord_send_message", true),
		spec("money_balances", false),
		spec("gmail_search", false),
		spec("os_exec", true),
		spec("web_search", false),
	}}

	execView, _ := restrictExec(inner, nil, "test (exec)", notCron)
	got, err := execView.Search(context.Background(), nil, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	names := make([]string, len(got))
	for i, sp := range got {
		names[i] = sp.Name
	}
	joined := strings.Join(names, ",")
	// The top-3 ALLOWED tools by relevance survive even though a disallowed
	// tool outranked them: without headroom the k=3 window would hold a
	// denial and the surface would come back short — the starvation that sank
	// the brief. The exec surface keeps the helper tools (team_await,
	// spawn_agent): a job that runs unattended may hand work to its helpers.
	for _, want := range []string{"team_await", "spawn_agent", "discord_send_message"} {
		if !strings.Contains(joined, want) {
			t.Errorf("exec surface lost %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "cron_add") {
		t.Errorf("exec surface leaked cron_add: %s", joined)
	}
	// Relevance order preserved among survivors.
	if names[0] != "team_await" {
		t.Errorf("ranking not preserved, got %s first", names[0])
	}

	// With k covering the catalog, EVERY allowed tool returns and every
	// denial is still cut.
	all, err := execView.Search(context.Background(), nil, len(inner.byRank))
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if len(all) != 7 {
		t.Errorf("full-window search returned %d tools, want the 7 allowed", len(all))
	}

	// The safe surface cuts every gated tool AND the helper tools: nobody is
	// there to answer for what a helper does.
	safeView, _ := restrictReadComms(inner, nil, "test (safe)", notCron)
	got, err = safeView.Search(context.Background(), nil, len(inner.byRank))
	if err != nil {
		t.Fatalf("safe search: %v", err)
	}
	for _, sp := range got {
		if sp.Gated || fanOut(sp.Name) || sp.Name == "cron_add" {
			t.Errorf("safe surface leaked %q", sp.Name)
		}
	}
	if len(got) != 3 {
		t.Errorf("safe surface returned %d tools, want the 3 plain ones", len(got))
	}
}

// The dispatcher checks the LIVE registry at call time: a tool outside the
// view is refused with the surface message, an allowed one reaches the inner
// dispatcher, and the answer changes when the inner registry changes — no
// frozen allowlist.
func TestRestrictedDispatcher_ChecksLive(t *testing.T) {
	inner := &rankedFake{byRank: []model.ToolSpec{
		spec("gmail_search", false),
		spec("os_exec", true),
	}}
	sink := &recordingDispatcher{}
	_, disp := restrictReadComms(inner, sink, "scheduled cron jobs", nil)

	out, handled, err := disp.Dispatch(context.Background(), model.ToolCall{Name: "os_exec"})
	if err != nil || !handled {
		t.Fatalf("refusal should be handled in-band: %v", err)
	}
	if !strings.Contains(out, "not available") || !strings.Contains(out, "scheduled cron jobs") {
		t.Errorf("refusal should name the surface: %s", out)
	}
	if sink.called != "" {
		t.Fatalf("gated tool must never reach the inner dispatcher, but %q did", sink.called)
	}

	if _, _, err := disp.Dispatch(context.Background(), model.ToolCall{Name: "gmail_search"}); err != nil {
		t.Fatalf("allowed dispatch: %v", err)
	}
	if sink.called != "gmail_search" {
		t.Fatalf("allowed tool should reach the inner dispatcher, got %q", sink.called)
	}

	// Simulate the operator /disabling the tool (it vanishes from the live
	// registry): the next dispatch must refuse without any rebuild.
	inner.byRank = inner.byRank[1:]
	sink.called = ""
	out, _, _ = disp.Dispatch(context.Background(), model.ToolCall{Name: "gmail_search"})
	if sink.called != "" || !strings.Contains(out, "not available") {
		t.Errorf("dispatcher used a stale allowlist: called=%q out=%s", sink.called, out)
	}
}

type recordingDispatcher struct{ called string }

func (r *recordingDispatcher) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	r.called = call.Name
	return "ok", true, nil
}
