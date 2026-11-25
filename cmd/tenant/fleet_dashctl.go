package main

// fleet_dashctl.go adapts the fleet to dashboard.FleetControl: the
// dashboard's Fleet page (TEN-533). It runs the same operations as `tenant
// fleet` and /fleet (fleetOps, fleetctl.go) over this process's own state,
// and shows a machine's own words as those screens do: through fleetQuote,
// so the page gets printable ASCII in quotes the text cannot close.
//
// Confirm is the dashboard's one way to confirm a machine. The dashboard
// calls it for a browser signed in on this computer
// (internal/dashboard/fleet.go, atThisComputer) or, while the operator has
// remote confirmation on (fleet.remote_confirm, TEN-545), for a signed-in
// browser elsewhere that gave the sign-in key again; nothing else in the
// dashboard, no relay and no tool reaches it (fleet_reach_test.go).

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"tenant/internal/approval"
	"tenant/internal/dashboard"
	"tenant/internal/fleet"
	"tenant/internal/secrets"
)

type dashFleet struct {
	ops fleetOps
	lc  *launchConfig
}

// newDashFleet builds the dashboard's fleet control over the live settings
// stg, read under mu, as the terminal's is (newFleetTUIControl).
func newDashFleet(c *commonFlags, stg *settings, mu *sync.Mutex) dashFleet {
	lc := c.lc
	if lc == nil {
		lc = &launchConfig{}
	}
	return dashFleet{lc: lc, ops: liveFleetOps(c, stg, mu, surfaceDashboard)}
}

// Init is Set up the fleet: what `tenant fleet init` does but for the
// recovery key, in this process, whose hub then starts the fleet port by
// itself. An address that is not one is the page's own plain words.
func (d dashFleet) Init(listen string, addresses []string, by string) error {
	if listen != "" {
		if _, _, err := net.SplitHostPort(listen); err != nil {
			return dashboard.PlainError("Nothing changed: the fleet port listens on a host:port address, like 0.0.0.0:9777.")
		}
	}
	if fleet.CheckAddresses(addresses) != nil {
		return dashboard.PlainError("Nothing changed: each address a machine dials is a host:port it can reach, like master.example:9777.")
	}
	if _, _, err := initFleet(d.ops, d.lc, listen, addresses, by); err != nil {
		return err
	}
	// The page that answers shows who holds the fleet: give this process's
	// hub its next look, as the command line's init waits for it.
	liveHolderLine(fleetInitWait)
	return nil
}

// dashTime is a time as the page shows it, "" for none.
func dashTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fleetTime(t)
}

// onMachine is a category's "what Allow lets the agent do" for a machine's
// own settings: the words were written for this computer.
func onMachine(can string) string {
	return strings.ReplaceAll(can, "on this computer", "on this machine")
}

func dashRuleRows(rules map[string]string) []dashboard.ToolRuleRow {
	out := make([]dashboard.ToolRuleRow, 0, len(rules))
	for _, p := range slices.Sorted(maps.Keys(rules)) {
		out = append(out, dashboard.ToolRuleRow{Pattern: p, Mode: rules[p]})
	}
	return out
}

