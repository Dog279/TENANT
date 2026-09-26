package cmdreg

import (
	"testing"
)

type run func() string

func cmd(names []string, section, summary string, rows ...Row) *Command[run] {
	return &Command[run]{Names: names, Section: section, Summary: summary, Rows: rows, Run: func() string { return names[0] }}
}

var sections = []Section{
	{ID: "a", Title: "Alpha", Aliases: []string{"first"}, Keys: []Row{{Syntax: "Ctrl-X", Desc: "a key"}}},
	{ID: "b", Title: "Bravo"},
}

func TestLookupAndRows(t *testing.T) {
	r, err := New(sections, []*Command[run]{
		cmd([]string{"/one", "/uno"}, "a", "the first", Row{Syntax: "/one x", Desc: "with x"}, Row{Syntax: "/one y", Desc: "listed in b", Section: "b"}),
		cmd([]string{"/two"}, "b", "the second"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Lookup("/UNO"); c == nil || c.Run() != "/one" {
		t.Fatal("alias lookup failed")
	}
	if r.Lookup("/three") != nil {
		t.Fatal("found a command that does not exist")
	}
	a := r.Rows("a")
	if len(a) != 2 || a[0].Syntax != "/one x" || a[1].Syntax != "Ctrl-X" {
		t.Fatalf("section a rows = %+v", a)
	}
	b := r.Rows("b")
	if len(b) != 2 || b[0].Syntax != "/one y" || b[1].Syntax != "/two" || b[1].Desc != "the second" {
		t.Fatalf("section b rows = %+v", b)
	}
	if s, ok := r.Section("FIRST"); !ok || s.ID != "a" {
		t.Fatal("section alias lookup failed")
	}
}

func TestNewRefusesBadCommands(t *testing.T) {
	bad := map[string][]*Command[run]{
		"no name":         {{Section: "a", Summary: "x", Run: func() string { return "" }}},
		"no summary":      {cmd([]string{"/x"}, "a", "")},
		"unknown section": {cmd([]string{"/x"}, "zz", "x")},
		"no handler":      {{Names: []string{"/x"}, Section: "a", Summary: "x"}},
		"duplicate name":  {cmd([]string{"/x"}, "a", "x"), cmd([]string{"/y", "/X"}, "b", "y")},
		"bad row section": {cmd([]string{"/x"}, "a", "x", Row{Syntax: "/x", Section: "zz"})},
	}
	for name, cmds := range bad {
		if _, err := New(sections, cmds); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New[run]([]Section{{ID: "a"}, {ID: "a"}}, nil); err == nil {
		t.Error("duplicate section accepted")
	}
}

func TestTable(t *testing.T) {
	got := Table([]Row{{Syntax: "/a", Desc: "short"}, {Syntax: "/a-very-long-command", Desc: "long"}}, "  ", 8)
	want := "  /a      short\n  /a-very-long-command  long\n"
	if got != want {
		t.Fatalf("Table = %q, want %q", got, want)
	}
	if Table(nil, "  ", 4) != "" {
		t.Fatal("an empty table rendered something")
	}
}
