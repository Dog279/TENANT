package dashboard

// fleet_bulk.go is TEN-546's part of the Fleet page (section 2.6): the
// fleet's defaults, its groups, one change to many machines, and the rollout
// that updates many machines a few at a time.
//
// Every route here that changes something takes a signed-in browser's own
// form only (signedInBrowser): the API key gets no more than it got before
// (fleetChange). A change that lets the agent do more, replaces machines'
// settings or updates them asks first; for many machines that step is a page
// the server draws, naming how many machines and which, and the second post
// looks every machine up again and uses nothing else the form says about it.
// Tightening goes through at once: friction on "go", never on "stop".
//
// Group names and rule patterns are the operator's own words; the page shows
// them through visibleText, as it shows rules. A machine's name is the
// control's escaped label (fleetName), in element content only.

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	// maxFleetSelection bounds a selection before anything is looked up.
	maxFleetSelection = 1024
	// fleetStepNames is how many machines a confirm step names before "and N more".
	fleetStepNames = 20

	// fleetSignIn is the 403 of a change here without a signed-in browser.
	fleetSignIn           = "Nothing changed: the fleet's defaults, its groups, a change to many machines and a rollout are made from the Fleet page in a signed-in browser, or with tenant fleet in a terminal on the computer running Tenant."
	fleetNoGroup          = "There is no group by that name. Nothing changed."
	fleetGroupsUnreadable = "The fleet's groups can't be read right now. Until they can, no group is shown, changed or applied. The Logs page has the reason."
	fleetDefaultsBroken   = "The fleet's default safety settings can't be read right now. Until they can, no machine can be confirmed or given a fresh copy. The Logs page has the reason."
	fleetGroupName        = "Nothing changed. A group's name is up to 63 letters, digits, dots, dashes or underscores, starts with a letter or a digit, isn't all, doesn't look like a machine id (m and 16 hex digits), and isn't another group's."
	fleetMovesNone        = "No machine's settings changed: Apply lays a group's rules over its members."
)

var (
	machineIDShape = regexp.MustCompile(`^m[0-9a-f]{16}$`)
	groupNameShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// validGroupName is the group name rule of the spec, checked on the way in
// so a name that can't be one is never looked up or put in a link.
func validGroupName(name string) bool {
	return groupNameShape.MatchString(name) && !machineIDShape.MatchString(name) && !strings.EqualFold(name, "all")
}

func (s *Server) mountFleetBulk(mux *http.ServeMux) {
	mux.HandleFunc("GET /fleet/group/{name}", s.handleFleetGroupPage)
	// The fleet's defaults move no machine; they are what a machine confirmed
	// later starts with. Allow, From this Tenant (which may be Allow) and an
	// allow rule ask first, as on one machine.
	mux.HandleFunc("POST /fleet/defaults/permission", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleFleetDefault))
	mux.HandleFunc("POST /fleet/defaults/rules/add", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleFleetDefaultRuleAdd))
	mux.HandleFunc("POST /fleet/defaults/rules/remove", needConfirm(func(r *http.Request) bool {
		return s.fleetDefaultRuleMode(strings.TrimSpace(r.FormValue("pattern"))) != "allow"
	}, s.handleFleetDefaultRuleRemove))
	// A group's changes move no machine: Apply is the step that does, and it
	// names how many.
	mux.HandleFunc("POST /fleet/groups/create", s.handleFleetGroupCreate)
	mux.HandleFunc("POST /fleet/groups/delete", needConfirm(nil, s.handleFleetGroupDelete))
	mux.HandleFunc("POST /fleet/groups/members/add", s.handleFleetGroupMembers)
	mux.HandleFunc("POST /fleet/groups/members/remove", s.handleFleetGroupMembers)
	mux.HandleFunc("POST /fleet/groups/rules/add", s.handleFleetGroupRuleAdd)
	mux.HandleFunc("POST /fleet/groups/rules/remove", s.handleFleetGroupRuleRemove)
	// These two draw their own confirm step: a page naming the machines.
	mux.HandleFunc("POST /fleet/groups/apply", s.handleFleetGroupApply)
	mux.HandleFunc("POST /fleet/bulk", s.handleFleetBulk)
	mux.HandleFunc("POST /fleet/rollout/stop", s.handleFleetRolloutStop)
}

