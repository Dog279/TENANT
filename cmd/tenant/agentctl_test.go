package main

import (
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/tui"
)

// userAgentRows filters out the built-in specialists (TEN-132) so CRUD
// assertions about the operator's OWN profiles stay stable.
func userAgentRows(rows []tui.AgentInfo) []tui.AgentInfo {
	var out []tui.AgentInfo
	for _, r := range rows {
		if !r.Builtin {
			out = append(out, r)
		}
	}
	return out
}

// agentControl wraps launchConfig.Agents — round-trip + validation rules.

func TestAgentControl_AddListShowRemove(t *testing.T) {
	dir := t.TempDir()
	// Seed providers so Add can validate against them.
	lc := &launchConfig{
		Provider: "dgx",
		Providers: map[string]*providerConfig{
			"dgx": {Kind: "vllm", Endpoint: "http://localhost:8000", Model: "aeon-ultimate"},
			"zai": {Kind: "zai", Endpoint: "https://api.z.ai/api/paas/v4", Model: "glm-4.6"},
		},
	}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	ac := &agentControl{cfgDir: dir}

	// Add a researcher pinned to Z.ai.
	status, err := ac.Add("researcher", "zai", "glm-4.6", "web researcher specialist", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.Contains(status, "zai/glm-4.6") {
		t.Errorf("status missing model: %q", status)
	}
	// Add a synthesizer pinned to DGX (model omitted → falls back to provider default).
	if _, err := ac.Add("synthesizer", "dgx", "", "report writer", ""); err != nil {
		t.Fatalf("Add synth: %v", err)
	}

	// List — sorted by name, both visible, both valid.
	rows, err := ac.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	user := userAgentRows(rows)
	if len(user) != 2 {
		t.Fatalf("want 2 user rows, got %d", len(user))
	}
	// Sorted order: researcher, synthesizer.
	if user[0].Name != "researcher" || user[1].Name != "synthesizer" {
		t.Errorf("sort wrong: %v", user)
	}
	if !user[0].Valid || !user[1].Valid {
		t.Errorf("rows not valid: %+v", user)
	}
	// synthesizer's effective model came from the provider (no profile override).
	if user[1].Model != "aeon-ultimate" {
		t.Errorf("synth model not resolved from provider: %q", user[1].Model)
	}

	// Show — full detail incl. soul (empty here).
	d, err := ac.Show("researcher")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if d.Provider != "zai" || d.Model != "glm-4.6" {
		t.Errorf("show wrong: %+v", d)
	}

	// Remove — researcher gone, synthesizer still there.
	if _, err := ac.Remove("researcher"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	rows, _ = ac.List()
	user = userAgentRows(rows)
	if len(user) != 1 || user[0].Name != "synthesizer" {
		t.Errorf("remove wrong: %+v", user)
	}
	// Remove-missing errors useful.
	if _, err := ac.Remove("ghost"); err == nil {
		t.Error("Remove(ghost) should error")
	}
}

// Add rejects bad inputs with useful errors.
func TestAgentControl_Add_Rejects(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{
		"dgx": {Kind: "vllm", Endpoint: "http://x", Model: "m"},
	}}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	ac := &agentControl{cfgDir: dir}

	cases := []struct{ name, prov, want string }{
		{"", "dgx", "name and provider"},
		{"researcher", "", "name and provider"},
		{"researcher", "nope", "not configured"},
		{"bad name!", "dgx", "unsafe characters"},
		{"with space", "dgx", "unsafe characters"},
		{"../slash", "dgx", "unsafe characters"},
	}
	for _, c := range cases {
		_, err := ac.Add(c.name, c.prov, "", "", "")
		if err == nil {
			t.Errorf("Add(%q,%q) should error", c.name, c.prov)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Add(%q,%q) error %q should contain %q", c.name, c.prov, err, c.want)
		}
	}
}

// SetSoul updates JUST the identity. Empty arg clears.
func TestAgentControl_SetSoul(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{
		"dgx": {Kind: "vllm", Endpoint: "http://x", Model: "m"},
	}}
	_ = lc.save(dir)
	ac := &agentControl{cfgDir: dir}
	_, _ = ac.Add("researcher", "dgx", "", "", "")

	body := "You are a thorough web researcher. Cite every claim."
	if _, err := ac.SetSoul("researcher", body); err != nil {
		t.Fatalf("SetSoul: %v", err)
	}
	d, _ := ac.Show("researcher")
	if d.Soul != body {
		t.Errorf("soul not persisted: %q", d.Soul)
	}
	rows, _ := ac.List()
	if !rows[0].HasSoul {
		t.Error("HasSoul=false after set")
	}

	// Clear.
	if _, err := ac.SetSoul("researcher", ""); err != nil {
		t.Fatalf("SetSoul clear: %v", err)
	}
	d, _ = ac.Show("researcher")
	if d.Soul != "" {
		t.Errorf("soul not cleared: %q", d.Soul)
	}
	// Unknown agent errors.
	if _, err := ac.SetSoul("ghost", "x"); err == nil {
		t.Error("SetSoul(ghost) should error")
	}
}

