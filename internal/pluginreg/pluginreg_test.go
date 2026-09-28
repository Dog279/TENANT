package pluginreg

import (
	"strings"
	"testing"
)

type desc struct {
	id      string
	tools   []string
	actions map[string]string
	events  *Events
}

func (d desc) PluginID() string                    { return d.id }
func (d desc) ToolNames() []string                 { return d.tools }
func (d desc) ActionCategories() map[string]string { return d.actions }
func (d desc) PluginEvents() *Events               { return d.events }

var rules = Rules{Categories: []string{"send", "write"}, Reserved: []string{"memory", "mcp:*"}}

func TestNewIndexesThePlugins(t *testing.T) {
	r, err := New(rules,
		desc{id: "x", tools: []string{"x_post", "x_search"}, actions: map[string]string{"x_post": "send"}},
		desc{id: "sql", tools: []string{"sql_query"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if all := r.All(); len(all) != 2 || all[0].id != "x" || all[1].id != "sql" {
		t.Fatalf("All = %+v, want registration order", all)
	}
	if id, ok := r.OwnerOfTool("x_search"); !ok || id != "x" {
		t.Fatalf("OwnerOfTool(x_search) = %q %v", id, ok)
	}
	if _, ok := r.OwnerOfTool("web_read"); ok {
		t.Fatal("an undeclared tool has an owner")
	}
	if cat, ok := r.ActionCategory("x_post"); !ok || cat != "send" {
		t.Fatalf("ActionCategory(x_post) = %q %v", cat, ok)
	}
	if got := r.Actions(); len(got) != 1 || got["x_post"] != "send" {
		t.Fatalf("Actions = %v", got)
	}
	if d, ok := r.Get("sql"); !ok || d.id != "sql" {
		t.Fatalf("Get(sql) = %+v %v", d, ok)
	}
}

func TestNewRefusesBrokenDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		ds   []desc
		want string
	}{
		{"empty id", []desc{{id: ""}}, "empty or padded"},
		{"padded id", []desc{{id: " x"}}, "empty or padded"},
		{"duplicate id", []desc{{id: "x"}, {id: "x"}}, "declared twice"},
		{"reserved id", []desc{{id: "memory"}}, "reserved"},
		{"reserved prefix", []desc{{id: "mcp:jira"}}, "reserved"},
		{"shared tool", []desc{{id: "a", tools: []string{"t"}}, {id: "b", tools: []string{"t"}}}, `tool "t" is declared by both "a" and "b"`},
		{"unknown category", []desc{{id: "a", actions: map[string]string{"a_do": "nuke"}}}, "unknown category"},
		{"shared action", []desc{{id: "a", actions: map[string]string{"go": "send"}}, {id: "b", actions: map[string]string{"go": "write"}}}, `action "go" is declared by both`},
	} {
		if _, err := New(rules, tc.ds...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestMustNewPanicsOnABug(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustNew accepted a duplicate plugin")
		}
	}()
	MustNew(rules, desc{id: "x"}, desc{id: "x"})
}
