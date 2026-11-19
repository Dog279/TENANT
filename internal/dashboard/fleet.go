package dashboard

// fleet.go is the Fleet page (TEN-533; docs/plans/gap6-fleet-plan.md, sections
// 2, 5 and 7): the machines this Tenant owns as a fleet master, each one's
// link, what it reports about itself, its memory and its approval settings,
// and the operator's changes to them: confirm, rename, revoke, the settings,
// and the drift view. cmd/tenant adapts the fleet to FleetControl (dashFleet),
// over the same operations as `tenant fleet` and /fleet.
//
// What a machine says about itself is another computer's text. The control
// hands it over already escaped to printable ASCII, cut short and in quotes
// the text cannot close; the page shows it in element content only, under a
// heading that says whose words they are, and never in an attribute or a
// Datastar expression (F1).
//
// Confirming a machine is the one step that lets the agent work on it, and
// the spec takes it from the local dashboard only. Here that is a browser
// signed in on the computer running Tenant (atThisComputer): the digits are
// not taken from the API key, through `tailscale serve` or a proxy, or from
// another address. The other forms follow the rest of the dashboard: any
// signed-in browser, with the confirm step where a change lets the agent do
// more or cannot be undone.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"tenant/internal/logs/catalog"
)

// FleetControl is the fleet as the dashboard reads and changes it. machine is
// always the id the master gave the machine, never its name.
type FleetControl interface {
	// View is the whole page's state in one read (called per GET).
	View() FleetView
	// Confirm makes a pending machine a confirmed one: code is the six digits
	// that machine shows on its own screen, set the category=mode pairs the
	// operator chose for it alone. The dashboard calls it from one place, for a
	// browser signed in on this computer (handleFleetConfirm).
	Confirm(machine, code string, set []string) error
	Rename(machine, name string) error
	// Revoke ends a machine; deleteSettings also removes its record and its
	// approval settings, where keeping them leaves both under its id.
	Revoke(machine string, deleteSettings bool) error
	SetPermission(machine, category, mode string) error
	SetRule(machine, pattern, mode string) error
	RemoveRule(machine, pattern string) (removed bool, err error)
	// Recopy replaces the machine's approval settings: from "master" with a
	// fresh copy of this Tenant's own, from "defaults" with the fleet's rules
	// laid over what it has.
	Recopy(machine, from string) error
	// Update starts sending the machine a newer Tenant over its link (`tenant
	// fleet update`): version "" is the latest release; allowOlder lets one
	// older than the machine runs go; arch names the machine's processor when
	// it is too old to say. It refuses at once what cannot start (offline,
	// not confirmed, held by another process); the rest shows in the
	// machine's Update.
	Update(machine, version, arch string, allowOlder bool) error
}

// The memory states of the spec's section 7 (cmd/tenant's memory* constants
// are the same strings). FleetMemoryUnknown comes with a detail saying why.
const (
	FleetMemoryUnknown  = ""
	FleetMemoryInStep   = "in_step"
	FleetMemoryBehind   = "behind"
	FleetMemoryNeedsKey = "needs_recovery_key"
)

// FleetView is the Fleet page in one struct.
type FleetView struct {
	Master bool   // this Tenant has a fleet
	Holder string // which process holds the fleet, in words
	Here   bool   // it is this one, which knows which machines are online
	Err    error  // why the machines can't be listed; the page says so plainly
	// NoFresh: what a machine confirmed now would start with can't be worked
	// out (the fleet's rules can't be read), so no machine has a drift to show
	// and none can be confirmed or given a fresh copy.
	NoFresh  bool
	Machines []FleetMachine
	Rules    []ToolRuleRow // the fleet's rules: what every newly confirmed machine starts with
}

