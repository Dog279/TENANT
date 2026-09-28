package main

import (
	"errors"
	"testing"

	"tenant/internal/dashboard"
)

// dashTeam is a config with a self-hosted and a hosted provider, a live team
// runtime, and the Helpers page's adapter over one agentControl.
func dashTeam(t *testing.T) (dashAgents, *TeamRuntime, string) {
	t.Helper()
	dir := t.TempDir()
	lc := &launchConfig{
		Provider: "gpu",
		Providers: map[string]*providerConfig{
			"gpu": {Kind: "vllm", Endpoint: "http://localhost:8000", Model: "aeon-ultimate"},
			"zai": {Kind: "zai", Endpoint: "https://api.z.ai/api/paas/v4", Model: "glm-4.6"},
		},
	}
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
	rt := newTeamRuntime(TeamConfig{})
	return newDashAgents(&agentControl{cfgDir: dir, rt: rt}).(dashAgents), rt, dir
}

func dashView(t *testing.T, d dashAgents, name string) dashboard.AgentProfileView {
	t.Helper()
	rows, err := d.Agents()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("%s missing from %+v", name, rows)
	return dashboard.AgentProfileView{}
}

// Every change from the page is saved to config.json and reaches the live
// registry the next spawn reads, and a model change keeps the instructions.
func TestDashAgents_LiveAndPersisted(t *testing.T) {
	d, rt, dir := dashTeam(t)
	both := func(name string) (*agentProfile, *agentProfile) {
		t.Helper()
		lc, err := loadLaunchConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		return lc.Agents[name], rt.AgentProfiles()[name]
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(d.Add("livecheck", "gpu", "", "checks things"))
	if saved, live := both("livecheck"); saved == nil || live == nil || saved.Description != "checks things" {
		t.Fatalf("after Add: saved %+v, live %+v", saved, live)
	}
	must(d.SetSoul("livecheck", "Be terse."))
	must(d.SetModel("livecheck", "zai", "glm-4.6"))
	saved, live := both("livecheck")
	if saved.Soul != "Be terse." || live.Soul != "Be terse." || saved.Provider != "zai" || live.Model != "glm-4.6" || saved.Description != "checks things" {
		t.Errorf("SetModel lost the instructions or didn't apply: saved %+v, live %+v", saved, live)
	}
	if v := dashView(t, d, "livecheck"); v.SendsTo != "Z.ai" || v.Model != "glm-4.6" || v.Builtin || v.Customized || !v.Valid || !v.HasSoul {
		t.Errorf("view = %+v", v)
	}
	if soul, err := d.Soul("livecheck"); err != nil || soul != "Be terse." {
		t.Errorf("Soul = %q, %v", soul, err)
	}

	must(d.Rename("livecheck", "livecheck2"))
	if old, _ := both("livecheck"); old != nil || rt.AgentProfiles()["livecheck"] != nil {
		t.Error("the old name survived the rename")
	}
	if saved, live := both("livecheck2"); saved == nil || live == nil || saved.Soul != "Be terse." {
		t.Errorf("after Rename: saved %+v, live %+v", saved, live)
	}
	must(d.Remove("livecheck2"))
	if saved, live := both("livecheck2"); saved != nil || live != nil {
		t.Errorf("after Remove: saved %+v, live %+v", saved, live)
	}

	// A built-in: editing makes it Customized (and still valid on the main
	// model); Reset (Remove) makes it the shipped one again.
	if v := dashView(t, d, "QA"); !v.Builtin || v.Customized || !v.Valid || v.Provider != "" || v.SendsTo != "" {
		t.Errorf("shipped QA = %+v", v)
	}
	must(d.SetSoul("QA", "Break it."))
	if v := dashView(t, d, "QA"); v.Builtin || !v.Customized || !v.Valid || v.Provider != "" || v.Model != "" {
		t.Errorf("customized QA = %+v", v)
	}
	if live := rt.AgentProfiles()["QA"]; live == nil || live.Soul != "Break it." {
		t.Errorf("customized QA live = %+v", live)
	}
	must(d.Remove("QA"))
	if v := dashView(t, d, "QA"); !v.Builtin || v.Customized {
		t.Errorf("reset QA = %+v", v)
	}
	if soul, err := d.Soul("Programmer"); err != nil || soul == "" {
		t.Errorf("the Programmer's shipped instructions = %q, %v", soul, err)
	}
}

// The errors the page shows as they are (PlainError), each pinned against
// agentControl's own wording, and the dashboard's stricter name rules.
func TestDashAgents_PlainErrors(t *testing.T) {
	d, _, dir := dashTeam(t)
	if err := d.Add("mine", "gpu", "", ""); err != nil {
		t.Fatal(err)
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	lc.Providers["bare"] = &providerConfig{Kind: "vllm", Endpoint: "http://localhost:9000"} // no default model
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"add over a built-in, any case", d.Add("programmer", "gpu", "", ""), "A helper named Programmer already exists."},
		{"add over one's own", d.Add("MINE", "gpu", "", ""), "A helper named mine already exists."},
		{"unsafe name", d.Add("my helper", "gpu", "", ""), "Helper names can use only letters, numbers, dashes and underscores."},
		{"unknown provider", d.Add("h1", "cloud", "", ""), "That model provider isn't set up yet. Add it on the Model page."},
		{"unknown provider on edit", d.SetModel("mine", "cloud", ""), "That model provider isn't set up yet. Add it on the Model page."},
		{"provider without a model", d.Add("h2", "bare", "", ""), "That model provider has no default model. Type the model's name."},
		{"rename a built-in", d.Rename("programmer", "coder"), "Built-in helpers can't be renamed."},
		{"rename onto a built-in", d.Rename("mine", "qa"), "A helper named QA already exists."},
		{"remove a built-in", d.Remove("Writer"), "Built-in helpers can't be removed."},
		{"rename to its own name", d.Rename("mine", "mine"), "That's already its name."},
	} {
		var pe dashboard.PlainError
		if !errors.As(tc.err, &pe) || string(pe) != tc.want {
			t.Errorf("%s: %v, want PlainError %q", tc.name, tc.err, tc.want)
		}
	}
	// A change of case alone is a rename, not a clash.
	if err := d.Rename("mine", "Mine"); err != nil {
		t.Errorf("rename mine → Mine: %v", err)
	}
	if note := (dashAgents{service: true}).RestartNote(); note != "Restart the Tenant service (open Services, find Tenant, click Restart)." {
		t.Errorf("service restart note = %q", note)
	}
	if note := (dashAgents{}).RestartNote(); note != "" {
		t.Errorf("restart note off a service = %q", note)
	}
}

// Only hosted-API kinds say where the work goes; self-hosted ones (even on
// another machine) and unknown kinds say nothing. Every hosted kind in the
// catalog gets a name.
func TestSendsTo_HostedKindsOnly(t *testing.T) {
	for kind, want := range map[string]string{
		"openai": "OpenAI", "anthropic": "Anthropic", "claudecode": "Anthropic", "grok": "Grok",
		"zai": "Z.ai", "zai-metered": "Z.ai", "sakana": "Sakana AI",
		"vllm": "", "llamacpp": "", "ollama": "", "echo": "", "nosuch": "",
	} {
		if got := sendsTo(kind); got != want {
			t.Errorf("sendsTo(%q) = %q, want %q", kind, got, want)
		}
	}
	for kind, pk := range providerKinds {
		if got := sendsTo(kind); (got == "") != pk.Local {
			t.Errorf("sendsTo(%q) = %q with Local %v", kind, got, pk.Local)
		}
	}
}