func (d dashFleet) View() dashboard.FleetView {
	var v dashboard.FleetView
	// Never made a master. (An identity that is there and does not load is a
	// listener that is not up, which Holder says.)
	if _, err := os.Lstat(fleet.MasterIdentityPath(d.ops.cfgDir)); err != nil {
		// What Set up the fleet starts from: what init itself would choose.
		v.Listen = cmp.Or(d.lc.Fleet.Listen, defaultFleetListen)
		v.Dial = strings.Join(fleetDefaultAddresses(v.Listen), ", ")
		return v
	}
	v.Master, v.RemoteConfirm = true, fleetRemoteConfirmOn(d.ops.cfgDir)
	if rec, _, err := d.ops.store.Recovery(); err == nil && rec == "" {
		v.NoRecovery = true
	}
	v.Holder, v.Here = fleetHolding()
	// The rollout is this process's own, in memory.
	if ro := fleetRollouter(); ro != nil {
		if st, ok := ro.State(); ok {
			v.Rollout = dashRollout(st, ro.updates)
		}
	}
	machines, err := d.ops.store.Machines()
	if err != nil {
		v.Err = err
		return v
	}
	f, err := d.ops.schemes()
	if err != nil {
		v.Err = err
		return v
	}
	fleetRules := f.Rules().Snapshot()
	v.Rules = dashRuleRows(fleetRules)
	local, localErr := d.ops.local()
	// The fleet's default categories (fleet_permissions): what the operator
	// set for new machines, over what this Tenant's own scheme would give,
	// clamped (approval.MasterCopy).
	if set, err := f.Defaults(); err != nil {
		v.DefaultsErr = err
	} else if localErr == nil {
		copied := approval.MasterCopy(local)
		for _, c := range approval.Categories {
			v.Defaults = append(v.Defaults, dashboard.FleetDefault{
				PermissionRow: dashboard.PermissionRow{Category: c, Mode: set[c], Title: approval.CategoryPlain[c].Title, Can: onMachine(approval.CategoryPlain[c].Can)},
				Inherited:     copied[c],
			})
		}
	}
	groups, groupsErr := f.Groups()
	v.GroupsErr = groupsErr
	// differs: for each confirmed machine with settings, whether Apply would
	// change them now. applyUnknown: that can't be worked out.
	differs, applyUnknown := map[string]bool{}, false
	// What each machine confirmed now would start with, its groups' rules in
	// it (Fresh): the other side of its drift. Without it (the fleet's rules,
	// defaults or groups can't be read) no drift is shown, and no pending
	// machine can be confirmed.
	fresh := func(id string) *approval.Scheme {
		if localErr != nil {
			return nil
		}
		s, err := f.Fresh(id, local)
		if err != nil {
			return nil
		}
		return &s
	}
	v.NoFresh = fresh("") == nil
	links := fleetLinks()
	for _, m := range machines {
		var link *fleet.MachineStatus
		if links != nil { // this process holds the fleet
			st := links[m.ID]
			link = &st
		}
		mv := dashMachine(m, link)
		for _, g := range groups {
			if _, in := slices.BinarySearch(g.Machines, m.ID); in {
				mv.Groups = append(mv.Groups, g.Name)
			}
		}
		// The memory column: what the holder's last comparison found, or
		// its record of it (memoryStateIn decrypts nothing).
		mv.Memory, mv.MemoryDetail = memoryStateIn(d.ops.dataDir, m.ID)
		if u := fleetUpdater(); u != nil {
			if st, ok := u.State(m.ID); ok {
				mv.Update = dashUpdate(st)
			}
		}
		if m.State == fleet.StatePending {
			if _, p, err := d.ops.preview(m, nil); err == nil {
				mv.Start = dashStart(p)
			}
		}
		if b, err := f.Child(m.ID); err == nil {
			mv.HasScheme = true
			for _, p := range b.Permissions() {
				mv.Perms = append(mv.Perms, dashboard.PermissionRow{Category: p.Category, Mode: p.Mode, Title: p.Title, Can: onMachine(p.Can)})
			}
			for _, r := range b.Rules() {
				mv.Rules = append(mv.Rules, dashboard.ToolRuleRow{Pattern: r.Pattern, Mode: r.Mode, Kind: r.Kind})
			}
			if s, err := f.Scheme(m.ID); err == nil && m.State == fleet.StateConfirmed {
				if applied, err := f.Applied(m.ID); err != nil {
					applyUnknown = true
				} else {
					differs[m.ID] = len(s.Drift(applied)) > 0
				}
			}
			if s, err := f.Scheme(m.ID); err == nil && !v.NoFresh && m.State != fleet.StatePending && m.State != fleet.StateRevoked {
				want := fresh(m.ID)
				if want == nil {
					want = &s // the layers changed under us: no drift this time
				}
				for _, dr := range s.Drift(*want) {
					row := dashboard.FleetDrift{Rule: dr.Rule, Key: dr.Key, Have: dr.Have, Want: dr.Want}
					if dr.Rule {
						row.FleetRule = dr.Want != "" && fleetRules[dr.Key] == dr.Want
					} else {
						row.Title = approval.CategoryPlain[dr.Key].Title
					}
					mv.Drift = append(mv.Drift, row)
				}
			}
		}
		v.Machines = append(v.Machines, mv)
	}
	for _, g := range groups {
		row := dashboard.FleetGroup{Name: g.Name, Members: g.Machines, Rules: dashRuleRows(g.ToolRules)}
		for _, id := range g.Machines {
			if differs[id] {
				row.Differ++
			}
		}
		if applyUnknown {
			row.Differ = -1
		}
		v.Groups = append(v.Groups, row)
	}
	return v
}

