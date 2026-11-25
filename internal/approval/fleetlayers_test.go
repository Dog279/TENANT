package approval

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
)

// layered is a fleet whose layers are loaded from the settings file's text.
func layered(t *testing.T, rules, defaults, groups string) *Fleet {
	t.Helper()
	f := NewFleet(testCategorize)
	if _, problems := f.Load(nil, json.RawMessage(rules)); len(problems) != 0 {
		t.Fatal(problems)
	}
	if _, problems := f.LoadLayers(json.RawMessage(defaults), json.RawMessage(groups)); len(problems) != 0 {
		t.Fatal(problems)
	}
	return f
}

func mustFresh(t *testing.T, f *Fleet, id string, local Scheme) Scheme {
	t.Helper()
	s, err := f.Fresh(id, local)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The fresh copy, with a conflict at every boundary: each layer wins over
// the one before it, and groups are laid in name order without regard to
// case.
func TestFleet_FreshLayersInOrder(t *testing.T) {
	local := Scheme{
		Permissions: map[string]string{CatExec: "allow", CatWeb: "allow", CatSend: "deny"},
		ToolRules:   map[string]string{"x_post": "allow", "web_interact": "ask"},
	}
	const id, other = "m0000000000000001", "m0000000000000002"
	f := layered(t,
		`{"x_post":"deny","web_interact":"deny"}`,
		`{"exec":"allow","web":"deny"}`,
		`{"Beta":{"machines":["`+id+`"],"tool_rules":{"x_post":"ask","os_write":"deny"}},
		  "alpha":{"machines":["`+id+`","`+other+`"],"tool_rules":{"x_post":"allow","web_interact":"allow","os_write":"allow"}}}`)
	s := mustFresh(t, f, id, local)
	for cat, want := range map[string]string{
		CatExec: "allow", // the master allows, the copy clamps to ask, the fleet default says allow: the default wins
		CatWeb:  "deny",  // the master allows, the fleet default denies
		CatSend: "deny",  // the master's own, no default
		CatMCP:  "ask",
	} {
		if s.Permissions[cat] != want {
			t.Errorf("%s is %q, want %q (%v)", cat, s.Permissions[cat], want, s.Permissions)
		}
	}
	for p, want := range map[string]string{
		"web_interact": "allow", // master ask < fleet rule deny < group alpha allow
		"x_post":       "ask",   // master allow < fleet deny < alpha allow < Beta ask: Beta comes after alpha
		"os_write":     "deny",  // alpha allow < Beta deny
	} {
		if s.ToolRules[p] != want {
			t.Errorf("rule %s is %q, want %q (%v)", p, s.ToolRules[p], want, s.ToolRules)
		}
	}
	// The other machine is in alpha alone.
	if o := mustFresh(t, f, other, local); o.ToolRules["x_post"] != "allow" || o.ToolRules["os_write"] != "allow" {
		t.Errorf("a member of alpha alone: %v", o.ToolRules)
	}
}

// A machine in no group gets today's Inherit, with the default categories in
// it; no default and no group is exactly the copy as it always was.
func TestFleet_FreshWithoutGroupsIsInherit(t *testing.T) {
	local := Scheme{Permissions: map[string]string{CatExec: "allow", CatWeb: "allow"}, ToolRules: map[string]string{"x_post": "allow"}}
	plainFleet := layered(t, `{"os_exec(df *)":"allow"}`, ``, ``)
	before := mustInherit(t, plainFleet, local)
	if got := mustFresh(t, plainFleet, "m0000000000000009", local); schemeJSON(got) != schemeJSON(before) {
		t.Fatalf("no layer: fresh %s, inherit %s", schemeJSON(got), schemeJSON(before))
	}
	f := layered(t, `{"os_exec(df *)":"allow"}`, `{"mcp":"deny"}`, `{"web":{"machines":["m0000000000000001"],"tool_rules":{"x_post":"deny"}}}`)
	want := before
	want.Permissions = maps.Clone(before.Permissions)
	want.Permissions[CatMCP] = "deny"
	if got := mustFresh(t, f, "m0000000000000009", local); schemeJSON(got) != schemeJSON(want) {
		t.Fatalf("in no group: fresh %s, want %s", schemeJSON(got), schemeJSON(want))
	}
	if got := mustInherit(t, f, local); schemeJSON(got) != schemeJSON(want) {
		t.Fatalf("Inherit has the default categories: %s, want %s", schemeJSON(got), schemeJSON(want))
	}
}

// After a layer is laid, the machine's own edit stands, whatever the group
// does meanwhile, until the operator applies again: then a laid rule
// replaces every own rule it answers for, and the categories stay.
func TestFleet_OwnEditStandsUntilApply(t *testing.T) {
	const id = "m00000000000000aa"
	f := layered(t, `{"x_post":"deny"}`, ``, `{"ops":{"machines":["`+id+`"],"tool_rules":{"os_exec(git *)":"deny"}}}`)
	if err := f.Put(id, mustFresh(t, f, id, Scheme{})); err != nil {
		t.Fatal(err)
	}
	b, _ := f.Child(id)
	if _, err := b.SetPermissionFrom("test", CatExec, "allow"); err != nil {
		t.Fatal(err)
	}
	for p, m := range map[string]string{"os_exec(git status)": "allow", "x_post": "allow", "web_interact": "allow"} {
		if err := b.SetRule(p, m); err != nil {
			t.Fatal(err)
		}
	}
	// The group changes: the machine does not move.
	if err := f.SetGroupRule("OPS", "web_interact", "deny"); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.Scheme(id); s.ToolRules["web_interact"] != "allow" || s.ToolRules["os_exec(git status)"] != "allow" || s.ToolRules["x_post"] != "allow" {
		t.Fatalf("a group's change moved its member: %v", s.ToolRules)
	}
	laid, err := f.Reapply(id)
	if err != nil || len(laid) != 1 || laid[0] != "ops" {
		t.Fatalf("apply: %v %v", laid, err)
	}
	s, _ := f.Scheme(id)
	for p, want := range map[string]string{
		"os_exec(git *)":      "deny",
		"os_exec(git status)": "", // the group's git * answers for it: gone
		"x_post":              "deny",
		"web_interact":        "deny",
	} {
		if s.ToolRules[p] != want {
			t.Errorf("after apply, rule %s is %q, want %q (%v)", p, s.ToolRules[p], want, s.ToolRules)
		}
	}
	if s.Permissions[CatExec] != "allow" {
		t.Errorf("apply moved a category: exec is %s", s.Permissions[CatExec])
	}
}

// Every hostile text of either key is refused as a whole: the fresh copy and
// apply refuse, the text survives a save as written, and machines that have
// a scheme keep deciding by it.
func TestFleet_LayersFailClosed(t *testing.T) {
	many := func(n int, item func(i int) string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = item(i)
		}
		return strings.Join(parts, ",")
	}
	badDefaults := map[string]string{
		"unknown category": `{"nope":"allow"}`,
		"bad mode":         `{"exec":"sometimes"}`,
		"duplicate key":    `{"exec":"deny","exec":"allow"}`,
		"null":             `null`,
		"null mode":        `{"exec":null}`,
		"trailing data":    `{"exec":"deny"} {}`,
		"not an object":    `["exec"]`,
		"cut short":        `{"exec":`,
		"upper case":       `{"EXEC":"deny"}`,
	}
	badGroups := map[string]string{
		"bad member id":        `{"web":{"machines":["not an id"]}}`,
		"member twice":         `{"web":{"machines":["m1","m1"]}}`,
		"names differ in case": `{"web":{},"WEB":{}}`,
		"name twice":           `{"web":{},"web":{}}`,
		"bad name":             `{"two words":{}}`,
		"name starts with -":   `{"-web":{}}`,
		"name all":             `{"All":{}}`,
		"name an id":           `{"m0123456789abcdef":{}}`,
		"name too long":        `{"` + strings.Repeat("a", 64) + `":{}}`,
		"null":                 `null`,
		"null group":           `{"web":null}`,
		"trailing data":        `{"web":{}} []`,
		"unknown field":        `{"web":{"machine":["m1"]}}`,
		"field twice in case":  `{"web":{"machines":[],"MACHINES":["m1"]}}`,
		"bad rule mode":        `{"web":{"tool_rules":{"x_post":"maybe"}}}`,
		"bad rule pattern":     `{"web":{"tool_rules":{"two words":"deny"}}}`,
		"rule twice":           `{"web":{"tool_rules":{"x_post":"deny","x_post":"allow"}}}`,
		"too many groups":      `{` + many(maxGroups+1, func(i int) string { return fmt.Sprintf(`"g%d":{}`, i) }) + `}`,
		"too many members":     `{"web":{"machines":[` + many(maxGroupMembers+1, func(i int) string { return fmt.Sprintf(`"m%d"`, i) }) + `]}}`,
		"too many rules":       `{"web":{"tool_rules":{` + many(maxGroupRules+1, func(i int) string { return fmt.Sprintf(`"t%d":"deny"`, i) }) + `}}}`,
	}
	const id = "m00000000000000bb"
	check := func(t *testing.T, defaults, groups string, key string) {
		f := NewFleet(testCategorize)
		if err := f.Put(id, Scheme{Permissions: map[string]string{CatExec: "allow"}}); err != nil {
			t.Fatal(err)
		}
		var savedDefaults, savedGroups json.RawMessage
		f.SetPersistLayers(func(snapshot func() (json.RawMessage, json.RawMessage)) { savedDefaults, savedGroups = snapshot() })
		n, problems := f.LoadLayers(json.RawMessage(defaults), json.RawMessage(groups))
		if n != 1 || len(problems) != 1 || !strings.HasPrefix(problems[0], key+": ") {
			t.Fatalf("load: %d change(s), problems %v", n, problems)
		}
		if _, err := f.Fresh(id, Scheme{}); err == nil {
			t.Error("a fresh copy was worked out")
		}
		if _, err := f.Reapply(id); err == nil && key == "fleet_groups" {
			t.Error("a group was applied")
		}
		// The machine that has a scheme keeps deciding by it.
		host := newTestBroker(nil)
		if !f.For(LocalOrigin(host), Machine{ID: id}).Confirm(t.Context(), "os_exec", "uptime") {
			t.Error("an existing machine stopped deciding")
		}
		// The other key can still change, and the text is written back as it was.
		if key == "fleet_groups" {
			if err := f.SetDefault(CatWeb, "deny"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.CreateGroup("x"); err == nil {
				t.Error("a group was made while the groups can't be read")
			}
			if string(savedGroups) != groups {
				t.Errorf("written back as %s", savedGroups)
			}
		} else {
			if _, err := f.CreateGroup("x"); err != nil {
				t.Fatal(err)
			}
			if err := f.SetDefault(CatWeb, "deny"); err == nil {
				t.Error("a default was set while the defaults can't be read")
			}
			if string(savedDefaults) != defaults {
				t.Errorf("written back as %s", savedDefaults)
			}
		}
		// The same text again is no edit; a fix is.
		if n, p := f.LoadLayers(savedDefaults, savedGroups); n != 0 || len(p) != 0 {
			t.Errorf("its own save read back: %d change(s), %v", n, p)
		}
		if n, p := f.LoadLayers(nil, nil); n == 0 || len(p) != 0 {
			t.Errorf("fixed: %d change(s), %v", n, p)
		}
		if _, err := f.Fresh(id, Scheme{}); err != nil {
			t.Errorf("fixed, and still refused: %v", err)
		}
	}
	for name, text := range badDefaults {
		t.Run("defaults "+name, func(t *testing.T) { check(t, text, "", "fleet_permissions") })
	}
	for name, text := range badGroups {
		t.Run("groups "+name, func(t *testing.T) { check(t, "", text, "fleet_groups") })
	}
}

// Load takes as an edit only what differs from what was last read or handed
// out to be written: a change not yet saved is not undone; a hand edit is
// applied; what was saved reads back as no change.
func TestFleet_LayersLoadAndSave(t *testing.T) {
	f := NewFleet(testCategorize)
	if _, p := f.LoadLayers(json.RawMessage(`{"web":"deny"}`), json.RawMessage(`{"ops":{"machines":["m1"]}}`)); len(p) != 0 {
		t.Fatal(p)
	}
	// A change of this process that no hook has written yet.
	if err := f.SetDefault(CatExec, "deny"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddMembers("ops", []string{"m2"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.LoadLayers(json.RawMessage(`{"web":"deny"}`), json.RawMessage(`{"ops":{"machines":["m1"]}}`)); n != 0 {
		t.Fatalf("the file as last read was taken as an edit (%d)", n)
	}
	if d, _ := f.Defaults(); d[CatExec] != "deny" {
		t.Fatalf("an unsaved change was undone: %v", d)
	}
	// Saved: the hook gets what is in memory.
	var defaults, groups json.RawMessage
	f.SetPersistLayers(func(snapshot func() (json.RawMessage, json.RawMessage)) { defaults, groups = snapshot() })
	if _, err := f.CreateGroup("Web.1"); err != nil {
		t.Fatal(err)
	}
	if string(defaults) != `{"exec":"deny","web":"deny"}` || string(groups) != `{"Web.1":{},"ops":{"machines":["m1","m2"]}}` {
		t.Fatalf("saved %s %s", defaults, groups)
	}
	if n, p := f.LoadLayers(defaults, groups); n != 0 || len(p) != 0 {
		t.Fatalf("what was saved read back: %d, %v", n, p)
	}
	// Round trip into another process.
	g := NewFleet(testCategorize)
	if n, p := g.LoadLayers(defaults, groups); n != 2 || len(p) != 0 {
		t.Fatalf("another process: %d, %v", n, p)
	}
	all, _ := g.Groups()
	if len(all) != 2 || all[0].Name != "ops" || all[1].Name != "Web.1" || len(all[0].Machines) != 2 {
		t.Fatalf("groups in name order: %+v", all)
	}
	// A hand edit is applied.
	if n, p := f.LoadLayers(json.RawMessage(`{"exec":"allow"}`), groups); n != 1 || len(p) != 0 {
		t.Fatalf("a hand edit: %d, %v", n, p)
	}
	if d, _ := f.Defaults(); len(d) != 1 || d[CatExec] != "allow" {
		t.Fatalf("after a hand edit: %v", d)
	}
	// Unset back to the master's copy; the last one leaves the key out.
	if ok, err := f.UnsetDefault(CatExec); !ok || err != nil || defaults != nil {
		t.Fatalf("unset: %v %v, saved %s", ok, err, defaults)
	}
	if ok, _ := f.UnsetDefault(CatExec); ok {
		t.Fatal("unset twice")
	}
}

// What a group's operations take and refuse.
func TestFleet_GroupOperations(t *testing.T) {
	f := NewFleet(testCategorize)
	saves := 0
	f.SetPersistLayers(func(snapshot func() (json.RawMessage, json.RawMessage)) { snapshot(); saves++ })
	if _, err := f.CreateGroup("Web"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"web", "WEB", "all", "ALL", "m0123456789ABCDEF", "", "-x", "a b", "é", strings.Repeat("a", 64)} {
		if _, err := f.CreateGroup(bad); err == nil {
			t.Errorf("made a group %q", bad)
		}
	}
	if _, err := f.AddMembers("web", []string{"m1", "bad id"}); err == nil {
		t.Error("took a member that is not an id")
	}
	if added, err := f.AddMembers("web", []string{"m2", "m1", "m2"}); err != nil || len(added) != 2 {
		t.Fatalf("add: %v %v", added, err)
	}
	if added, _ := f.AddMembers("web", []string{"m1"}); len(added) != 0 {
		t.Error("added a member twice")
	}
	if err := f.SetGroupRule("web", " x_post ", "Block"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetGroupRule("web", "x_post", "sometimes"); err == nil {
		t.Error("took a bad mode")
	}
	if err := f.SetGroupRule("nope", "x_post", "deny"); err == nil {
		t.Error("a rule for a group that does not exist")
	}
	g, _ := f.Group("WEB")
	if g.Name != "Web" || g.ToolRules["x_post"] != "deny" || strings.Join(g.Machines, ",") != "m1,m2" {
		t.Fatalf("group %+v", g)
	}
	if left, err := f.LeaveGroups("m1"); err != nil || len(left) != 1 {
		t.Fatalf("leave: %v %v", left, err)
	}
	if removed, _ := f.RemoveMembers("web", []string{"m2", "m9"}); len(removed) != 1 {
		t.Fatalf("remove: %v", removed)
	}
	if ok, _ := f.RemoveGroupRule("web", "x_post"); !ok {
		t.Fatal("rule not removed")
	}
	if ok, _ := f.RemoveGroupRule("web", "x_post"); ok {
		t.Fatal("rule removed twice")
	}
	before := saves
	if _, err := f.DeleteGroup("WEB"); err != nil || saves != before+1 {
		t.Fatalf("delete: %v (%d saves)", err, saves-before)
	}
	if _, err := f.DeleteGroup("web"); err == nil {
		t.Fatal("deleted twice")
	}
	// Bounds.
	for i := range maxGroups {
		if _, err := f.CreateGroup(fmt.Sprintf("g%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.CreateGroup("one-more"); err == nil {
		t.Error("over the bound of groups")
	}
	ids := make([]string, maxGroupMembers+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%d", i)
	}
	if _, err := f.AddMembers("g0", ids); err == nil {
		t.Error("over the bound of members")
	}
	if g, _ := f.Group("g0"); len(g.Machines) != 0 {
		t.Error("a refused add changed the group")
	}
	for i := range maxGroupRules {
		if err := f.SetGroupRule("g1", fmt.Sprintf("t%d", i), "deny"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.SetGroupRule("g1", "one_more", "deny"); err == nil {
		t.Error("over the bound of rules")
	}
	if err := f.SetGroupRule("g1", "t0", "ask"); err != nil {
		t.Errorf("a rule it has, at the bound: %v", err)
	}
	// Unknown categories and modes for the defaults.
	if err := f.SetDefault("nope", "deny"); err == nil {
		t.Error("a default for no category")
	}
	if err := f.SetDefault(CatExec, "sometimes"); err == nil {
		t.Error("a default with a bad mode")
	}
}