// Re-adding the same name updates the model but preserves any prior soul/desc
// (operator could be just changing the provider, not re-typing everything).
func TestAgentControl_Add_PreservesPriorSoulOnPartialUpdate(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{
		"dgx": {Kind: "vllm", Endpoint: "http://x", Model: "m1"},
		"zai": {Kind: "zai", Endpoint: "https://api.z.ai", Model: "glm-4.6"},
	}}
	_ = lc.save(dir)
	ac := &agentControl{cfgDir: dir}

	_, _ = ac.Add("researcher", "dgx", "", "old desc", "")
	_, _ = ac.SetSoul("researcher", "thorough researcher")

	// Re-add: change provider, leave description + soul blank to inherit.
	_, _ = ac.Add("researcher", "zai", "glm-4.6", "", "")

	d, _ := ac.Show("researcher")
	if d.Provider != "zai" {
		t.Errorf("provider not updated: %q", d.Provider)
	}
	if d.Description != "old desc" {
		t.Errorf("description clobbered: %q", d.Description)
	}
	if d.Soul != "thorough researcher" {
		t.Errorf("soul clobbered: %q", d.Soul)
	}
}

// Rename moves a profile lossless — all fields carry over to the new name,
// the old name no longer resolves. Refuses to overwrite an existing target.
func TestAgentControl_Rename(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{
		"dgx": {Kind: "vllm", Endpoint: "http://x", Model: "m"},
	}}
	_ = lc.save(dir)
	ac := &agentControl{cfgDir: dir}
	_, _ = ac.Add("researcher", "dgx", "", "deep web", "")
	_, _ = ac.SetSoul("researcher", "be thorough")

	status, err := ac.Rename("researcher", "deep-researcher")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if !strings.Contains(status, "renamed") {
		t.Errorf("status missing 'renamed': %q", status)
	}

	rows, _ := ac.List()
	user := userAgentRows(rows)
	if len(user) != 1 || user[0].Name != "deep-researcher" {
		t.Fatalf("want [deep-researcher] user row after rename, got %v", user)
	}
	d, _ := ac.Show("deep-researcher")
	if d.Description != "deep web" || d.Soul != "be thorough" {
		t.Errorf("rename clobbered fields: %+v", d)
	}
	// Old name no longer resolves.
	if _, err := ac.Show("researcher"); err == nil {
		t.Error("old name should not resolve after rename")
	}
}

// Rename rejects bad inputs cleanly.
func TestAgentControl_Rename_Rejects(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{
		"dgx": {Kind: "vllm", Endpoint: "http://x", Model: "m"},
	}}
	_ = lc.save(dir)
	ac := &agentControl{cfgDir: dir}
	_, _ = ac.Add("a", "dgx", "", "", "")
	_, _ = ac.Add("b", "dgx", "", "", "")

	cases := []struct{ old_, new_, want string }{
		{"", "x", "both old and new"},
		{"a", "", "both old and new"},
		{"a", "a", "same"},
		{"ghost", "x", "no agent named"},
		{"a", "b", "already exists"},
		{"a", "bad name!", "unsafe characters"},
	}
	for _, c := range cases {
		_, err := ac.Rename(c.old_, c.new_)
		if err == nil {
			t.Errorf("Rename(%q,%q) should error", c.old_, c.new_)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Rename(%q,%q) error %q should contain %q", c.old_, c.new_, err, c.want)
		}
	}
}