// FleetMachine is one machine, render-ready. OS, User, Version, Sandbox and
// Ceiling are the machine's own words about itself, each already in quotes it
// cannot close; Remote and LastEnded are quoted the same way.
type FleetMachine struct {
	ID, Name    string
	State       string // pending | confirmed | paused | revoked
	Online      bool
	Link        string // its link in words; "" when another process holds the fleet
	Remote      string // the address it is connected from, when online
	LinkVersion int    // the link version it speaks, when online
	Calls       int    // tool calls in flight
	// LastEnded is why its last link ended or was not made, "" with nothing to say.
	LastEnded                                 string
	Certificate, Enrolled, Confirmed, Revoked string

	OS, User, Version, Sandbox, Ceiling string

	Memory       string // a FleetMemory state
	MemoryDetail string

	// HasScheme: it has approval settings that can be read. A confirmed
	// machine without them is refused every request.
	HasScheme bool
	Perms     []PermissionRow
	Rules     []ToolRuleRow
	// Drift is where its settings differ from a fresh copy of this Tenant's.
	Drift []FleetDrift
	// Start is what a pending machine would be confirmed with; nil when that
	// can't be worked out, and then it can't be confirmed.
	Start *FleetStart
	// Update is the machine's last update from this process; nil with none.
	Update *FleetUpdate
}

// FleetUpdate is how far a machine's update has got. Lines are what the
// operator is told, in order; what the machine said in them is quoted.
type FleetUpdate struct {
	From, To string
	// Stage is fetching, sending or applying while it runs; then done,
	// rolled_back, failed or refused.
	Stage             string
	Running           bool
	Lines             []string
	Started, Finished string
}

// Words is the stage as the page says it.
func (u FleetUpdate) Words() string {
	return strings.ReplaceAll(u.Stage, "_", " ")
}

// FleetStart is the approval settings a pending machine starts with.
type FleetStart struct {
	Kept  bool // kept under its id from the machine that had it before
	Perms []FleetStartPerm
	Rules []FleetStartRule
}

// FleetStartPerm is one category of a FleetStart; Note says where its mode
// came from when that is not simply this Tenant's own.
type FleetStartPerm struct {
	PermissionRow
	Note string
}

type FleetStartRule struct {
	ToolRuleRow
	Note string
}

// FleetDrift is one difference between a machine's approval settings and a
// fresh copy: a category (Key is its id, Title its name) or a rule (Key is
// its pattern). Have and Want are ask, allow or deny; a rule only one side
// has is "" on the other.
type FleetDrift struct {
	Rule       bool
	Key, Title string
	Have, Want string
	FleetRule  bool // the fresh copy's side is one of the fleet's rules
}

var (
	fleetModeLabel = map[string]string{"ask": "Ask first", "allow": "Allow", "deny": "Block"}
	fleetRuleLabel = map[string]string{"ask": "Ask first", "allow": "Don't ask", "deny": "Block"}
)

// Words says the difference so the operator can act on it, with the names the
// buttons on the same page carry.
func (d FleetDrift) Words() string {
	if !d.Rule {
		return "this machine is set to " + fleetModeLabel[d.Have] + "; a fresh copy would be " + fleetModeLabel[d.Want] + "."
	}
	out := ""
	switch {
	case d.Want == "":
		out = "only this machine has this rule (" + fleetRuleLabel[d.Have] + "); a fresh copy would not."
	case d.Have == "":
		out = "this machine doesn't have this rule; a fresh copy would (" + fleetRuleLabel[d.Want] + ")."
	default:
		out = "this machine's rule is " + fleetRuleLabel[d.Have] + "; a fresh copy's would be " + fleetRuleLabel[d.Want] + "."
	}
	if d.FleetRule {
		out += " It is one of the fleet's rules."
	}
	return out
}

// Href is the machine's own page. The id is escaped though the master makes
// it: a link must not be able to aim at another route (F1).
func (m FleetMachine) Href() string { return "/fleet/" + url.PathEscape(m.ID) }

// StateWords is the machine's state as the list shows it, and Pill its class.
func (m FleetMachine) StateWords() string {
	switch {
	case m.State == "pending":
		return "waiting for you"
	case m.State == "confirmed" && m.Online:
		return "online"
	case m.State == "confirmed" && m.Link != "":
		return "offline"
	}
	return m.State
}

func (m FleetMachine) Pill() string {
	switch m.StateWords() {
	case "online":
		return "on-tag"
	case "waiting for you":
		return "tag-new"
	case "revoked":
		return "act-errtag"
	}
	return "risk"
}