// fleetChange is the gate of every change here: a fleet control (else the
// page says it is off), and the page's own form in a signed-in browser,
// never the API key. It has answered when it returns false.
func (s *Server) fleetChange(w http.ResponseWriter, r *http.Request) bool {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return false
	}
	if _, ok := s.signedInBrowser(r); !ok {
		refused(r, "fleet change without a signed-in browser", r.Pattern)
		http.Error(w, fleetSignIn, http.StatusForbidden)
		return false
	}
	return true
}

// fleetShown is the list's machines under the group filter: every machine,
// or the members of group (named case-insensitively) in the list's order.
// noGroup: the fleet has no such group.
func fleetShown(v FleetView, group string) (shown []FleetMachine, noGroup bool) {
	if group == "" {
		return v.Machines, false
	}
	g := findGroup(v, group)
	if g == nil {
		return nil, true
	}
	for _, m := range v.Machines {
		if slices.Contains(g.Members, m.ID) {
			shown = append(shown, m)
		}
	}
	return shown, false
}

func findGroup(v FleetView, name string) *FleetGroup {
	if !validGroupName(name) {
		return nil
	}
	for i := range v.Groups {
		if strings.EqualFold(v.Groups[i].Name, name) {
			return &v.Groups[i]
		}
	}
	return nil
}

// --- the page's own reads ---