// SetModel swaps just provider+model, preserves description + soul.
func TestAgentControl_SetModel(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{
		"dgx": {Kind: "vllm", Endpoint: "http://x", Model: "old-model"},
		"zai": {Kind: "zai", Endpoint: "https://api.z.ai", Model: "glm-4.6"},
	}}
	_ = lc.save(dir)
	ac := &agentControl{cfgDir: dir}
	_, _ = ac.Add("researcher", "dgx", "", "the role", "")
	_, _ = ac.SetSoul("researcher", "stay focused")

	// Swap to Z.ai (no model override → uses provider default).
	status, err := ac.SetModel("researcher", "zai", "")
	if err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if !strings.Contains(status, "zai/glm-4.6") {
		t.Errorf("status missing zai/glm-4.6: %q", status)
	}
	d, _ := ac.Show("researcher")
	if d.Provider != "zai" {
		t.Errorf("provider not updated: %q", d.Provider)
	}
	if d.Description != "the role" || d.Soul != "stay focused" {
		t.Errorf("SetModel clobbered preserved fields: %+v", d)
	}

	// Swap to DGX with an explicit model override.
	if _, err := ac.SetModel("researcher", "dgx", "aeon-ultimate"); err != nil {
		t.Fatalf("SetModel override: %v", err)
	}
	d, _ = ac.Show("researcher")
	if d.Provider != "dgx" || d.Model != "aeon-ultimate" {
		t.Errorf("override not applied: %+v", d)
	}
	// Soul + description still intact.
	if d.Soul != "stay focused" {
		t.Errorf("soul lost on second swap: %q", d.Soul)
	}
}

// SetModel rejects bad inputs.
func TestAgentControl_SetModel_Rejects(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{
		"dgx":         {Kind: "vllm", Endpoint: "http://x", Model: "m"},
		"vllm-no-mdl": {Kind: "vllm", Endpoint: "http://x"},
	}}
	_ = lc.save(dir)
	ac := &agentControl{cfgDir: dir}
	_, _ = ac.Add("researcher", "dgx", "", "", "")

	cases := []struct{ name, prov, mdl, want string }{
		{"", "dgx", "", "name and provider"},
		{"researcher", "", "", "name and provider"},
		{"ghost", "dgx", "", "no agent named"},
		{"researcher", "nope", "", "is not configured"},
		{"researcher", "vllm-no-mdl", "", "has no Model"},
	}
	for _, c := range cases {
		_, err := ac.SetModel(c.name, c.prov, c.mdl)
		if err == nil {
			t.Errorf("SetModel(%q,%q,%q) should error", c.name, c.prov, c.mdl)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("SetModel(%q,%q,%q) error %q should contain %q", c.name, c.prov, c.mdl, err, c.want)
		}
	}
}

// safeAgentName guards the spawn-id slug shape.
func TestSafeAgentName(t *testing.T) {
	good := []string{"researcher", "data_analyst", "synth-2", "a", "X"}
	bad := []string{"", "bad name", "../etc", "rm-rf/", "a:b", "name."}
	for _, n := range good {
		if !safeAgentName(n) {
			t.Errorf("safeAgentName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if safeAgentName(n) {
			t.Errorf("safeAgentName(%q) = true, want false", n)
		}
	}
}

// renderAgentsForOrchestrator builds the system-prompt snippet describing
// available named agents. Empty registry → empty string (orchestrator's
// prompt unchanged).
func TestRenderAgentsForOrchestrator(t *testing.T) {
	if got := renderAgentsForOrchestrator(nil); got != "" {
		t.Errorf("nil registry should produce empty, got %q", got)
	}
	if got := renderAgentsForOrchestrator(map[string]*agentProfile{}); got != "" {
		t.Errorf("empty registry should produce empty, got %q", got)
	}
	got := renderAgentsForOrchestrator(map[string]*agentProfile{
		"researcher": {Provider: "zai", Model: "glm-4.6", Description: "web research"},
		"writer":     {Provider: "dgx", Description: "synthesis"},
	})
	for _, want := range []string{
		"researcher", "zai/glm-4.6", "web research",
		"writer", "dgx", "synthesis",
		"spawn_agent(role=<name>",
		// TEN-139: coding-delegation steering present when agents exist.
		"coding/implementation specialist", "spawn_agent BY DEFAULT",
		// TEN-140: delegate-and-keep-working steering + the must-await boundary.
		"keep doing your own independent work", "team_await only when you need their results",
		"always before any final answer that depends on",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prompt missing %q:\n%s", want, got)
		}
	}
}