// MemoryWords is the machine's memory state in the spec's words.
func (m FleetMachine) MemoryWords() string {
	switch m.Memory {
	case FleetMemoryInStep:
		return "in step"
	case FleetMemoryBehind:
		return "behind"
	case FleetMemoryNeedsKey:
		return "needs recovery key"
	}
	return "unknown"
}

// Approvals sums up the machine's approval settings for the list.
func (m FleetMachine) Approvals() string {
	switch {
	case m.State == "pending":
		return "none until you confirm it"
	case !m.HasScheme && m.State == "revoked":
		return "none"
	case !m.HasScheme:
		return "none that can be read: every request to it is refused"
	}
	n := map[string]int{}
	for _, p := range m.Perms {
		n[p.Mode]++
	}
	out := fmt.Sprintf("%d ask first, %d allowed, %d blocked", n["ask"], n["allow"], n["deny"])
	if r := len(m.Rules); r == 1 {
		out += ", 1 rule"
	} else if r > 1 {
		out += fmt.Sprintf(", %d rules", r)
	}
	return out
}

// SetFleet installs the fleet control after construction (mirrors SetAccess).
func (s *Server) SetFleet(c FleetControl) { s.fleet = c }

// mountFleetSSR registers the Fleet page, its forms and its API row. Mounted
// unconditionally; handlers nil-guard on s.fleet.
func (s *Server) mountFleetSSR(mux *http.ServeMux) {
	mux.HandleFunc("GET /fleet", s.handleFleetPage)
	mux.HandleFunc("GET /fleet/{id}", s.handleFleetMachinePage)
	mux.HandleFunc("GET /api/fleet", s.handleFleetAPI)
	// Confirming has no second click: typing the machine's six digits is the
	// step. Revoking, replacing a machine's settings, and anything that lets
	// the agent do more on it ask first (DESIGN.md §5).
	mux.HandleFunc("POST /fleet/confirm", s.handleFleetConfirm)
	mux.HandleFunc("POST /fleet/rename", s.handleFleetRename)
	mux.HandleFunc("POST /fleet/revoke", needConfirm(nil, s.handleFleetRevoke))
	mux.HandleFunc("POST /fleet/permission", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleFleetPermission))
	mux.HandleFunc("POST /fleet/rules/add", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleFleetRuleAdd))
	mux.HandleFunc("POST /fleet/rules/remove", needConfirm(func(r *http.Request) bool {
		return s.fleetRuleMode(r.FormValue("machine"), strings.TrimSpace(r.FormValue("pattern"))) != "allow"
	}, s.handleFleetRuleRemove))
	mux.HandleFunc("POST /fleet/scheme", needConfirm(nil, s.handleFleetScheme))
	// An update restarts the machine's Tenant and ends its background
	// commands: it asks first.
	mux.HandleFunc("POST /fleet/update", needConfirm(nil, s.handleFleetUpdate))
}

// The page's fixed words.
const (
	fleetGone       = "That machine isn't part of this fleet. Nothing changed."
	fleetUnreadable = "The list of machines can't be read right now. Until it can, no machine is called. The Logs page has the reason."
	// fleetNotHere is the 403 a confirmation gets from anywhere but a browser
	// signed in on the computer running Tenant.
	fleetNotHere = "Nothing changed: a machine is confirmed in a browser on the computer running Tenant, signed in to the dashboard at its 127.0.0.1 or localhost address, or in the terminal."
)

type fleetPageData struct {
	layoutData
	Configured bool
	View       FleetView
	Problem    string        // the plain line when the machines can't be listed
	One        *FleetMachine // a machine's own page; nil is the list
	Local      bool          // this browser may confirm a machine (atThisComputer)
	Msg, Err   string
}

// fleetData reads the page's state; id picks one machine's page.
func (s *Server) fleetData(r *http.Request, id string) fleetPageData {
	d := fleetPageData{layoutData: layoutData{Title: "Fleet", Page: "fleet"}}
	if s.fleet == nil {
		return d
	}
	d.Configured, d.View, d.Local = true, s.fleet.View(), s.atThisComputer(r)
	if d.View.Err != nil {
		d.Problem = s.flashFor(r.Context(), "fleet list", d.View.Err, fleetUnreadable)
	}
	if i := slices.IndexFunc(d.View.Machines, func(m FleetMachine) bool { return m.ID == id }); id != "" && i >= 0 {
		d.One = &d.View.Machines[i]
	}
	return d
}

