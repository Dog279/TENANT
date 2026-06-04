package main

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/model"
)

func TestDiscordToolAllowed(t *testing.T) {
	// Read/comms tools (+ the two kept Discord comms tools): allowed in BOTH modes.
	alwaysAllow := []model.ToolSpec{
		{Name: "web_search"}, {Name: "web_read"}, {Name: "web_find"},
		{Name: "sql_query"}, {Name: "gmail_search"}, {Name: "gmail_read"},
		{Name: "calendar_list"}, {Name: "memory_recall"}, {Name: "os_sysinfo"},
		{Name: "wiki_read"}, {Name: "discord_read_channel"}, {Name: "discord_list_guilds"},
		{Name: "discord_send_message", Gated: true}, {Name: "discord_react", Gated: true},
	}
	for _, sp := range alwaysAllow {
		if !discordToolAllowed(sp, false) || !discordToolAllowed(sp, true) {
			t.Errorf("%s should be ALLOWED offsite in BOTH modes", sp.Name)
		}
	}

	// Gated dangerous tools: CUT without exec opt-in, AVAILABLE with it (each
	// still hits the button approver at dispatch — that's a separate layer).
	execGated := []model.ToolSpec{
		{Name: "os_exec", Gated: true}, {Name: "os_write", Gated: true},
		{Name: "os_exec_dangerous", Gated: true}, {Name: "web_transact", Gated: true},
		{Name: "web_click", Gated: true}, {Name: "web_fill", Gated: true},
		{Name: "sql_exec", Gated: true}, {Name: "gmail_send", Gated: true},
		{Name: "imessage_send", Gated: true}, {Name: "x_post", Gated: true},
	}
	for _, sp := range execGated {
		if discordToolAllowed(sp, false) {
			t.Errorf("%s must be CUT without exec mode", sp.Name)
		}
		if !discordToolAllowed(sp, true) {
			t.Errorf("%s must be AVAILABLE in exec mode", sp.Name)
		}
	}

	// team/orchestra: CUT in BOTH modes — fan-out is never offered offsite.
	neverAllow := []model.ToolSpec{
		{Name: "team_spawn"}, {Name: "team_status"}, {Name: "orchestrate", Gated: true},
		{Name: "orchestrator"},
	}
	for _, sp := range neverAllow {
		if discordToolAllowed(sp, false) || discordToolAllowed(sp, true) {
			t.Errorf("%s must be CUT offsite even in exec mode (fan-out)", sp.Name)
		}
	}
}

type recordDispatcher struct{ called []string }

func (r *recordDispatcher) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	r.called = append(r.called, call.Name)
	return "ok", false, nil
}

func TestRestrictForDiscord(t *testing.T) {
	full := []model.ToolSpec{
		{Name: "web_search"},
		{Name: "os_exec", Gated: true},
		{Name: "discord_send_message", Gated: true},
		{Name: "team_spawn"},
	}
	inner := &recordDispatcher{}
	reg, disp, gate := restrictForDiscord(full, inner)

	regNames := func() map[string]bool {
		names := map[string]bool{}
		for _, sp := range reg.All() {
			names[sp.Name] = true
		}
		return names
	}

	// Exec OFF (default): read/comms visible; os_exec + team_spawn hidden.
	off := regNames()
	if !off["web_search"] || !off["discord_send_message"] {
		t.Errorf("allowed tools missing from the exec-off registry: %v", off)
	}
	if off["os_exec"] || off["team_spawn"] {
		t.Errorf("dangerous/fan-out tools leaked into the exec-off registry: %v", off)
	}
	// An allowed tool dispatches through; os_exec is refused and never reaches inner.
	if _, isErr, _ := disp.Dispatch(context.Background(), model.ToolCall{Name: "web_search"}); isErr {
		t.Error("an allowed tool should dispatch")
	}
	out, isErr, _ := disp.Dispatch(context.Background(), model.ToolCall{Name: "os_exec"})
	if !isErr || !strings.Contains(out, "not available over Discord") {
		t.Errorf("os_exec must be refused while exec is off, got %q (isErr=%v)", out, isErr)
	}
	if len(inner.called) != 1 { // only web_search reached inner
		t.Fatalf("a cut tool must NOT reach inner, calls=%v", inner.called)
	}

	// Flip exec ON (live): os_exec now visible + dispatchable; team_spawn STILL cut.
	gate.set(true)
	on := regNames()
	if !on["os_exec"] {
		t.Error("os_exec should appear in the registry once exec mode is on")
	}
	if on["team_spawn"] {
		t.Error("team_spawn must STAY cut even in exec mode (fan-out)")
	}
	if _, isErr, _ := disp.Dispatch(context.Background(), model.ToolCall{Name: "os_exec"}); isErr {
		t.Error("os_exec should dispatch through once exec mode is on")
	}
	if len(inner.called) != 2 || inner.called[1] != "os_exec" {
		t.Fatalf("os_exec should reach inner in exec mode, calls=%v", inner.called)
	}
	if _, isErr, _ := disp.Dispatch(context.Background(), model.ToolCall{Name: "team_spawn"}); !isErr {
		t.Error("team_spawn must be refused even in exec mode")
	}

	// Flip back OFF: os_exec hidden + refused again (the toggle is live).
	gate.set(false)
	if regNames()["os_exec"] {
		t.Error("os_exec should hide again when exec mode is turned off")
	}
	if _, isErr, _ := disp.Dispatch(context.Background(), model.ToolCall{Name: "os_exec"}); !isErr {
		t.Error("os_exec must be refused again once exec mode is off")
	}
}
