package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"tenant/internal/orchestra"
)

// builtinAgentProfiles ships the 5 specialists (NOT Main), each soul-only and
// inheriting the primary model, with a condensed IDENTITY+RULES persona.
func TestBuiltinAgentProfiles(t *testing.T) {
	b := builtinAgentProfiles()
	for _, role := range []string{"Programmer", "Researcher", "Writer", "QA", "Strategist"} {
		ap := b[role]
		if ap == nil {
			t.Fatalf("missing built-in specialist %q", role)
		}
		if !ap.Builtin || ap.Provider != "" {
			t.Errorf("%s: want Builtin + empty Provider (inherit), got Builtin=%v Provider=%q", role, ap.Builtin, ap.Provider)
		}
		if strings.TrimSpace(ap.Description) == "" {
			t.Errorf("%s: missing description (shown to the orchestrator)", role)
		}
		// IDENTITY ("You are <Role>") + RULES ("# Operating Rules") are present...
		if !strings.Contains(ap.Soul, role) || !strings.Contains(ap.Soul, "# Operating Rules") {
			t.Errorf("%s: soul missing IDENTITY or RULES section", role)
		}
		// ...but the full SOUL.md voice essay is dropped, so the spawnable soul is
		// strictly smaller than the full triplet (keeps it under the prompt reserve).
		full, err := builtinAgentsFS.ReadFile("builtinsouls/agents/" + role + "/SOUL.md")
		if err != nil {
			t.Fatalf("%s: read SOUL.md: %v", role, err)
		}
		if strings.Contains(ap.Soul, strings.TrimSpace(string(full))) {
			t.Errorf("%s: spawnable persona must NOT embed the full SOUL.md voice essay (budget)", role)
		}
	}
	if b["Main"] != nil {
		t.Error("Main must NOT be a spawnable specialist (its conductor identity fights the headless member prompt)")
	}
	if len(b) != 5 {
		t.Errorf("expected exactly 5 built-in specialists, got %d", len(b))
	}
}

// effectiveAgents merges built-ins under config; a same-named config entry wins.
func TestEffectiveAgents_Override(t *testing.T) {
	lc := &launchConfig{Agents: map[string]*agentProfile{
		"Programmer": {Provider: "zai", Description: "my override"},
	}}
	eff := effectiveAgents(lc)
	if eff["Programmer"] == nil || eff["Programmer"].Description != "my override" || eff["Programmer"].Provider != "zai" {
		t.Errorf("config must override the built-in by name: %+v", eff["Programmer"])
	}
	if eff["Researcher"] == nil || !eff["Researcher"].Builtin {
		t.Error("non-overridden built-ins must remain")
	}
	// nil config still yields the built-ins (survives a failed load).
	if len(effectiveAgents(nil)) != 5 {
		t.Errorf("effectiveAgents(nil) should return the 5 built-ins, got %d", len(effectiveAgents(nil)))
	}
}

// A built-in (empty Provider) inherits the orchestrator's primary router and
// applies its soul; the lookup is case-insensitive so a lowercased role fires.
func TestRouterForProfile_BuiltinInherits(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: dir}
	router, _ := buildRouter(c, log)
	st, closeStores, _ := openStores(c)
	defer closeStores()
	bus := orchestra.NewBus()
	bus.Register("main")
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: newToolMux(),
		OrchID: "main", Log: log,
		AgentProfiles: effectiveAgents(nil), // just the built-ins
		CfgDir:        dir,
	})

	gotR, gotP := rt.routerForProfile("Programmer")
	if gotR != router {
		t.Error("a built-in must inherit the orchestrator's PRIMARY router, not a pinned one")
	}
	if gotP == nil || !gotP.Builtin || !strings.Contains(gotP.Soul, "# Operating Rules") {
		t.Errorf("built-in soul not applied: %+v", gotP)
	}
	// Case-insensitive: the model may emit "programmer".
	gotR2, gotP2 := rt.routerForProfile("programmer")
	if gotR2 != router || gotP2 == nil {
		t.Errorf("a lowercased role must still resolve the built-in: r-equal=%v p=%v", gotR2 == router, gotP2 != nil)
	}
}

// The orchestrator prompt lists the 5 specialists with an inherited-model label,
// deterministically (stable across launches).
func TestRenderAgentsForOrchestrator_Builtins(t *testing.T) {
	eff := effectiveAgents(nil)
	s := renderAgentsForOrchestrator(eff)
	for _, name := range []string{"Programmer", "Researcher", "Writer", "QA", "Strategist"} {
		if !strings.Contains(s, name) {
			t.Errorf("orchestrator prompt missing specialist %q", name)
		}
	}
	if !strings.Contains(s, "your model (inherited)") {
		t.Errorf("built-ins should render an inherited-model label:\n%s", s)
	}
	if s != renderAgentsForOrchestrator(eff) {
		t.Error("render must be deterministic (sorted) across calls")
	}
}