func (s *Server) handleFleetPage(w http.ResponseWriter, r *http.Request) {
	d := s.fleetData(r, "")
	d.Msg, d.Err = r.URL.Query().Get("msg"), r.URL.Query().Get("err")
	s.render(w, s.tmpl.fleet, d)
}

func (s *Server) handleFleetMachinePage(w http.ResponseWriter, r *http.Request) {
	d := s.fleetData(r, r.PathValue("id"))
	if d.Configured && d.View.Master && d.View.Err == nil && d.One == nil {
		redirectFlash(w, r, "/fleet", "", fleetGone)
		return
	}
	d.Msg, d.Err = r.URL.Query().Get("msg"), r.URL.Query().Get("err")
	s.render(w, s.tmpl.fleet, d)
}

// fleetDone answers a Fleet form: in place for a Datastar post, with
// #fleet-body re-rendered; a 303 with the flash otherwise. id is the machine
// whose page the form is on: while the fleet still has it the answer is that
// page, else the list.
func (s *Server) fleetDone(w http.ResponseWriter, r *http.Request, id, msg, errMsg string) {
	d := s.fleetData(r, id)
	page := "/fleet"
	if d.One != nil {
		page = d.One.Href()
	}
	s.formDone(w, r, page, msg, errMsg, func() (string, error) { return part(s.tmpl.fleet, "fleet-body", d) })
}

// fleetMachine is the machine a form names in its machine field, as the fleet
// lists it now. Nothing a form sends about a machine is used but its id, and
// that only once it is one of the fleet's.
func (s *Server) fleetMachine(r *http.Request) (FleetMachine, bool) {
	id := r.FormValue("machine")
	for _, m := range s.fleet.View().Machines {
		if m.ID == id && id != "" {
			return m, true
		}
	}
	return FleetMachine{}, false
}

// atThisComputer reports whether r comes from a browser signed in on the
// computer running Tenant: the narrowest reading of the spec's "local
// dashboard", which is where a fleet machine may be confirmed. All of:
//
//   - the connection comes from a loopback address;
//   - the request names the dashboard by a loopback name (127.0.0.1,
//     localhost, [::1]), not a tailnet or proxy name the dashboard also answers to;
//   - no proxy passed it on: `tailscale serve` and a reverse proxy on this
//     computer connect from loopback too, and say so in their headers;
//   - it carries a live sign-in session. The API key (Authorization: Bearer)
//     is what a script holds, not a person at a browser, and does not count.
//
// A browser on another computer that reaches the loopback port through an SSH
// tunnel passes: it is as local as the command line run over that SSH.
func (s *Server) atThisComputer(r *http.Request) bool {
	host, _, ok := splitAuthority(r.Host)
	peer := net.ParseIP(remoteIP(r.RemoteAddr))
	if !ok || !isLoopbackHost(host) || peer == nil || !peer.IsLoopback() || !s.validCookie(r) {
		return false
	}
	for name := range r.Header {
		name = strings.ToLower(name)
		if name == "forwarded" || name == "via" || strings.HasPrefix(name, "x-forwarded-") || strings.HasPrefix(name, "x-real-") ||
			strings.HasPrefix(name, "tailscale-") || strings.HasPrefix(name, "cf-") {
			return false
		}
	}
	return true
}

// confirmRefused files the Security record of a confirmation the dashboard
// turned away itself (wrong digits are the control's to record).
func confirmRefused(r *http.Request, m FleetMachine, actor, reason string) {
	_ = catalog.FleetConfirmRefused.Audit(auditCtx(r), "name", m.Name, "machine", m.ID, "reason", reason, "actor", actor, "surface", "dashboard")
}

