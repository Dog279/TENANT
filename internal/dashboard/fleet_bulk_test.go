package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// The fake's side of TEN-546: the fleet's defaults, its groups, Bulk and the
// rollout, kept in its view as the real control keeps them in the settings
// file and in memory. Bulk fails, per machine, where the fleet would: a
// pending or revoked machine, one with no readable settings, a rule it does
// not have; the others change.

func (f *fakeFleet) SetDefault(category, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "default|"+category+"|"+mode)
	if f.err != nil {
		return f.err
	}
	i := slices.IndexFunc(f.view.Defaults, func(p FleetDefault) bool { return p.Category == category })
	if i < 0 {
		return PlainError("That safety check doesn't exist. Nothing changed.")
	}
	f.view.Defaults[i].Mode = mode
	return nil
}

func (f *fakeFleet) SetDefaultRule(pattern, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "defrule|"+pattern+"|"+mode)
	if f.err != nil {
		return f.err
	}
	f.view.Rules = setRule(f.view.Rules, pattern, mode)
	return nil
}

func (f *fakeFleet) RemoveDefaultRule(pattern string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "defunrule|"+pattern)
	if f.err != nil {
		return false, f.err
	}
	n := len(f.view.Rules)
	f.view.Rules = slices.DeleteFunc(f.view.Rules, func(r ToolRuleRow) bool { return r.Pattern == pattern })
	return len(f.view.Rules) < n, nil
}

func setRule(rules []ToolRuleRow, pattern, mode string) []ToolRuleRow {
	rules = slices.DeleteFunc(slices.Clone(rules), func(r ToolRuleRow) bool { return r.Pattern == pattern })
	return append(rules, ToolRuleRow{Pattern: pattern, Mode: mode})
}

// group finds a group as the control does, case-insensitively.
func (f *fakeFleet) group(op, name string, args ...string) (*FleetGroup, error) {
	f.calls = append(f.calls, strings.Join(append([]string{op, name}, args...), "|"))
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.view.Groups {
		if strings.EqualFold(f.view.Groups[i].Name, name) {
			return &f.view.Groups[i], nil
		}
	}
	return nil, PlainError(fleetNoGroup)
}

func (f *fakeFleet) CreateGroup(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g, err := f.group("group-create", name); g != nil {
		return PlainError("A group by that name exists already. Nothing changed.")
	} else if !errors.Is(err, PlainError(fleetNoGroup)) {
		return err
	}
	f.view.Groups = append(f.view.Groups, FleetGroup{Name: name})
	slices.SortFunc(f.view.Groups, func(a, b FleetGroup) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return nil
}

func (f *fakeFleet) DeleteGroup(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.group("group-delete", name); err != nil {
		return err
	}
	f.view.Groups = slices.DeleteFunc(f.view.Groups, func(g FleetGroup) bool { return strings.EqualFold(g.Name, name) })
	return nil
}

func (f *fakeFleet) AddMembers(name string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.group("group-add", name, ids...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !slices.Contains(g.Members, id) {
			g.Members = append(g.Members, id)
		}
	}
	return nil
}

func (f *fakeFleet) RemoveMembers(name string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.group("group-remove", name, ids...)
	if err != nil {
		return err
	}
	g.Members = slices.DeleteFunc(g.Members, func(id string) bool { return slices.Contains(ids, id) })
	return nil
}

func (f *fakeFleet) SetGroupRule(name, pattern, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.group("group-rule", name, pattern, mode)
	if err == nil {
		g.Rules = setRule(g.Rules, pattern, mode)
	}
	return err
}

func (f *fakeFleet) RemoveGroupRule(name, pattern string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.group("group-unrule", name, pattern)
	if err != nil {
		return false, err
	}
	n := len(g.Rules)
	g.Rules = slices.DeleteFunc(g.Rules, func(r ToolRuleRow) bool { return r.Pattern == pattern })
	return len(g.Rules) < n, nil
}

func (f *fakeFleet) Bulk(op FleetBulkOp, ids []string) []FleetBulkResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join([]string{"bulk", op.Action, strings.Join(ids, ","), op.Category, op.Pattern, op.Mode}, "|"))
	var out []FleetBulkResult
	for _, id := range ids {
		res := FleetBulkResult{ID: id}
		i := slices.IndexFunc(f.view.Machines, func(m FleetMachine) bool { return m.ID == id })
		if i < 0 {
			res.Said = "isn't part of this fleet"
			out = append(out, res)
			continue
		}
		m := &f.view.Machines[i]
		res.Name = m.Name
		switch {
		case m.State == "pending":
			res.Said = "is waiting to be confirmed"
		case m.State == "revoked":
			res.Said = "is revoked"
		case !m.HasScheme && op.Action != FleetBulkCopy:
			res.Said = "has no approval settings that can be read"
		case op.Action == FleetBulkUnrule && !slices.ContainsFunc(m.Rules, func(r ToolRuleRow) bool { return r.Pattern == op.Pattern }):
			res.Said = "has no such rule"
		default:
			res.Done = true
			switch op.Action {
			case FleetBulkPermission:
				for j := range m.Perms {
					if m.Perms[j].Category == op.Category {
						m.Perms[j].Mode = op.Mode
					}
				}
			case FleetBulkRule:
				m.Rules = setRule(m.Rules, op.Pattern, op.Mode)
			case FleetBulkUnrule:
				m.Rules = slices.DeleteFunc(m.Rules, func(r ToolRuleRow) bool { return r.Pattern == op.Pattern })
			case FleetBulkApply:
				m.Drift = nil
			case FleetBulkCopy:
				m.Drift, m.HasScheme, m.Perms, m.Rules = nil, true, fleetPerms("ask"), nil
			}
		}
		out = append(out, res)
	}
	return out
}

func (f *fakeFleet) StartRollout(ids []string, version, arch string, allowOlder bool, limit int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join([]string{"rollout", strings.Join(ids, ","), version, arch, fmt.Sprint(allowOlder), fmt.Sprint(limit)}, "|"))
	switch {
	case f.err != nil:
		return f.err
	case f.view.Rollout != nil && f.view.Rollout.Running:
		return PlainError("Nothing was sent: a rollout is running already: stop it, or wait for it to end.")
	}
	ro := &FleetRollout{Version: version, Limit: limit, Started: "2026-11-24 10:00", Running: true}
	for _, id := range ids {
		ro.Machines = append(ro.Machines, FleetRolloutMachine{ID: id, Name: f.nameOf(id), Status: "waiting"})
	}
	f.view.Rollout = ro
	return nil
}