// dashMachine is a machine's record and its link as the page shows them. It
// is where another computer's text enters the page: what the machine reports
// about itself, the address it connected from and why its link ended each go
// through fleetQuote, and its name through fleetName, as on the terminal's
// screens. link is nil when another process holds the fleet.
func dashMachine(m fleet.Machine, link *fleet.MachineStatus) dashboard.FleetMachine {
	mv := dashboard.FleetMachine{
		ID: m.ID, Name: fleetName(m.Name), State: string(m.State),
		Certificate: m.Fingerprint, Enrolled: dashTime(m.EnrolledAt), Confirmed: dashTime(m.ConfirmedAt), Revoked: dashTime(m.RevokedAt),
		OS: fleetQuote(m.Reported.OS), User: fleetQuote(m.Reported.User), Version: fleetQuote(m.Reported.Version),
		Sandbox: fleetQuote(m.Reported.Sandbox), Ceiling: fleetQuote(m.Reported.Ceiling),
	}
	if link == nil {
		return mv
	}
	mv.Link, mv.Online = linkText(m, *link), link.Online && m.State == fleet.StateConfirmed
	if link.Online {
		mv.Remote, mv.LinkVersion, mv.Calls = fleetQuote(link.Remote), link.Link, link.InFlight
	}
	if link.LastError != "" {
		mv.LastEnded = fleetQuote(link.LastError)
	}
	return mv
}

// dashStart is the confirm screen's scheme for the page: every category and
// rule a pending machine would start with, each with the screen's own note of
// where it came from.
func dashStart(p fleetPreview) *dashboard.FleetStart {
	out := &dashboard.FleetStart{Kept: p.Kept}
	modes := approval.SavedModes(p.Scheme.Permissions)
	for _, c := range approval.Categories {
		mode := modes[c].String()
		out.Perms = append(out.Perms, dashboard.FleetStartPerm{
			PermissionRow: dashboard.PermissionRow{Category: c, Mode: mode, Title: approval.CategoryPlain[c].Title, Can: onMachine(approval.CategoryPlain[c].Can)},
			Note:          p.catNote(c, mode),
		})
	}
	for _, r := range dashRuleRows(p.Scheme.ToolRules) {
		out.Rules = append(out.Rules, dashboard.FleetStartRule{ToolRuleRow: r, Note: p.ruleNote(r.Pattern, r.Mode)})
	}
	return out
}

// machine is the fleet's machine with that id. The page names a machine by
// its id only: a name is a label, and never looked up here.
func (d dashFleet) machine(id string) (fleet.Machine, error) {
	m, err := d.ops.store.Machine(id)
	if errors.Is(err, fleet.ErrUnknown) {
		return m, dashboard.PlainError("That machine isn't part of this fleet. Nothing changed.")
	}
	return m, err
}

