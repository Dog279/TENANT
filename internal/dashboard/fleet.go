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
// another address. The operator can widen that at the master's terminal
// (fleet.remote_confirm, TEN-545; DESIGN.md §5): a signed-in browser
// elsewhere may then confirm a machine by giving the sign-in key again. A
// request that carries only the API key still confirms nothing; that keeps a
// script from confirming by accident, and is no boundary against whoever
// holds the key, which is the sign-in key too. The other forms follow the rest
// of the dashboard: any signed-in browser, with the confirm step where a change lets
// the agent do more or cannot be undone.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"tenant/internal/logs/catalog"
)

// FleetControl is the fleet as the dashboard reads and changes it. machine is
// always the id the master gave the machine, never its name.
type FleetControl interface {
	// View is the whole page's state in one read (called per GET).
	View() FleetView
	// Init makes this Tenant a fleet master (`tenant fleet init`), all but the
	// recovery key, which is made at a terminal: listen is where the fleet port
	// binds ("" keeps what is set), addresses what its machines dial (none
	// keeps the list, or guesses a first one). by says who asked, for the
	// Security record. On a Tenant that is a master already it replaces
	// nothing. The dashboard calls it from one place, for a signed-in browser
	// (handleFleetInit).
	Init(listen string, addresses []string, by string) error
	// Confirm makes a pending machine a confirmed one: code is the six digits
	// that machine shows on its own screen, set the category=mode pairs the
	// operator chose for it alone. by is "" for a browser signed in on this
	// computer; else it says which signed-in browser elsewhere asked, for the
	// Security record, and the control confirms only while the operator's
	// setting allows that (FleetView.RemoteConfirm). The dashboard calls it
	// from one place (handleFleetConfirm).
	Confirm(machine, code string, set []string, by string) error
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
	// it is too old to say. build, when it is not "", is the full path of an
	// archive on this Tenant's computer to send in place of a release: a
	// build of the operator's own (`--build`). It refuses at once what cannot
	// start (offline, not confirmed, held by another process); the rest shows
	// in the machine's Update.
	Update(machine, version, arch, build string, allowOlder bool) error
	// Recover reads the machine's memory from its newest copy made under
	// another fleet key, with identity, the fleet's recovery identity the
	// operator brought (`tenant fleet recover`), and makes it the machine's
	// memory here; was is the id the machine had when the copy was made, ""
	// for its own. identity is zeroed when Recover returns. It says what it
	// did.
	Recover(machine, was string, identity []byte) (string, error)
	// Enroll makes the one-time code that adds a machine as name (`tenant
	// fleet enroll`), with the lines to paste on it: boot for a child that
	// runs as root or SYSTEM from boot, else as the user. by says who asked,
	// for the Security record, which never holds the code. The dashboard calls
	// it from one place, for a signed-in browser (handleFleetAdd).
	Enroll(name string, boot bool, by string) (FleetEnrollment, error)

	// The fleet's defaults, its groups, changes to many machines and the
	// rollout (TEN-546). The dashboard calls each from a signed-in browser
	// only, after its confirm step where one is due (fleet_bulk.go). None of
	// the defaults' or the groups' methods moves a machine: a machine's own
	// settings change only through Bulk (and the single-machine methods
	// above), each change audited once per machine.

	// SetDefault sets what a newly confirmed machine starts with for one
	// safety category (settings key fleet_permissions): mode is ask, allow or
	// deny, or "" to unset it, so that a new machine takes this Tenant's own
	// mode for it again. It refuses a category that is not
	// one, and refuses while fleet_permissions cannot be read (the text stays
	// as written). It moves no machine.
	SetDefault(category, mode string) error
	// SetDefaultRule adds or replaces one of the fleet's rules (fleet_rules),
	// pattern -> ask, allow or deny, as `tenant fleet permissions --defaults
	// add` does; RemoveDefaultRule removes one and reports whether there was
	// one. Neither moves a machine.
	SetDefaultRule(pattern, mode string) error
	RemoveDefaultRule(pattern string) (removed bool, err error)
	// CreateGroup makes an empty group. A name is 1 to 63 of [A-Za-z0-9._-],
	// starts with a letter or a digit, is not a machine id, is not "all", and
	// is unique case-insensitively; anything else is a PlainError that says
	// so. DeleteGroup removes a group with its rules and memberships; its
	// members' settings stay as they are. group is matched case-insensitively.
	CreateGroup(name string) error
	DeleteGroup(group string) error
	// AddMembers and RemoveMembers put machines (by id, each one the fleet
	// has, in any state) in a group or take them out. Adding one that is in
	// it already, or removing one that is not, is no error. They move no
	// machine.
	AddMembers(group string, machines []string) error
	RemoveMembers(group string, machines []string) error
	// SetGroupRule adds or replaces a rule of a group (the same patterns and
	// modes as the fleet's rules); RemoveGroupRule removes one and reports
	// whether there was one. They move no machine: Apply (Bulk with
	// FleetBulkApply over the members) is the step that does.
	SetGroupRule(group, pattern, mode string) error
	RemoveGroupRule(group, pattern string) (removed bool, err error)
	// Bulk runs one operation on each machine in machines (ids the dashboard
	// has checked against View), through the same single-machine code the
	// methods above use, so each machine gets its own audit record. It never
	// stops at the first failure: it returns one result per machine, in the
	// order given, saying whether it was done and, when not, why in a plain
	// sentence (pending, revoked, no approval settings that can be read, no
	// such category, the store's refusal...). A machine that does not
	// qualify is a result, not an error.
	Bulk(op FleetBulkOp, machines []string) []FleetBulkResult
	// StartRollout updates machines (ids, in that order) to version ("" is
	// the latest release), how many at once limit says (1 to 8), as
	// `tenant fleet update` does for each: arch names the processor of a
	// machine too old to say, allowOlder lets an older version go. It
	// refuses at once, as a PlainError, a version or arch that is not one, a
	// limit outside 1 to 8, a rollout that is running already, and a process
	// that does not hold the fleet (with Update's words). The rest shows in
	// FleetView.Rollout: the first machine that rolls back or fails stops it.
	StartRollout(machines []string, version, arch string, allowOlder bool, limit int) error
	// StopRollout asks the running rollout to start no more machines; those
	// in flight finish. It reports whether one was running.
	StopRollout() bool
}

