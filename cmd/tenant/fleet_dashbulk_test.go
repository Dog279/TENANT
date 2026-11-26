package main

// fleet_dashbulk_test.go: the Fleet page's defaults, groups and changes to
// many machines (TEN-546) on the real files, through the real dashboard over
// HTTP: fleet_dashctl_test.go's lab, with two confirmed machines and one that
// is still pending. What the page does must be what `tenant fleet` does: the
// same settings keys, the same schemes, one record per machine changed.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"tenant/internal/dashboard"
	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
)

// savedMap is one of the settings file's JSON objects as a map.
func savedMap[V any](t *testing.T, raw json.RawMessage) map[string]V {
	t.Helper()
	out := map[string]V{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the settings file holds %s: %v", raw, err)
	}
	return out
}

func TestFleetDashDefaultsGroupsAndBulk(t *testing.T) {
	d := newFleetDash(t, settings{Permissions: map[string]string{"exec": "allow", "web": "allow"}})
	a, b := d.l.machine("web1", false), d.l.machine("web2", false)
	p := d.l.enroll("web3", fleet.Reported{}) // waits to be confirmed
	state := func(id string) fleet.State {
		m, err := fleet.Open(d.l.cfg).Machine(id)
		if err != nil {
			t.Fatal(err)
		}
		return m.State
	}
	if state(a.id) != fleet.StateConfirmed || state(b.id) != fleet.StateConfirmed || state(p.id) != fleet.StatePending {
		t.Fatalf("the lab: %s %s %s", state(a.id), state(b.id), state(p.id))
	}
	// send posts a form from the signed-in browser without its confirm step.
	send := func(path string, kv ...string) fleetAnswer {
		t.Helper()
		form := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			form.Add(kv[i], kv[i+1])
		}
		return d.send(http.MethodPost, path, form, d.browser)
	}
	settingsRecords := func() []string { return recordsOf(d.ev, catalog.FleetSettingsChanged) }
	machineRecords := func() []string { return recordsOf(d.ev, catalog.FleetMachineChanged) }

	// --- refusals first: never the API key, on any of the new routes ---
	key := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+fleetDashKey)
		r.Header.Set("Origin", d.base)
	}
	for path, form := range map[string]url.Values{
		"/fleet/defaults/permission":    {"category": {"web"}, "mode": {"deny"}},
		"/fleet/defaults/rules/add":     {"pattern": {"x_post"}, "mode": {"deny"}},
		"/fleet/defaults/rules/remove":  {"pattern": {"x_post"}},
		"/fleet/groups/create":          {"name": {"web"}},
		"/fleet/groups/delete":          {"group": {"web"}},
		"/fleet/groups/members/add":     {"group": {"web"}, "machine": {a.id}},
		"/fleet/groups/members/remove":  {"group": {"web"}, "machine": {a.id}},
		"/fleet/groups/rules/add":       {"group": {"web"}, "pattern": {"x_post"}, "mode": {"allow"}},
		"/fleet/groups/rules/remove":    {"group": {"web"}, "pattern": {"x_post"}},
		"/fleet/groups/apply":           {"group": {"web"}},
		"/fleet/bulk":                   {"action": {"permission"}, "category": {"web"}, "cat_mode": {"allow"}, "machine": {a.id}},
		"/fleet/rollout/stop":           {},
		"/fleet/bulk?the=update":        {"action": {"update"}, "machine": {a.id}, "at_once": {"1"}},
		"/fleet/bulk?the=copy":          {"action": {"copy"}, "machine": {a.id}},
		"/fleet/defaults/permission?u=": {"category": {"exec"}, "mode": {"allow"}},
	} {
		form.Set("confirm", "yes")
		if ans := d.send(http.MethodPost, path, form, key); ans.code != http.StatusForbidden || !strings.Contains(ans.body, "signed-in browser") {
			t.Errorf("the API key on %s: %d %q", path, ans.code, ans.body)
		}
	}
	if s := d.l.saved(); len(s.FleetPermissions) != 0 || len(s.FleetGroups) != 0 || len(s.FleetRules) != 0 {
		t.Fatalf("the API key changed the fleet's layers: %s %s %s", s.FleetPermissions, s.FleetGroups, s.FleetRules)
	}
	before := d.l.schemeOf(a.id)

	// --- the defaults for new machines ---
	if ans := send("/fleet/defaults/permission", "category", "exec", "mode", "allow"); ans.code != http.StatusPreconditionRequired {
		t.Errorf("a default of Allow without its second step: %d", ans.code)
	}
	if ans := d.post("/fleet/defaults/permission", "category", "web", "mode", "deny"); ans.errMsg != "" || !strings.Contains(ans.msg, "No machine that is confirmed already changes") {
		t.Errorf("a default of Deny: %d msg %q err %q", ans.code, ans.msg, ans.errMsg)
	}
	if got := savedMap[string](t, d.l.saved().FleetPermissions); len(got) != 1 || got["web"] != "deny" {
		t.Errorf("fleet_permissions after the page set web: %v", got)
	}
	if !hasRecord(settingsRecords(), "operator@dashboard", "what=defaults for new machines", "change=default set", "web deny") {
		t.Errorf("no Security record of the default:\n%s", strings.Join(settingsRecords(), "\n"))
	}
	if ans := d.post("/fleet/defaults/permission", "category", "root", "mode", "deny"); !strings.Contains(ans.errMsg, "doesn't exist") {
		t.Errorf("a default for a category that is not one: msg %q err %q", ans.msg, ans.errMsg)
	}
	if ans := d.post("/fleet/defaults/rules/add", "pattern", "x_post", "mode", "deny"); ans.errMsg != "" {
		t.Errorf("a fleet rule: err %q", ans.errMsg)
	}
	if got := savedMap[string](t, d.l.saved().FleetRules); got["x_post"] != "deny" {
		t.Errorf("fleet_rules after the page added one: %v", got)
	}
	if !hasRecord(settingsRecords(), "operator@dashboard", "change=rule set", "x_post deny") {
		t.Errorf("no Security record of the fleet rule:\n%s", strings.Join(settingsRecords(), "\n"))
	}
	// What the page reads back: every category, what is set, and what this
	// Tenant's own would give (exec is allowed here).
	view := newDashFleet(d.l.c, d.l.stg, &d.l.stgMu).View()
	def := func(cat string) dashboard.FleetDefault {
		i := slices.IndexFunc(view.Defaults, func(x dashboard.FleetDefault) bool { return x.Category == cat })
		if i < 0 {
			t.Fatalf("the view has no default for %s: %+v", cat, view.Defaults)
		}
		return view.Defaults[i]
	}
	if w, e := def("web"), def("exec"); w.Mode != "deny" || w.Inherited != "allow" || e.Mode != "" || e.Inherited != "allow" {
		t.Errorf("the view's defaults: web %+v, exec %+v", w, e)
	}
	// Unset gives it back to this Tenant's own; that may loosen, so it asks.
	if ans := send("/fleet/defaults/permission", "category", "web", "mode", "unset"); ans.code != http.StatusPreconditionRequired {
		t.Errorf("unsetting a default without its second step: %d", ans.code)
	}
	if ans := d.post("/fleet/defaults/permission", "category", "web", "mode", "unset"); ans.errMsg != "" {
		t.Errorf("unsetting a default: err %q", ans.errMsg)
	}
	if got := savedMap[string](t, d.l.saved().FleetPermissions); len(got) != 0 {
		t.Errorf("fleet_permissions after unset: %v", got)
	}
	// None of that moved a machine.
	if after := d.l.schemeOf(a.id); !maps2Equal(after.Permissions, before.Permissions) || !maps2Equal(after.ToolRules, before.ToolRules) {
		t.Fatalf("a change to the defaults moved a machine: %v -> %v", before, after)
	}

	// --- groups ---
	for name, want := range map[string]string{"all": "group's name", "m0123456789abcdef": "group's name", "has space": "group's name", "": "group's name"} {
		if ans := d.post("/fleet/groups/create", "name", name); !strings.Contains(ans.errMsg, want) {
			t.Errorf("a group named %q: msg %q err %q", name, ans.msg, ans.errMsg)
		}
	}
	if ans := d.post("/fleet/groups/create", "name", "web"); ans.errMsg != "" {
		t.Fatalf("making a group: err %q", ans.errMsg)
	}
	if ans := d.post("/fleet/groups/create", "name", "WEB"); !strings.Contains(ans.errMsg, "already") {
		t.Errorf("a second group of the same name in another case: msg %q err %q", ans.msg, ans.errMsg)
	}
	if ans := d.post("/fleet/groups/members/add", "group", "web", "machine", a.id, "machine", b.id, "machine", p.id); ans.errMsg != "" || !strings.Contains(ans.msg, "No machine's settings changed") {
		t.Fatalf("adding machines to a group: msg %q err %q", ans.msg, ans.errMsg)
	}
	if ans := d.post("/fleet/groups/members/add", "group", "web", "machine", "m9999999999999999"); !strings.Contains(ans.errMsg, "isn't part of this fleet") {
		t.Errorf("adding a machine the fleet does not have: msg %q err %q", ans.msg, ans.errMsg)
	}
	if ans := d.post("/fleet/groups/members/add", "group", "web", "machine", "web1"); ans.errMsg == "" { // its name is not its id
		t.Errorf("adding a machine by its name was taken: %q", ans.msg)
	}
	// An allow rule in a group takes no second step: it moves no machine.
	if ans := send("/fleet/groups/rules/add", "group", "web", "pattern", "os_exec(git status *)", "mode", "allow"); ans.errMsg != "" || !strings.Contains(ans.msg, "No machine's settings changed") {
		t.Fatalf("a group rule: %d msg %q err %q", ans.code, ans.msg, ans.errMsg)
	}
	type groupEntry struct {
		Machines  []string          `json:"machines"`
		ToolRules map[string]string `json:"tool_rules"`
	}
	if g := savedMap[groupEntry](t, d.l.saved().FleetGroups)["web"]; len(g.Machines) != 3 || g.ToolRules["os_exec(git status *)"] != "allow" {
		t.Errorf("fleet_groups after the page's changes: %+v", g)
	}
	if after := d.l.schemeOf(a.id); !maps2Equal(after.ToolRules, before.ToolRules) {
		t.Fatalf("a change to a group moved a member: %v -> %v", before.ToolRules, after.ToolRules)
	}
	if !hasRecord(settingsRecords(), "operator@dashboard", "what=group web", "change=machines added") || !hasRecord(settingsRecords(), "what=group web", "change=rule set") {
		t.Errorf("no Security records of the group's changes:\n%s", strings.Join(settingsRecords(), "\n"))
	}
	// The group's page, the filtered list, and what the view says of it.
	if page := d.page("/fleet/group/web"); !strings.Contains(page, "web1") || !strings.Contains(page, "web3") || !strings.Contains(page, "os_exec(git status *)") {
		t.Errorf("the group's page:\n%s", page)
	}
	other := d.l.machine("db1", false)
	if page := d.page("/fleet?group=web"); !strings.Contains(page, a.id) || strings.Contains(page, other.id) {
		t.Errorf("the list under the group's filter shows a machine outside it, or misses a member:\n%s", page)
	}
	view = newDashFleet(d.l.c, d.l.stg, &d.l.stgMu).View()
	if len(view.Groups) != 1 || view.Groups[0].Name != "web" || len(view.Groups[0].Members) != 3 || view.Groups[0].Differ != 2 {
		t.Errorf("the view's groups (two confirmed members lack the laid rules): %+v", view.Groups)
	}
	if i := slices.IndexFunc(view.Machines, func(m dashboard.FleetMachine) bool { return m.ID == a.id }); i < 0 || !slices.Equal(view.Machines[i].Groups, []string{"web"}) {
		t.Errorf("the view does not say web1 is in web")
	}

	// --- apply: the step names the machines, then one record per machine ---
	changed := func() int { return countRecords(machineRecords(), "operator@dashboard", "change=scheme changed") }
	step := send("/fleet/groups/apply", "group", "web")
	if step.code != http.StatusOK || !strings.Contains(step.body, "Yes, apply to 2 machines") || !strings.Contains(step.body, "is waiting to be confirmed") {
		t.Fatalf("the step of applying a group: %d\n%s", step.code, step.body)
	}
	if changed() != 0 || d.l.schemeOf(a.id).ToolRules["os_exec(git status *)"] != "" {
		t.Fatal("the step itself changed a machine")
	}
	done := d.post("/fleet/groups/apply", "group", "web")
	if done.code != http.StatusOK || !strings.Contains(done.body, "Done on 2 of 3.") || !strings.Contains(done.body, "web3") {
		t.Fatalf("applying a group: %d\n%s", done.code, done.body)
	}
	for _, id := range []string{a.id, b.id} {
		if rules := d.l.schemeOf(id).ToolRules; rules["os_exec(git status *)"] != "allow" || rules["x_post"] != "deny" {
			t.Errorf("after apply, %s has %v: want the fleet's rule and the group's", id, rules)
		}
	}
	if _, has := d.l.saved().FleetSchemes[p.id]; has || state(p.id) != fleet.StatePending {
		t.Error("applying a group gave the pending machine a scheme, or confirmed it")
	}
	if changed() != 2 || !hasRecord(machineRecords(), "operator@dashboard", "name=web1", "change=scheme changed", "group(s) web") {
		t.Errorf("want one record per machine changed, naming the group:\n%s", strings.Join(machineRecords(), "\n"))
	}
	if d.l.schemeOf(other.id).ToolRules["os_exec(git status *)"] != "" {
		t.Error("applying a group moved a machine outside it")
	}

	// --- one change to many machines ---
	// Tightening goes through at once, and says which machine it could not change.
	tight := send("/fleet/bulk", "action", "permission", "category", "web", "cat_mode", "deny", "machine", a.id, "machine", b.id, "machine", p.id, "machine", "m9999999999999999")
	if tight.code != http.StatusOK || !strings.Contains(tight.body, "Done on 2 of 4.") || !strings.Contains(tight.body, pageText("isn't part of this fleet")) || !strings.Contains(tight.body, "it is pending") {
		t.Fatalf("a tightening on many machines: %d\n%s", tight.code, tight.body)
	}
	if d.l.schemeOf(a.id).Permissions["web"] != "deny" || d.l.schemeOf(b.id).Permissions["web"] != "deny" || d.l.schemeOf(other.id).Permissions["web"] == "deny" {
		t.Error("the tightening did not land on the two machines chosen, or landed elsewhere")
	}
	// Loosening shows the step first and changes nothing until it is taken.
	loose := send("/fleet/bulk", "action", "rule", "pattern", "web_fetch", "rule_mode", "allow", "machine", a.id, "machine", other.id)
	if loose.code != http.StatusOK || !strings.Contains(loose.body, "Yes, add it to 2 machines") || !strings.Contains(loose.body, `name="machine" value="`+other.id+`"`) {
		t.Fatalf("the step of a loosening on many machines: %d\n%s", loose.code, loose.body)
	}
	if d.l.schemeOf(a.id).ToolRules["web_fetch"] != "" {
		t.Fatal("the step of a loosening changed a machine")
	}
	rules := countRecords(machineRecords(), "operator@dashboard", "change=rule changed")
	if ans := d.post("/fleet/bulk", "action", "rule", "pattern", "web_fetch", "rule_mode", "allow", "machine", a.id, "machine", other.id); !strings.Contains(ans.body, "Done on 2 of 2.") {
		t.Fatalf("the loosening, confirmed: %d\n%s", ans.code, ans.body)
	}
	if d.l.schemeOf(a.id).ToolRules["web_fetch"] != "allow" || d.l.schemeOf(other.id).ToolRules["web_fetch"] != "allow" || d.l.schemeOf(b.id).ToolRules["web_fetch"] != "" {
		t.Error("the rule did not land on the two machines chosen, or landed elsewhere")
	}
	if got := countRecords(machineRecords(), "operator@dashboard", "change=rule changed"); got != rules+2 {
		t.Errorf("%d records of the rule on two machines, want one each", got-rules)
	}
	// Everyone shown under a filter is worked out here, not taken from the form.
	if ans := d.post("/fleet/bulk", "action", "unrule", "pattern", "web_fetch", "all", "shown", "group", "web", "machine", other.id); !strings.Contains(ans.body, "Done on 1 of 3.") {
		t.Fatalf("removing a rule from everyone shown in the group: %d\n%s", ans.code, ans.body)
	}
	if d.l.schemeOf(a.id).ToolRules["web_fetch"] != "" || d.l.schemeOf(other.id).ToolRules["web_fetch"] != "allow" {
		t.Error("everyone shown was taken from the form's own list, not from the group")
	}
	// Copy again on many: a fresh copy with the layers, the machine's own gone.
	if ans := d.post("/fleet/bulk", "action", "copy", "machine", a.id); !strings.Contains(ans.body, "Done on 1 of 1.") {
		t.Fatalf("copy again: %d\n%s", ans.code, ans.body)
	}
	if s := d.l.schemeOf(a.id); s.Permissions["web"] != "allow" || s.Permissions["exec"] != "allow" || s.ToolRules["os_exec(git status *)"] != "allow" || s.ToolRules["x_post"] != "deny" {
		t.Errorf("a fresh copy of a member of web: %+v", s)
	}

	// --- a group goes; its members keep what it laid ---
	if ans := send("/fleet/groups/delete", "group", "web"); ans.code != http.StatusPreconditionRequired {
		t.Errorf("deleting a group without its second step: %d", ans.code)
	}
	if ans := d.post("/fleet/groups/delete", "group", "web"); ans.errMsg != "" {
		t.Errorf("deleting a group: err %q", ans.errMsg)
	}
	if len(d.l.saved().FleetGroups) != 0 || d.l.schemeOf(b.id).ToolRules["os_exec(git status *)"] != "allow" {
		t.Errorf("after the group is deleted: fleet_groups %s, web2's rules %v", d.l.saved().FleetGroups, d.l.schemeOf(b.id).ToolRules)
	}
}

// maps2Equal reports whether two string maps hold the same entries (nil and
// empty are the same).
func maps2Equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