func (f *fakeFleet) nameOf(id string) string {
	for _, m := range f.view.Machines {
		if m.ID == id {
			return m.Name
		}
	}
	return id
}

func (f *fakeFleet) StopRollout() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "rollout-stop")
	if f.view.Rollout == nil || !f.view.Rollout.Running || f.view.Rollout.Stopped != "" {
		return false
	}
	f.view.Rollout.Stopped = "stopped by the operator"
	return true
}

// fleetDefaults is the fleet's defaults as the fake keeps them: Run commands
// set to Deny, Act on websites set to Allow, the rest unset (from this
// Tenant, which gives Ask).
func fleetDefaults() []FleetDefault {
	var out []FleetDefault
	for _, p := range fleetPerms("") {
		d := FleetDefault{PermissionRow: p, Inherited: "ask"}
		switch p.Category {
		case "exec":
			d.Mode = "deny"
		case "web":
			d.Mode = "allow"
		}
		out = append(out, d)
	}
	return out
}

// --- refusals first ---

// fleetChanges are the TEN-546 routes that change something, each with a
// form that would go through from the browser on this computer.
var fleetChanges = []struct {
	path string
	form url.Values
}{
	{"/fleet/defaults/permission", url.Values{"category": {"web"}, "mode": {"deny"}}},
	{"/fleet/defaults/rules/add", url.Values{"pattern": {"write_note"}, "mode": {"deny"}}},
	{"/fleet/defaults/rules/remove", url.Values{"pattern": {"os_run(systemctl status *)"}}},
	{"/fleet/groups/create", url.Values{"name": {"edge"}}},
	{"/fleet/groups/delete", url.Values{"group": {"web"}}},
	{"/fleet/groups/members/add", url.Values{"group": {"web"}, "machine": {"m0000000000000002"}}},
	{"/fleet/groups/members/remove", url.Values{"group": {"web"}, "machine": {fleetConfirmed}}},
	{"/fleet/groups/rules/add", url.Values{"group": {"web"}, "pattern": {"x"}, "mode": {"allow"}}},
	{"/fleet/groups/rules/remove", url.Values{"group": {"web"}, "pattern": {"os_run(nginx -t)"}}},
	{"/fleet/groups/apply", url.Values{"group": {"web"}}},
	{"/fleet/bulk", url.Values{"action": {"permission"}, "category": {"web"}, "cat_mode": {"allow"}, "machine": {fleetConfirmed}}},
	{"/fleet/rollout/stop", url.Values{}},
}

