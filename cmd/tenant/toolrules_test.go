package main

import (
	"strings"
	"testing"
)

func TestToolRules_MatchingAndPrecedence(t *testing.T) {
	r := newToolRules()
	must := func(p, m string) {
		t.Helper()
		if err := r.Set(p, m); err != nil {
			t.Fatalf("Set(%q): %v", p, err)
		}
	}
	must("write_note", "allow")                   // bare name: any connector, or the local tool
	must("mcp:127.0.0.1:9000:write_note", "deny") // exact id beats the bare name
	must("mcp:127.0.0.1:9001:*", "allow")         // glob: that whole server
	must("delete_*", "deny")                      // glob on a bare name: any connector
	must("*:search_*", "allow")                   // glob with a connector prefix
	must("os_exec", "ask")                        // a local tool, exact

	cases := map[string]string{
		"mcp:127.0.0.1:9000:write_note": "deny",  // exact wins over the bare-name allow
		"mcp:127.0.0.1:9002:write_note": "allow", // bare name matches on another connector
		"write_note":                    "allow", // …and the local tool of that name
		"mcp:127.0.0.1:9001:anything":   "allow", // server glob
		"mcp:127.0.0.1:9001:delete_all": "allow", // two globs: the server glob has more literal characters than "delete_*", so it wins — deny deletes there with mcp:127.0.0.1:9001:delete_*
		"mcp:host:delete_user":          "deny",
		"mcp:host:search_users":         "allow",
		"os_exec":                       "ask",
		"mcp:host:unknown":              "",
	}
	for action, want := range cases {
		got, _, ok := r.Lookup(action)
		if !ok {
			got = ""
		}
		if got != want {
			t.Errorf("Lookup(%q) = %q, want %q", action, got, want)
		}
	}
	if r.Mode("mcp:host:unknown") != "" || r.Mode("os_exec") != "ask" {
		t.Error("Mode should mirror Lookup")
	}
}

// Two globs matching the same action: the one with more literal characters wins.
func TestToolRules_GlobSpecificity(t *testing.T) {
	r := newToolRules()
	_ = r.Set("mcp:127.0.0.1:9001:*", "allow")
	_ = r.Set("mcp:127.0.0.1:9001:delete_*", "deny")
	if m, pat, _ := r.Lookup("mcp:127.0.0.1:9001:delete_all"); m != "deny" || pat != "mcp:127.0.0.1:9001:delete_*" {
		t.Errorf("more specific glob should win: %s via %s", m, pat)
	}
	if m, _, _ := r.Lookup("mcp:127.0.0.1:9001:list"); m != "allow" {
		t.Errorf("server glob should still cover the rest: %s", m)
	}
}

func TestGlobMatch(t *testing.T) {
	yes := [][2]string{{"delete_*", "delete_user"}, {"*:search_*", "mcp:h:1:search_x"}, {"a?c", "abc"}, {"*", "anything:at:all"}, {"mcp:127.0.0.1:9000/b:*", "mcp:127.0.0.1:9000/b:x"}}
	no := [][2]string{{"delete_*", "undelete_user"}, {"a?c", "ac"}, {"x*y", "xay z"}, {"exact", "exact:more"}}
	for _, c := range yes {
		if !globMatch(c[0], c[1]) {
			t.Errorf("globMatch(%q, %q) should match", c[0], c[1])
		}
	}
	for _, c := range no {
		if globMatch(c[0], c[1]) {
			t.Errorf("globMatch(%q, %q) should not match", c[0], c[1])
		}
	}
}

func TestToolRules_ValidationPersistLoadAndOrder(t *testing.T) {
	r := newToolRules()
	var persisted []map[string]string
	r.persist = func(m map[string]string) { persisted = append(persisted, m) }

	for _, bad := range []string{"", "two words", "*", "**", strings.Repeat("x", 201)} {
		if err := r.Set(bad, "allow"); err == nil {
			t.Errorf("Set(%q) should be rejected", bad)
		}
	}
	if err := r.Set("os_exec", "maybe"); err == nil {
		t.Error("bad mode should be rejected")
	}
	if len(persisted) != 0 {
		t.Fatal("rejected rules must not persist")
	}
	_ = r.Set(" write_note ", "ALLOW") // trimmed + normalized
	_ = r.Set("delete_*", "deny")
	_ = r.Set("mcp:h:1:x", "ask")
	if len(persisted) != 3 || persisted[2]["write_note"] != "allow" || persisted[2]["mcp:h:1:x"] != "ask" {
		t.Fatalf("persist snapshots = %v", persisted)
	}
	if !r.Remove("delete_*") || r.Remove("delete_*") {
		t.Error("Remove should report presence")
	}
	rules := r.Rules()
	if len(rules) != 2 || rules[0].Pattern != "mcp:h:1:x" || rules[0].Kind != "exact" || rules[1].Pattern != "write_note" || rules[1].Kind != "name" {
		t.Errorf("Rules() order = %+v (exact, then names, then globs)", rules)
	}

	// Load replaces without persisting and skips junk.
	before := len(persisted)
	n, skipped := r.Load(map[string]string{"a_*": "deny", "bad one": "allow", "b": "nope", "c": "prompt"})
	if n != 2 || skipped != 2 || len(persisted) != before {
		t.Errorf("Load = %d/%d persisted=%d", n, skipped, len(persisted)-before)
	}
	snap := r.Snapshot()
	if snap["c"] != "ask" || snap["a_*"] != "deny" || len(snap) != 2 {
		t.Errorf("after Load: %v", snap)
	}
	if !rulesEqual(snap, map[string]string{"c": "ask", "a_*": "deny"}) || rulesEqual(snap, map[string]string{"c": "ask"}) {
		t.Error("rulesEqual")
	}
}