// Confirm takes the six digits a pending machine shows. The Security records
// of a confirmation and of a refused one are fleetOps.confirmMachine's. by
// names a signed-in browser that is not on this computer: it confirms only
// while config.json says so now (read here again, whatever the page showed
// when it was drawn), and its records carry the remote surface and by.
func (d dashFleet) Confirm(id, code string, pairs []string, by string) error {
	m, err := d.machine(id)
	if err != nil {
		return err
	}
	ops := d.ops
	if by != "" {
		if !fleetRemoteConfirmOn(ops.cfgDir) {
			return dashboard.PlainError("Nothing changed: confirming a machine from a browser that is not on the computer running Tenant is turned off. It is turned on in a terminal there: tenant fleet remote-confirm on")
		}
		ops.surface, ops.by = surfaceDashboardRemote, by
	}
	set, err := parseSet(pairs)
	if err != nil {
		return err
	}
	_, _, err = ops.confirmMachine(m.ID, code, set)
	switch {
	case errors.Is(err, fleet.ErrWrongCode):
		return dashboard.PlainError("Those aren't the six digits that machine shows. Nothing changed. If they never match, the machine waiting here is not the one in front of you: revoke it.")
	case err != nil && m.State != fleet.StatePending:
		return dashboard.PlainError("That machine isn't waiting to be confirmed. Nothing changed.")
	}
	return err
}

func (d dashFleet) Rename(id, name string) error {
	m, err := d.machine(id)
	if err != nil {
		return err
	}
	_, err = d.ops.rename(m.ID, name)
	return err
}

func (d dashFleet) Revoke(id string, deleteSettings bool) error {
	m, err := d.machine(id)
	if err != nil {
		return err
	}
	_, err = d.ops.revoke(m.ID, deleteSettings)
	return err
}

// child is the machine's own approval scheme, as an editor takes it.
func (d dashFleet) child(id string) (*approvalBroker, error) {
	m, err := d.machine(id)
	if err != nil {
		return nil, err
	}
	f, err := d.ops.schemes()
	if err != nil {
		return nil, err
	}
	return f.Child(m.ID)
}

func (d dashFleet) SetPermission(id, category, mode string) error {
	b, err := d.child(id)
	if err != nil {
		return err
	}
	known, err := b.SetPermissionFrom(surfaceDashboard, category, mode)
	if err == nil && !known {
		err = errors.New("no category " + fleetQuote(category))
	}
	return err
}

func (d dashFleet) SetRule(id, pattern, mode string) error {
	b, err := d.child(id)
	if err != nil {
		return err
	}
	return b.SetRule(pattern, mode)
}

func (d dashFleet) RemoveRule(id, pattern string) (bool, error) {
	b, err := d.child(id)
	if err != nil {
		return false, err
	}
	return b.RemoveRule(pattern)
}

func (d dashFleet) Recopy(id, from string) error {
	m, err := d.machine(id)
	if err != nil {
		return err
	}
	f, err := d.ops.schemes()
	if err != nil {
		return err
	}
	_, err = d.ops.recopy(f, m, from)
	return err
}

// Enroll makes a code for a new machine, as `tenant fleet enroll` does, with
// the lines to paste for the way it runs (enrollPaste). A name the fleet does
// not take is the page's own plain words.
func (d dashFleet) Enroll(name string, boot bool, by string) (dashboard.FleetEnrollment, error) {
	if _, err := fleetIdentity(d.ops.cfgDir); err != nil {
		return dashboard.FleetEnrollment{}, dashboard.PlainError("No code was made: this Tenant has no fleet yet. Set it up on this page first, or run tenant fleet init in a terminal on the computer running Tenant.")
	}
	if addrs, err := d.ops.store.Addresses(); err == nil && len(addrs) == 0 {
		return dashboard.FleetEnrollment{}, dashboard.PlainError("No code was made: the fleet has no address for a machine to dial. Add one in a terminal: tenant fleet address add HOST:PORT")
	}
	runs := "as the user"
	if boot {
		runs = "from boot"
	}
	e, err := d.ops.issueCode(name, 0, "", by+"; for a child that runs "+runs)
	if err != nil {
		return dashboard.FleetEnrollment{}, err
	}
	out := dashboard.FleetEnrollment{Name: fleetName(e.Name), Code: e.Code, Expires: dashTime(e.Expires), Boot: boot,
		Addresses: e.Addresses, Warning: addressWarning(e.Addresses)}
	lines, note := enrollPaste(e.Code, boot)
	for _, l := range lines {
		out.Lines = append(out.Lines, dashboard.FleetPasteLine{For: l.For, Line: l.Line})
	}
	out.Note = note
	return out, nil
}

