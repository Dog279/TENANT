package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/approval"
	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// recordsOf lists the captured records of def as "actor@surface k=v …".
func recordsOf(ev *channelEvents, def *event.Def) []string {
	var out []string
	for _, e := range ev.matching(def, "") {
		l := e.Actor + "@" + e.Surface
		for _, f := range e.Data {
			l += " " + f.Key + "=" + f.Text()
		}
		out = append(out, l)
	}
	return out
}

func countRecords(records []string, parts ...string) int {
	n := 0
	for _, r := range records {
		if hasRecord([]string{r}, parts...) {
			n++
		}
	}
	return n
}

// groupLab is a master with five machines: alpha, beta and gamma confirmed,
// delta pending, eps revoked with its scheme kept.
type groupLab struct {
	*fleetLab
	ev                             *channelEvents
	alpha, beta, gamma, delta, eps labChild
}

func newGroupLab(t *testing.T) *groupLab {
	t.Helper()
	g := &groupLab{ev: captureChannelEvents(t), fleetLab: newFleetLab(t)}
	g.settings(settings{Permissions: map[string]string{"exec": "allow", "web": "allow"}})
	g.master()
	g.alpha, g.beta, g.gamma = g.enroll("alpha", fleet.Reported{}), g.enroll("beta", fleet.Reported{}), g.enroll("gamma", fleet.Reported{})
	g.confirm("alpha", g.alpha)
	g.confirm("beta", g.beta)
	g.confirm("gamma", g.gamma)
	g.delta = g.enroll("delta", fleet.Reported{})
	g.eps = g.enroll("eps", fleet.Reported{})
	g.confirm("eps", g.eps)
	g.must("revoke", "eps", "--keep-scheme")
	return g
}

// spoil makes a machine's entry of fleet_schemes one that can't be read.
func (g *groupLab) spoil(id string) {
	g.t.Helper()
	stg := g.saved()
	stg.FleetSchemes[id] = json.RawMessage(`{"permissions":{"exec":"sometimes"}}`)
	g.settings(*stg)
}

// line is one machine's line of a bulk change's answer, up to mark.
func line(name, mark string) string { return fmt.Sprintf("%-20s %s", name, mark) }

// Refusals first: what a group is not, and what is never changed by a
// refused command.
func TestFleetGroupRefusals(t *testing.T) {
	g := newGroupLab(t)
	before := g.saved()
	for _, bad := range [][]string{
		{"group", "create", "all"}, {"group", "create", "ALL"}, {"group", "create", "m0123456789abcdef"}, {"group", "create", "-web"},
		{"group", "create", "two words"}, {"group", "create", strings.Repeat("a", 64)}, {"group", "show", "nope"},
		{"group", "add", "nope", "alpha"}, {"group", "rules", "nope", "add", "x_post", "deny"}, {"group", "apply", "nope"}, {"group", "bogus"},
		{"permissions", "--group", "nope", "set", "web", "deny"}, {"permissions", "--group", "x", "--all", "set", "web", "deny"},
		{"permissions", "--all", "--defaults"}, {"permissions", "--all"}, {"permissions", "--all", "--from", "elsewhere"},
		{"permissions", "--defaults", "set", "nope", "allow"}, {"permissions", "--defaults", "set", "exec", "sometimes"}, {"permissions", "--defaults", "unset", "exec"},
		{"update", "--group", "nope"},
	} {
		if _, err := g.run(bad...); err == nil {
			t.Errorf("tenant fleet %s was accepted", strings.Join(bad, " "))
		}
	}
	g.must("group", "create", "web")
	for _, bad := range [][]string{
		{"group", "create", "WEB"}, {"group", "add", "web", "nobody"}, {"group", "add", "web", "alpha", "nobody"},
		{"group", "rules", "web", "add", "x_post", "maybe"}, {"group", "rules", "web", "add", "two words", "deny"}, {"group", "rules", "web", "rm", "x_post"},
	} {
		if _, err := g.run(bad...); err == nil {
			t.Errorf("tenant fleet %s was accepted", strings.Join(bad, " "))
		}
	}
	after := g.saved()
	if tight(after.FleetGroups) != `{"web":{}}` || after.FleetPermissions != nil || len(after.FleetSchemes) != len(before.FleetSchemes) {
		t.Errorf("refused commands changed the file: groups %s, defaults %s", after.FleetGroups, after.FleetPermissions)
	}
	for id, raw := range before.FleetSchemes {
		if string(after.FleetSchemes[id]) != string(raw) {
			t.Errorf("a refused command changed %s's scheme", id)
		}
	}
}

