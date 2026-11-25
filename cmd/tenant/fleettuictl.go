package main

// fleettuictl.go adapts the fleet to tui.FleetControl: /fleet and
// /permissions fleet in the terminal app (TEN-531). It runs the same
// operations as `tenant fleet` (fleetctl.go), over this process's own state:
// defaultFleet and the live settings. It works whether this process holds the
// fleet or another does; only the holder knows which machines are online.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"tenant/internal/approval"
	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
	"tenant/internal/tui"
)

type fleetTUIControl struct {
	ops fleetOps
	lc  *launchConfig
}

// newFleetTUIControl builds the terminal's fleet control. stg is the live
// settings, read under mu: the master's local scheme as its settings file
// holds it, which a new machine's scheme is copied from.
func newFleetTUIControl(c *commonFlags, stg *settings, mu *sync.Mutex) fleetTUIControl {
	lc := c.lc
	if lc == nil {
		lc = &launchConfig{}
	}
	return fleetTUIControl{lc: lc, ops: liveFleetOps(c, stg, mu, surfaceTUI)}
}

// liveFleetOps are the fleet's operations over a running process's own state:
// defaultFleet, and the live settings stg read under mu. surface names where
// the operator is (the terminal app, the dashboard) in the Security records.
func liveFleetOps(c *commonFlags, stg *settings, mu *sync.Mutex, surface string) fleetOps {
	return fleetOps{
		cfgDir: c.cfgDir, dataDir: c.dataDir, store: fleet.Open(c.cfgDir), surface: surface,
		schemes: func() (*approval.Fleet, error) { return defaultFleet, nil },
		local: func() (approval.Scheme, error) {
			mu.Lock()
			defer mu.Unlock()
			return approval.Scheme{Permissions: maps.Clone(stg.Permissions), ToolRules: maps.Clone(stg.ToolRules)}, nil
		},
	}
}

// fleetHolding says which process holds the fleet of this process's config
// dir, in words; here is true when it is this one, which then knows which
// machines are online.
func fleetHolding() (head string, here bool) {
	if mgr := fleetManager(); mgr != nil {
		return "this process holds the fleet, on " + mgr.Addr().String(), true
	}
	if elsewhere := fleetHeldElsewhere(); elsewhere != "" {
		return strings.TrimPrefix(elsewhere, "fleet: "), false
	}
	head = "no process holds the fleet yet, so no machine can connect"
	if h := theFleet.Load(); h != nil {
		h.mu.Lock()
		if h.problem != "" {
			head = "the fleet listener is not up: " + fleetQuote(h.problem)
		}
		h.mu.Unlock()
	}
	return head, false
}

// links is each machine's link, when this process holds the fleet.
func fleetLinks() map[string]fleet.MachineStatus {
	mgr := fleetManager()
	if mgr == nil {
		return nil
	}
	all, err := mgr.Machines()
	if err != nil {
		return nil
	}
	out := make(map[string]fleet.MachineStatus, len(all))
	for _, st := range all {
		out[st.ID] = st
	}
	return out
}