// handleFleetConfirm is POST /fleet/confirm (machine, code, set…): the six
// digits a pending machine shows, typed by a person signed in on this
// computer. It is the dashboard's only way to confirm a machine. Attempts are
// taken one at a time and slowed per machine as sign-in is per address: after
// loginFreeTries failures each further one doubles a wait, up to loginMaxWait.
func (s *Server) handleFleetConfirm(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	// The page's own form, sent by the browser that shows it: secure() checked
	// the Origin of a cookie's request already, and this route takes no other.
	if !s.atThisComputer(r) || r.Header.Get("Origin") == "" || !s.checkOrigin(r) {
		confirmRefused(r, m, r.RemoteAddr, "not asked from a browser signed in on the computer running Tenant")
		http.Error(w, fleetNotHere, http.StatusForbidden)
		return
	}
	if !found {
		s.fleetDone(w, r, "", "", fleetGone)
		return
	}
	s.confirmMu.Lock()
	defer s.confirmMu.Unlock()
	// ponytail: the counts live in sign-in's table, which starts again when it
	// passes 10,000 addresses. Its own table if that is ever worth an attack: a
	// guess still needs a browser signed in on this computer.
	tries := "fleet confirm " + m.ID // no address reads like it
	if wait := s.loginWait(tries); wait > 0 {
		confirmRefused(r, m, "operator", "too many failed attempts")
		in := "a second"
		if wait > time.Second {
			in = fmt.Sprintf("%d seconds", int((wait+time.Second-1)/time.Second))
		}
		s.fleetDone(w, r, m.ID, "", "Too many wrong codes for this machine. Try again in "+in+".")
		return
	}
	var set []string
	for _, pair := range r.Form["set"] { // a category left as shown sends ""
		if pair != "" {
			set = append(set, pair)
		}
	}
	if err := s.fleet.Confirm(m.ID, r.FormValue("code"), set); err != nil {
		s.loginFailed(tries)
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet confirm", err, "Couldn't confirm that machine. Nothing changed."))
		return
	}
	s.loginOK(tries)
	s.fleetDone(w, r, m.ID, m.Name+" is confirmed. Your agent can work on it now, as its approval settings say.", "")
}

func (s *Server) handleFleetRename(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	name := strings.TrimSpace(r.FormValue("name"))
	switch {
	case !found:
		s.fleetDone(w, r, "", "", fleetGone)
		return
	case name == "":
		s.fleetDone(w, r, m.ID, "", "Type the machine's new name.")
		return
	}
	if err := s.fleet.Rename(m.ID, name); err != nil {
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet rename", err,
			"Couldn't rename that machine. A name is up to 63 letters, digits, dots, dashes or underscores, starts with a letter or a digit, and isn't another machine's."))
		return
	}
	s.fleetDone(w, r, m.ID, "Renamed. Its approval settings and everything else stay with it.", "")
}

// handleFleetRevoke is POST /fleet/revoke (machine, settings=keep|delete).
func (s *Server) handleFleetRevoke(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	what := r.FormValue("settings")
	switch {
	case !found:
		s.fleetDone(w, r, "", "", fleetGone)
		return
	case what != "keep" && what != "delete":
		s.fleetDone(w, r, m.ID, "", "Choose whether its approval settings are kept or deleted. Nothing changed.")
		return
	}
	if err := s.fleet.Revoke(m.ID, what == "delete"); err != nil {
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet revoke", err, "Couldn't finish revoking that machine. Open its page to see where it stands; the Logs page has the reason."))
		return
	}
	if what == "delete" {
		s.fleetDone(w, r, m.ID, m.Name+" is revoked: it can no longer connect, and its record and approval settings are deleted.", "")
		return
	}
	s.fleetDone(w, r, m.ID, m.Name+" is revoked: it can no longer connect. Its approval settings are kept under its id.", "")
}

// handleFleetPermission is POST /fleet/permission (machine, category, mode):
// one safety setting of one machine. Allow passed needConfirm.
func (s *Server) handleFleetPermission(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	if !found {
		s.fleetDone(w, r, "", "", fleetGone)
		return
	}
	cat, mode := r.FormValue("category"), strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	i := slices.IndexFunc(m.Perms, func(p PermissionRow) bool { return p.Category == cat })
	switch {
	case modeWords[mode] == "":
		s.fleetDone(w, r, m.ID, "", "Pick Ask first, Allow or Block.")
		return
	case i < 0:
		s.fleetDone(w, r, m.ID, "", "That safety check doesn't exist for this machine. Nothing changed.")
		return
	}
	if err := s.fleet.SetPermission(m.ID, cat, mode); err != nil {
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet permission", err, "Couldn't change that safety setting."))
		return
	}
	s.fleetDone(w, r, m.ID, "On "+m.Name+": "+m.Perms[i].Title+" "+modeWords[mode]+".", "")
}

