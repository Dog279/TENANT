package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// Every descriptor declares what AC1 of TEN-321 asks of a plugin: identity,
// version, tools and permission classes; and a plugin that gates anything
// declares the class of every gated tool.
func TestEveryPluginDeclaresItself(t *testing.T) {
	for _, d := range plugins().All() {
		if d.ID == "" || d.Title == "" || d.Version < 1 {
			t.Errorf("plugin %q lacks identity or version: %+v", d.ID, d)
		}
		tools := d.Tools()
		if len(tools) == 0 {
			t.Errorf("plugin %q declares no tools", d.ID)
		}
		gated := 0
		for _, spec := range tools {
			if spec.Gated {
				gated++
			}
		}
		if gated > 0 && len(d.Actions) == 0 {
			t.Errorf("plugin %q has %d gated tool(s) but declares no permission class", d.ID, gated)
		}
		if !d.PerAgent && d.ID != "os" && d.StubHint == "" {
			t.Errorf("plugin %q has no stub hint: an unconfigured plugin would be invisible", d.ID)
		}
		if d.Configure != nil {
			if k := d.Configure(); k.ID != d.ID {
				t.Errorf("plugin %q's config schema is filed as %q", d.ID, k.ID)
			}
		}
	}
}

// The host's own approval actions and the plugins' never overlap, so a
// category is never decided by which table was read first.
func TestHostAndPluginActionsAreDisjoint(t *testing.T) {
	for action := range hostActionCategories {
		if _, ok := plugins().ActionCategory(action); ok {
			t.Errorf("action %q is declared by the host and by a plugin", action)
		}
	}
	all := allActionCategories()
	for action, cat := range all {
		if got := categorize(action); got != cat {
			t.Errorf("categorize(%q) = %q, but the table says %q", action, got, cat)
		}
	}
}

// The shared mux built with no plugin configured carries a stub for exactly
// the registry's shared plugins, each under its declared id with its declared
// tools, disabled; os is registered for real; web is left to each agent.
func TestTheStubCatalogIsTheRegistry(t *testing.T) {
	c := &commonFlags{cfgDir: t.TempDir(), dataDir: t.TempDir()}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	byPlugin := map[string][]string{}
	for _, ti := range mux.ToolList() {
		byPlugin[ti.Plugin] = append(byPlugin[ti.Plugin], ti.Name)
		if ti.Enabled && ti.Plugin != "memory" {
			t.Errorf("%s (%s) is enabled with nothing configured", ti.Name, ti.Plugin)
		}
	}
	for _, d := range plugins().All() {
		got := byPlugin[d.ID]
		if d.PerAgent {
			if len(got) != 0 {
				t.Errorf("per-agent plugin %q is on the shared mux: %v", d.ID, got)
			}
			continue
		}
		want := d.ToolNames()
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("plugin %q registered %v, declares %v", d.ID, got, want)
		}
	}
}

// /configure lists exactly the plugins whose schema it manages; `tenant setup`
// offers its plugins in their declared order.
func TestTheConfigureSurfacesAreTheRegistry(t *testing.T) {
	for _, d := range plugins().All() {
		_, inConfigure := skillKinds[d.ID]
		want := d.Configure != nil && !d.SetupOnly
		if inConfigure != want {
			t.Errorf("plugin %q: in /configure = %v, want %v", d.ID, inConfigure, want)
		}
		_, setupOnly := setupOnlyKinds()[d.ID]
		if setupOnly != (d.SetupOnly && d.Configure != nil) {
			t.Errorf("plugin %q: setup-only = %v", d.ID, setupOnly)
		}
	}
	var ids []string
	for _, k := range wizardCatalog(t.TempDir()) {
		ids = append(ids, k.ID)
	}
	if got := strings.Join(ids, ","); got != "wiki,sql,gsuite,x,imessage,discord,github,web,os" {
		t.Errorf("tenant setup order = %s", got)
	}
}

// Each plugin's CLI commands and doctor checks reach the CLI and doctor.
func TestPluginCommandsAndChecksAreWired(t *testing.T) {
	for _, d := range plugins().All() {
		for _, c := range d.CLI {
			if got := cliRegistry().Lookup(c.Names[0]); got == nil {
				t.Errorf("plugin %q's command %q is not in the CLI", d.ID, c.Names[0])
			}
		}
	}
	names := map[string]bool{}
	for _, ck := range pluginDoctorChecks() {
		names[ck.Name] = true
	}
	for _, want := range []string{"wiki dir (if configured)", "chrome (if web enabled)", "gsuite (if enabled)", "os_processes tool"} {
		if !names[want] {
			t.Errorf("doctor lacks the plugin check %q", want)
		}
	}
}

// A tool override counts toward its plugin's group by the registry's exact
// tool index, not by a name prefix (the gsuite_ prefix matched no tool).
func TestToolGroupsUseTheRegistry(t *testing.T) {
	c := &commonFlags{lc: &launchConfig{}}
	groups := configuredToolGroups(c, &pluginFlags{}, map[string]bool{"gmail_send": true, "github_get_pr": true, "money_spend": true, "memory_search": true})
	for _, want := range []string{"gsuite", "github", "simplefin", "memory"} {
		if !slices.Contains(groups, want) {
			t.Errorf("groups = %v, lacks %q", groups, want)
		}
	}
}
