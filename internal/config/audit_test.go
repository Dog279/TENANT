package config

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/logs/event"
)

type audits []*event.Event

func (a *audits) Emit(context.Context, *event.Event) {}
func (a *audits) Audit(_ context.Context, e *event.Event) error {
	*a = append(*a, e)
	return nil
}

func (a audits) field(i int, key string) string {
	for _, f := range a[i].Data {
		if f.Key == key {
			return f.Text()
		}
	}
	return ""
}

// Every save of config.json or a settings file that changes something files
// one Security record: the file and each changed setting, old → new, with
// secret values (dashboard.auth, an MCP server's env_set) replaced by their
// names.
func TestSavesAreAuditedWithSecretsByName(t *testing.T) {
	var got audits
	event.Install(&got)
	t.Cleanup(func() { event.Install(nil) })
	dir := t.TempDir()

	lc := &Launch{Provider: "zai", Providers: map[string]*Provider{"zai": {Kind: "zai", Model: "glm-4.6"}}}
	lc.Dashboard.Auth = "dash-token-value-1"
	lc.MCPServers = []MCPServer{{Name: "files", Command: []string{"srv"}, EnvSet: map[string]string{"API": "env-secret-value"}}}
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := lc.Save(dir); err != nil { // unchanged: no record
		t.Fatal(err)
	}
	if err := Persist(dir, nil, func(l *Launch) { l.Providers["zai"].Model = "glm-5"; l.Dashboard.Auth = "dash-token-value-2" }); err != nil {
		t.Fatal(err)
	}
	s := &Settings{Tools: map[string]bool{"os_exec": false}}
	if err := s.Save(dir, "alpha"); err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("%d records, want 3", len(got))
	}
	for i, e := range got {
		if e.Provider != "security" || e.ID != 4501 {
			t.Errorf("record %d is %s/%d", i, e.Provider, e.ID)
		}
		for _, f := range e.Data {
			if strings.Contains(f.Text(), "value") {
				t.Errorf("record %d holds a secret: %s=%s", i, f.Key, f.Text())
			}
		}
	}
	first := got.field(0, "diff")
	for _, want := range []string{
		"dashboard.auth: (none) → «secret:dashboard.auth»",
		"mcp_servers[0].env_set.API: (none) → «secret:mcp_servers[0].env_set.API»",
		`providers.zai.model: (none) → "glm-4.6"`,
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first save's diff lacks %q:\n%s", want, first)
		}
	}
	if d := got.field(1, "diff"); got.field(1, "file") != "config.json" || got.field(1, "changes") != "2" ||
		d != "dashboard.auth: «secret:dashboard.auth» → «secret:dashboard.auth»\nproviders.zai.model: \"glm-4.6\" → \"glm-5\"" {
		t.Errorf("the change: %s, %s changes:\n%s", got.field(1, "file"), got.field(1, "changes"), d)
	}
	if got.field(2, "file") != "settings.alpha.json" || got.field(2, "diff") != "tools.os_exec: (none) → false" {
		t.Errorf("the settings file: %s:\n%s", got.field(2, "file"), got.field(2, "diff"))
	}
}