func (s *Server) handleFleetRuleAdd(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	pattern, mode := strings.TrimSpace(r.FormValue("pattern")), strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	switch {
	case !found:
		s.fleetDone(w, r, "", "", fleetGone)
		return
	case pattern == "" || ruleWords[mode] == "":
		s.fleetDone(w, r, m.ID, "", "A rule needs a tool name or pattern, and Ask first, Don't ask or Block.")
		return
	}
	if err := s.fleet.SetRule(m.ID, pattern, mode); err != nil {
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet rule add", err, "Couldn't add that rule. Check the tool name or pattern."))
		return
	}
	s.fleetDone(w, r, m.ID, "Rule added for "+m.Name+": what it matches "+ruleWords[mode]+".", "")
}

// fleetRuleMode is the mode of a machine's rule for pattern, "" with none:
// removing a rule asks first unless it let the tool go ahead, decided from
// the rules themselves, never from what the form says the rule was.
func (s *Server) fleetRuleMode(machine, pattern string) string {
	if s.fleet == nil {
		return ""
	}
	for _, m := range s.fleet.View().Machines {
		if m.ID != machine {
			continue
		}
		for _, rule := range m.Rules {
			if rule.Pattern == pattern {
				return strings.ToLower(rule.Mode)
			}
		}
	}
	return ""
}

func (s *Server) handleFleetRuleRemove(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	if !found {
		s.fleetDone(w, r, "", "", fleetGone)
		return
	}
	removed, err := s.fleet.RemoveRule(m.ID, strings.TrimSpace(r.FormValue("pattern")))
	switch {
	case err != nil:
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet rule remove", err, "Couldn't remove that rule."))
	case !removed:
		s.fleetDone(w, r, m.ID, "", "This machine has no such rule. Nothing changed.")
	default:
		s.fleetDone(w, r, m.ID, "Rule removed for "+m.Name+".", "")
	}
}

// handleFleetScheme is POST /fleet/scheme (machine, from=master|defaults):
// the drift view's two actions.
func (s *Server) handleFleetScheme(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	from := r.FormValue("from")
	switch {
	case !found:
		s.fleetDone(w, r, "", "", fleetGone)
		return
	case from != "master" && from != "defaults":
		s.fleetDone(w, r, m.ID, "", "Choose Copy again or Apply fleet rules. Nothing changed.")
		return
	}
	if err := s.fleet.Recopy(m.ID, from); err != nil {
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet scheme", err, "Couldn't replace that machine's approval settings. Nothing changed."))
		return
	}
	if from == "master" {
		s.fleetDone(w, r, m.ID, m.Name+" has a fresh copy of this Tenant's approval settings.", "")
		return
	}
	s.fleetDone(w, r, m.ID, "The fleet's rules are laid over the approval settings of "+m.Name+" again.", "")
}

// handleFleetUpdate is POST /fleet/update (machine, version, allow_older=yes,
// arch): start updating a machine over its link. What can be refused at once
// is the flash; the rest is the machine's Update on its page and in the API.
func (s *Server) handleFleetUpdate(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	if !found {
		s.fleetDone(w, r, "", "", fleetGone)
		return
	}
	err := s.fleet.Update(m.ID, strings.TrimSpace(r.FormValue("version")), strings.TrimSpace(r.FormValue("arch")), r.FormValue("allow_older") == "yes")
	if err != nil {
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet update", err, "Couldn't start updating that machine. Nothing was sent."))
		return
	}
	s.fleetDone(w, r, m.ID, "Updating "+m.Name+": this page shows how far it has got when you reload it.", "")
}

// --- GET /api/fleet ---

// restFleet is the Fleet page as JSON: read-only. The operator's changes are
// the page's forms, as Access and Approvals & safety are; a confirmation is
// never taken with the API key.
type restFleet struct {
	Master   bool               `json:"master"`
	Holder   string             `json:"holder,omitempty"`
	HeldHere bool               `json:"held_here"`
	Error    string             `json:"error,omitempty"`
	Machines []restFleetMachine `json:"machines"`
	Rules    map[string]string  `json:"fleet_rules,omitempty"`
}