// A group is made, filled and given rules, each change with one record of
// its own; no change to a group moves a machine; applying it does, one record
// per machine changed, skipping with reasons those that do not qualify.
func TestFleetGroupLifecycle(t *testing.T) {
	g := newGroupLab(t)
	alphaBefore := g.schemeOf(g.alpha.id)

	out := g.must("group", "create", "Web")
	if !strings.Contains(out, "group Web made") {
		t.Fatalf("create: %s", out)
	}
	out = g.must("group", "add", "web", "alpha", "beta", "delta", g.eps.id)
	if !strings.Contains(out, "No machine's scheme changed") || !strings.Contains(out, "tenant fleet group apply Web") {
		t.Fatalf("add: %s", out)
	}
	out = g.must("group", "rules", "web", "add", "os_exec(uptime *)", "allow")
	if !strings.Contains(out, "No machine's scheme changed") {
		t.Fatalf("rules add: %s", out)
	}
	g.must("group", "rules", "web", "add", "x_post", "deny")
	if s := g.schemeOf(g.alpha.id); schemeJSONOf(s) != schemeJSONOf(alphaBefore) {
		t.Fatalf("a group's change moved alpha: %v", s)
	}
	settingsRecs := recordsOf(g.ev, catalog.FleetSettingsChanged)
	for _, want := range [][]string{
		{"operator@cli", "what=group Web", "change=created"},
		{"operator@cli", "what=group Web", "change=machines added", "alpha", "beta", "delta", "eps"},
		{"operator@cli", "what=group Web", "change=rule set", "os_exec(uptime *) allow"},
		{"operator@cli", "what=group Web", "change=rule set", "x_post deny"},
	} {
		if !hasRecord(settingsRecs, want...) {
			t.Errorf("no record %v in %v", want, settingsRecs)
		}
	}
	if len(settingsRecs) != 4 {
		t.Errorf("%d records for 4 changes: %v", len(settingsRecs), settingsRecs)
	}
	if show := g.must("group", "show", "WEB"); !strings.Contains(show, "alpha") || !strings.Contains(show, "pending") || !strings.Contains(show, "os_exec(uptime *)") {
		t.Errorf("show:\n%s", show)
	}
	if list := g.must("group"); !strings.Contains(list, "Web") || !strings.Contains(list, "4 machine(s)") {
		t.Errorf("list:\n%s", list)
	}

	// Beta's own entry can't be read; delta is pending, eps revoked.
	g.spoil(g.beta.id)
	machineRecsBefore := len(recordsOf(g.ev, catalog.FleetMachineChanged))
	out, err := g.run("group", "apply", "web")
	if exitCode(err) != 1 {
		t.Fatalf("apply with members it could not change: %v\n%s", err, out)
	}
	for _, want := range []string{line("alpha", "changed"), line("beta", "NOT changed"), line("delta", "NOT changed  it is pending"), line("eps", "NOT changed  it is revoked")} {
		if !strings.Contains(out, want) {
			t.Errorf("apply's answer lacks %q:\n%s", want, out)
		}
	}
	if s := g.schemeOf(g.alpha.id); s.ToolRules["x_post"] != "deny" || s.ToolRules["os_exec(uptime *)"] != "allow" || s.Permissions["web"] != alphaBefore.Permissions["web"] {
		t.Errorf("alpha after apply: %v", s)
	}
	if raw := g.saved().FleetSchemes[g.beta.id]; tight(raw) != `{"permissions":{"exec":"sometimes"}}` {
		t.Errorf("beta's unreadable entry was written over: %s", raw)
	}
	recs := recordsOf(g.ev, catalog.FleetMachineChanged)[machineRecsBefore:]
	if len(recs) != 1 || !hasRecord(recs, "name=alpha", "change=scheme changed", "group(s) Web") {
		t.Errorf("apply's records, want one for alpha alone: %v", recs)
	}
	if n := len(recordsOf(g.ev, catalog.FleetSettingsChanged)); n != 4 {
		t.Errorf("apply filed a record for the batch (%d settings records)", n)
	}

	// Deleted: the members keep what it laid.
	g.must("group", "delete", "web")
	if s := g.schemeOf(g.alpha.id); s.ToolRules["x_post"] != "deny" {
		t.Errorf("a deleted group took its rules back: %v", s.ToolRules)
	}
	if g.saved().FleetGroups != nil {
		t.Errorf("no group left, but the key stays: %s", g.saved().FleetGroups)
	}
}