// dashUpdate is a machine's last update as the page shows it.
func dashUpdate(st fleetUpdateState) *dashboard.FleetUpdate {
	return &dashboard.FleetUpdate{From: st.From, To: st.To, Stage: st.Stage, Running: !st.Over(), Lines: st.Lines,
		Started: dashTime(st.Started), Finished: dashTime(st.Finished)}
}

// Update starts updating a machine over its link, in this process, which
// must be the one that holds the fleet: only it has the links.
func (d dashFleet) Update(id, version, arch string, allowOlder bool) error {
	m, err := d.machine(id)
	if err != nil {
		return err
	}
	mgr, upd := fleetManager(), fleetUpdater()
	if mgr == nil || upd == nil {
		if elsewhere := fleetHeldElsewhere(); elsewhere != "" {
			return dashboard.PlainError("Nothing was sent: " + strings.TrimPrefix(elsewhere, "fleet: ") + ", and a machine is updated from the process that holds the fleet.")
		}
		return dashboard.PlainError("Nothing was sent: no process holds the fleet right now, so no machine is online.")
	}
	if err := upd.Start(mgr, m.ID, fleetUpdateOpts{version: version, arch: arch, allowOlder: allowOlder}); err != nil {
		return dashboard.PlainError("Nothing was sent: " + err.Error() + ".")
	}
	return nil
}

// StartRollout updates many machines a few at a time, through this
// process's rollout (fleetrollout.go): the process that holds the fleet, as
// Update needs. The rollout does not check the version or the processor
// itself, so they are checked here before anything starts; machines are
// named by id, and one the fleet no longer has goes in by its id, where its
// update refuses it and the rollout skips it.
func (d dashFleet) StartRollout(ids []string, version, arch string, allowOlder bool, limit int) error {
	switch {
	case version != "" && !versionArg.MatchString(version):
		return dashboard.PlainError("Nothing was sent: " + fleetQuote(version) + " is not a version (vX.Y.Z).")
	case arch != "" && arch != "amd64" && arch != "arm64":
		return dashboard.PlainError("Nothing was sent: the processor is amd64 or arm64, not " + fleetQuote(arch) + ".")
	case limit < 1 || limit > fleetRolloutLimit:
		return dashboard.PlainError("Nothing was sent: a rollout updates 1 to 8 machines at once.")
	}
	mgr, ro := fleetManager(), fleetRollouter()
	if mgr == nil || ro == nil {
		if elsewhere := fleetHeldElsewhere(); elsewhere != "" {
			return dashboard.PlainError("Nothing was sent: " + strings.TrimPrefix(elsewhere, "fleet: ") + ", and a machine is updated from the process that holds the fleet.")
		}
		return dashboard.PlainError("Nothing was sent: no process holds the fleet right now, so no machine is online.")
	}
	machines := make([]fleetRolloutMachine, 0, len(ids))
	for _, id := range ids {
		m, err := d.ops.store.Machine(id)
		switch {
		case errors.Is(err, fleet.ErrUnknown):
			m.Name = id
		case err != nil:
			return err
		}
		machines = append(machines, fleetRolloutMachine{ID: id, Name: m.Name})
	}
	if err := ro.Start(mgr, machines, fleetUpdateOpts{version: version, arch: arch, allowOlder: allowOlder}, limit); err != nil {
		return dashboard.PlainError("Nothing was sent: " + err.Error() + ".")
	}
	return nil
}