type restFleetMachine struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Online      bool   `json:"online"`
	Link        string `json:"link,omitempty"`
	LinkVersion int    `json:"link_version,omitempty"`
	Calls       int    `json:"calls_in_flight,omitempty"`
	Certificate string `json:"certificate"`
	Enrolled    string `json:"enrolled,omitempty"`
	Confirmed   string `json:"confirmed,omitempty"`
	Revoked     string `json:"revoked,omitempty"`
	// Reported is the machine's own words, each value escaped and in quotes
	// as the page shows it.
	Reported     restFleetReported `json:"reported_by_the_machine"`
	Memory       string            `json:"memory"` // in_step | behind | needs_recovery_key | not_available
	MemoryDetail string            `json:"memory_detail,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
	ToolRules    map[string]string `json:"tool_rules,omitempty"`
	Drift        []restFleetDrift  `json:"drift,omitempty"`
	Update       *restFleetUpdate  `json:"update,omitempty"`
}

// restFleetUpdate is a machine's last update from this process.
type restFleetUpdate struct {
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Stage    string   `json:"stage"` // fetching | sending | applying | done | rolled_back | failed | refused
	Running  bool     `json:"running"`
	Lines    []string `json:"lines"`
	Started  string   `json:"started"`
	Finished string   `json:"finished,omitempty"`
}

type restFleetReported struct {
	OS      string `json:"os"`
	User    string `json:"user"`
	Version string `json:"version"`
	Sandbox string `json:"sandbox"`
	Ceiling string `json:"ceiling"`
}

type restFleetDrift struct {
	Category string `json:"category,omitempty"`
	Rule     string `json:"rule,omitempty"`
	Have     string `json:"have,omitempty"`
	Want     string `json:"want,omitempty"`
}

func (s *Server) handleFleetAPI(w http.ResponseWriter, r *http.Request) {
	out := restFleet{Machines: []restFleetMachine{}}
	if s.fleet == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	v := s.fleet.View()
	out.Master, out.Holder, out.HeldHere, out.Rules = v.Master, v.Holder, v.Here, ruleMap(v.Rules)
	if v.Err != nil {
		out.Error = s.flashFor(r.Context(), "fleet list", v.Err, fleetUnreadable)
	}
	for _, m := range v.Machines {
		rm := restFleetMachine{
			ID: m.ID, Name: m.Name, State: m.State, Online: m.Online, Link: m.Link, LinkVersion: m.LinkVersion, Calls: m.Calls,
			Certificate: m.Certificate, Enrolled: m.Enrolled, Confirmed: m.Confirmed, Revoked: m.Revoked,
			Reported: restFleetReported{m.OS, m.User, m.Version, m.Sandbox, m.Ceiling},
			Memory:   m.Memory, MemoryDetail: m.MemoryDetail, ToolRules: ruleMap(m.Rules),
		}
		if u := m.Update; u != nil {
			rm.Update = &restFleetUpdate{From: u.From, To: u.To, Stage: u.Stage, Running: u.Running, Lines: u.Lines, Started: u.Started, Finished: u.Finished}
		}
		if rm.Memory == FleetMemoryUnknown {
			rm.Memory = "not_available"
		}
		for _, p := range m.Perms {
			if rm.Permissions == nil {
				rm.Permissions = map[string]string{}
			}
			rm.Permissions[p.Category] = p.Mode
		}
		for _, d := range m.Drift {
			rd := restFleetDrift{Have: d.Have, Want: d.Want}
			if d.Rule {
				rd.Rule = d.Key
			} else {
				rd.Category = d.Key
			}
			rm.Drift = append(rm.Drift, rd)
		}
		out.Machines = append(out.Machines, rm)
	}
	writeJSON(w, http.StatusOK, out)
}

func ruleMap(rules []ToolRuleRow) map[string]string {
	if len(rules) == 0 {
		return nil
	}
	out := make(map[string]string, len(rules))
	for _, r := range rules {
		out[r.Pattern] = r.Mode
	}
	return out
}