func schemeJSONOf(s approval.Scheme) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// One change made to many machines goes through the one-machine path: each
// changed machine has its own record, a failure stops nothing, and the
// command exits 1 when any machine was not changed.
func TestFleetBulkPermissions(t *testing.T) {
	g := newGroupLab(t)
	g.must("group", "create", "ops")
	g.must("group", "add", "ops", "alpha", "beta", "delta")
	g.spoil(g.beta.id)

	out, err := g.run("permissions", "--group", "ops", "set", "web", "deny")
	if exitCode(err) != 1 || !strings.Contains(out, line("alpha", "changed")) || !strings.Contains(out, line("beta", "NOT changed")) || !strings.Contains(out, "it is pending") {
		t.Fatalf("set on a group: %v\n%s", err, out)
	}
	if s := g.schemeOf(g.alpha.id); s.Permissions["web"] != "deny" {
		t.Errorf("alpha web = %s", s.Permissions["web"])
	}
	if s := g.schemeOf(g.gamma.id); s.Permissions["web"] == "deny" {
		t.Error("gamma is not in the group and changed")
	}
	if recs := recordsOf(g.ev, catalog.ModeChanged); countRecords(recs, "broker=fleet:"+g.alpha.id, "category=web", "mode=deny") != 1 {
		t.Errorf("the category change's records: %v", recs)
	}

	// --all: every confirmed machine (beta's entry can't be read).
	before := len(recordsOf(g.ev, catalog.FleetMachineChanged))
	out, err = g.run("permissions", "--all", "add", "os_exec(df *)", "allow")
	if exitCode(err) != 1 || strings.Contains(out, "delta") || strings.Contains(out, "eps") || !strings.Contains(out, line("gamma", "changed")) {
		t.Fatalf("--all add: %v\n%s", err, out)
	}
	recs := recordsOf(g.ev, catalog.FleetMachineChanged)[before:]
	if len(recs) != 2 || countRecords(recs, "change=rule changed", "os_exec(df *)") != 2 || !hasRecord(recs, "name=alpha") || !hasRecord(recs, "name=gamma") {
		t.Errorf("one record per machine changed: %v", recs)
	}
	// All of them changed: exit 0.
	if _, err := g.run("group", "remove", "ops", "beta", "delta"); err != nil {
		t.Fatal(err)
	}
	if out, err := g.run("permissions", "--group", "ops", "--from", "master"); err != nil || !strings.Contains(out, line("alpha", "changed      a fresh copy")) {
		t.Fatalf("--from master on a group whose every member qualifies: %v\n%s", err, out)
	}
}

// The defaults for new machines: set and unset from the command line, shown
// on the confirm screen as a fleet default, and moving no machine.
func TestFleetDefaultCategories(t *testing.T) {
	g := newGroupLab(t)
	alpha := g.schemeOf(g.alpha.id)
	out := g.must("permissions", "--defaults", "set", "exec", "allow")
	if !strings.Contains(out, "exec is allow on new machines") || !strings.Contains(out, "moves none") {
		t.Fatalf("set: %s", out)
	}
	g.must("permissions", "--defaults", "set", "mcp", "deny")
	if got := tight(g.saved().FleetPermissions); got != `{"exec":"allow","mcp":"deny"}` {
		t.Fatalf("fleet_permissions = %s", got)
	}
	if schemeJSONOf(g.schemeOf(g.alpha.id)) != schemeJSONOf(alpha) {
		t.Fatal("a default moved a machine")
	}
	if list := g.must("permissions", "--defaults"); !strings.Contains(list, "exec         allow") || !strings.Contains(list, "mcp          deny") {
		t.Errorf("the defaults listing:\n%s", list)
	}
	g.must("group", "create", "ops")
	g.must("group", "rules", "ops", "add", "x_post", "deny")
	delta := g.delta // pending: the lab has made the five codes a minute the enroll endpoint takes
	g.must("group", "add", "ops", "delta")
	g.answer = ""
	screen, _ := g.run("confirm", "delta")
	for _, want := range []string{"exec         allow  fleet default", "mcp          deny   fleet default", "x_post", "group ops"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the confirm screen lacks %q:\n%s", want, screen)
		}
	}
	g.confirm("delta", delta)
	if s := g.schemeOf(delta.id); s.Permissions["exec"] != "allow" || s.Permissions["mcp"] != "deny" || s.ToolRules["x_post"] != "deny" {
		t.Errorf("delta at confirm: %v", s)
	}
	if !hasRecord(recordsOf(g.ev, catalog.FleetSettingsChanged), "what=defaults for new machines", "change=default set", "exec allow") {
		t.Error("no record of the default set")
	}
	g.must("permissions", "--defaults", "unset", "exec")
	if got := tight(g.saved().FleetPermissions); got != `{"mcp":"deny"}` {
		t.Fatalf("after unset: %s", got)
	}
	// Copy again takes the defaults and the groups.
	g.must("group", "add", "ops", "alpha")
	g.must("permissions", "alpha", "--from", "master")
	if s := g.schemeOf(g.alpha.id); s.Permissions["mcp"] != "deny" || s.ToolRules["x_post"] != "deny" || s.Permissions["exec"] != "allow" {
		t.Errorf("alpha copied again: %v", s)
	}
}