// The operations Bulk runs on each machine (FleetBulkOp.Action).
const (
	FleetBulkPermission = "permission" // set Category to Mode (ask, allow, deny)
	FleetBulkRule       = "rule"       // add or replace the rule Pattern -> Mode
	FleetBulkUnrule     = "unrule"     // remove the rule Pattern
	FleetBulkApply      = "apply"      // lay the fleet's rules, then its groups' rules in name order, over its settings
	FleetBulkCopy       = "copy"       // replace its settings with a fresh copy from this Tenant
)

// FleetBulkOp is the one operation a Bulk call runs on every machine. Only
// the fields its Action reads are set.
type FleetBulkOp struct {
	Action            string
	Category, Pattern string
	Mode              string // ask | allow | deny
}

// FleetBulkResult is what Bulk did to one machine: Done, or Said, the plain
// sentence that says why not. Name is the machine's name as the page shows
// names (already escaped by the control).
type FleetBulkResult struct {
	ID, Name string
	Done     bool
	Said     string
}

// FleetEnrollment is a code just made, render-ready. Code is a secret while
// it lives: it is in the answer to the request that made it and nowhere else,
// not in a flash, a URL, the page's state or a log.
type FleetEnrollment struct {
	Name, Code string
	Expires    string // when the code stops working, as the page shows a time
	Boot       bool
	Addresses  []string // what the code tells the machine to dial
	// Digits: the machine will wait for its six digits to be typed
	// (fleet.confirm: digits). False: the code confirms it (TEN-553).
	Digits bool
	// Warning is the plain warning when none of them can be dialled from
	// another site; "" with none.
	Warning string
	Lines   []FleetPasteLine
	// Note says why the line that installs Tenant too is missing; "" when it
	// is there.
	Note string
}