// StopRollout asks this process's rollout to start no more machines.
func (d dashFleet) StopRollout() bool {
	ro := fleetRollouter()
	return ro != nil && ro.Stop()
}

// dashRollout is the rollout as the page shows it: a running machine with
// its update's stage (read from the updates the rollout runs), each name
// through fleetName. A reason is the update's own line, which already quotes
// what the machine said.
func dashRollout(st fleetRolloutState, upd interface {
	State(machine string) (fleetUpdateState, bool)
}) *dashboard.FleetRollout {
	out := &dashboard.FleetRollout{Version: st.Version, Limit: st.Limit, Started: dashTime(st.Started), Finished: dashTime(st.Finished),
		Running: !st.Over(), Stopped: st.Stopped}
	for _, m := range st.Machines {
		row := dashboard.FleetRolloutMachine{ID: m.ID, Name: fleetName(m.Name), Status: m.Status, Reason: m.Reason}
		if m.Status == fleetRollRunning && upd != nil {
			if u, ok := upd.State(m.ID); ok {
				row.Stage = u.Stage
			}
		}
		out.Machines = append(out.Machines, row)
	}
	return out
}

// --- the fleet's defaults, its groups and changes to many machines (TEN-546) ---
//
// Each is fleetOps' own operation (fleetgroups.go), so the Fleet page does
// what `tenant fleet permissions --defaults`, `tenant fleet group` and
// `tenant fleet permissions --group` do and files the same records, with the
// dashboard as their surface. The page names a machine by its id only.

// dashSaid is a refusal of one of these operations as the page says it: the
// operation's own plain sentence. Its words are this Tenant's and the
// operator's own (a group's name, a pattern), escaped where the fleet checks
// them; no machine's words are in it.
func dashSaid(err error) error {
	if err == nil {
		return nil
	}
	return dashboard.PlainError("Nothing changed: " + strings.TrimSuffix(strings.TrimPrefix(err.Error(), "fleet: "), ".") + ".")
}

// SetDefault sets one of the fleet's default categories for new machines, or
// with mode "" gives it back to this Tenant's own. It moves no machine.
func (d dashFleet) SetDefault(category, mode string) error {
	if mode == "" {
		_, err := d.ops.unsetDefault(category)
		return dashSaid(err)
	}
	return dashSaid(d.ops.setDefault(category, mode))
}

func (d dashFleet) SetDefaultRule(pattern, mode string) error {
	return dashSaid(d.ops.setDefaultRule(pattern, mode))
}

func (d dashFleet) RemoveDefaultRule(pattern string) (bool, error) {
	removed, err := d.ops.removeDefaultRule(pattern)
	return removed, dashSaid(err)
}

func (d dashFleet) CreateGroup(name string) error {
	_, err := d.ops.createGroup(name)
	return dashSaid(err)
}

func (d dashFleet) DeleteGroup(group string) error {
	_, err := d.ops.deleteGroup(group)
	return dashSaid(err)
}

// AddMembers and RemoveMembers take machine ids. fleetOps finds a machine by
// the name it carries or by its id, and no name has the shape of an id
// (fleet.json's own rule), so an id finds only its own machine.
func (d dashFleet) AddMembers(group string, machines []string) error {
	for _, id := range machines {
		if !machineIDShape.MatchString(id) {
			return dashboard.PlainError("Nothing changed: a machine is named by its id.")
		}
	}
	_, err := d.ops.addToGroup(group, machines)
	return dashSaid(err)
}

func (d dashFleet) RemoveMembers(group string, machines []string) error {
	for _, id := range machines {
		if !machineIDShape.MatchString(id) {
			return dashboard.PlainError("Nothing changed: a machine is named by its id.")
		}
	}
	_, err := d.ops.removeFromGroup(group, machines)
	return dashSaid(err)
}

func (d dashFleet) SetGroupRule(group, pattern, mode string) error {
	return dashSaid(d.ops.setGroupRule(group, pattern, mode))
}