// A revoke that deletes the scheme takes the machine out of every group; one
// that keeps it keeps the memberships, for a machine that adopts the id.
func TestFleetRevokeAndGroups(t *testing.T) {
	g := newGroupLab(t)
	g.must("group", "create", "a")
	g.must("group", "create", "b")
	g.must("group", "add", "a", "alpha", "gamma")
	g.must("group", "add", "b", "alpha")
	g.must("revoke", "gamma", "--keep-scheme")
	if info := g.must("group", "show", "a"); !strings.Contains(info, "gamma") {
		t.Fatalf("a keeping revoke dropped gamma:\n%s", info)
	}
	g.must("revoke", "alpha", "--delete-scheme")
	for _, name := range []string{"a", "b"} {
		if info := g.must("group", "show", name); strings.Contains(info, "alpha") || strings.Contains(info, g.alpha.id) {
			t.Errorf("group %s keeps a deleted machine:\n%s", name, info)
		}
	}
	if !hasRecord(recordsOf(g.ev, catalog.FleetMachineChanged), "name=alpha", "change=removed", "it left group(s) a, b") {
		t.Error("the removal's record does not name the groups")
	}
}

// The settings watcher takes a hand edit of either key, never its own save,
// and tells the operator of a key it can't read, which is kept as written.
func TestFleetLayersWatcher(t *testing.T) {
	f := newFleetSettings(t, `{"tools":{},"fleet_schemes":{"m1":{}}}`)
	f.write(`{"tools":{},"fleet_schemes":{"m1":{}},"fleet_permissions":{"web":"deny"},"fleet_groups":{"ops":{"machines":["m1"],"tool_rules":{"x_post":"deny"}}}}`)
	if got := f.apply(); !strings.Contains(got, "2 fleet scheme change(s)") {
		t.Fatalf("a hand edit of both keys: %q", got)
	}
	if d, _ := defaultFleet.Defaults(); d["web"] != "deny" {
		t.Fatalf("defaults %v", d)
	}
	if gs, _ := defaultFleet.GroupsOf("m1"); len(gs) != 1 || gs[0].ToolRules["x_post"] != "deny" {
		t.Fatalf("groups %v", gs)
	}
	// Its own save is no edit.
	if err := defaultFleet.SetDefault("exec", "deny"); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultFleet.CreateGroup("db"); err != nil {
		t.Fatal(err)
	}
	if got := f.apply(); got != "" {
		t.Fatalf("its own save read as an edit: %q", got)
	}
	if got := tight(f.file().FleetPermissions); got != `{"exec":"deny","web":"deny"}` {
		t.Fatalf("saved %s", got)
	}
	// Unreadable: told, refused for a fresh copy, kept as written by the next save.
	f.write(`{"tools":{},"fleet_schemes":{"m1":{}},"fleet_permissions":{"web":"deny"},"fleet_groups":{"ops":{},"OPS":{}}}`)
	if got := f.apply(); !strings.Contains(got, "fleet_groups") {
		t.Fatalf("an unreadable key: %q", got)
	}
	if _, err := defaultFleet.Fresh("m1", approval.Scheme{}); err == nil {
		t.Fatal("a fresh copy with groups that can't be read")
	}
	if err := defaultFleet.SetDefault("mcp", "deny"); err != nil {
		t.Fatal(err)
	}
	if got := tight(f.file().FleetGroups); got != `{"ops":{},"OPS":{}}` {
		t.Fatalf("the unreadable groups were written over: %s", got)
	}
}