// liveHolderLine says which process holds the fleet, for a process that may
// take it itself: its hub sees a new fleet at its next look, so the line waits
// up to wait for this process or another to hold it.
func liveHolderLine(wait time.Duration) string {
	deadline := time.Now().Add(wait)
	for {
		head, here := fleetHolding()
		if here || theFleet.Load() == nil || fleetHeldElsewhere() != "" || !time.Now().Before(deadline) {
			return head
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Init is /fleet init: what `tenant fleet init` does, in this process, whose
// hub then starts the fleet port by itself. The recovery key is the step the
// app hands the terminal to (ensureRecovery, unchanged): made, shown once and
// proven there, or proven if it was recorded and never proven.
func (f fleetTUIControl) Init(listen string, addresses []string) (tui.FleetInit, error) {
	id, created, err := initFleet(f.ops, f.lc, listen, addresses, "")
	if err != nil {
		return tui.FleetInit{}, err
	}
	store := f.ops.store
	in := tui.FleetInit{Done: func(recErr error) string {
		said := initSaid(f.ops, f.lc, id, created)
		switch rec, proven, _ := store.Recovery(); {
		case rec != "" && !proven.IsZero():
			said += fmt.Sprintf("\n  recovery key   proven %s; its identity is yours to keep, Tenant has only its public half", fleetTime(proven))
		case rec != "":
			said += "\n  recovery key   recorded and never proven: /fleet init asks for the identity you kept"
		default:
			said += "\n  recovery key   not made, so no machine's memory is kept yet. Nothing was recorded, and an identity the terminal showed is void: /fleet init makes one when you are ready to save it"
		}
		if recErr != nil && !errors.Is(recErr, errRecoveryNotSetUp) {
			said += "\n  " + recErr.Error()
		}
		return said + "\n  " + liveHolderLine(fleetInitWait)
	}}
	if rec, proven, err := store.Recovery(); err != nil || rec == "" || proven.IsZero() {
		in.Recovery = func() error { return ensureRecovery(store) }
	}
	return in, nil
}

func (f fleetTUIControl) Status() string {
	// Never made a master. (An identity that is there and does not load is a
	// listener that is not up, told below.)
	if _, err := os.Lstat(fleet.MasterIdentityPath(f.ops.cfgDir)); err != nil {
		return "Fleet: this Tenant has no fleet yet: /fleet init"
	}
	machines, err := f.ops.store.Machines()
	if err != nil {
		return "Fleet: " + err.Error()
	}
	links := fleetLinks()
	head, here := fleetHolding()
	foot := ""
	switch {
	case here:
		foot = "  Machines are connected only while this app is open: a master with a fleet should run `tenant serve` as its service (tenant service install)."
	case fleetHeldElsewhere() != "":
		foot = "  Which machines are online is known there, and calls to them are made there."
	}
	return "Fleet: " + head + "\n" + listText(machines, links) + "\n" + memoryLines(f.ops.dataDir, machines) + foot
}

// memoryLines is each machine's memory (TEN-532), one line each, for /fleet.
func memoryLines(dataDir string, machines []fleet.Machine) string {
	var out strings.Builder
	for _, m := range machines {
		if m.State == fleet.StatePending || m.State == fleet.StateRevoked {
			continue
		}
		state, detail := memoryStateIn(dataDir, m.ID)
		fmt.Fprintf(&out, "  memory of %-20s %s", fleetName(m.Name), memoryWords(state))
		if detail != "" {
			out.WriteString(": " + detail)
		}
		out.WriteString("\n")
	}
	return out.String()
}

func (f fleetTUIControl) Show(name string) (string, error) {
	m, err := f.ops.machine(name)
	if err != nil {
		return "", err
	}
	var link *fleet.MachineStatus
	if st, ok := fleetLinks()[m.ID]; ok {
		link = &st
	}
	return f.ops.show(m, link), nil
}

func (f fleetTUIControl) Confirm(name, code string, pairs []string) (string, error) {
	set, err := parseSet(pairs)
	if err != nil {
		return "", err
	}
	if code == "" {
		m, err := f.ops.machine(name)
		if err != nil {
			return "", err
		}
		if m.State != fleet.StatePending {
			return "", fmt.Errorf("%s is %s, not waiting for confirmation", fleetName(m.Name), m.State)
		}
		_, p, err := f.ops.preview(m, set)
		if err != nil {
			return "", err
		}
		return confirmScreen(m, p) + fmt.Sprintf("\nTo confirm, type the six digits it shows on its own screen:  /fleet confirm %s <six digits>%s\n"+
			"To start a category looser or tighter on this machine alone, add pairs:  /fleet confirm %s <six digits> exec=allow",
			fleetName(m.Name), strings.Join(append([]string{""}, pairs...), " "), fleetName(m.Name)), nil
	}
	m, _, err := f.ops.confirmMachine(name, code, set)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("confirmed %s (%s). The master may call it from now on; its scheme: /permissions fleet %s", fleetName(m.Name), m.ID, fleetName(m.Name)), nil
}

// Enroll makes a code for a new machine and says what `tenant fleet enroll`
// says: the code, then the lines to paste and the next step.
func (f fleetTUIControl) Enroll(name string) (string, error) {
	e, err := f.ops.issueCode(name, 0, "", "")
	if err != nil {
		// The same words as the command line's, with this app's command.
		return "", errors.New(strings.ReplaceAll(err.Error(), "run `tenant fleet init`", "/fleet init"))
	}
	text := strings.ReplaceAll(enrollText(e), "tenant fleet confirm ", "/fleet confirm ")
	return e.Code + "\n\n" + text, nil
}

func (f fleetTUIControl) Revoke(name, scheme string) (string, error) {
	m, err := f.ops.machine(name)
	if err != nil {
		return "", err
	}
	n := fleetName(m.Name)
	switch scheme {
	case "":
		return fmt.Sprintf("Revoke %s (%s)? Its certificate is refused from then on and its link is closed.\n"+
			"  /fleet revoke %s keep     keeps its approval scheme under its id, for a machine enrolled later to adopt\n"+
			"  /fleet revoke %s delete   deletes its approval scheme and its record", n, m.ID, n, n), nil
	case "keep", "delete":
		if _, err := f.ops.revoke(m.ID, scheme == "delete"); err != nil {
			return "", err
		}
		if scheme == "delete" {
			return fmt.Sprintf("revoked %s (%s): its certificate is refused, its record and approval scheme are deleted, and %s", n, m.ID, memoryGoneWords(f.ops.dataDir, m.ID)), nil
		}
		return fmt.Sprintf("revoked %s (%s): its certificate is refused. Its approval scheme is kept under its id", n, m.ID), nil
	}
	return "", errors.New("say keep or delete: what becomes of its approval scheme")
}

func (f fleetTUIControl) Perms(name string) (tui.PermissionControl, error) {
	m, err := f.ops.machine(name)
	if err != nil {
		return nil, err
	}
	return defaultFleet.Child(m.ID)
}

func (f fleetTUIControl) Defaults() tui.FleetDefaults {
	return fleetDefaultsEditor{fleetRuleSet{defaultFleet.Rules()}, f.ops}
}

// Group is /fleet group: `tenant fleet group`'s words, with this app's
// commands in them.
func (f fleetTUIControl) Group(args []string) (string, error) {
	text, _, err := groupText(f.ops, "/fleet", args)
	return text, err
}

// fleetDefaultsEditor is the defaults for new machines as the permissions
// editor takes them: the categories the operator set (fleetOps, so each
// change is recorded) and the fleet's rules.
type fleetDefaultsEditor struct {
	fleetRuleSet
	ops fleetOps
}

func (e fleetDefaultsEditor) Permissions() []tui.PermissionInfo {
	f, err := e.ops.schemes()
	if err != nil {
		return nil
	}
	set, _ := f.Defaults()
	var out []tui.PermissionInfo
	for _, c := range approval.Categories {
		if mode := set[c]; mode != "" {
			out = append(out, tui.PermissionInfo{Category: c, Mode: mode, Desc: "fleet default; " + approval.CategoryDesc[c],
				Title: approval.CategoryPlain[c].Title, Can: onMachine(approval.CategoryPlain[c].Can)})
		}
	}
	return out
}

func (e fleetDefaultsEditor) SetPermission(category, mode string) (bool, error) {
	if !slices.Contains(approval.Categories, category) {
		return false, nil
	}
	return true, e.ops.setDefault(category, mode)
}

func (e fleetDefaultsEditor) UnsetPermission(category string) (bool, error) {
	return e.ops.unsetDefault(category)
}

func (f fleetTUIControl) Relay(args []string) (string, error) {
	out, err := fleetRelaySwitch(f.ops.cfgDir, f.lc, args)
	if h := theFleet.Load(); err == nil && h != nil {
		h.mu.Lock()
		h.cfgSeen = [2]int64{} // read config.json again at the next look, whatever its timestamp says
		h.relays = map[string]bool{surfaceDiscord: f.lc.Relay.AllowFleet, surfaceIMessage: f.lc.IMessage.AllowFleet}
		h.mu.Unlock()
	}
	return out, err
}

// fleetNames is a list of machine ids as their names, for display; an id no
// machine has any more shows as itself.
func fleetNames(ids []string) []string {
	st := fleetStore()
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id
		if st == nil {
			continue
		}
		if m, err := st.Machine(id); err == nil && m.State != fleet.StateRevoked {
			out[i] = fleetName(m.Name)
		} else if err == nil {
			out[i] = id + " (revoked)"
		}
	}
	return out
}

// SetFleet is /cron fleet: the fleet machines job id may reach, by name or
// id. The job keeps their ids, so a rename does not move it and a revoked
// machine's name, given to another, does not either.
func (a tuiCron) SetFleet(id string, machines []string) (tui.CronJobView, error) {
	st := fleetStore()
	if st == nil && len(machines) > 0 {
		return tui.CronJobView{}, errors.New("this Tenant has no fleet: tenant fleet init")
	}
	ids := make([]string, 0, len(machines))
	for _, ref := range machines {
		m, err := (fleetOps{store: st}).machine(ref)
		if err != nil {
			return tui.CronJobView{}, err
		}
		if m.State == fleet.StateRevoked {
			return tui.CronJobView{}, fmt.Errorf("%s is revoked", fleetName(m.Name))
		}
		ids = append(ids, m.ID)
	}
	j, err := a.m.engine.SetFleet(id, ids)
	if err != nil {
		return tui.CronJobView{}, err
	}
	_ = catalog.ScheduledJobChanged.Audit(context.Background(), "job", j.Label(), "id", j.ID,
		"state", "fleet machines: "+strings.Join(slices.Clone(j.Fleet), ", "), "actor", actorOperator, "surface", surfaceTUI)
	return tui.CronJobView{ID: j.ID, Name: j.Name, Spec: j.Spec, Enabled: j.Enabled, Kind: j.KindOf(), Exec: j.Exec, TZ: j.TZ,
		NextRun: cronFmtTime(j.NextRun), Fleet: fleetNames(j.Fleet)}, nil
}