func (d dashFleet) RemoveGroupRule(group, pattern string) (bool, error) {
	removed, err := d.ops.removeGroupRule(group, pattern)
	return removed, dashSaid(err)
}

// Bulk makes one change to each machine, through the single-machine path of
// fleetOps (fleetOps.bulk): each machine changed has its own record, the batch
// none, and one that fails or does not qualify is a result with its reason.
// The results are in the order the ids came.
func (d dashFleet) Bulk(op dashboard.FleetBulkOp, ids []string) []dashboard.FleetBulkResult {
	var edit []string // permEdit's words, for a change to a scheme's own entries
	switch op.Action {
	case dashboard.FleetBulkPermission:
		edit = []string{"set", op.Category, op.Mode}
	case dashboard.FleetBulkRule:
		edit = []string{"add", op.Pattern, op.Mode}
	case dashboard.FleetBulkUnrule:
		edit = []string{"rm", op.Pattern}
	}
	ms := make([]fleet.Machine, len(ids))
	for i, id := range ids {
		m, err := d.ops.store.Machine(id) // by id, never by name
		if err != nil {
			m = fleet.Machine{ID: id, Name: id} // no state: bulk says no machine has this id
		}
		ms[i] = m
	}
	rs, err := d.ops.bulk(ms, func(f *approval.Fleet, m fleet.Machine) (string, error) {
		switch op.Action {
		case dashboard.FleetBulkApply:
			return d.ops.recopy(f, m, "defaults")
		case dashboard.FleetBulkCopy:
			return d.ops.recopy(f, m, "master")
		case dashboard.FleetBulkPermission, dashboard.FleetBulkRule, dashboard.FleetBulkUnrule:
			return d.ops.editMachine(f, m, edit)
		}
		return "", errors.New("that is not a change this page makes")
	})
	out := make([]dashboard.FleetBulkResult, len(ids))
	for i, m := range ms {
		out[i] = dashboard.FleetBulkResult{ID: m.ID, Name: fleetName(m.Name)}
		switch {
		case err != nil: // the fleet's schemes can't be read: nothing was tried
			out[i].Said = "the fleet's approval settings can't be read right now"
		case i < len(rs):
			out[i].Done, out[i].Said = rs[i].OK, strings.TrimPrefix(rs[i].Said, "fleet: ")
		}
	}
	return out
}

// machineIDShape is what a machine id looks like (fleet.json's own rule).
var machineIDShape = regexp.MustCompile(`^m[0-9a-f]{16}$`)

// Recover brings the fleet's recovery identity to the process that holds the
// fleet (tenant fleet recover, through POST /fleet/recover): the machine's
// copy made under another fleet key is read with it and becomes its memory
// here (fleetMemory.recoverWith). identity is moved into locked memory at
// once, which zeroes it, and released when this returns.
func (d dashFleet) Recover(id, was string, identity []byte) (string, error) {
	buf, err := secrets.NewLockedBuffer(identity)
	if err != nil {
		clear(identity)
		return "", err
	}
	defer buf.Release()
	m, err := d.machine(id)
	if err != nil {
		return "", err
	}
	if was != "" && !machineIDShape.MatchString(was) {
		return "", dashboard.PlainError("Nothing changed: --was takes a machine id (m and 16 hex digits), as `tenant child status` on the machine shows it.")
	}
	if fleetManager() == nil {
		if elsewhere := fleetHeldElsewhere(); elsewhere != "" {
			return "", dashboard.PlainError("Nothing changed: " + strings.TrimPrefix(elsewhere, "fleet: ") + ", and a machine's memory is read there.")
		}
		return "", dashboard.PlainError("Nothing changed: no process holds the fleet right now, so no machine is online.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	said, err := fleetRecover(ctx, m.ID, was, buf)
	if err != nil {
		return "", dashboard.PlainError("Nothing changed: " + err.Error() + ".")
	}
	d.ops.audit(m, "memory recovered", said)
	return said, nil
}
