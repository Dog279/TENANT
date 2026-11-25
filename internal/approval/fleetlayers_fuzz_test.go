package approval

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// fuzzLayer checks one key's text as LoadLayers reads it: either the whole
// key is read (every value valid, within its bounds) and what is saved reads
// back as the same thing, or it is refused as a whole, the fresh copy and
// apply are refused, and the text is written back as it was.
func fuzzLayer(t *testing.T, defaults, groups []byte) {
	const id = "m0000000000000001"
	f := NewFleet(testCategorize)
	if err := f.Put(id, Scheme{}); err != nil {
		t.Fatal(err)
	}
	var savedDefaults, savedGroups json.RawMessage
	saved := false
	f.SetPersistLayers(func(snapshot func() (json.RawMessage, json.RawMessage)) {
		savedDefaults, savedGroups = snapshot()
		saved = true
	})
	_, problems := f.LoadLayers(defaults, groups)
	if n, p := f.LoadLayers(defaults, groups); n != 0 || len(p) != 0 {
		t.Fatalf("the same text again: %d change(s), %v", n, p)
	}
	_, freshErr := f.Fresh(id, Scheme{})
	if (len(problems) == 0) != (freshErr == nil) {
		t.Fatalf("problems %v, but Fresh says %v", problems, freshErr)
	}
	for _, p := range problems {
		if len(p) > 600 {
			t.Fatalf("a problem %d bytes long", len(p))
		}
	}
	all, groupsErr := f.Groups()
	if groupsErr == nil {
		if len(all) > maxGroups {
			t.Fatalf("%d groups", len(all))
		}
		for _, g := range all {
			if ValidGroupName(g.Name) != nil || len(g.Machines) > maxGroupMembers || len(g.ToolRules) > maxGroupRules {
				t.Fatalf("a group past the checks: %+v", g)
			}
			for _, m := range g.Machines {
				if !machineID.MatchString(m) {
					t.Fatalf("member %q", m)
				}
			}
			if _, err := strictRules(g.ToolRules); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.Reapply(id); err != nil {
			t.Fatalf("apply with readable groups: %v", err)
		}
	} else if _, err := f.Reapply(id); err == nil {
		t.Fatal("applied with groups that can't be read")
	}
	d, defaultsErr := f.Defaults()
	for cat, mode := range d {
		if m, ok := ParseMode(mode); !isCategory(cat) || !ok || m.String() != mode {
			t.Fatalf("default %q=%q", cat, mode)
		}
	}
	// Save, through whichever key can change, and read it back elsewhere.
	if groupsErr == nil {
		_, _ = f.CreateGroup("fuzz-saves")
	} else {
		_ = f.SetDefault(CatMCP, "deny")
	}
	if !saved {
		return // the fuzzer named a group fuzz-saves, or made the most groups there may be
	}
	if groupsErr != nil && !bytes.Equal(savedGroups, groups) {
		t.Fatalf("groups that can't be read written back as %q", savedGroups)
	}
	if defaultsErr != nil && !bytes.Equal(savedDefaults, defaults) {
		t.Fatalf("defaults that can't be read written back as %q", savedDefaults)
	}
	if n, p := f.LoadLayers(savedDefaults, savedGroups); n != 0 || len(p) != 0 {
		t.Fatalf("its own save read back: %d change(s), %v", n, p)
	}
	g := NewFleet(testCategorize)
	if _, p := g.LoadLayers(savedDefaults, savedGroups); len(p) != len(problems) {
		t.Fatalf("another process reads the save with problems %v, this one had %v", p, problems)
	}
}

func FuzzFleetDefaults(f *testing.F) {
	for _, seed := range []string{
		``, `{}`, `{"exec":"allow","web":"deny"}`, `{"exec":" Block "}`, `{"nope":"allow"}`, `{"exec":"sometimes"}`,
		`{"exec":"deny","exec":"allow"}`, `null`, `{"exec":null}`, `{"exec":"deny"} {}`, `["exec"]`, `{"exec":`,
		`{"EXEC":"deny"}`, `{"exec":1}`, "{\"exec\x00\":\"deny\"}", `[[[[[[[[{}]]]]]]]]`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) { fuzzLayer(t, raw, nil) })
}

func FuzzFleetGroups(f *testing.F) {
	for _, seed := range []string{
		``, `{}`, `{"web":{"machines":["m0000000000000001","m2"],"tool_rules":{"x_post":"deny","os_exec(git *)":"allow"}}}`,
		`{"Beta":{},"alpha":{"machines":["m0000000000000001"]}}`, `{"web":{"machines":["not an id"]}}`,
		`{"web":{"machines":["m1","m1"]}}`, `{"web":{},"WEB":{}}`, `{"two words":{}}`, `{"all":{}}`,
		`{"m0123456789abcdef":{}}`, `{"` + strings.Repeat("a", 64) + `":{}}`, `null`, `{"web":null}`,
		`{"web":{}} []`, `{"web":{"machine":[]}}`, `{"web":{"machines":[],"MACHINES":[]}}`,
		`{"web":{"tool_rules":{"x_post":"maybe"}}}`, `{"web":{"tool_rules":{" x_post":"deny","x_post":"allow"}}}`,
		`{"web":{"machines":null,"tool_rules":null}}`, "{\"w\x1b[2J\":{}}", `{"web":{"machines":"m1"}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) { fuzzLayer(t, nil, raw) })
}