// FleetPasteLine is one line to paste on the new machine, and where it runs.
type FleetPasteLine struct{ For, Line string }

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
	Master bool // this Tenant has a fleet
	// Listen and Dial are what Set up the fleet starts from, on a Tenant with
	// no fleet: where the fleet port would bind, and the addresses a new
	// machine would be told to dial, separated by commas.
	Listen, Dial string
	// NoRecovery: the fleet has no recovery key, so no machine's memory is
	// kept. It is made at a terminal, never on this page.
	NoRecovery bool
	Holder     string // which process holds the fleet, in words
	Here       bool   // it is this one, which knows which machines are online
	Err        error  // why the machines can't be listed; the page says so plainly
	// NoFresh: what a machine confirmed now would start with can't be worked
	// out (the fleet's rules can't be read), so no machine has a drift to show
	// and none can be confirmed or given a fresh copy.
	NoFresh bool
	// RemoteConfirm: the operator let a signed-in browser that is not on this
	// computer confirm a machine, with the sign-in key again
	// (fleet.remote_confirm, off unless set at the master's terminal).
	RemoteConfirm bool
	// ConfirmDigits: a machine that enrolls waits for its six digits to be
	// typed (fleet.confirm: digits). False, the default: its one-time code
	// confirms it (TEN-553).
	ConfirmDigits bool
	Machines      []FleetMachine
	Rules         []ToolRuleRow // the fleet's rules: what every newly confirmed machine starts with

	// Defaults are the fleet's default safety categories, one per category in
	// the order the page lists them: what a newly confirmed machine starts
	// with. nil when they can't be shown (not wired, or DefaultsErr).
	Defaults []FleetDefault
	// DefaultsErr: fleet_permissions can't be read. The page says so plainly
	// and offers no change to the categories; the reason goes to the log.
	DefaultsErr error
	// Groups are the fleet's groups in name order. GroupsErr: fleet_groups
	// can't be read, so no group can be shown, changed or applied until it is
	// fixed (the page says so; the reason goes to the log).
	Groups    []FleetGroup
	GroupsErr error
	// LayersKey names everything Apply fleet rules and Copy again lay, as it
	// is at this read: this Tenant's own settings, the defaults, the fleet's
	// rules and the groups. "" when it can't be worked out. A confirm step
	// carries it, and its second post is refused when it has changed.
	LayersKey string
	// Rollout is the current or last rollout, this process's own or the one
	// it found saved when it took the fleet; nil with none, and in a process
	// that does not hold the fleet.
	Rollout *FleetRollout
}

// FleetDefault is one safety category of the fleet's defaults. Mode is ask,
// allow or deny where the operator set it for new machines, "" where it is
// unset: then a new machine takes this Tenant's own mode, which
// Inherited says (ask, allow or deny, as of this read). Can is worded for a
// machine ("on this machine").
type FleetDefault struct {
	PermissionRow
	Inherited string
}

// FleetGroup is one group, render-ready. Name is the operator's own word, as
// written. Members are machine ids, in the order the control keeps them.
// Differ is how many of its confirmed members have settings that differ from
// what Apply would lay over them now; -1 when that can't be worked out.
type FleetGroup struct {
	Name    string
	Members []string
	Rules   []ToolRuleRow
	Differ  int
}

// Href is the group's own page. The name is escaped though the control checks
// it: a link must not be able to aim at another route (F1).
func (g FleetGroup) Href() string { return "/fleet/group/" + url.PathEscape(g.Name) }

// FleetRollout is a rollout as far as it has got. Times are as the page shows
// them ("" for none). Stopped is "" while it runs on its own and when it
// ended on its own, else why it stopped ("stopped by the operator", "stopped
// at <name>", "cut short when this Tenant stopped"). The process that holds
// the fleet saves it, and a Tenant started later shows it as it stood.
type FleetRollout struct {
	Version           string // as asked for; "" is the latest release
	Limit             int    // how many at once
	Started, Finished string
	Running           bool
	Stopped           string
	Machines          []FleetRolloutMachine // in the order they were given
}

// FleetRolloutMachine is one machine of a rollout. Status is waiting,
// running, done, skipped, rolled_back, failed, not_started or interrupted (its
// update was under way when the Tenant that ran the rollout stopped); Stage is
// its update's stage while it runs, and for an interrupted one what its update
// has come to since; Reason says why it ended as it did (it may hold what the
// machine said, quoted by the control).
type FleetRolloutMachine struct {
	ID, Name, Status, Stage, Reason string
}

// Href is the machine's own page.
func (m FleetRolloutMachine) Href() string { return "/fleet/" + url.PathEscape(m.ID) }

// Words is the machine's status as the rollout card says it.
func (m FleetRolloutMachine) Words() string {
	switch m.Status {
	case "running":
		if m.Stage != "" {
			return "running: " + strings.ReplaceAll(m.Stage, "_", " ")
		}
		return "running"
	case "not_started":
		return "not started"
	case "interrupted":
		switch m.Stage {
		case "done", "rolled_back":
			return "interrupted, then " + strings.ReplaceAll(m.Stage, "_", " ")
		}
	}
	return strings.ReplaceAll(m.Status, "_", " ")
}

