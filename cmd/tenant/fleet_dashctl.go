package main

// fleet_dashctl.go adapts the fleet to dashboard.FleetControl: the
// dashboard's Fleet page (TEN-533). It runs the same operations as `tenant
// fleet` and /fleet (fleetOps, fleetctl.go) over this process's own state,
// and shows a machine's own words as those screens do: through fleetQuote,
// so the page gets printable ASCII in quotes the text cannot close.
//
// Confirm is the dashboard's one way to confirm a machine. The dashboard
// calls it only for a browser signed in on this computer
// (internal/dashboard/fleet.go, atThisComputer); nothing else in the
// dashboard, no relay and no tool reaches it (fleet_reach_test.go).

import (
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"tenant/internal/approval"
	"tenant/internal/dashboard"
	"tenant/internal/fleet"
)

// fleetMemory says where each machine's memory stands, for the Fleet page's
// memory column: a dashboard.FleetMemory state, and a line of detail or "".
// TEN-532 (the shadows and the copies on the machines) supplies the real one.
type fleetMemory interface {
	State(machineID string) (state, detail string)
}

// memoryNotBuilt is the fleet's memory before TEN-532: no machine's is known.
type memoryNotBuilt struct{}

func (memoryNotBuilt) State(string) (string, string) { return dashboard.FleetMemoryUnknown, "" }

// fleetMemoryOf is what the Fleet page asks. TEN-532 replaces it.
var fleetMemoryOf fleetMemory = memoryNotBuilt{}

type dashFleet struct{ ops fleetOps }

// newDashFleet builds the dashboard's fleet control over the live settings
// stg, read under mu, as the terminal's is (newFleetTUIControl).
func newDashFleet(c *commonFlags, stg *settings, mu *sync.Mutex) dashFleet {
	return dashFleet{ops: liveFleetOps(c, stg, mu, surfaceDashboard)}
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
		return v
	}
	v.Master = true
	v.Holder, v.Here = fleetHolding()
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
	// What a machine confirmed now would start with: the other side of each
	// machine's drift. Without it (the fleet's rules can't be read) no drift
	// is shown, and no pending machine can be confirmed.
	var fresh *approval.Scheme
	if local, err := d.ops.local(); err == nil {
		if s, err := f.Inherit(local); err == nil {
			fresh = &s
		}
	}
	v.NoFresh = fresh == nil
	links := fleetLinks()
	for _, m := range machines {
		var link *fleet.MachineStatus
		if links != nil { // this process holds the fleet
			st := links[m.ID]
			link = &st
		}
		mv := dashMachine(m, link)
		mv.Memory, mv.MemoryDetail = fleetMemoryOf.State(m.ID)
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
			if s, err := f.Scheme(m.ID); err == nil && fresh != nil && m.State != fleet.StatePending && m.State != fleet.StateRevoked {
				for _, dr := range s.Drift(*fresh) {
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
// of a confirmation and of a refused one are fleetOps.confirmMachine's.
func (d dashFleet) Confirm(id, code string, pairs []string) error {
	m, err := d.machine(id)
	if err != nil {
		return err
	}
	set, err := parseSet(pairs)
	if err != nil {
		return err
	}
	_, _, err = d.ops.confirmMachine(m.ID, code, set)
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
	return d.ops.recopy(f, m, from)
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