// Counts is the group's size in words.
func (g FleetGroup) Counts() string {
	return plural(len(g.Members), "machine") + ", " + plural(len(g.Rules), "rule")
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// Categories are the safety categories the bulk bar offers: the fleet's
// defaults', else the first machine's with settings.
func (d fleetPageData) Categories() []PermissionRow {
	if len(d.View.Defaults) > 0 {
		out := make([]PermissionRow, len(d.View.Defaults))
		for i, p := range d.View.Defaults {
			out[i] = p.PermissionRow
		}
		return out
	}
	for _, m := range d.View.Machines {
		if len(m.Perms) > 0 {
			return m.Perms
		}
	}
	return nil
}

// DefaultSegs are the fleet defaults' segmented controls.
func (d fleetPageData) DefaultSegs() []PermSeg {
	out := make([]PermSeg, len(d.View.Defaults))
	for i, p := range d.View.Defaults {
		out[i] = PermSeg{Action: "/fleet/defaults/permission", Category: p.Category, Title: p.Title, Mode: p.Mode, Unset: true,
			Allow:     "Every machine confirmed from now on starts with your agent able to " + p.Can + " without asking you. No machine that is confirmed already changes.",
			UnsetSaid: "Every machine confirmed from now on takes this from this Tenant's own setting, which is " + fleetModeLabel[p.Inherited] + " now. No machine that is confirmed already changes."}
	}
	return out
}

// DefaultSaid is what a default category means for a new machine.
func (p FleetDefault) Said() string {
	switch p.Mode {
	case "":
		return "From this Tenant: " + fleetModeLabel[p.Inherited] + " now, as this Tenant's own setting is (no looser than Ask first for the five that are kept at Ask)."
	case "allow":
		return "A new machine can " + p.Can + " without asking you."
	case "deny":
		return "A new machine can't " + p.Can + ". It won't ask."
	}
	return "A new machine asks you before it can " + p.Can + "."
}

// DefaultRules is the fleet's rules' editor.
func (d fleetPageData) DefaultRules() RuleEditor {
	return RuleEditor{Add: "/fleet/defaults/rules/add", Remove: "/fleet/defaults/rules/remove", Rules: d.View.Rules,
		Empty:       "No fleet rules. A new machine starts with a copy of this Tenant's own settings.",
		Placeholder: "a tool name, or a command rule as a request for a machine shows it",
		AllowSaid:   "Every machine confirmed from now on can do what this rule matches without asking you, whatever its safety settings say. No machine that is confirmed already changes.",
		RemoveSaid:  "Machines confirmed from now on follow their safety settings for what this rule matched, which may let your agent do it without asking you."}
}

// MachineSegs are a machine's segmented controls, as /safety's.
func (d fleetPageData) MachineSegs() []PermSeg {
	m := d.One
	out := make([]PermSeg, len(m.Perms))
	for i, p := range m.Perms {
		out[i] = PermSeg{Action: "/fleet/permission", Category: p.Category, Title: orElse(p.Title, p.Category), Mode: p.Mode,
			Allow:  "Your agent can " + p.Can + " without asking you. This is for this machine only.",
			Hidden: []HiddenField{{"machine", m.ID}}}
	}
	return out
}

// MachineRules is a machine's rule editor, as the Tools page's.
func (d fleetPageData) MachineRules() RuleEditor {
	return RuleEditor{Add: "/fleet/rules/add", Remove: "/fleet/rules/remove", Rules: d.One.Rules, Hidden: []HiddenField{{"machine", d.One.ID}},
		Empty:       "No rules for this machine yet.",
		Placeholder: "a tool name, or a command rule as a request for this machine shows it",
		AllowSaid:   "On this machine your agent can do what this rule matches without asking you, whatever the safety settings above say.",
		RemoveSaid:  "What this rule matched then follows this machine's safety settings, which may let your agent do it without asking you."}
}

// GroupRules is a group's rule editor: no step, it moves no machine.
func (d fleetPageData) GroupRules() RuleEditor {
	return RuleEditor{Add: "/fleet/groups/rules/add", Remove: "/fleet/groups/rules/remove", Rules: d.Group.Rules, Hidden: []HiddenField{{"group", d.Group.Name}},
		Empty:       "No rules in this group yet.",
		Placeholder: "a tool name, or a command rule as a request for a machine shows it"}
}

// GroupHref is the link of a group the machine page names.
func (d fleetPageData) GroupHref(name string) string { return FleetGroup{Name: name}.Href() }

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- the fleet's defaults ---

func (s *Server) handleFleetDefault(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	v := s.fleet.View()
	cat, mode := r.FormValue("category"), strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	i := slices.IndexFunc(v.Defaults, func(p FleetDefault) bool { return p.Category == cat })
	switch {
	case modeWords[mode] == "" && mode != "unset":
		s.fleetDone(w, r, "", "", "Choose Ask, Allow, Deny or From this Tenant. Nothing changed.")
		return
	case v.DefaultsErr != nil:
		s.fleetDone(w, r, "", "", s.flashFor(r.Context(), "fleet defaults", v.DefaultsErr, fleetDefaultsBroken))
		return
	case i < 0 && v.Defaults != nil:
		s.fleetDone(w, r, "", "", "That safety check doesn't exist. Nothing changed.")
		return
	}
	title := "That safety setting"
	if i >= 0 {
		title = v.Defaults[i].Title
	}
	set, said := mode, modeWords[mode]
	if mode == "unset" {
		set, said = "", "comes from this Tenant's own setting again"
	}
	if err := s.fleet.SetDefault(cat, set); err != nil {
		s.fleetDone(w, r, "", "", s.flashFor(r.Context(), "fleet default", err, "Couldn't change the fleet's default. Nothing changed."))
		return
	}
	s.fleetDone(w, r, "", "For machines confirmed from now on, "+title+" "+said+". No machine that is confirmed already changes.", "")
}

// fleetDefaultRuleMode is the mode of the fleet's rule for pattern, "" with
// none: removing it asks first unless it let the tool go ahead, decided from
// the rules themselves.
func (s *Server) fleetDefaultRuleMode(pattern string) string {
	if s.fleet == nil {
		return ""
	}
	for _, rule := range s.fleet.View().Rules {
		if rule.Pattern == pattern {
			return strings.ToLower(rule.Mode)
		}
	}
	return ""
}

func (s *Server) handleFleetDefaultRuleAdd(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	pattern, mode := strings.TrimSpace(r.FormValue("pattern")), strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if pattern == "" || ruleWords[mode] == "" {
		s.fleetDone(w, r, "", "", "A rule needs a tool name or pattern, and Ask first, Don't ask or Block.")
		return
	}
	if err := s.fleet.SetDefaultRule(pattern, mode); err != nil {
		s.fleetDone(w, r, "", "", s.flashFor(r.Context(), "fleet default rule add", err, "Couldn't add that rule. Check the tool name or pattern."))
		return
	}
	s.fleetDone(w, r, "", "Fleet rule added: on machines confirmed from now on, what it matches "+ruleWords[mode]+". No machine that is confirmed already changes until the fleet's rules are applied to it.", "")
}

func (s *Server) handleFleetDefaultRuleRemove(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	removed, err := s.fleet.RemoveDefaultRule(strings.TrimSpace(r.FormValue("pattern")))
	switch {
	case err != nil:
		s.fleetDone(w, r, "", "", s.flashFor(r.Context(), "fleet default rule remove", err, "Couldn't remove that rule."))
	case !removed:
		s.fleetDone(w, r, "", "", "The fleet has no such rule. Nothing changed.")
	default:
		s.fleetDone(w, r, "", "Fleet rule removed. No machine that is confirmed already changes.", "")
	}
}

// --- groups ---

// fleetGroupData is a group's page, named case-insensitively; d.Group is nil
// when the fleet has no such group.
func (s *Server) fleetGroupData(r *http.Request, name string) fleetPageData {
	d := s.fleetData(r, "")
	g := findGroup(d.View, name)
	if g == nil || d.View.GroupsErr != nil {
		return d
	}
	d.Group = g
	for _, m := range d.View.Machines {
		if slices.Contains(g.Members, m.ID) {
			d.Members = append(d.Members, m)
		} else {
			d.Others = append(d.Others, m)
		}
	}
	for _, id := range g.Members {
		if !slices.ContainsFunc(d.View.Machines, func(m FleetMachine) bool { return m.ID == id }) {
			d.Strays = append(d.Strays, id)
		}
	}
	return d
}

func (s *Server) handleFleetGroupPage(w http.ResponseWriter, r *http.Request) {
	d := s.fleetGroupData(r, r.PathValue("name"))
	if d.Configured && d.View.Master && d.View.Err == nil && d.View.GroupsErr == nil && d.Group == nil {
		redirectFlash(w, r, "/fleet", "", fleetNoGroup)
		return
	}
	d.Msg, d.Err = r.URL.Query().Get("msg"), r.URL.Query().Get("err")
	s.render(w, s.tmpl.fleet, d)
}

// fleetGroupDone answers a group's form with its page, or with the list once
// the group is gone.
func (s *Server) fleetGroupDone(w http.ResponseWriter, r *http.Request, name, msg, errMsg string) {
	d := s.fleetGroupData(r, name)
	page := "/fleet"
	if d.Group != nil {
		page = d.Group.Href()
	}
	s.formDone(w, r, page, msg, errMsg, func() (string, error) { return part(s.tmpl.fleet, "fleet-body", d) })
}

// formGroup is the group a form names in its group field, as the fleet has it
// now; "" with the plain reason when there is none.
func (s *Server) formGroup(r *http.Request) (FleetGroup, string) {
	v := s.fleet.View()
	if v.GroupsErr != nil {
		return FleetGroup{}, s.flashFor(r.Context(), "fleet groups", v.GroupsErr, fleetGroupsUnreadable)
	}
	g := findGroup(v, strings.TrimSpace(r.FormValue("group")))
	if g == nil {
		return FleetGroup{}, fleetNoGroup
	}
	return *g, ""
}

func (s *Server) handleFleetGroupCreate(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if !validGroupName(name) {
		s.fleetDone(w, r, "", "", fleetGroupName)
		return
	}
	if err := s.fleet.CreateGroup(name); err != nil {
		s.fleetDone(w, r, "", "", s.flashFor(r.Context(), "fleet group create", err, fleetGroupName))
		return
	}
	s.fleetDone(w, r, "", "The group "+name+" is made. Open it to add machines and rules.", "")
}

// handleFleetGroupDelete is a plain post from the group's own page: the page
// it is on is the group it deletes, so the answer is the list.
func (s *Server) handleFleetGroupDelete(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	g, why := s.formGroup(r)
	if why != "" {
		s.fleetDone(w, r, "", "", why)
		return
	}
	if err := s.fleet.DeleteGroup(g.Name); err != nil {
		s.fleetGroupDone(w, r, g.Name, "", s.flashFor(r.Context(), "fleet group delete", err, "Couldn't delete that group. Nothing changed."))
		return
	}
	s.fleetDone(w, r, "", "The group "+g.Name+" is deleted. Its members' settings stay as they are.", "")
}

// handleFleetGroupMembers is POST /fleet/groups/members/add and /remove
// (group, machine…): each machine one the fleet has, by id. An id the fleet
// no longer has can still be taken out of a group that lists it.
func (s *Server) handleFleetGroupMembers(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	g, why := s.formGroup(r)
	if why != "" {
		s.fleetDone(w, r, "", "", why)
		return
	}
	add := strings.HasSuffix(r.URL.Path, "/add")
	ms, unknown, why := fleetSelection(r, s.fleet.View())
	if why != "" {
		s.fleetGroupDone(w, r, g.Name, "", why)
		return
	}
	ids := make([]string, 0, len(ms)+len(unknown))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	for _, id := range unknown {
		if add || !slices.Contains(g.Members, id) {
			s.fleetGroupDone(w, r, g.Name, "", fleetGone)
			return
		}
		ids = append(ids, id)
	}
	var err error
	if add {
		err = s.fleet.AddMembers(g.Name, ids)
	} else {
		err = s.fleet.RemoveMembers(g.Name, ids)
	}
	if err != nil {
		s.fleetGroupDone(w, r, g.Name, "", s.flashFor(r.Context(), "fleet group members", err, "Couldn't change the group's machines. Nothing changed."))
		return
	}
	said := plural(len(ids), "machine") + " added to " + g.Name + ". "
	if !add {
		said = plural(len(ids), "machine") + " taken out of " + g.Name + ". "
	}
	s.fleetGroupDone(w, r, g.Name, said+fleetMovesNone, "")
}

func (s *Server) handleFleetGroupRuleAdd(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	g, why := s.formGroup(r)
	if why != "" {
		s.fleetDone(w, r, "", "", why)
		return
	}
	pattern, mode := strings.TrimSpace(r.FormValue("pattern")), strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if pattern == "" || ruleWords[mode] == "" {
		s.fleetGroupDone(w, r, g.Name, "", "A rule needs a tool name or pattern, and Ask first, Don't ask or Block.")
		return
	}
	if err := s.fleet.SetGroupRule(g.Name, pattern, mode); err != nil {
		s.fleetGroupDone(w, r, g.Name, "", s.flashFor(r.Context(), "fleet group rule add", err, "Couldn't add that rule. Check the tool name or pattern."))
		return
	}
	s.fleetGroupDone(w, r, g.Name, "Rule added to "+g.Name+": once applied, what it matches "+ruleWords[mode]+". "+fleetMovesNone, "")
}

func (s *Server) handleFleetGroupRuleRemove(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	g, why := s.formGroup(r)
	if why != "" {
		s.fleetDone(w, r, "", "", why)
		return
	}
	removed, err := s.fleet.RemoveGroupRule(g.Name, strings.TrimSpace(r.FormValue("pattern")))
	switch {
	case err != nil:
		s.fleetGroupDone(w, r, g.Name, "", s.flashFor(r.Context(), "fleet group rule remove", err, "Couldn't remove that rule."))
	case !removed:
		s.fleetGroupDone(w, r, g.Name, "", "This group has no such rule. Nothing changed.")
	default:
		s.fleetGroupDone(w, r, g.Name, "Rule removed from "+g.Name+". "+fleetMovesNone, "")
	}
}

// handleFleetGroupApply is POST /fleet/groups/apply (group): Apply fleet
// rules on each of its members. Its first post is the confirm step naming
// them; the second looks the group's members up again.
func (s *Server) handleFleetGroupApply(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	g, why := s.formGroup(r)
	if why != "" {
		s.fleetDone(w, r, "", "", why)
		return
	}
	d := s.fleetGroupData(r, g.Name)
	req := fleetBulkReq{op: FleetBulkOp{Action: FleetBulkApply}, machines: d.Members, unknown: d.Strays}
	if r.FormValue("confirm") != "yes" {
		s.fleetStep(w, r, d, req, "/fleet/groups/apply", []HiddenField{{"group", g.Name}}, d.Group.Href())
		return
	}
	s.fleetBulkDo(w, r, d, req)
}

// --- many machines ---

// fleetBulkReq is a change to many machines, as the server resolved it.
type fleetBulkReq struct {
	op               FleetBulkOp
	update           bool
	version, arch    string
	allowOlder       bool
	atOnce           int
	machines         []FleetMachine // the fleet's, in the list's order
	unknown          []string       // ids named that the fleet has no machine for
	catTitle, catCan string
	fields           []HiddenField // the action's own fields, for the second post
}

// fleetBulkStep is the confirm step of a change to many machines: what will
// happen, to how many machines and which, those already known to be skipped,
// and the one form that does it.
type fleetBulkStep struct {
	Route, What, Yes string
	Danger           bool
	Count            int
	Names            []string
	More             int
	Skips            []fleetBulkLine
	Hidden           []HiddenField
	Back             string // where Cancel goes
}

// fleetBulkReply is what a change to many machines did: "Done on 5 of 7."
// and one line per machine that was not changed.
type fleetBulkReply struct {
	Head  string
	Lines []fleetBulkLine
}

type fleetBulkLine struct{ Name, Said string }

// fleetSelection is the machines a form names: the checked ids, or with
// all=shown every machine the list shows under the form's group filter,
// worked out here and never from a list the browser sent. The ids are
// bounded, checked for shape and deduplicated before anything is looked up.
func fleetSelection(r *http.Request, v FleetView) (ms []FleetMachine, unknown []string, why string) {
	if r.FormValue("all") == "shown" {
		shown, noGroup := fleetShown(v, strings.TrimSpace(r.FormValue("group")))
		if noGroup {
			return nil, nil, fleetNoGroup
		}
		if len(shown) == 0 {
			return nil, nil, "No machine is shown, so nothing changed."
		}
		return shown, nil, ""
	}
	raw := r.Form["machine"]
	if len(raw) > maxFleetSelection {
		return nil, nil, fmt.Sprintf("Nothing changed: that is more than %d machines at once.", maxFleetSelection)
	}
	want := map[string]bool{}
	var order []string
	for _, id := range raw {
		if !machineIDShape.MatchString(id) {
			return nil, nil, "Nothing changed: a machine is named by its id, m and 16 hex digits."
		}
		if !want[id] {
			want[id] = true
			order = append(order, id)
		}
	}
	if len(order) == 0 {
		return nil, nil, "Check at least one machine, or choose every machine shown. Nothing changed."
	}
	for _, m := range v.Machines {
		if want[m.ID] {
			ms = append(ms, m)
			delete(want, m.ID)
		}
	}
	for _, id := range order {
		if want[id] {
			unknown = append(unknown, id)
		}
	}
	return ms, unknown, ""
}

// parseBulk reads POST /fleet/bulk's action and its fields.
func (s *Server) parseBulk(r *http.Request, v FleetView) (fleetBulkReq, string) {
	var req fleetBulkReq
	action := r.FormValue("action")
	field := func(name string) string {
		val := strings.TrimSpace(r.FormValue(name))
		req.fields = append(req.fields, HiddenField{name, val})
		return val
	}
	switch action {
	case FleetBulkPermission:
		req.op.Category, req.op.Mode = field("category"), strings.ToLower(field("cat_mode"))
		var p *PermissionRow
		cats := (fleetPageData{View: v}).Categories()
		if i := slices.IndexFunc(cats, func(c PermissionRow) bool { return c.Category == req.op.Category }); i >= 0 {
			p = &cats[i]
		}
		switch {
		case p == nil:
			return req, "That safety check doesn't exist. Nothing changed."
		case modeWords[req.op.Mode] == "":
			return req, "Pick Ask, Allow or Deny. Nothing changed."
		}
		req.catTitle, req.catCan = p.Title, p.Can
	case FleetBulkRule, FleetBulkUnrule:
		req.op.Pattern = field("pattern")
		if action == FleetBulkRule {
			req.op.Mode = strings.ToLower(field("rule_mode"))
		}
		if req.op.Pattern == "" || action == FleetBulkRule && ruleWords[req.op.Mode] == "" {
			return req, "A rule needs a tool name or pattern, and Ask first, Don't ask or Block."
		}
	case FleetBulkApply, FleetBulkCopy:
	case "update":
		req.update = true
		req.version, req.arch, req.allowOlder = field("version"), field("arch"), field("allow_older") == "yes"
		n, err := strconv.Atoi(orElse(field("at_once"), "1"))
		switch {
		case err != nil || n < 1 || n > 8:
			return req, "Choose how many machines update at once: 1 to 8. Nothing was sent."
		case len(req.version) > 48 || len(req.arch) > 16:
			return req, "That isn't a version. Nothing was sent."
		}
		req.atOnce = n
	default:
		return req, "Choose what to do with the machines. Nothing changed."
	}
	req.op.Action = action
	req.fields = append(req.fields, HiddenField{"action", action})
	return req, ""
}

// needsStep is whether the change asks first (2.6): anything that lets the
// agent do more, replaces settings or updates. Removing a rule asks unless
// every chosen machine that has it lets the tool go ahead with it.
func (req fleetBulkReq) needsStep() bool {
	switch req.op.Action {
	case FleetBulkPermission, FleetBulkRule:
		return req.op.Mode != "ask" && req.op.Mode != "deny"
	case FleetBulkUnrule:
		for _, m := range req.machines {
			for _, rule := range m.Rules {
				if rule.Pattern == req.op.Pattern && strings.ToLower(rule.Mode) != "allow" {
					return true
				}
			}
		}
		return false
	}
	return true
}

// skip is why a machine will not take the change, where that is known
// before it is tried; "" when it may.
func (req fleetBulkReq) skip(m FleetMachine) string {
	switch {
	case m.State == "pending":
		return "is waiting to be confirmed"
	case m.State == "revoked":
		return "is revoked"
	case req.update && m.State != "confirmed":
		return "isn't confirmed"
	case req.update && !m.Online:
		return "is offline: nothing is queued"
	case !req.update && req.op.Action != FleetBulkCopy && !m.HasScheme:
		return "has no approval settings that can be read"
	}
	return ""
}

// handleFleetBulk is POST /fleet/bulk (action, machine… or all=shown with
// group, and the action's fields): one change to many machines, or the
// rollout. A change that asks first answers its first post with the step.
func (s *Server) handleFleetBulk(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	d := s.fleetData(r, "")
	if d.View.Err != nil {
		s.fleetDone(w, r, "", "", d.Problem)
		return
	}
	req, why := s.parseBulk(r, d.View)
	if why == "" {
		req.machines, req.unknown, why = fleetSelection(r, d.View)
	}
	if why != "" {
		s.fleetDone(w, r, "", "", why)
		return
	}
	if req.needsStep() && r.FormValue("confirm") != "yes" {
		back, fields := "/fleet", req.fields
		if d.Filter != "" && !d.NoGroup {
			// The answer shows the list under the same filter. The second post
			// names its machines by id: the group only picks the page.
			back += "?group=" + url.QueryEscape(d.Filter)
			fields = append(slices.Clone(fields), HiddenField{"group", d.Filter})
		}
		s.fleetStep(w, r, d, req, "/fleet/bulk", fields, back)
		return
	}
	s.fleetBulkDo(w, r, d, req)
}

// fleetStep answers with the confirm step: in place of #fleet-body for a
// Datastar post, a whole page otherwise. Its form carries the machines it
// names, by id, and the action's fields (a group's Apply carries the group
// alone: its second post takes the members the group has then). With no
// machine that can take the change there is nothing to confirm: the answer
// says why for each.
func (s *Server) fleetStep(w http.ResponseWriter, r *http.Request, d fleetPageData, req fleetBulkReq, route string, fields []HiddenField, back string) {
	st := &fleetBulkStep{Route: route, Back: back, Hidden: slices.Clone(fields)}
	byID := route == "/fleet/bulk"
	for _, id := range req.unknown {
		st.Skips = append(st.Skips, fleetBulkLine{id, "isn't part of this fleet"})
	}
	for _, m := range req.machines {
		if why := req.skip(m); why != "" {
			st.Skips = append(st.Skips, fleetBulkLine{m.Name, why})
			continue
		}
		st.Count++
		if len(st.Names) < fleetStepNames {
			st.Names = append(st.Names, m.Name)
		}
		if byID {
			st.Hidden = append(st.Hidden, HiddenField{"machine", m.ID})
		}
	}
	st.More = st.Count - len(st.Names)
	if st.Count == 0 {
		reply := &fleetBulkReply{Head: "None of these machines can take that. Nothing changed."}
		reply.Lines = st.Skips
		d.Result = reply
		s.fleetAnswer(w, r, d, "", reply.Head)
		return
	}
	n := plural(st.Count, "machine")
	switch o := req.op; {
	case req.update:
		to := "the latest release"
		if req.version != "" {
			to = req.version
		}
		st.What = "Update " + n + " to " + to + ", " + strconv.Itoa(req.atOnce) + " at a time. Each restarts into the new version and its background commands end; one whose new version does not reach this Tenant within a minute puts the old one back. The first that rolls back or fails stops the rollout: no further machine is started."
		st.Yes = "Yes, update " + n
	case o.Action == FleetBulkPermission:
		// Only Allow asks first (parseBulk took ask, allow and deny alone).
		st.What = req.catTitle + " is set to Allow on " + n + ": on each, your agent can " + req.catCan + " without asking you."
		st.Yes = "Yes, set it on " + n
	case o.Action == FleetBulkRule:
		st.What = "The rule " + visibleText(o.Pattern) + " is added on " + n + ": on each, your agent can do what it matches without asking you, whatever its safety settings say."
		st.Yes = "Yes, add it to " + n
	case o.Action == FleetBulkUnrule:
		st.What = "The rule " + visibleText(o.Pattern) + " is removed from " + n + ". What it matched then follows each machine's safety settings, which may let your agent do it without asking you."
		st.Yes = "Yes, remove it from " + n
	case o.Action == FleetBulkApply:
		st.What = "The fleet's rules, then the rules of each machine's groups in name order, are laid over the approval settings of " + n + ". A rule of a machine's own that one of them covers is replaced; safety settings stay as they are."
		st.Yes = "Yes, apply to " + n
	case o.Action == FleetBulkCopy:
		st.What = "The approval settings of " + n + " are replaced by a fresh copy of this Tenant's, with the fleet's rules and their groups' rules. Anything set or allowed for one of them alone goes."
		st.Yes, st.Danger = "Yes, copy again on "+n, true
	}
	d.Step = st
	s.fleetAnswer(w, r, d, "", "")
}

// fleetBulkDo does the change, or starts the rollout, on the machines the
// server resolved, and answers with what happened to each.
func (s *Server) fleetBulkDo(w http.ResponseWriter, r *http.Request, d fleetPageData, req fleetBulkReq) {
	ids := make([]string, 0, len(req.machines))
	for _, m := range req.machines {
		ids = append(ids, m.ID)
	}
	if req.update {
		// The rollout reports per machine on the page; one the fleet doesn't
		// have is said here.
		if len(ids) == 0 {
			s.fleetDone(w, r, "", "", fleetGone)
			return
		}
		if err := s.fleet.StartRollout(ids, req.version, req.arch, req.allowOlder, req.atOnce); err != nil {
			s.fleetDone(w, r, "", "", s.flashFor(r.Context(), "fleet rollout", err, "Couldn't start the rollout. Nothing was sent."))
			return
		}
		msg := "The rollout started: " + plural(len(ids), "machine") + ", " + strconv.Itoa(req.atOnce) + " at a time. Reload this page to see how far it has got."
		warn := ""
		if n := len(req.unknown); n > 0 {
			warn = msg + " " + plural(n, "machine") + " named aren't part of this fleet and were left out."
		}
		s.fleetAnswer(w, r, s.fleetData(r, ""), msg, warn)
		return
	}
	var results []FleetBulkResult
	if len(ids) > 0 {
		results = s.fleet.Bulk(req.op, ids)
	}
	reply := &fleetBulkReply{}
	done := 0
	for i, id := range ids {
		res := FleetBulkResult{ID: id, Said: "was not tried: the change stopped before it"}
		if j := slices.IndexFunc(results, func(x FleetBulkResult) bool { return x.ID == id }); j >= 0 {
			res = results[j]
		}
		if res.Done {
			done++
			continue
		}
		reply.Lines = append(reply.Lines, fleetBulkLine{orElse(req.machines[i].Name, res.Name), orElse(res.Said, "was not changed")})
	}
	for _, id := range req.unknown {
		reply.Lines = append(reply.Lines, fleetBulkLine{id, "isn't part of this fleet"})
	}
	total := len(ids) + len(req.unknown)
	reply.Head = fmt.Sprintf("Done on %d of %d.", done, total)
	if d.Group != nil {
		d = s.fleetGroupData(r, d.Group.Name)
	} else {
		d = s.fleetData(r, "")
	}
	d.Result = reply
	if done == total {
		s.fleetAnswer(w, r, d, reply.Head, "")
		return
	}
	s.fleetAnswer(w, r, d, "", reply.Head+" "+plural(len(reply.Lines), "machine")+" not changed: the page lists why.")
}

// fleetAnswer is the answer to a change to many machines and its step. A
// Datastar post gets #fleet-body in place (with an alert toast when warn
// says not everything happened). A plain post gets the page itself, not a
// redirect: the per-machine lines don't fit in a flash.
func (s *Server) fleetAnswer(w http.ResponseWriter, r *http.Request, d fleetPageData, msg, warn string) {
	page := "/fleet"
	if d.Group != nil {
		page = d.Group.Href()
	}
	if r.Header.Get("Datastar-Request") != "" {
		s.formReply(w, r, page, msg, warn, func() (string, error) { return part(s.tmpl.fleet, "fleet-body", d) })
		return
	}
	d.Msg, d.Err = msg, warn
	s.render(w, s.tmpl.fleet, d)
}

// handleFleetRolloutStop is POST /fleet/rollout/stop: no step, it is the
// safe direction.
func (s *Server) handleFleetRolloutStop(w http.ResponseWriter, r *http.Request) {
	if !s.fleetChange(w, r) {
		return
	}
	if !s.fleet.StopRollout() {
		s.fleetDone(w, r, "", "", "No rollout is running. Nothing changed.")
		return
	}
	s.fleetDone(w, r, "", "The rollout starts no more machines. Those updating now finish, and this page shows how they end when you reload it.", "")
}