// Pill is the class of the machine's status pill.
func (m FleetRolloutMachine) Pill() string {
	status := m.Status
	if status == "interrupted" && (m.Stage == "done" || m.Stage == "rolled_back") {
		status = m.Stage
	}
	switch status {
	case "done":
		return "on-tag"
	case "rolled_back", "failed":
		return "act-errtag"
	case "running", "waiting", "interrupted":
		return "risk"
	}
	return "tag"
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
	// Update is the machine's last update from this Tenant, this process's or
	// one saved by the process that held the fleet before it; nil with none.
	Update *FleetUpdate
	// Groups are the names of the groups it is in, in name order.
	Groups []string
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
	// Away is since when the machine has not redialled: it took the update,
	// never said how it went and has no link now. "" otherwise.
	Away string
}

// Words is the stage as the page says it.
func (u FleetUpdate) Words() string {
	return strings.ReplaceAll(u.Stage, "_", " ")
}

// Pill is the class of the stage's pill.
func (u FleetUpdate) Pill() string {
	switch {
	case u.Stage == "done":
		return "on-tag"
	case u.Running:
		return "risk"
	}
	return "act-errtag"
}

// Last is the last thing said of the update: how it ended, and why.
func (u FleetUpdate) Last() string {
	if len(u.Lines) == 0 {
		return ""
	}
	return u.Lines[len(u.Lines)-1]
}

// FleetStart is the approval settings a pending machine starts with.
type FleetStart struct {
	Kept  bool // kept under its id from the machine that had it before
	Perms []FleetStartPerm
	Rules []FleetStartRule
	// Key names these settings, so the confirm form can carry it: when it
	// comes back and the machine's start is no longer this one (a default, a
	// fleet rule or one of its groups was edited meanwhile), the page shows
	// the new start and asks again instead of confirming what nobody read.
	Key string
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
	// step (and, from a browser elsewhere, the sign-in key again). Revoking, replacing a machine's settings, and anything that lets
	// the agent do more on it ask first (DESIGN.md §5).
	mux.HandleFunc("POST /fleet/confirm", s.handleFleetConfirm)
	// Making this Tenant a master opens the fleet port and nothing else: no
	// machine joins without a code and its six digits. It needs a signed-in
	// browser, never the API key (handleFleetInit).
	mux.HandleFunc("POST /fleet/init", s.handleFleetInit)
	// A code joins nothing by itself: the machine that spends it waits until
	// its six digits are typed where a machine is confirmed (handleFleetConfirm).
	// Making one needs a signed-in browser, never the API key (handleFleetAdd).
	mux.HandleFunc("POST /fleet/add", s.handleFleetAdd)
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
	// The recovery identity reads every copy the fleet's machines hold: it
	// asks first, and it is the request's body, never a form field or a URL.
	mux.HandleFunc("POST /fleet/recover", needConfirm(nil, s.handleFleetRecover))
	// The fleet's defaults, groups, many machines at once, the rollout (TEN-546).
	s.mountFleetBulk(mux)
}

// The page's fixed words.
const (
	fleetGone = "That machine isn't part of this fleet. Nothing changed."
	// fleetStartChanged answers a confirmation whose form listed settings the
	// machine would no longer start with.
	fleetStartChanged = "Nothing changed: what this machine would start with was edited since this page was drawn (a default, a fleet rule or one of its groups). Read it again below, then confirm."
	fleetUnreadable   = "The list of machines can't be read right now. Until it can, no machine is called. The Logs page has the reason."
	// fleetNotHere is the 403 a confirmation gets from anywhere but a browser
	// signed in on the computer running Tenant, unless the operator turned
	// remote confirmation on and the request is a signed-in browser's.
	fleetNotHere = "Nothing changed: a machine is confirmed in a browser on the computer running Tenant, signed in to the dashboard at its 127.0.0.1 or localhost address, or in the terminal. A signed-in browser elsewhere can confirm one only after tenant fleet remote-confirm on is run in a terminal on that computer."
	// The step-up a browser elsewhere takes: the sign-in key again.
	fleetKeyWrong = "Nothing changed: that isn't the dashboard's sign-in key. From this browser a machine is confirmed with the key again, as the file dashboard-auth-token on the computer running Tenant holds it."
	fleetKeyInURL = "Nothing changed: the sign-in key goes in the form, never in the address. The key in that address may now be in your browser's history: to change it, delete the file dashboard-auth-token and restart Tenant."
	// surfaceRemote is the surface of a Security record this page files for a
	// confirmation asked from a browser that is not on this computer.
	surfaceRemote = "dashboard-remote"
	// fleetAddSignIn is the 403 a request to make a code gets without a
	// signed-in browser behind it.
	fleetAddSignIn = "No code was made: a code for a new machine is made from the Fleet page in a signed-in browser, or with tenant fleet enroll in a terminal on the computer running Tenant."
	// fleetInitSignIn is the 403 a request to make this Tenant a master gets
	// without a signed-in browser behind it.
	fleetInitSignIn = "Nothing changed: a fleet is set up from the Fleet page in a signed-in browser, or with tenant fleet init in a terminal on the computer running Tenant."
	fleetInitFailed = "The fleet couldn't be set up. The Logs page has the reason."
	fleetAddName    = "No code was made. A name is up to 63 letters, digits, dots, dashes or underscores, starts with a letter or a digit, doesn't look like a machine id (m and 16 hex digits), and isn't another machine's."
)