// Every change here takes the page's own form in a signed-in browser: not
// the API key (with or without the page's Origin), not signed out, not with
// no Origin or another one. The API key gets a plain 403 and a Security
// record; secure() refuses the others first. None reaches the control.
func TestFleetBulkRoutesTakeASignedInBrowserOnly(t *testing.T) {
	c := installCapture(t)
	for _, tc := range fleetChanges {
		f := fleetFake(6)
		f.view.Rollout = &FleetRollout{Running: true}
		h := fleetServer(f).served()
		form := withField(tc.form, "confirm", "yes")
		for _, set := range []struct {
			name   string
			set    func(*http.Request)
			code   int
			reason string
		}{
			{"the API key", func(r *http.Request) { r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:9770"; bearer("s3cret")(r) }, http.StatusForbidden, ""},
			{"the API key with the page's Origin", func(r *http.Request) { here(r); r.Header.Del("Cookie"); bearer("s3cret")(r) }, http.StatusForbidden, ""},
			{"the API key beside a stale sign-in", func(r *http.Request) {
				here(r)
				r.Header.Set("Cookie", dashCookieName+"=old")
				bearer("s3cret")(r)
			}, http.StatusForbidden, ""},
			{"signed out", func(r *http.Request) { here(r); r.Header.Del("Cookie") }, http.StatusUnauthorized, "token"},
			{"no Origin", func(r *http.Request) { here(r); r.Header.Del("Origin") }, http.StatusForbidden, "csrf"},
			{"another site", and(here, header("Origin", "http://evil.example")), http.StatusForbidden, "origin"},
		} {
			rec := sendForm(h, http.MethodPost, tc.path, form, set.set)
			if rec.Code != set.code || envelope(rec) != set.reason {
				t.Errorf("%s, %s: %d (refused for %q), want %d for %q", tc.path, set.name, rec.Code, envelope(rec), set.code, set.reason)
			}
			if set.reason == "" && !strings.Contains(rec.Body.String(), fleetSignIn) {
				t.Errorf("%s, %s: the refusal doesn't say where it is done:\n%s", tc.path, set.name, rec.Body.String())
			}
			if got := f.called(); len(got) != 0 {
				t.Fatalf("%s, %s reached the control: %q", tc.path, set.name, got)
			}
		}
		if n := c.countSecurity("reason=fleet change without a signed-in browser", "route=POST "+tc.path); n != 3 {
			t.Errorf("%s: %d Security records of the API key's refusals, want 3", tc.path, n)
		}
		// From the page itself it goes through.
		if rec := sendForm(h, http.MethodPost, tc.path, form, here); rec.Code == http.StatusForbidden || len(f.called()) == 0 {
			t.Errorf("%s from the page: %d, calls %q\n%s", tc.path, rec.Code, f.called(), rec.Body.String())
		}
	}
}

// Each loosening asks first, and without its second click changes nothing:
// 428 where the step is the page's <details>, the server's confirm page for
// a change to many machines.
func TestFleetBulkLooseningAsksFirst(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		form       url.Values
		page       bool // the confirm page, not a 428
	}{
		{"a default to Allow", "/fleet/defaults/permission", url.Values{"category": {"exec"}, "mode": {"allow"}}, false},
		{"a default to Allow, shouted", "/fleet/defaults/permission", url.Values{"category": {"exec"}, "mode": {" ALLOW"}}, false},
		{"a default back to this Tenant's", "/fleet/defaults/permission", url.Values{"category": {"exec"}, "mode": {"unset"}}, false},
		{"a default with no mode", "/fleet/defaults/permission", url.Values{"category": {"exec"}}, false},
		{"a fleet rule that lets a tool run", "/fleet/defaults/rules/add", url.Values{"pattern": {"x"}, "mode": {"allow"}}, false},
		{"removing a fleet rule that blocks", "/fleet/defaults/rules/remove", url.Values{"pattern": {longURL(3)}}, false},
		{"removing a fleet rule nobody has", "/fleet/defaults/rules/remove", url.Values{"pattern": {"nothing"}}, false},
		{"deleting a group", "/fleet/groups/delete", url.Values{"group": {"web"}}, false},
		{"applying a group", "/fleet/groups/apply", url.Values{"group": {"web"}}, true},
		{"Allow on many", "/fleet/bulk", url.Values{"action": {"permission"}, "category": {"web"}, "cat_mode": {"allow"}, "machine": {fleetConfirmed, "m0000000000000002"}}, true},
		{"Allow, shouted, on many", "/fleet/bulk", url.Values{"action": {"permission"}, "category": {"web"}, "cat_mode": {"ALLOW"}, "machine": {fleetConfirmed}}, true},
		{"a Don't ask rule on many", "/fleet/bulk", url.Values{"action": {"rule"}, "pattern": {"x"}, "rule_mode": {"allow"}, "machine": {fleetConfirmed}}, true},
		{"removing a rule that asks from many", "/fleet/bulk", url.Values{"action": {"unrule"}, "pattern": {"os_run(" + longURL(0) + " *)"}, "machine": {fleetConfirmed}}, true},
		{"applying on many", "/fleet/bulk", url.Values{"action": {"apply"}, "all": {"shown"}}, true},
		{"copying again on many", "/fleet/bulk", url.Values{"action": {"copy"}, "machine": {fleetConfirmed}}, true},
		{"updating many", "/fleet/bulk", url.Values{"action": {"update"}, "machine": {fleetConfirmed}}, true},
	} {
		f := fleetFake(6)
		rec := sendForm(fleetServer(f).Handler(), http.MethodPost, tc.path, tc.form, here)
		switch {
		case tc.page && (rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Before this goes ahead") || !strings.Contains(rec.Body.String(), `name="confirm" value="yes"`)):
			t.Errorf("%s: %d, want the confirm page:\n%.2000s", tc.name, rec.Code, rec.Body.String())
		case !tc.page && rec.Code != http.StatusPreconditionRequired:
			t.Errorf("%s: %d, want 428", tc.name, rec.Code)
		}
		if got := f.called(); len(got) != 0 {
			t.Errorf("%s without its second click reached the control: %q", tc.name, got)
		}
	}
	// Tightening needs no second click: Ask, Deny, a Block rule, removing a
	// Don't ask rule, Stop.
	for _, tc := range []struct {
		path string
		form url.Values
		call string
	}{
		{"/fleet/defaults/permission", url.Values{"category": {"web"}, "mode": {"deny"}}, "default|web|deny"},
		{"/fleet/defaults/rules/add", url.Values{"pattern": {"x"}, "mode": {"deny"}}, "defrule|x|deny"},
		{"/fleet/defaults/rules/remove", url.Values{"pattern": {"os_run(systemctl status *)"}}, "defunrule|os_run(systemctl status *)"},
		{"/fleet/bulk", url.Values{"action": {"permission"}, "category": {"web"}, "cat_mode": {"deny"}, "machine": {fleetConfirmed}}, "bulk|permission|" + fleetConfirmed + "|web||deny"},
		{"/fleet/bulk", url.Values{"action": {"rule"}, "pattern": {"x"}, "rule_mode": {"deny"}, "machine": {fleetConfirmed}}, "bulk|rule|" + fleetConfirmed + "||x|deny"},
		{"/fleet/bulk", url.Values{"action": {"unrule"}, "pattern": {"os_run(" + longURL(1) + " *)"}, "machine": {fleetConfirmed}}, "bulk|unrule|" + fleetConfirmed + "||os_run(" + longURL(1) + " *)|"},
		{"/fleet/rollout/stop", url.Values{}, "rollout-stop"},
	} {
		f := fleetFake(6)
		f.view.Rollout = &FleetRollout{Running: true}
		rec := sendForm(fleetServer(f).Handler(), http.MethodPost, tc.path, tc.form, here)
		if got := f.called(); rec.Code == http.StatusPreconditionRequired || strings.Contains(rec.Body.String(), "Before this goes ahead") || !slices.Equal(got, []string{tc.call}) {
			t.Errorf("%s %v: %d, calls %q, want %q at once", tc.path, tc.form, rec.Code, got, tc.call)
		}
	}
}

// The confirm page names the action, how many machines and which (twenty,
// then how many more), those it already knows will be left out and why, and
// carries exactly the machines it names; Datastar gets it in place.
func TestFleetBulkConfirmStepNamesTheMachines(t *testing.T) {
	f := fleetFake(30)
	s := fleetServer(f)
	rec := sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", url.Values{"action": {"copy"}, "all": {"shown"}}, here)
	body := rec.Body.String()
	// 30 machines: m0 pending, every m%5==4 revoked (6 of them): 23 named.
	for _, want := range []string{
		"The approval settings of 23 machines are replaced by a fresh copy",
		"and 3 more.",
		"<li>" + longName(0) + `: <span class="dim">is waiting to be confirmed</span></li>`,
		"<li>" + longName(4) + `: <span class="dim">is revoked</span></li>`,
		`<button type="submit" class="btn sm danger">Yes, copy again on 23 machines</button>`,
		`<input type="hidden" name="action" value="copy">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the step is missing %q", want)
		}
	}
	if n := strings.Count(body, `<input type="hidden" name="machine"`); n != 23 {
		t.Errorf("the step carries %d machines, want the 23 it names", n)
	}
	if strings.Contains(body, `name="machine" value="`+fleetPending+`"`) || strings.Count(body, "<li>") != 20+1+6 {
		t.Errorf("the step names or carries a machine it leaves out (%d names)", strings.Count(body, "<li>"))
	}
	if strings.Contains(body, `name="all"`) {
		t.Error("the step carries all=shown: its second post must name the machines it showed")
	}
	// In place for Datastar: #fleet-body, no toast text.
	f.view.Machines[6].Online = true
	rec = sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", url.Values{"action": {"update"}, "version": {"v2.0.0"}, "at_once": {"3"}, "machine": {fleetConfirmed, "m0000000000000002", "m0000000000000006"}}, and(here, datastar))
	body = rec.Body.String()
	for _, want := range []string{`data: elements <div id="fleet-body">`, "Update 2 machines to v2.0.0, 3 at a time.", "<li>" + longName(1) + "</li>",
		"<li>" + longName(6) + "</li>", "<li>" + longName(2) + `: <span class="dim">is offline: nothing is queued</span></li>`, "Yes, update 2 machines", `<input type="hidden" name="at_once" value="3">`} {
		if !strings.Contains(body, want) {
			t.Errorf("the update's step in place is missing %q:\n%.3000s", want, body)
		}
	}
	if len(f.called()) != 0 {
		t.Fatalf("a step reached the control: %q", f.called())
	}
	// Nothing in the selection can take it: no step, and why for each.
	rec = sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", url.Values{"action": {"apply"}, "machine": {fleetPending, "m0000000000000004"}}, here)
	if body := rec.Body.String(); !strings.Contains(body, "None of these machines can take that. Nothing changed.") || strings.Contains(body, "Before this goes ahead") || len(f.called()) != 0 {
		t.Errorf("a selection nothing can take: calls %q\n%.2000s", f.called(), body)
	}
}

// The second post looks every machine up again and takes nothing else the
// form says about one: an id the fleet doesn't have is said, not sent; one
// that is pending or revoked now is the control's to refuse, in the answer;
// an id twice is sent once; ids that aren't ids, or too many, change nothing.
func TestFleetBulkForgedSecondPost(t *testing.T) {
	f := fleetFake(6)
	s := fleetServer(f)
	post := func(form url.Values) *httptest.ResponseRecorder {
		return sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", withField(form, "confirm", "yes"), here)
	}
	rec := post(url.Values{"action": {"apply"}, "machine": {"m9999999999999999", fleetPending, "m0000000000000004", fleetConfirmed, fleetConfirmed}})
	if got := f.called(); !slices.Equal(got, []string{"bulk|apply|" + fleetPending + "," + fleetConfirmed + ",m0000000000000004|||"}) {
		t.Errorf("the control got %q: want each machine the fleet has once, in the list's order", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"Done on 1 of 4.", "m9999999999999999: <span class=\"dim\">isn&#39;t part of this fleet</span>",
		longName(0) + `: <span class="dim">is waiting to be confirmed</span>`, longName(4) + `: <span class="dim">is revoked</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer is missing %q:\n%.3000s", want, body)
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("a plain post's answer: %d, want the page with the lines (a flash can't hold them)", rec.Code)
	}
	for name, form := range map[string]url.Values{
		"2000 ids":           {"action": {"apply"}, "machine": slices.Repeat([]string{fleetConfirmed}, 2000)},
		"1025 ids":           {"action": {"apply"}, "machine": slices.Repeat([]string{fleetConfirmed}, maxFleetSelection+1)},
		"a name, not an id":  {"action": {"apply"}, "machine": {longName(1)}},
		"an id in capitals":  {"action": {"apply"}, "machine": {strings.ToUpper(fleetConfirmed)}},
		"an id with a space": {"action": {"apply"}, "machine": {fleetConfirmed + " "}},
		"nothing chosen":     {"action": {"apply"}},
		"an action not one":  {"action": {"delete"}, "machine": {fleetConfirmed}},
		"a mode not one":     {"action": {"permission"}, "category": {"web"}, "cat_mode": {"always"}, "machine": {fleetConfirmed}},
		"a category not one": {"action": {"permission"}, "category": {"root"}, "cat_mode": {"deny"}, "machine": {fleetConfirmed}},
		"a rule with none":   {"action": {"rule"}, "pattern": {" "}, "rule_mode": {"deny"}, "machine": {fleetConfirmed}},
		"9 at once":          {"action": {"update"}, "at_once": {"9"}, "machine": {fleetConfirmed}},
		"0 at once":          {"action": {"update"}, "at_once": {"0"}, "machine": {fleetConfirmed}},
		"a group not one":    {"action": {"apply"}, "all": {"shown"}, "group": {"nope"}},
	} {
		before := len(f.called())
		rec := post(form)
		if _, msg, errMsg := fleetFlash(rec); msg != "" || errMsg == "" || len(f.called()) != before {
			t.Errorf("%s: %d msg %q err %q, calls %q", name, rec.Code, msg, errMsg, f.called()[before:])
		}
	}
	// "Every machine shown" is the server's: ids sent beside it are not read.
	before := len(f.called())
	post(url.Values{"action": {"permission"}, "category": {"web"}, "cat_mode": {"deny"}, "all": {"shown"}, "group": {"WEB"}, "machine": {"m0000000000000005"}})
	if got := f.called()[before:]; !slices.Equal(got, []string{"bulk|permission|" + fleetConfirmed + ",m0000000000000002|web||deny"}) {
		t.Errorf("every machine shown in the group web: %q", got)
	}
}

// A change to many machines that some can't take changes the others, and
// the answer says which were not changed and why: never one flash that
// hides it.
func TestFleetBulkPartialFailure(t *testing.T) {
	f := fleetFake(6)
	s := fleetServer(f)
	// m3 has no readable settings, m0 is pending; m1 and m2 change.
	ids := []string{fleetPending, fleetConfirmed, "m0000000000000002", "m0000000000000003"}
	rec := sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", url.Values{"action": {"permission"}, "category": {"exec"}, "cat_mode": {"deny"}, "machine": ids}, and(here, datastar))
	body := rec.Body.String()
	for _, want := range []string{
		`<div id="toast" class="toast err" role="alert"><span class="toast-text">Done on 2 of 4. 2 machines not changed: the page lists why.`,
		`data: elements <div id="fleet-body">`,
		`<div class="card-head">Done on 2 of 4.</div>`,
		longName(0) + `: <span class="dim">is waiting to be confirmed</span>`,
		longName(3) + `: <span class="dim">has no approval settings that can be read</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer is missing %q:\n%.3000s", want, body)
		}
	}
	for id, want := range map[string]string{fleetConfirmed: "deny", "m0000000000000002": "deny"} {
		if m := f.machine(id); m.Perms[0].Category != "exec" || m.Perms[0].Mode != want {
			t.Errorf("%s: %+v", id, m.Perms[0])
		}
	}
	// All of them: a plain toast.
	rec = sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", url.Values{"action": {"rule"}, "pattern": {"x"}, "rule_mode": {"ask"}, "machine": {fleetConfirmed, "m0000000000000002"}}, and(here, datastar))
	if !strings.Contains(rec.Body.String(), `class="toast ok"><span class="toast-text">Done on 2 of 2.`) {
		t.Errorf("all done:\n%.1000s", rec.Body.String())
	}
}

// A rollout starts from the bar's Update after its step, with what the form
// asked for, on the machines in the list's order; the second one while it
// runs is the control's refusal; Stop needs no step.
func TestFleetBulkRollout(t *testing.T) {
	f := fleetFake(6)
	s := fleetServer(f)
	form := url.Values{"action": {"update"}, "version": {" v2.0.0 "}, "allow_older": {"yes"}, "at_once": {"2"}, "machine": {"m0000000000000002", fleetConfirmed}, "confirm": {"yes"}}
	rec := sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", form, and(here, datastar))
	if got := f.called(); !slices.Equal(got, []string{"rollout|" + fleetConfirmed + ",m0000000000000002|v2.0.0||true|2"}) {
		t.Errorf("the control got %q", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "The rollout started: 2 machines, 2 at a time.") || !strings.Contains(body, `<div class="card-head">Rollout <span class="dim">· to v2.0.0, 2 at a time</span></div>`) {
		t.Errorf("the answer:\n%.3000s", body)
	}
	rec = sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", form, here)
	if _, _, errMsg := fleetFlash(rec); !strings.Contains(errMsg, "a rollout is running already") {
		t.Errorf("a second rollout: %q", errMsg)
	}
	rec = sendForm(s.Handler(), http.MethodPost, "/fleet/rollout/stop", nil, here)
	if _, msg, _ := fleetFlash(rec); !strings.Contains(msg, "starts no more machines") {
		t.Errorf("stop: %q", msg)
	}
	rec = sendForm(s.Handler(), http.MethodPost, "/fleet/rollout/stop", nil, here)
	if _, _, errMsg := fleetFlash(rec); errMsg != "No rollout is running. Nothing changed." {
		t.Errorf("stop twice: %q", errMsg)
	}
}

// The rollout card in each state: running (with each machine's live stage,
// and Stop), stopped at a machine that rolled back, ended on its own.
func TestFleetRolloutCard(t *testing.T) {
	f := fleetFake(4)
	f.view.Rollout = &FleetRollout{Limit: 2, Started: "2026-11-24 10:00", Running: true, Machines: []FleetRolloutMachine{
		{ID: fleetConfirmed, Name: longName(1), Status: "running", Stage: "sending"},
		{ID: "m0000000000000002", Name: longName(2), Status: "skipped", Reason: `web3 is offline (offline since 2026-11-24 09:00). Nothing is queued`},
		{ID: "m0000000000000003", Name: longName(3), Status: "waiting"},
	}}
	s := fleetServer(f)
	body := get(t, s, "/fleet").Body.String()
	for _, want := range []string{
		`Rollout <span class="dim">· to the latest release, 2 at a time</span>`, `<span class="risk">running</span>`,
		`<a class="linkbtn" href="/fleet/` + fleetConfirmed + `">` + longName(1) + `</a> <span class="risk">running: sending</span>`,
		`<span class="tag">skipped</span> <span class="dim">web3 is offline`, `<span class="risk">waiting</span>`,
		`action="/fleet/rollout/stop"`, "to see how far it has got",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a running rollout is missing %q", want)
		}
	}
	f.mu.Lock()
	ro := f.view.Rollout
	ro.Running, ro.Finished, ro.Stopped = false, "2026-11-24 10:05", "stopped at "+longName(1)
	ro.Machines[0].Status, ro.Machines[0].Stage, ro.Machines[0].Reason = "rolled_back", "", `rolled back to "1.2.3": "<b>it exited</b>"`
	ro.Machines[2].Status, ro.Machines[2].Reason = "not_started", "not started: the rollout was stopped at "+longName(1)
	f.mu.Unlock()
	body = get(t, s, "/fleet").Body.String()
	for _, want := range []string{`<span class="tag">ended</span>`, "ended 2026-11-24 10:05", "The rollout was stopped at " + longName(1) + ".",
		`<span class="act-errtag">rolled back</span> <span class="dim">rolled back to &#34;1.2.3&#34;: &#34;&lt;b&gt;it exited&lt;/b&gt;&#34;</span>`, `<span class="tag">not started</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("a stopped rollout is missing %q", want)
		}
	}
	if strings.Contains(body, `action="/fleet/rollout/stop"`) {
		t.Error("an ended rollout offers Stop")
	}
	f.mu.Lock()
	f.view.Rollout = nil
	f.mu.Unlock()
	if body := get(t, s, "/fleet").Body.String(); strings.Contains(body, "Rollout <span") {
		t.Error("no rollout, and the card shows")
	}
}

// The group filter shows a group's members only, with the groups as links
// above the table; a group the fleet doesn't have shows no machine and
// says so.
func TestFleetGroupFilter(t *testing.T) {
	s := fleetServer(fleetFake(6))
	all := get(t, s, "/fleet").Body.String()
	for _, want := range []string{`<strong>All</strong> · <a class="linkbtn" href="/fleet?group=db">db</a> <span class="dim">(2)</span> · <a class="linkbtn" href="/fleet?group=web">web</a> <span class="dim">(2)</span>`} {
		if !strings.Contains(all, want) {
			t.Errorf("the list is missing %q", want)
		}
	}
	if n := strings.Count(all, `name="machine" value="`); n != 6 {
		t.Errorf("the list has %d boxes, want one per machine", n)
	}
	web := get(t, s, "/fleet?group=WEB").Body.String()
	if n := strings.Count(web, `name="machine" value="`); n != 2 || !strings.Contains(web, `value="`+fleetConfirmed+`"`) || !strings.Contains(web, `<input type="hidden" name="group" value="WEB">`) ||
		!strings.Contains(web, `<a class="linkbtn" href="/fleet">All</a>`) || !strings.Contains(web, "every machine shown (2)") {
		t.Errorf("the group web: %d boxes\n%s", n, web)
	}
	for _, q := range []string{"nope", "all", fleetConfirmed, "<b>x</b>"} {
		body := get(t, s, "/fleet?group="+url.QueryEscape(q)).Body.String()
		if !strings.Contains(body, "There is no group by that name, so no machine is shown.") || strings.Contains(body, `name="machine"`) {
			t.Errorf("the group %q: want the empty state", q)
		}
	}
}

// The fleet's defaults: each category with /safety's control and a fourth
// state, From this Tenant, that says what it gives now; the rules with the
// machine page's editor; and each change, unset too.
func TestFleetDefaultsEditor(t *testing.T) {
	f := fleetFake(3)
	s := fleetServer(f)
	body := get(t, s, "/fleet").Body.String()
	for _, want := range []string{
		"The fleet's defaults",
		`<div class="seg" role="group" aria-label="Run commands">`,
		`<button type="button" class="btn sm on" disabled aria-pressed="true">Deny</button>`,
		`<button type="button" class="btn sm on" disabled aria-pressed="true">From this Tenant</button>`,
		"From this Tenant: Ask first now, as this Tenant&#39;s own setting is",
		"A new machine can&#39;t run commands and programs on this machine. It won&#39;t ask.",
		`<input type="hidden" name="category" value="exec"><input type="hidden" name="mode" value="allow"><input type="hidden" name="confirm" value="yes"><button type="submit" class="btn sm">Yes, allow</button>`,
		`<input type="hidden" name="category" value="web"><input type="hidden" name="mode" value="unset">`,
		`action="/fleet/defaults/rules/add"`, `<span class="nm">os_run(systemctl status *)</span> <span class="tag">runs without asking</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the defaults card is missing %q", want)
		}
	}
	if strings.Contains(body, "They are changed in the terminal") {
		t.Error("the page still says the fleet's rules are changed in the terminal")
	}
	for _, tc := range []struct {
		path, call, msg string
		form            url.Values
	}{
		{"/fleet/defaults/permission", "default|exec|", "For machines confirmed from now on, Run commands comes from this Tenant&#39;s own setting again.", url.Values{"category": {"exec"}, "mode": {"unset"}}},
		{"/fleet/defaults/permission", "default|web|ask", "For machines confirmed from now on, Act on websites asks you first.", url.Values{"category": {"web"}, "mode": {"ask"}}},
		{"/fleet/defaults/permission", "default|mcp|allow", "Connect remote services is allowed without asking.", url.Values{"category": {"mcp"}, "mode": {"allow"}}},
		{"/fleet/defaults/rules/add", "defrule|write_note|allow", "Fleet rule added", url.Values{"pattern": {" write_note "}, "mode": {"allow"}}},
		{"/fleet/defaults/rules/remove", "defunrule|write_note", "Fleet rule removed.", url.Values{"pattern": {"write_note"}}},
	} {
		before := len(f.called())
		rec := sendForm(s.Handler(), http.MethodPost, tc.path, withField(tc.form, "confirm", "yes"), and(here, datastar))
		if got := f.called()[before:]; !slices.Equal(got, []string{tc.call}) || !strings.Contains(rec.Body.String(), tc.msg) || !strings.Contains(rec.Body.String(), `data: elements <div id="fleet-body">`) {
			t.Errorf("%s %v: calls %q\n%.800s", tc.path, tc.form, got, rec.Body.String())
		}
	}
	if d := f.View().Defaults; d[0].Mode != "" || d[3].Mode != "ask" || d[6].Mode != "allow" {
		t.Errorf("the defaults now: %+v", d)
	}
	// What isn't one changes nothing.
	for name, form := range map[string]url.Values{
		"a mode not one":     {"category": {"web"}, "mode": {"sometimes"}, "confirm": {"yes"}},
		"a category not one": {"category": {"root"}, "mode": {"deny"}},
		"a rule with none":   {"pattern": {""}, "mode": {"deny"}},
	} {
		path := "/fleet/defaults/permission"
		if _, ok := form["pattern"]; ok {
			path = "/fleet/defaults/rules/add"
		}
		before := len(f.called())
		if _, _, errMsg := fleetFlash(sendForm(s.Handler(), http.MethodPost, path, form, here)); errMsg == "" || len(f.called()) != before {
			t.Errorf("%s: err %q, calls %q", name, errMsg, f.called()[before:])
		}
	}
	// Unreadable: said plainly, the reason in the log, never on the page.
	c := installCapture(t)
	f.mu.Lock()
	f.view.Defaults, f.view.DefaultsErr = nil, errors.New("fleet_permissions: invalid character 'x' at offset 3")
	f.mu.Unlock()
	body = get(t, s, "/fleet").Body.String()
	if !strings.Contains(body, htmlText(fleetDefaultsBroken)) || strings.Contains(body, "offset 3") || strings.Contains(body, `action="/fleet/defaults/permission"`) {
		t.Errorf("unreadable defaults:\n%s", body)
	}
	if !c.has("2201 ", "action=fleet defaults", "offset 3") {
		t.Errorf("the reason isn't in the log: %q", c.lines())
	}
	if _, _, errMsg := fleetFlash(sendForm(s.Handler(), http.MethodPost, "/fleet/defaults/permission", url.Values{"category": {"web"}, "mode": {"deny"}}, here)); errMsg != fleetDefaultsBroken {
		t.Errorf("a change while they can't be read: %q", errMsg)
	}
}

// A group's page, its changes, and the states: none, some, unreadable.
func TestFleetGroups(t *testing.T) {
	f := fleetFake(6)
	s := fleetServer(f)
	page := get(t, s, "/fleet/group/web").Body.String()
	for _, want := range []string{
		`<strong class="nm">web</strong> <span class="dim">· group of 2 machines, 1 rule</span>`,
		"Changing this group, its machines or its rules, moves no machine.",
		`<a class="linkbtn" href="/fleet/` + fleetConfirmed + `">` + longName(1) + `</a> <span class="on-tag">online</span>`,
		`Apply to its 2 members`, "1 member's settings differ from what Apply would give it now.",
		`<option value="` + fleetPending + `">` + longName(0) + ` (waiting for you)</option>`,
		`action="/fleet/groups/rules/add"`, `<span class="nm">os_run(nginx -t)</span> <span class="risk">asks you first</span>`,
		`<button type="submit" name="mode" value="allow" class="btn sm">Don't ask</button>`, // no step: it moves no machine
		`<summary class="btn sm danger"><span class="c-open">Delete this group</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the group's page is missing %q", want)
		}
	}
	if strings.Contains(page, `<option value="`+fleetConfirmed+`"`) {
		t.Error("the add form offers a machine that is in the group already")
	}
	// Named in any case; a name that can't be one, or no such group, is the list.
	if rec := get(t, s, "/fleet/group/WEB"); rec.Code != http.StatusOK {
		t.Errorf("the group in capitals: %d", rec.Code)
	}
	for _, name := range []string{"nope", "all", fleetConfirmed, "-web", strings.Repeat("a", 64)} {
		rec := get(t, s, "/fleet/group/"+url.PathEscape(name))
		if p, _, errMsg := fleetFlash(rec); rec.Code != http.StatusSeeOther || p != "/fleet" || errMsg != fleetNoGroup {
			t.Errorf("the group %q: %d to %q saying %q", name, rec.Code, p, errMsg)
		}
	}
	db := get(t, s, "/fleet/group/db").Body.String()
	if !strings.Contains(db, `<span class="nm">m00000000000000ff</span> <span class="dim">no longer one of the fleet's machines</span>`) {
		t.Errorf("a member the fleet no longer has:\n%s", db)
	}

	// Its changes move no machine, and say so.
	for _, tc := range []struct {
		path, call, msg string
		form            url.Values
	}{
		{"/fleet/groups/create", "group-create|edge", "The group edge is made.", url.Values{"name": {" edge "}}},
		{"/fleet/groups/members/add", "group-add|web|" + fleetPending + "|m0000000000000003", "2 machines added to web. " + fleetMovesNone, url.Values{"group": {"Web"}, "machine": {"m0000000000000003", fleetPending, fleetPending}}},
		{"/fleet/groups/members/remove", "group-remove|web|" + fleetConfirmed, "1 machine taken out of web. " + fleetMovesNone, url.Values{"group": {"web"}, "machine": {fleetConfirmed}}},
		{"/fleet/groups/members/remove", "group-remove|db|m00000000000000ff", "1 machine taken out of db.", url.Values{"group": {"db"}, "machine": {"m00000000000000ff"}}},
		{"/fleet/groups/rules/add", "group-rule|web|write_note|allow", "Rule added to web: once applied, what it matches runs without asking. " + fleetMovesNone, url.Values{"group": {"web"}, "pattern": {"write_note"}, "mode": {"allow"}}},
		{"/fleet/groups/rules/remove", "group-unrule|web|os_run(nginx -t)", "Rule removed from web. " + fleetMovesNone, url.Values{"group": {"web"}, "pattern": {"os_run(nginx -t)"}}},
	} {
		before := len(f.called())
		rec := sendForm(s.Handler(), http.MethodPost, tc.path, tc.form, and(here, datastar))
		if got := f.called()[before:]; !slices.Equal(got, []string{tc.call}) || !strings.Contains(rec.Body.String(), htmlText(tc.msg)) {
			t.Errorf("%s: calls %q, want %q\n%.800s", tc.path, got, tc.call, rec.Body.String())
		}
	}
	for name, tc := range map[string]struct {
		path string
		form url.Values
	}{
		"a name that can't be one":        {"/fleet/groups/create", url.Values{"name": {"web servers"}}},
		"all":                             {"/fleet/groups/create", url.Values{"name": {"ALL"}}},
		"a machine id":                    {"/fleet/groups/create", url.Values{"name": {fleetConfirmed}}},
		"a member the fleet doesn't have": {"/fleet/groups/members/add", url.Values{"group": {"web"}, "machine": {"m9999999999999999"}}},
		"a member by name":                {"/fleet/groups/members/add", url.Values{"group": {"web"}, "machine": {longName(1)}}},
		"a group the fleet doesn't have":  {"/fleet/groups/rules/add", url.Values{"group": {"nope"}, "pattern": {"x"}, "mode": {"deny"}}},
		"a rule with no mode":             {"/fleet/groups/rules/add", url.Values{"group": {"web"}, "pattern": {"x"}}},
	} {
		before := len(f.called())
		rec := sendForm(s.Handler(), http.MethodPost, tc.path, tc.form, here)
		if _, msg, errMsg := fleetFlash(rec); msg != "" || errMsg == "" || len(f.called()) != before {
			t.Errorf("%s: msg %q err %q calls %q", name, msg, errMsg, f.called()[before:])
		}
	}
	if _, _, errMsg := fleetFlash(sendForm(s.Handler(), http.MethodPost, "/fleet/groups/create", url.Values{"name": {"WEB"}}, here)); !strings.Contains(errMsg, "exists already") {
		t.Errorf("a name another group has in another case: %q", errMsg)
	}

	// Apply: the step names its members; the second post takes the members
	// the group has then, and says how each went.
	rec := sendForm(s.Handler(), http.MethodPost, "/fleet/groups/apply", url.Values{"group": {"web"}}, here)
	if body := rec.Body.String(); !strings.Contains(body, "laid over the approval settings of 1 machine.") || !strings.Contains(body, "<li>"+longName(3)+": ") || !strings.Contains(body, "<li>"+longName(0)+": ") ||
		strings.Contains(body, `name="machine"`) || !strings.Contains(body, `<input type="hidden" name="group" value="web">`) {
		t.Errorf("the group's apply step:\n%.3000s", body)
	}
	before := len(f.called())
	rec = sendForm(s.Handler(), http.MethodPost, "/fleet/groups/apply", url.Values{"group": {"web"}, "confirm": {"yes"}, "machine": {"m0000000000000005"}}, here)
	if got := f.called()[before:]; !slices.Equal(got, []string{"bulk|apply|" + fleetPending + ",m0000000000000002,m0000000000000003|||"}) {
		t.Errorf("apply took %q", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Done on 1 of 3.") || !strings.Contains(body, `<strong class="nm">web</strong>`) {
		t.Errorf("apply's answer:\n%.2000s", body)
	}
	// Delete: always its step; a plain post, answered with the list.
	if rec := sendForm(s.Handler(), http.MethodPost, "/fleet/groups/delete", url.Values{"group": {"web"}, "confirm": {"yes"}}, here); fleetMsg(rec) != "The group web is deleted. Its members' settings stay as they are." {
		t.Errorf("delete: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// None, and unreadable.
	f.mu.Lock()
	f.view.Groups = nil
	f.mu.Unlock()
	if body := get(t, s, "/fleet").Body.String(); !strings.Contains(body, "No groups yet.") || strings.Contains(body, "Show:") {
		t.Errorf("no groups:\n%s", body)
	}
	c := installCapture(t)
	f.mu.Lock()
	f.view.GroupsErr = errors.New("fleet_groups: duplicate group \"Web\"")
	f.mu.Unlock()
	if body := get(t, s, "/fleet").Body.String(); !strings.Contains(body, htmlText(fleetGroupsUnreadable)) || strings.Contains(body, "duplicate") || strings.Contains(body, `action="/fleet/groups/create"`) {
		t.Errorf("unreadable groups:\n%s", body)
	}
	if body := get(t, s, "/fleet/"+fleetConfirmed).Body.String(); !strings.Contains(body, htmlText(fleetGroupsUnreadable)) {
		t.Error("a machine's page doesn't say its groups can't be read")
	}
	if !c.has("2201 ", "action=fleet groups", "duplicate") {
		t.Errorf("the reason isn't in the log: %q", c.lines())
	}
	before = len(f.called())
	if _, _, errMsg := fleetFlash(sendForm(s.Handler(), http.MethodPost, "/fleet/groups/apply", url.Values{"group": {"web"}, "confirm": {"yes"}}, here)); errMsg != fleetGroupsUnreadable || len(f.called()) != before {
		t.Errorf("apply while groups can't be read: %q", errMsg)
	}
}

// A machine's page has /safety's control and labels, /tools' rule editor,
// its groups and which layers Apply fleet rules lays.
func TestFleetMachineParity(t *testing.T) {
	s := fleetServer(fleetFake(3))
	one := get(t, s, "/fleet/"+fleetConfirmed).Body.String()
	for _, want := range []string{
		`<div class="seg" role="group" aria-label="Run commands">`,
		`<button type="button" class="btn sm on" disabled aria-pressed="true">Allow</button>`,
		`<input type="hidden" name="machine" value="` + fleetConfirmed + `"><input type="hidden" name="category" value="write"><button type="submit" name="mode" value="deny" class="btn sm">Deny</button>`,
		"Can create and change files on this machine without asking you.",
		`<a class="linkbtn nm" href="/fleet/group/web">web</a>`,
		"Apply fleet rules lays the fleet's rules over this machine's settings, then the rules of each group it is in, in name order",
		`action="/fleet/rules/add"`,
	} {
		if !strings.Contains(one, want) {
			t.Errorf("the machine's page is missing %q", want)
		}
	}
	safety := withSession(New(Config{}, nil, nil, nil, nil, nil))
	safety.SetSafety(&fakeSafety{perms: safetyPerms()}, nil)
	page := get(t, safety, "/safety").Body.String()
	if !strings.Contains(page, `<div class="seg" role="group" aria-label="Run commands">`) || !strings.Contains(page, `<button type="submit" name="mode" value="deny" class="btn sm">Deny</button>`) {
		t.Errorf("/safety no longer has the shared control:\n%s", page)
	}
}

// F1 over TEN-546's pages: a machine's name, a group's name and rules, a
// rollout's reasons, and a rule typed into the bar reach the list, a group's
// page, a confirm step and an answer as element text only, escaped as
// html/template escapes it, never in a Datastar expression; each page has no
// more markup for them than for plain words.
func TestFleetBulkPagesShowWordsAsText(t *testing.T) {
	render := func(evil string) map[string]string {
		f := fleetFake(4)
		f.view.Machines[1].Name, f.view.Machines[3].Name = evil, evil
		f.view.Groups[1].Rules = []ToolRuleRow{{Pattern: evil, Mode: "ask"}}
		f.view.Groups = append(f.view.Groups, FleetGroup{Name: evil, Members: []string{fleetConfirmed}})
		f.view.Rollout = &FleetRollout{Version: "v2.0.0", Limit: 1, Running: true, Stopped: "stopped at " + evil, Machines: []FleetRolloutMachine{
			{ID: fleetConfirmed, Name: evil, Status: "failed", Reason: evil}}}
		s := fleetServer(f)
		post := func(form url.Values) string {
			return sendForm(s.Handler(), http.MethodPost, "/fleet/bulk", form, here).Body.String()
		}
		return map[string]string{
			"list":   get(t, s, "/fleet").Body.String(),
			"group":  get(t, s, "/fleet/group/web").Body.String(),
			"step":   post(url.Values{"action": {"rule"}, "pattern": {evil}, "rule_mode": {"allow"}, "machine": {fleetConfirmed, "m0000000000000003"}}),
			"answer": post(url.Values{"action": {"apply"}, "confirm": {"yes"}, "machine": {fleetConfirmed, "m0000000000000003"}}),
		}
	}
	clean := render("linux")
	for _, evil := range hostileSaid {
		pages := render(evil)
		esc, shown := htmlText(evil), htmlText(visibleText(evil))
		for page, want := range map[string][]string{
			"list":   {`">` + esc + `</a><div class="dim nm">` + fleetConfirmed, `<a class="linkbtn nm" href="/fleet/group/`, `">` + shown + `</a> <span class="dim">1 machine, 0 rules</span>`, `<span class="act-errtag">failed</span> <span class="dim">` + esc + `</span>`, "The rollout was stopped at " + esc + "."},
			"group":  {`<span class="nm">` + shown + `</span> <span class="risk">asks you first</span>`, `<input type="hidden" name="pattern" value="`},
			"step":   {"The rule " + htmlText(visibleText(evil)) + " is added on 1 machine", "<li>" + esc + "</li>", "<li>" + esc + `: <span class="dim">has no approval settings`},
			"answer": {"Done on 1 of 2.", esc + `: <span class="dim">has no approval settings that can be read</span>`},
		} {
			for _, w := range want {
				if !strings.Contains(pages[page], w) {
					t.Errorf("the %s page for %q is missing exactly %q", page, evil, w)
				}
			}
		}
		for page, got := range pages {
			for _, tag := range []string{"<", "<form", "<a ", "<script", "<img", "<h1", "<table", "<input", " data-", "onerror=\""} {
				if g, w := strings.Count(got, tag), strings.Count(clean[page], tag); g != w {
					t.Errorf("the %s page for %q has %d of %q, plain words give %d", page, evil, g, tag, w)
				}
			}
			if bad := injectedExprs(got); len(bad) > 0 {
				t.Errorf("the %s page for %q puts it in a Datastar expression: %q", page, evil, bad)
			}
		}
	}
}