// /fleet group in the terminal app is `tenant fleet group`, in the app's
// own words, and the defaults' categories are in /permissions fleet --defaults.
func TestFleetTUIGroupsAndDefaults(t *testing.T) {
	g := newGroupLab(t)
	g.sync() // the running process reads what the command line wrote
	ctl := newFleetTUIControl(g.c, g.stg, &g.stgMu)
	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "ops"}, "/fleet group add ops"},
		{[]string{"add", "ops", "alpha", "beta"}, "/fleet group apply ops"},
		{[]string{"rules", "ops", "add", "x_post", "deny"}, "No machine's scheme changed"},
		{[]string{"rules", "ops"}, "x_post"},
		{[]string{"show", "ops"}, "beta"},
		{nil, "ops"},
		{[]string{"apply", "ops"}, line("beta", "changed")},
		{[]string{"remove", "ops", "beta"}, "removed beta"},
		{[]string{"delete", "ops"}, "group ops deleted"},
	} {
		out, err := ctl.Group(step.args)
		if err != nil || !strings.Contains(out, step.want) {
			t.Fatalf("/fleet group %v: %v\n%s", step.args, err, out)
		}
	}
	if s, _ := defaultFleet.Scheme(g.alpha.id); s.ToolRules["x_post"] != "deny" {
		t.Errorf("alpha after /fleet group apply: %v", s.ToolRules)
	}
	if _, err := ctl.Group([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "usage: /fleet group") {
		t.Errorf("a bad verb: %v", err)
	}
	d := ctl.Defaults()
	if ok, err := d.SetPermission("exec", "allow"); !ok || err != nil {
		t.Fatalf("set: %v %v", ok, err)
	}
	if ok, _ := d.SetPermission("nope", "allow"); ok {
		t.Fatal("a default for no category")
	}
	if p := d.Permissions(); len(p) != 1 || p[0].Category != "exec" || p[0].Mode != "allow" {
		t.Fatalf("the defaults' categories: %+v", p)
	}
	if had, err := d.UnsetPermission("exec"); !had || err != nil || len(d.Permissions()) != 0 {
		t.Fatalf("unset: %v %v", had, err)
	}
	if !hasRecord(recordsOf(g.ev, catalog.FleetSettingsChanged), "operator@tui", "change=default unset") {
		t.Error("no record from the terminal app")
	}

	// Targets the dashboard will name: machines by name or id, once each, by
	// name; a group's member no machine has is a result with the reason.
	ms, err := ctl.ops.targets(fleetSelection{Refs: []string{"gamma", g.alpha.id, "alpha"}})
	if err != nil || len(ms) != 2 || ms[0].Name != "alpha" || ms[1].Name != "gamma" {
		t.Fatalf("targets: %v %v", ms, err)
	}
	if _, err := ctl.ops.targets(fleetSelection{Refs: []string{"nobody"}}); err == nil {
		t.Error("a target no machine is")
	}
	if _, err := defaultFleet.CreateGroup("ghosts"); err != nil {
		t.Fatal(err)
	}
	if _, err := defaultFleet.AddMembers("ghosts", []string{"m00000000000000ff", g.gamma.id}); err != nil {
		t.Fatal(err)
	}
	rs, err := ctl.ops.applyGroup("ghosts")
	if err != nil || len(rs) != 2 || !rs[0].OK || rs[1].OK || rs[1].Said != "no machine in this fleet has this id" {
		t.Fatalf("apply over a member no machine has: %+v %v", rs, err)
	}
	if out, _ := ctl.Group([]string{"show", "ghosts"}); !strings.Contains(out, "no machine in this fleet has this id") {
		t.Errorf("show:\n%s", out)
	}
	if removed, err := ctl.ops.removeFromGroup("ghosts", []string{"m00000000000000ff"}); err != nil || len(removed) != 1 {
		t.Errorf("remove a member no machine has, by its id: %v %v", removed, err)
	}
}

// tight is a key of the settings file as written, without the file's
// indentation.
func tight(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}