// TEN-139/140: the delegation + keep-working steering must NOT leak into the
// no-team prompt — the len(agents)==0 short-circuit keeps it empty so operators
// who never define agents see an unchanged orchestrator prompt.
func TestRenderAgentsForOrchestrator_NoSteeringWithoutAgents(t *testing.T) {
	for _, agents := range []map[string]*agentProfile{nil, {}} {
		got := renderAgentsForOrchestrator(agents)
		if got != "" {
			t.Errorf("no agents must yield empty prompt, got %q", got)
		}
	}
}

// profileSystemPrompt appends the profile's soul to the base member prompt.
// Empty soul → no change (backward compatible).
func TestProfileSystemPrompt(t *testing.T) {
	base := profileSystemPrompt("main-r-1", "researcher", "main", "")
	if !strings.Contains(base, "You are") {
		t.Errorf("base prompt missing: %q", base)
	}
	if strings.Contains(base, "--- Your identity ---") {
		t.Error("empty soul should not add identity block")
	}

	withSoul := profileSystemPrompt("main-r-1", "researcher", "main",
		"You are a thorough web researcher. Cite every claim.")
	if !strings.Contains(withSoul, "--- Your identity ---") {
		t.Error("non-empty soul should add identity block")
	}
	if !strings.Contains(withSoul, "thorough web researcher") {
		t.Error("soul body missing")
	}
	// Base prompt content STILL present (not replaced).
	if !strings.Contains(withSoul, "You are") {
		t.Error("base member prompt missing when soul added")
	}
}

// buildProfileRouter validates inputs upfront (missing provider, missing key
// on a keyed kind, missing model). Real router build is integration-tested
// via the live DGX run.
func TestBuildProfileRouter_RejectsMisconfig(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{
		"dgx-good":    {Kind: "vllm", Endpoint: "http://x", Model: "m"},
		"zai-no-key":  {Kind: "zai", Endpoint: "https://api.z.ai", Model: "glm-4.6", Auth: authCfg{Mode: "apikey", Stored: true}},
		"vllm-no-mdl": {Kind: "vllm", Endpoint: "http://x"},
	}}
	_ = lc.save(dir)

	cases := []struct {
		name, profile, want string
		ap                  *agentProfile
	}{
		{"nil profile", "x", "nil", nil},
		{"missing provider", "x", "unknown provider", &agentProfile{Provider: "ghost"}},
		{"keyed no secret", "x", "needs an API key", &agentProfile{Provider: "zai-no-key"}},
		{"no model resolvable", "x", "no Model configured", &agentProfile{Provider: "vllm-no-mdl"}},
	}
	for _, c := range cases {
		_, err := buildProfileRouter(c.profile, c.ap, lc, dir, model.Profile{}, nil)
		if err == nil {
			t.Errorf("[%s] should error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("[%s] error %q should contain %q", c.name, err.Error(), c.want)
		}
	}

	// Happy path: vllm with model → router builds.
	r, err := buildProfileRouter("ok", &agentProfile{Provider: "dgx-good"}, lc, dir, model.Profile{}, nil)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if r == nil {
		t.Fatal("router nil on happy path")
	}
}

// launchConfig round-trip preserves Agents.
func TestLaunchConfig_AgentsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &launchConfig{
		Agents: map[string]*agentProfile{
			"researcher": {Provider: "zai", Model: "glm-4.6", Description: "web", Soul: "be thorough"},
			"writer":     {Provider: "dgx"},
		},
	}
	if err := in.save(dir); err != nil {
		t.Fatal(err)
	}
	out, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Agents) != 2 {
		t.Fatalf("want 2 agents, got %d", len(out.Agents))
	}
	r := out.Agents["researcher"]
	if r == nil || r.Provider != "zai" || r.Model != "glm-4.6" || r.Soul != "be thorough" {
		t.Errorf("round-trip lost data: %+v", r)
	}
	// Verify the file itself is JSON-valid + readable.
	if _, err := loadLaunchConfig(dir); err != nil {
		t.Errorf("re-read failed: %v", err)
	}
	_ = filepath.Join // keep import live for portability
}