type fleetPageData struct {
	layoutData
	Configured bool
	View       FleetView
	Problem    string        // the plain line when the machines can't be listed
	One        *FleetMachine // a machine's own page; nil is the list
	Local      bool          // this browser may confirm a machine (atThisComputer)
	// Remote: this browser is elsewhere and may confirm a machine by giving
	// the sign-in key again (the operator turned remote confirmation on).
	Remote   bool
	Msg, Err string
	// Added is the code this request made, shown in this answer only.
	Added *FleetEnrollment

	// TEN-546 (fleet_bulk.go).
	Filter  string         // the list's group filter as asked (?group=); "" is every machine
	Shown   []FleetMachine // the machines the list shows: all, or the filter's group's members
	NoGroup bool           // the filter names no group the fleet has
	Group   *FleetGroup    // a group's own page; nil otherwise
	Members []FleetMachine // that group's members the fleet has, and Others the rest
	Others  []FleetMachine
	Strays  []string // member ids the fleet has no machine for
	// The plain lines when the defaults or the groups can't be read.
	DefaultsProblem, GroupsProblem string
	Step                           *fleetBulkStep  // the confirm step of a change to many machines, in place of the page
	Result                         *fleetBulkReply // what a change to many machines did, per machine
}

// fleetData reads the page's state; id picks one machine's page.
func (s *Server) fleetData(r *http.Request, id string) fleetPageData {
	d := fleetPageData{layoutData: layoutData{Title: "Fleet", Page: "fleet"}}
	if s.fleet == nil {
		return d
	}
	d.Configured, d.View, d.Local = true, s.fleet.View(), s.atThisComputer(r)
	d.Remote = !d.Local && d.View.RemoteConfirm && s.validCookie(r)
	if d.View.Err != nil {
		d.Problem = s.flashFor(r.Context(), "fleet list", d.View.Err, fleetUnreadable)
	}
	if i := slices.IndexFunc(d.View.Machines, func(m FleetMachine) bool { return m.ID == id }); id != "" && i >= 0 {
		d.One = &d.View.Machines[i]
	}
	if d.View.DefaultsErr != nil {
		d.DefaultsProblem = s.flashFor(r.Context(), "fleet defaults", d.View.DefaultsErr, fleetDefaultsBroken)
	}
	if d.View.GroupsErr != nil {
		d.GroupsProblem = s.flashFor(r.Context(), "fleet groups", d.View.GroupsErr, fleetGroupsUnreadable)
	}
	d.Filter = strings.TrimSpace(r.FormValue("group"))
	d.Shown, d.NoGroup = fleetShown(d.View, d.Filter)
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
// turned away itself (wrong digits are the control's to record). surface is
// "dashboard", or surfaceRemote for a browser that is not on this computer.
func confirmRefused(r *http.Request, m FleetMachine, actor, reason, surface string) {
	_ = catalog.FleetConfirmRefused.Audit(auditCtx(r), "name", m.Name, "machine", m.ID, "reason", reason, "actor", actor, "surface", surface)
}

// proxySaid is what a proxy on this computer says of who it passed on, for the
// record of a confirmation asked from elsewhere: behind `tailscale serve` or a
// reverse proxy every request arrives from loopback, and the address alone
// says nothing. It is the tailnet login the proxy names, else the address it
// forwards for: another party's words, cut short and in quotes they cannot
// close, marked as not checked. Nothing is decided by it. A request that did
// not come from this computer has no proxy here to speak for it: "".
func proxySaid(r *http.Request) string {
	if !fromLoopback(r) {
		return ""
	}
	said := r.Header.Get("Tailscale-User-Login")
	if said == "" {
		said = r.Header.Get("X-Forwarded-For")
	}
	if said == "" {
		return ""
	}
	if len(said) > 128 {
		said = said[:128]
	}
	return "; its proxy says " + strconv.QuoteToASCII(said) + " (not checked)"
}

// keyAgain is the step-up a browser that is not on this computer takes before
// it confirms a machine: the dashboard's sign-in key, in the body, compared as
// sign-in compares it and counted against the same per-address wait. A
// session someone stole or left open is then not enough. It returns what the
// page says and what the Security record says when the key does not pass, ""
// when it does.
func (s *Server) keyAgain(r *http.Request) (said, reason string) {
	key, inBody := bodyOnly(r, "key")
	wait := s.loginWait(r.RemoteAddr)
	switch {
	case !inBody:
		KeyInURL.Log(r.Context(), "cred", "dashboard sign-in key")
		return fleetKeyInURL, "the sign-in key was sent in the address"
	case wait > 0:
		SignInFailed.Log(r.Context(), "remote", r.RemoteAddr, "throttled", wait.String())
		return fmt.Sprintf("Nothing changed: too many wrong keys from this address. Try again in %d seconds.", int(wait/time.Second)+1),
			"too many wrong sign-in keys from this address"
	case s.cfg.Auth == "" || subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.Auth)) != 1:
		s.loginFailed(r.RemoteAddr)
		SignInFailed.Log(r.Context(), "remote", r.RemoteAddr)
		return fleetKeyWrong, "the sign-in key given again was wrong"
	}
	s.loginOK(r.RemoteAddr)
	return "", ""
}

// handleFleetConfirm is POST /fleet/confirm (machine, code, set…): the six
// digits a pending machine shows, typed by a person signed in on this
// computer. It is the dashboard's only way to confirm a machine. Attempts are
// taken one at a time and slowed per machine as sign-in is per address: after
// loginFreeTries failures each further one doubles a wait, up to loginMaxWait.
//
// Where the operator turned remote confirmation on (FleetView.RemoteConfirm,
// TEN-545), a signed-in browser that is not on this computer may send the
// form too, with what makes up for "at this computer": it must be the page's
// own form (a live session and its Origin, never the API key), and it gives
// the sign-in key again in a key field (keyAgain). The per-machine wait is the
// same, and every refusal and the confirmation itself are recorded as asked
// from elsewhere (surfaceRemote), with the session and the address, and what
// a proxy on this computer said of who it passed on (proxySaid).
//
// The key again stops a session without the key: a cookie that leaked, a
// browser left signed in. It does not stop whoever holds the key, who can sign
// in and send this form from anywhere the dashboard is reached (DESIGN.md §5).
func (s *Server) handleFleetConfirm(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	// The page's own form, sent by the browser that shows it: secure() checked
	// the Origin of a cookie's request already, and this route takes no other.
	by, actor, surface := "", "operator", "dashboard"
	if !s.atThisComputer(r) || r.Header.Get("Origin") == "" || !s.checkOrigin(r) {
		session, ok := s.signedInBrowser(r)
		if !ok || !s.fleet.View().RemoteConfirm {
			confirmRefused(r, m, r.RemoteAddr, "not asked from a browser signed in on the computer running Tenant", surface)
			http.Error(w, fleetNotHere, http.StatusForbidden)
			return
		}
		by, surface = "dashboard session "+sessionTag(session)+" from "+r.RemoteAddr+proxySaid(r), surfaceRemote
		actor = by
	}
	if !found {
		s.fleetDone(w, r, "", "", fleetGone)
		return
	}
	s.confirmMu.Lock()
	defer s.confirmMu.Unlock()
	if by != "" {
		if said, reason := s.keyAgain(r); said != "" {
			confirmRefused(r, m, actor, reason, surface)
			s.fleetDone(w, r, m.ID, "", said)
			return
		}
	}
	// What is confirmed is what the page showed (TEN-546): the form carries
	// the name of the settings it listed, and one of them may have been edited
	// since, a group's rules without any step. Then nothing is confirmed, no
	// attempt is counted, and the answer is the page as it reads now. A form
	// without the field is not checked: the page's own always has it.
	if shown := r.FormValue("start"); shown != "" && (m.Start == nil || m.Start.Key != shown) {
		d := s.fleetData(r, m.ID) // drawn again in place: the form now lists, and carries, what it would start with
		s.formWarn(w, r, m.Href(), fleetStartChanged, func() (string, error) { return part(s.tmpl.fleet, "fleet-body", d) })
		return
	}
	// ponytail: the counts live in sign-in's table, which starts again when it
	// passes 10,000 addresses. Its own table if that is ever worth an attack: a
	// guess still needs a browser signed in on this computer, or elsewhere with
	// the key.
	tries := "fleet confirm " + m.ID // no address reads like it
	if wait := s.loginWait(tries); wait > 0 {
		confirmRefused(r, m, actor, "too many failed attempts", surface)
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
	if err := s.fleet.Confirm(m.ID, r.FormValue("code"), set, by); err != nil {
		s.loginFailed(tries)
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet confirm", err, "Couldn't confirm that machine. Nothing changed."))
		return
	}
	s.loginOK(tries)
	s.fleetDone(w, r, m.ID, m.Name+" is confirmed. Your agent can work on it now, as its approval settings say.", "")
}

// signedInBrowser is the session of a request a signed-in browser sent from
// the dashboard's own page: a live sign-in cookie and the page's anti-forgery
// check (an Origin equal to the dashboard's). The API key is not one.
func (s *Server) signedInBrowser(r *http.Request) (session string, ok bool) {
	c, err := r.Cookie(dashCookieName)
	if err != nil || !s.validSession(c.Value) || r.Header.Get("Origin") == "" || !s.checkOrigin(r) {
		return "", false
	}
	return c.Value, true
}

// maxFleetAddresses bounds the addresses field before it is split.
const maxFleetAddresses = 2048

// handleFleetInit is POST /fleet/init (listen, addresses): Set up the fleet.
// It makes this Tenant a fleet master as `tenant fleet init` does, all but
// the recovery key: that key's identity is shown once and typed back at a
// terminal, and this page neither shows nor takes it, so until it is made
// there the page says no machine's memory is kept.
//
// It takes a signed-in browser's session only, as Add machine does: the API
// key makes no fleet. It opens the fleet port; no machine joins by it without
// a code and its six digits, typed where a machine is confirmed
// (handleFleetConfirm).
func (s *Server) handleFleetInit(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	session, ok := s.signedInBrowser(r)
	if !ok {
		refused(r, "fleet set up without a signed-in browser", "POST /fleet/init")
		http.Error(w, fleetInitSignIn, http.StatusForbidden)
		return
	}
	listen, dial := strings.TrimSpace(r.FormValue("listen")), r.FormValue("addresses")
	if len(listen) > 255 || len(dial) > maxFleetAddresses {
		s.fleetDone(w, r, "", "", "Nothing changed: that is too long for an address.")
		return
	}
	addresses := strings.FieldsFunc(dial, func(c rune) bool { return c == ',' || c == ';' || unicode.IsSpace(c) })
	if err := s.fleet.Init(listen, addresses, "dashboard session "+sessionTag(session)+" from "+r.RemoteAddr); err != nil {
		s.fleetDone(w, r, "", "", s.flashFor(r.Context(), "fleet init", err, fleetInitFailed))
		return
	}
	s.fleetDone(w, r, "", "This Tenant is a fleet master. Add the first machine below.", "")
}

// maxFleetName bounds the name field before it reaches the fleet, which
// takes 63 characters at most (the same check `tenant fleet enroll` makes).
const maxFleetName = 63

// handleFleetAdd is POST /fleet/add (name, runs=user|boot): Add machine. It
// makes the one-time code for a new machine and answers with the lines to
// paste on it, in this response and nowhere else: never a redirect, so the
// code is in no URL, no flash and no browser history entry, and it is not
// stored. The fleet's Security record says a code was made, for which name,
// by which session; it never holds the code.
//
// It takes a signed-in browser's session only, with the page's anti-forgery
// check (an Origin equal to the dashboard's): the API key makes no code. A
// code is the first half of a machine joining; the other half, the six
// digits, stays at this computer unless the operator turned remote
// confirmation on there (handleFleetConfirm).
func (s *Server) handleFleetAdd(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	session, ok := s.signedInBrowser(r)
	if !ok {
		refused(r, "fleet code without a signed-in browser", "POST /fleet/add")
		http.Error(w, fleetAddSignIn, http.StatusForbidden)
		return
	}
	name, runs := strings.TrimSpace(r.FormValue("name")), r.FormValue("runs")
	switch {
	case name == "" || len(name) > maxFleetName:
		s.fleetDone(w, r, "", "", fleetAddName)
		return
	case runs != "user" && runs != "boot":
		s.fleetDone(w, r, "", "", "Choose whether it runs as you or from boot. No code was made.")
		return
	}
	e, err := s.fleet.Enroll(name, runs == "boot", "dashboard session "+sessionTag(session)+" from "+r.RemoteAddr)
	if err != nil {
		s.fleetDone(w, r, "", "", s.flashFor(r.Context(), "fleet add", err, fleetAddName))
		return
	}
	d := s.fleetData(r, "")
	d.Added = &e
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("Datastar-Request") != "" {
		s.formReply(w, r, "/fleet", "A code for "+e.Name+" is below. It is shown this once.", "",
			func() (string, error) { return part(s.tmpl.fleet, "fleet-body", d) })
		return
	}
	s.render(w, s.tmpl.fleet, d)
}

// sessionTag names a signed-in session in a record without being its key:
// the first eight hex digits of its SHA-256.
func sessionTag(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
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
// arch, build): start updating a machine over its link. What can be refused
// at once is the flash; the rest is the machine's Update on its page and in
// the API. build is the command line's (`tenant fleet update --build`): the
// page has no field for it.
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
	err := s.fleet.Update(m.ID, strings.TrimSpace(r.FormValue("version")), strings.TrimSpace(r.FormValue("arch")), r.FormValue("build"), r.FormValue("allow_older") == "yes")
	if err != nil {
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet update", err, "Couldn't start updating that machine. Nothing was sent."))
		return
	}
	s.fleetDone(w, r, m.ID, "Updating "+m.Name+": this page shows how far it has got when you reload it.", "")
}

// maxRecoveryIdentity bounds the body of POST /fleet/recover: an identity
// is 74 characters, and some room for a line end or a pasted space.
const maxRecoveryIdentity = 1 << 10

// handleFleetRecover is POST /fleet/recover?machine=ID[&was=ID]&confirm=yes
// with the recovery identity as the whole body (`tenant fleet recover`). The
// machine and the confirmation are in the URL so that the body is never
// parsed as a form: the identity is read as bytes, handed on, and zeroed.
// It has no form on the page; the command line posts it.
func (s *Server) handleFleetRecover(w http.ResponseWriter, r *http.Request) {
	// The identity reads every machine's memory: it may cross only a
	// connection nobody on the way can read, from this computer or over TLS.
	// Anything else is refused before the body is read, and the refusal
	// says nothing of what was sent.
	if !fromLoopback(r) && r.TLS == nil {
		s.fleetDone(w, r, "", "", "Nothing was read: the recovery identity is taken only from this computer or over HTTPS, and this request came over plain HTTP from another computer, not this computer. Run `tenant fleet recover` on the master itself.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRecoveryIdentity))
	defer clear(body)
	if s.fleet == nil {
		s.fleetDone(w, r, "", "", "")
		return
	}
	m, found := s.fleetMachine(r)
	if !found {
		s.fleetDone(w, r, "", "", fleetGone)
		return
	}
	if err != nil || len(body) == 0 {
		s.fleetDone(w, r, m.ID, "", "Nothing was read: the recovery identity is the body of the request.")
		return
	}
	said, err := s.fleet.Recover(m.ID, strings.TrimSpace(r.URL.Query().Get("was")), body)
	if err != nil {
		s.fleetDone(w, r, m.ID, "", s.flashFor(r.Context(), "fleet recover", err, "Couldn't read that machine's memory. Nothing changed."))
		return
	}
	s.fleetDone(w, r, m.ID, "The memory of "+m.Name+": "+said+".", "")
}

// fromLoopback reports whether r's connection comes from this computer.
func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// restFleetUpdate is a machine's last update from this Tenant.
type restFleetUpdate struct {
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Stage    string   `json:"stage"` // fetching | sending | applying | done | rolled_back | failed | refused
	Running  bool     `json:"running"`
	Lines    []string `json:"lines"`
	Started  string   `json:"started"`
	Finished string   `json:"finished,omitempty"`
	// Away: the machine took the update, never said how it went and has not
	// redialled since this time.
	Away string `json:"not_redialled_since,omitempty"`
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
			rm.Update = &restFleetUpdate{From: u.From, To: u.To, Stage: u.Stage, Running: u.Running, Lines: u.Lines, Started: u.Started, Finished: u.Finished, Away: u.Away}
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
