package main

// fleetctl.go is `tenant fleet …` (TEN-531; docs/plans/gap6-fleet-plan.md,
// sections 2, 5 and 7): the operator's side of a master's fleet. It works on
// the files (fleet.json, the settings file, config.json), so it works whether
// or not a Tenant is running, and whichever process holds the fleet: the
// holder sees a change within seconds (fleethub.go). The same operations are
// behind /fleet in the terminal (fleettuictl.go) and the dashboard's Fleet
// page (fleet_dashctl.go).
//
// Confirming a machine is the one step that lets a master call it. It is
// taken here, in the terminal's /fleet confirm and on the dashboard's Fleet
// page opened on this computer, and nowhere else: no tool, no relay and no
// approval card leads to it (fleetOps.confirmMachine). The operator can let a
// signed-in browser elsewhere take it too, with the sign-in key again
// (`tenant fleet remote-confirm`, off by default; TEN-545).

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	xterm "github.com/charmbracelet/x/term"
	"github.com/mattn/go-isatty"

	"tenant/internal/approval"
	"tenant/internal/fleet"
	"tenant/internal/fleet/shadow"
	"tenant/internal/logs/catalog"
	"tenant/internal/secrets"
	"tenant/internal/tui"
)

// --- text a machine supplied ---

// fleetQuote is text the master did not write (what a machine reports about
// itself, an address, a refusal) as a screen may show it: in quotes, cut to
// 64 characters, every control character, escape sequence and character
// outside ASCII as its escape. It can then draw nothing, hide nothing, pass
// for nothing by look-alike letters, and neither end the line it stands on
// nor start a label of its own.
func fleetQuote(s string) string { return fleetQuoteN(s, 64) }

// fleetSaid is fleetQuote for a machine's longer words: why it refused or
// rolled back an update.
func fleetSaid(s string) string { return fleetQuoteN(s, 512) }

func fleetQuoteN(s string, most int) string {
	cut := len(s) > utf8.UTFMax*most
	if cut {
		s = s[:utf8.UTFMax*most] // more than most characters either way
	}
	if r := []rune(s); len(r) > most {
		s, cut = string(r[:most]), true
	}
	q := strconv.QuoteToASCII(s)
	if cut {
		q += "..."
	}
	return q
}

// fleetName is a machine's name as a screen shows it: bare while it is the
// plain label the store accepts, quoted (fleetQuote) the moment it is not.
func fleetName(s string) string {
	plain := s != "" && len(s) <= 63
	for i := 0; plain && i < len(s); i++ {
		c := s[i]
		plain = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '.' || c == '-' || c == '_')
	}
	if plain {
		return s
	}
	return fleetQuote(s)
}

func fleetTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// reportedLines is what a machine said about itself, each value marked as
// its own words by the heading and by its quotes.
func reportedLines(r fleet.Reported, indent string) []string {
	out := []string{indent + "Reported by the machine (its own words; the master has not checked them):"}
	for _, kv := range [][2]string{{"os", r.OS}, {"user", r.User}, {"version", r.Version}, {"sandbox", r.Sandbox}, {"ceiling", r.Ceiling}} {
		out = append(out, fmt.Sprintf("%s  %-9s %s", indent, kv[0], fleetQuote(kv[1])))
	}
	return out
}

// schemeLines lists a scheme: every category, then its rules by pattern.
// catNote and ruleNote add a remark to a line, or "".
func schemeLines(s approval.Scheme, indent string, catNote func(cat, mode string) string, ruleNote func(pattern, mode string) string) []string {
	var out []string
	modes := approval.SavedModes(s.Permissions)
	for _, c := range approval.Categories {
		mode, note := modes[c].String(), ""
		if catNote != nil {
			note = catNote(c, mode)
		}
		out = append(out, strings.TrimRight(fmt.Sprintf("%s%-12s %-5s  %s", indent, c, mode, note), " "))
	}
	if len(s.ToolRules) == 0 {
		return append(out, indent+"tool rules: none")
	}
	out = append(out, indent+"tool rules (checked before the category):")
	for _, p := range slices.Sorted(maps.Keys(s.ToolRules)) {
		note := ""
		if ruleNote != nil {
			note = ruleNote(p, s.ToolRules[p])
		}
		out = append(out, strings.TrimRight(fmt.Sprintf("%s  %-40s %-5s  %s", indent, fleetRule(p), s.ToolRules[p], note), " "))
	}
	return out
}

// fleetRule is a rule's pattern as a screen shows it: itself while it is
// printable ASCII, else quoted.
func fleetRule(p string) string {
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] > 0x7e {
			return fleetQuote(p)
		}
	}
	return p
}

// --- the operations, shared by the command line and the terminal ---

// fleetOps are the operator's changes to a fleet. The command line builds one
// over the files; the terminal app builds one over the running process's own
// state (defaultFleet, its live settings).
type fleetOps struct {
	cfgDir  string
	dataDir string // where the machines' memory is kept (fleetmemory.go)
	store   *fleet.Store
	surface string // cli | tui | dashboard | dashboard-remote, for the Security record
	// by says who asked for a confirmation when it is not the operator at this
	// computer (a dashboard session elsewhere); "" otherwise. It goes into the
	// confirmation's records.
	by string
	// schemes is the fleet's approval schemes, saved when changed. The command
	// line reads them from the settings file at each call, so what it saves
	// over is what is there now.
	schemes func() (*approval.Fleet, error)
	// local is the master's local scheme as its settings file holds it: what
	// a new machine's scheme is copied from.
	local func() (approval.Scheme, error)
}

// machine finds a machine by the name it carries now, or by its id.
func (o fleetOps) machine(ref string) (fleet.Machine, error) {
	m, err := o.store.ByName(ref)
	if errors.Is(err, fleet.ErrUnknown) {
		m, err = o.store.Machine(ref)
	}
	if errors.Is(err, fleet.ErrUnknown) {
		return fleet.Machine{}, fmt.Errorf("no machine %s in this fleet (tenant fleet list)", fleetQuote(ref))
	}
	return m, err
}

// fleetPreview is the scheme a machine would be confirmed with, and where
// each part of it came from.
type fleetPreview struct {
	Scheme approval.Scheme
	// Kept: the scheme was kept under this id by a revoke, and the machine
	// adopted the id. Nothing is copied from the master then.
	Kept       bool
	Master     map[string]approval.Mode // the master's own category modes
	Defaults   map[string]string        // the fleet's default categories for new machines
	FleetRules map[string]string        // the fleet's rules for new machines
	Groups     []approval.Group         // the machine's groups, in the order their rules were laid
	Set        map[string]string        // what the operator set on the confirm screen
}

// parseSet reads the operator's category=mode pairs for the confirm screen.
func parseSet(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		cat, mode, ok := strings.Cut(p, "=")
		cat = strings.ToLower(strings.TrimSpace(cat))
		m, valid := approval.ParseMode(strings.TrimSpace(mode))
		if !ok || !valid || !slices.Contains(approval.Categories, cat) {
			return nil, fmt.Errorf("%s is not category=mode: a category is one of %s, a mode ask, allow or deny",
				fleetQuote(p), strings.Join(approval.Categories, ", "))
		}
		out[cat] = m.String()
	}
	return out, nil
}

// preview works out the scheme machine m starts with: the one kept under its
// id if it adopted one, else its fresh copy (approval.Fleet.Fresh): a copy of
// the master's local scheme with exec, write, destructive, send and sandbox
// no looser than ask, the fleet's default categories, the fleet's rules and
// its groups' rules laid over it; then what the operator set here.
func (o fleetOps) preview(m fleet.Machine, set map[string]string) (*approval.Fleet, fleetPreview, error) {
	p := fleetPreview{Set: set}
	f, err := o.schemes()
	if err != nil {
		return nil, p, err
	}
	local, err := o.local()
	if err != nil {
		return nil, p, err
	}
	p.Master, p.FleetRules = approval.SavedModes(local.Permissions), f.Rules().Snapshot()
	if kept, err := f.Scheme(m.ID); err == nil {
		p.Scheme, p.Kept = kept, true
	} else if p.Scheme, err = f.Fresh(m.ID, local); err != nil {
		return nil, p, err
	} else {
		// Fresh worked, so neither can fail.
		p.Defaults, _ = f.Defaults()
		p.Groups, _ = f.GroupsOf(m.ID)
	}
	for cat, mode := range set {
		p.Scheme.Permissions[cat] = mode
	}
	return f, p, nil
}

// audit files an operator's change to the fleet: its audit record in the
// Security log, and what happened in the fleet log. Both are written before it
// returns, so a one-shot command's reach the log book as a running Tenant's
// do. (A fleet being made has its own line in the fleet log: initFleet.)
func (o fleetOps) audit(m fleet.Machine, change, detail string) {
	_ = catalog.FleetMachineChanged.Audit(context.Background(), "name", m.Name, "machine", m.ID, "change", change, "detail", detail,
		"actor", actorOperator, "surface", o.surface)
	if change != "initialised" {
		_ = fleet.MachineChanged.Audit(context.Background(), "name", m.Name, "machine", m.ID, "change", change, "detail", detail, "surface", o.surface)
	}
}

// confirmMachine makes a pending machine a confirmed one: the operator typed the
// six digits that machine shows. It is the only caller of Store.Confirm
// outside the store's own package, and its own callers are the command line
// (fleetConfirmCmd, which reads the digits from a terminal), the terminal
// app's /fleet confirm and the dashboard's Fleet page, which takes the digits
// from a browser signed in on this computer or, once the operator turned
// remote confirmation on, from a signed-in browser elsewhere that gave the
// sign-in key again (dashFleet.Confirm; o.by then says which): nothing a
// relay or a tool can reach.
//
// The machine's scheme is saved first and the machine confirmed second, so a
// machine is never callable without a scheme; if the confirmation fails, a
// scheme made for it is taken back.
func (o fleetOps) confirmMachine(ref, code string, set map[string]string) (fleet.Machine, fleetPreview, error) {
	m, err := o.machine(ref)
	if err != nil {
		return m, fleetPreview{}, err
	}
	refuse := func(err error) (fleet.Machine, fleetPreview, error) {
		reason := err.Error()
		if o.by != "" {
			reason += "; asked by " + o.by
		}
		_ = catalog.FleetConfirmRefused.Audit(context.Background(), "name", m.Name, "machine", m.ID, "reason", reason,
			"actor", actorOperator, "surface", o.surface)
		_ = fleet.ConfirmRefused.Audit(context.Background(), "name", m.Name, "machine", m.ID, "reason", reason, "surface", o.surface)
		return m, fleetPreview{}, err
	}
	if m.State != fleet.StatePending {
		return refuse(fmt.Errorf("%s is %s, not waiting for confirmation", fleetName(m.Name), m.State))
	}
	id, err := fleetIdentity(o.cfgDir)
	if err != nil {
		return m, fleetPreview{}, err
	}
	code = strings.Join(strings.Fields(code), "")
	if subtle.ConstantTimeCompare([]byte(fleet.ConfirmCode(id.Fingerprint, m.Fingerprint)), []byte(code)) != 1 {
		return refuse(fleet.ErrWrongCode)
	}
	f, p, err := o.preview(m, set)
	if err != nil {
		return m, p, err
	}
	if err := f.Put(m.ID, p.Scheme); err != nil {
		return m, p, err
	}
	got, err := o.store.Confirm(id.Fingerprint, m.ID, code)
	if err != nil {
		if !p.Kept {
			f.Delete(m.ID)
		}
		return refuse(err)
	}
	// The first inventory (TEN-532): facts about the machine, each built by
	// the master from one field the machine reported, checked
	// (fleetInventoryFacts). The process that holds the fleet writes them
	// into the machine's memory, at its next tick; this may be another one.
	inventory := ""
	if err := markInventory(o.dataDir, got.ID); err != nil {
		inventory = "; its inventory was not recorded: " + err.Error()
	}
	detail, _ := json.Marshal(struct {
		Reported fleet.Reported    `json:"reported_by_the_machine"`
		Set      map[string]string `json:"set_by_the_operator,omitempty"`
		Kept     bool              `json:"scheme_kept_with_the_id,omitempty"`
		By       string            `json:"asked_by,omitempty"`
	}{got.Reported, set, p.Kept, o.by})
	o.audit(got, "confirmed", string(detail)+inventory)
	return got, p, nil
}

// revoke ends a machine: its certificate is refused from now on and the
// process that holds the fleet closes its link within a second. With
// deleteScheme its approval scheme and its record go too, and its id can no
// longer be adopted; without, both stay under the id.
func (o fleetOps) revoke(ref string, deleteScheme bool) (fleet.Machine, error) {
	m, err := o.machine(ref)
	if err != nil {
		return m, err
	}
	if err := o.store.Revoke(m.ID); err != nil {
		return m, err
	}
	if !deleteScheme {
		o.audit(m, "revoked", "its approval scheme is kept under its id")
		return m, nil
	}
	f, err := o.schemes()
	if err != nil {
		return m, fmt.Errorf("%s is revoked, but its approval scheme was not deleted: %w", fleetName(m.Name), err)
	}
	f.Delete(m.ID)
	// And its groups: the id can no longer come back. (A revoke that keeps the
	// scheme keeps them, for a machine that adopts the id.) Groups that can't
	// be read keep the id in their text: harmless once the record is gone, as
	// no machine can have the id again, and said.
	groups, groupsErr := f.LeaveGroups(m.ID)
	if err := o.store.Remove(m.ID); err != nil {
		return m, fmt.Errorf("%s is revoked, but its record was not removed: %w", fleetName(m.Name), err)
	}
	// Its memory goes with it (TEN-532): the shadow, its snapshots and what
	// was kept aside. The copies on the machine are the machine's.
	memory, err := removeFleetMemory(o.cfgDir, o.dataDir, m.ID)
	switch {
	case groupsErr != nil:
		memory = "it is still listed in the groups, which can't be read; " + memory
	case len(groups) > 0:
		memory = "it left group(s) " + strings.Join(groups, ", ") + "; " + memory
	}
	if err != nil {
		o.audit(m, "removed", "revoked; its record and approval scheme are deleted; its memory was not: "+err.Error())
		return m, fmt.Errorf("%s is revoked and its record and approval scheme are deleted, but its memory was not: %w", fleetName(m.Name), err)
	}
	o.audit(m, "removed", "revoked; its record and approval scheme are deleted; "+memory)
	if groupsErr != nil {
		return m, fmt.Errorf("%s is revoked and its record and approval scheme are deleted, but it is still listed in the groups: %w", fleetName(m.Name), groupsErr)
	}
	return m, nil
}

func (o fleetOps) rename(ref, name string) (fleet.Machine, error) {
	m, err := o.machine(ref)
	if err != nil {
		return m, err
	}
	if err := o.store.Rename(m.ID, name); err != nil {
		return m, err
	}
	o.audit(m, "renamed", "now "+name)
	return m, nil
}

// --- screens ---

// confirmScreen is what the operator reads before typing the six digits.
func confirmScreen(m fleet.Machine, p fleetPreview) string {
	lines := []string{
		fmt.Sprintf("Confirm machine %s", fleetName(m.Name)),
		fmt.Sprintf("  id          %s   (given by this master; the name is the one it was enrolled under)", m.ID),
		fmt.Sprintf("  enrolled    %s", fleetTime(m.EnrolledAt)),
		fmt.Sprintf("  certificate %s", m.Fingerprint),
	}
	lines = append(lines, reportedLines(m.Reported, "  ")...)
	if p.Kept {
		lines = append(lines, "  Approval scheme it starts with (kept under this id from the machine that had it before; nothing is copied from this master):")
	} else {
		lines = append(lines, "  Approval scheme it starts with (a copy of this master's local scheme; exec, write, destructive, send and sandbox",
			"  no looser than ask, whatever the master allows; then the fleet's defaults for new machines, then the rules of its",
			"  groups, each laid over the one before and winning):")
	}
	lines = append(lines, schemeLines(p.Scheme, "    ", p.catNote, p.ruleNote)...)
	lines = append(lines, "  After this the two schemes are independent: a later change on the master does not move this machine.")
	return strings.Join(lines, "\n")
}

// catNote says where a category's mode on the confirm screen came from, when
// it is not simply the master's own: the operator set it here, it is the
// fleet's default for new machines, or the master allows what a machine
// starts no looser than ask.
func (p fleetPreview) catNote(cat, mode string) string {
	switch master, known := p.Master[cat]; {
	case p.Set[cat] != "":
		return "set by you, for this machine only"
	case !p.Kept && p.Defaults[cat] != "":
		return "fleet default"
	case !p.Kept && known && master.String() != mode:
		return "(this master: " + master.String() + ")"
	}
	return ""
}

// ruleNote marks a rule a layer laid: the last of the machine's groups that
// has it (the one that won), else the fleet's rules for new machines.
func (p fleetPreview) ruleNote(pattern, mode string) string {
	for _, g := range slices.Backward(p.Groups) {
		if g.ToolRules[pattern] != "" {
			if g.ToolRules[pattern] == mode {
				return "group " + g.Name
			}
			break
		}
	}
	if p.FleetRules[pattern] == mode {
		return "fleet rule"
	}
	return ""
}

// recopy gives a machine its scheme again: from "defaults" (Apply fleet
// rules) the fleet's rules and then its groups' rules, in name order, are
// laid over the scheme it has, its categories kept (Fleet.Reapply); from
// "master" it gets its fresh copy, as a machine confirmed now would
// (Fleet.Fresh). f is the fleet's schemes (o.schemes). said is what was done,
// in words.
func (o fleetOps) recopy(f *approval.Fleet, m fleet.Machine, from string) (said string, err error) {
	switch from {
	case "defaults":
		groups, err := f.Reapply(m.ID)
		if err != nil {
			return "", err
		}
		said = "the fleet's rules laid over its approval scheme again"
		if len(groups) > 0 {
			said += ", then the rules of group(s) " + strings.Join(groups, ", ")
		}
		o.audit(m, "scheme changed", said)
	case "master":
		if m.State == fleet.StatePending || m.State == fleet.StateRevoked {
			return "", fmt.Errorf("%s is %s: a machine gets its scheme when it is confirmed", fleetName(m.Name), m.State)
		}
		local, err := o.local()
		if err != nil {
			return "", err
		}
		s, err := f.Fresh(m.ID, local)
		if err != nil {
			return "", err
		}
		groups, _ := f.GroupsOf(m.ID) // Fresh read them
		if err := f.Put(m.ID, s); err != nil {
			return "", err
		}
		said = "a fresh copy of the master's local scheme, with the fleet's defaults"
		if len(groups) > 0 {
			names := make([]string, len(groups))
			for i, g := range groups {
				names[i] = g.Name
			}
			said += " and the rules of group(s) " + strings.Join(names, ", ")
		}
		o.audit(m, "scheme changed", said)
	default:
		return "", fmt.Errorf("--from %s: want defaults or master", fleetQuote(from))
	}
	return said, nil
}

// listText is the fleet's machines, one per line. links is the state of each
// machine's link, which only the process that holds the fleet knows; nil
// leaves the column out.
func listText(machines []fleet.Machine, links map[string]fleet.MachineStatus) string {
	if len(machines) == 0 {
		return "  no machines yet: tenant fleet enroll <name>"
	}
	head := fmt.Sprintf("  %-20s %-10s", "NAME", "STATE")
	if links != nil {
		head += fmt.Sprintf(" %-30s", "LINK")
	}
	lines := []string{head + fmt.Sprintf(" %-12s %-20s %s", "OS*", "USER*", "ID")}
	for _, m := range machines {
		line := fmt.Sprintf("  %-20s %-10s", fleetName(m.Name), m.State)
		if links != nil {
			line += fmt.Sprintf(" %-30s", linkText(m, links[m.ID]))
		}
		lines = append(lines, line+fmt.Sprintf(" %-12s %-20s %s", fleetQuote(m.Reported.OS), fleetQuote(m.Reported.User), m.ID))
	}
	return strings.Join(append(lines, "  * reported by the machine: its own words, not checked"), "\n")
}

// linkText is one machine's link as the holder of the fleet sees it.
func linkText(m fleet.Machine, st fleet.MachineStatus) string {
	switch {
	case m.State == fleet.StatePending:
		return "waits for confirmation"
	case m.State != fleet.StateConfirmed:
		return "-"
	case st.Online:
		return "online since " + fleetTime(st.Since)
	case st.Since.IsZero():
		return "offline (not seen yet)"
	}
	return "offline, last seen " + fleetTime(st.Since)
}

// showText is everything known about one machine.
func showText(m fleet.Machine, link *fleet.MachineStatus, scheme *approval.Scheme, schemeErr error) string {
	lines := []string{
		"Machine " + fleetName(m.Name),
		"  id          " + m.ID,
		"  state       " + string(m.State),
		"  certificate " + m.Fingerprint,
		"  enrolled    " + fleetTime(m.EnrolledAt),
	}
	if !m.ConfirmedAt.IsZero() {
		lines = append(lines, "  confirmed   "+fleetTime(m.ConfirmedAt))
	}
	if !m.RevokedAt.IsZero() {
		lines = append(lines, "  revoked     "+fleetTime(m.RevokedAt))
	}
	if link != nil {
		l := "  link        " + linkText(m, *link)
		if link.Online {
			l += fmt.Sprintf(", from %s, link version %d, %d call(s) in flight", fleetQuote(link.Remote), link.Link, link.InFlight)
		}
		lines = append(lines, l)
		if link.LastError != "" {
			lines = append(lines, "  last ended  "+fleetQuote(link.LastError))
		}
	}
	lines = append(lines, reportedLines(m.Reported, "  ")...)
	switch {
	case scheme != nil:
		lines = append(lines, "  Approval scheme (this machine's own; tenant fleet permissions "+fleetName(m.Name)+"):")
		lines = append(lines, schemeLines(*scheme, "    ", nil, nil)...)
	case m.State == fleet.StatePending:
		lines = append(lines, "  Approval scheme: none until it is confirmed (tenant fleet confirm "+fleetName(m.Name)+")")
	default:
		lines = append(lines, fmt.Sprintf("  Approval scheme: none (%v). Every call to it is refused.", schemeErr))
	}
	return strings.Join(lines, "\n")
}

// --- the command line ---

func cmdFleet(_ context.Context, args []string) error {
	if len(args) == 0 {
		return fleetUsage()
	}
	run := map[string]func([]string) error{
		"init": fleetInitCmd, "enroll": fleetEnrollCmd, "list": fleetListCmd, "ls": fleetListCmd, "show": fleetShowCmd,
		"confirm": fleetConfirmCmd, "rename": fleetRenameCmd, "revoke": fleetRevokeCmd,
		"permissions": fleetPermissionsCmd, "perms": fleetPermissionsCmd, "relay": fleetRelayCmd,
		"update": fleetUpdateCmd, "recover": fleetRecoverCmd, "address": fleetAddressCmd, "group": fleetGroupCmd,
		"remote-confirm": fleetRemoteConfirmCmd,
	}[args[0]]
	if run == nil {
		return fleetUsage()
	}
	return run(args[1:])
}

func fleetUsage() error {
	return errors.New(`usage: tenant fleet <command>

  init [--listen ADDR] [--address HOST:PORT]...
                          make this Tenant a fleet master (once); set where the fleet
                          port listens and the addresses its machines dial
  enroll <name> [--ttl 15m] [--adopt ID]
                          print a one-time code and the line to paste on the new machine,
                          with Tenant on it or with nothing on it
  address [list | add HOST:PORT | remove HOST:PORT]
                          the addresses a code tells a new machine to dial, one at a time
  list                    the machines, and which process holds the fleet
  show <name>             everything known about one machine
  confirm <name> [--set category=mode]...
                          confirm a pending machine: you type the six digits it shows
  rename <name> <new-name>
  revoke <name> [--keep-scheme | --delete-scheme]
  permissions <name> [set <category> <mode> | add <pattern> <mode> | rm <pattern>]
  permissions <name> --from defaults|master
                          lay the fleet's rules and its groups' rules over one machine again,
                          or copy the master's scheme again (with the fleet's defaults and groups)
  permissions --group <group> | --all  [set <category> <mode> | add <pattern> <mode> | rm <pattern>]
  permissions --group <group> | --all  --from defaults|master
                          the same change to each member of a group, or to every confirmed
                          machine: one line each; exits 1 if any was not changed
  permissions --defaults [set <category> <mode> | unset <category> | add <pattern> <mode> | rm <pattern>]
                          the categories and rules every newly confirmed machine starts with
  group [show <group> | create <group> | delete <group>]
  group add|remove <group> <machine>...
  group rules <group> [add <pattern> <mode> | rm <pattern>]
                          machine groups: changing one moves no machine
  group apply <group>     lay the fleet's rules, then each member's groups' rules (by name), over
                          each confirmed member's scheme, on purpose
  relay [discord|imessage [on|off]]
                          whether a turn from that relay may reach the fleet at all (off by default)
  remote-confirm [on|off] whether a signed-in dashboard browser on another computer may confirm a
                          machine, with the sign-in key again (off by default)
  update <name> | --all | --group <group> [--version vX.Y.Z] [--allow-older] [--arch amd64|arm64]
                          send a machine a newer Tenant over its link, through the running
                          Tenant that holds the fleet; the machine rolls back if it fails
  recover <name> [--was ID]
                          read a machine's memory made under another fleet key (a master rebuilt
                          from nothing): give the recovery identity at the terminal or on stdin`)
}

// fleetCLI is what a `tenant fleet` command works with.
type fleetCLI struct {
	c     *commonFlags
	store *fleet.Store
	ops   fleetOps
	pos   []string // the positional arguments
}

// openFleetCLI parses a command's flags and opens the fleet's files. Flags
// are taken wherever they stand among the arguments (parseInterspersed): the
// flag package alone stops at the first argument that is not a flag, and a
// --config after one would be read as an argument, leaving the command to
// work on the default config dir.
func openFleetCLI(fs *flag.FlagSet, args []string) (*fleetCLI, error) {
	c := bindCommon(fs)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	if err := c.resolveDirs(); err != nil {
		return nil, err
	}
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		return nil, err
	}
	c.lc = lc
	store := fleet.Open(c.cfgDir)
	return &fleetCLI{c: c, store: store, pos: pos, ops: fleetOps{
		cfgDir: c.cfgDir, dataDir: c.dataDir, store: store, surface: surfaceCLI,
		schemes: func() (*approval.Fleet, error) { return fileFleetSchemes(c.cfgDir, c.agent) },
		local: func() (approval.Scheme, error) {
			stg, err := loadSettings(c.cfgDir, c.agent)
			return approval.Scheme{Permissions: stg.Permissions, ToolRules: stg.ToolRules}, err
		},
	}}, nil
}

// fileFleetSchemes is the fleet's approval schemes as the settings file holds
// them now, for a command-line process: each change is written back to the
// file, where the running master's settings watcher finds it.
//
// ponytail: the settings file has no lock between processes. The file is read
// again at each save, so this command keeps what the master saved meanwhile;
// a master that saves its own stale copy inside the few seconds before its
// watcher reads this change undoes it, and the machine is then refused every
// call until `tenant fleet permissions <name> --from master`. A lock both
// writers take, if that is ever seen.
func fileFleetSchemes(cfgDir, agent string) (*approval.Fleet, error) {
	stg, err := loadSettings(cfgDir, agent)
	if err != nil {
		return nil, err // never written over what could not be read
	}
	f := approval.NewFleet(categorize)
	_, problems := f.Load(stg.FleetSchemes, stg.FleetRules)
	_, more := f.LoadLayers(stg.FleetPermissions, stg.FleetGroups)
	if problems = append(problems, more...); len(problems) > 0 {
		fmt.Fprintln(fleetNotes, "note: "+strings.Join(problems, "\nnote: "))
	}
	var mu sync.Mutex
	// Each hook reads the file again and writes its own two keys over it.
	save := func(set func(stg *settings)) {
		mu.Lock()
		defer mu.Unlock()
		if fresh, err := loadSettings(cfgDir, agent); err == nil {
			stg = fresh
		}
		set(stg)
		saveSettingsOrTell(stg, cfgDir, agent, func(s string) { fmt.Fprintln(fleetNotes, s) })
	}
	f.SetPersist(func(snapshot func() (map[string]json.RawMessage, json.RawMessage)) {
		save(func(stg *settings) { stg.FleetSchemes, stg.FleetRules = snapshot() })
	})
	f.SetPersistLayers(func(snapshot func() (json.RawMessage, json.RawMessage)) {
		save(func(stg *settings) { stg.FleetPermissions, stg.FleetGroups = snapshot() })
	})
	return f, nil
}

// fleetNotes is where a command writes what is not its result: instructions,
// notes, a prompt. A person reads them; a script that takes the command's
// standard output (the enrollment code) does not get them. The command-line
// process writes here, and the terminal app only while it has handed the
// terminal to init's recovery step (/fleet init); serve never does.
var fleetNotes io.Writer = os.Stderr

// fleetAsk is how a command asks the operator; tests replace it.
var fleetAsk = terminalAsk

// terminalAsk reads one line the operator types at the terminal. It refuses a
// standard input that is not a terminal: what it asks for is a person's
// answer, and a command run by a script or by the agent has no person at it.
func terminalAsk(prompt string) (string, error) {
	if fd := os.Stdin.Fd(); !isatty.IsTerminal(fd) && !isatty.IsCygwinTerminal(fd) {
		return "", errors.New("this needs a person at a terminal: standard input is not one")
	}
	fmt.Fprint(fleetNotes, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// fleetInitWait is how long init and enroll wait for a running Tenant to take
// the fleet before they say that none has.
var fleetInitWait = 2*settingsWatchInterval + 2*time.Second

// holderLine says which process holds the fleet in cfgDir, waiting up to
// wait for one to take it.
func holderLine(cfgDir string, wait time.Duration) string {
	deadline := time.Now().Add(wait)
	for {
		// A holder takes the lock first and records who and where it is a
		// moment later: the line waits for that record while the wait lasts.
		h, held := fleet.CurrentHolder(cfgDir)
		if held && (h.PID != 0 && h.Address != "" || !time.Now().Before(deadline)) {
			return strings.TrimPrefix((&fleet.HeldError{Holder: h}).Error(), "fleet: ")
		}
		if !time.Now().Before(deadline) {
			return "no Tenant process holds the fleet, so no machine can connect. It is held while `tenant serve` (or the terminal app) runs on this config: " +
				"a master with a fleet should run `tenant serve` as its service (tenant service install)"
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// fleetDefaultAddresses guesses what a machine can dial to reach a master
// listening on listen: the bind host if it names one, else this computer's
// name and its IPv4 addresses. A machine tries each in turn.
func fleetDefaultAddresses(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "0" {
		return nil
	}
	hosts := []string{host}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		hosts = nil
		if name, err := os.Hostname(); err == nil {
			hosts = append(hosts, name)
		}
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, n.IP.String())
			}
		}
	}
	var out []string
	for _, h := range hosts {
		if fleetName(h) == h && len(out) < 16 { // a name or an IPv4 address fleet.json takes
			out = append(out, net.JoinHostPort(h, port))
		}
	}
	return out
}

// initFleet makes this Tenant a master if it is not one, and sets what the
// operator passed. It is what `tenant fleet init` does, what enroll does
// first, and what /fleet init and the Fleet page's Set up the fleet do in a
// running Tenant (o and lc are then its own). by says who asked, for the
// Security record ("" at a terminal). created reports that the identity was
// made now.
func initFleet(o fleetOps, lc *launchConfig, listen string, addresses []string, by string) (id fleet.Identity, created bool, err error) {
	// Before anything is made: a refused address leaves no half-made fleet.
	if err := fleet.CheckAddresses(addresses); err != nil {
		return id, false, err
	}
	if listen != "" {
		if _, _, err := net.SplitHostPort(listen); err != nil {
			return id, false, fmt.Errorf("--listen %s is not a host:port address", fleetQuote(listen))
		}
		// Before the identity: a running master starts its listener the moment
		// it sees one, on the address config.json names then.
		if err := persistLaunchConfig(o.cfgDir, lc, func(lc *launchConfig) { lc.Fleet.Listen = listen }); err != nil {
			return id, false, err
		}
	}
	path := fleet.MasterIdentityPath(o.cfgDir)
	// ponytail: two inits at the same instant could each mint an identity and
	// the second replace the first; take fleet.json's lock around this if
	// init is ever scripted in parallel.
	if id, err = fleet.LoadIdentity(path); errors.Is(err, fs.ErrNotExist) {
		if id, err = fleet.MintIdentity(path); err == nil {
			created = true
			detail := "certificate " + id.Fingerprint
			if by != "" {
				detail += "; asked by " + by
			}
			o.audit(fleet.Machine{Name: "(this master)"}, "initialised", detail)
		}
	}
	if err != nil {
		return id, false, err
	}
	// The fleet key every machine's memory is encrypted to (TEN-532). A fleet
	// made before there was one gets it now. The recovery key needs a person
	// at a terminal and is init's own step (ensureRecovery), not enroll's.
	if err := ensureFleetKey(o.cfgDir); err != nil {
		return id, created, err
	}
	if len(addresses) == 0 {
		if have, err := o.store.Addresses(); err != nil {
			return id, created, err
		} else if len(have) == 0 {
			addresses = fleetDefaultAddresses(cmp.Or(lc.Fleet.Listen, defaultFleetListen))
		}
	}
	before, _ := o.store.Addresses()
	if len(addresses) > 0 {
		if err := o.store.SetAddresses(addresses); err != nil {
			return id, created, err
		}
	}
	// The fleet log, written before this returns so a command's line is kept.
	switch now, _ := o.store.Addresses(); {
	case created:
		_ = fleet.Initialised.Audit(context.Background(), "certificate", id.Fingerprint, "listen", cmp.Or(lc.Fleet.Listen, defaultFleetListen),
			"addresses", strings.Join(now, ", "), "surface", o.surface)
	case !slices.Equal(before, now):
		_ = fleet.AddressesChanged.Audit(context.Background(), "addresses", strings.Join(now, ", "), "surface", o.surface)
	}
	return id, created, nil
}

// initSaid is what init says of the fleet it made or found: its certificate,
// where its port listens, what its machines dial and how its key is kept.
func initSaid(o fleetOps, lc *launchConfig, id fleet.Identity, created bool) string {
	var b strings.Builder
	if created {
		b.WriteString("this Tenant is now a fleet master\n")
	} else {
		b.WriteString("this Tenant is a fleet master already; nothing was replaced\n")
	}
	fmt.Fprintf(&b, "  certificate    %s\n  listens on     %s   (fleet.listen)\n", id.Fingerprint, cmp.Or(lc.Fleet.Listen, defaultFleetListen))
	if addrs, _ := o.store.Addresses(); len(addrs) == 0 {
		b.WriteString("  machines dial  nothing yet: tenant fleet address add HOST:PORT\n")
	} else {
		fmt.Fprintf(&b, "  machines dial  %s   (change: tenant fleet address add|remove HOST:PORT)\n", strings.Join(addrs, ", "))
		if w := addressWarning(addrs); w != "" {
			b.WriteString("  " + w + "\n")
		}
	}
	return b.String() + "  fleet key      " + fleetKeyWords(o.cfgDir)
}

func fleetInitCmd(args []string) error {
	fs := flag.NewFlagSet("fleet init", flag.ContinueOnError)
	listen := fs.String("listen", "", "host:port the fleet port binds (default "+defaultFleetListen+"); saved as fleet.listen in config.json")
	var addresses stringList
	fs.Var(&addresses, "address", "a host:port the machines dial to reach this master (repeatable; replaces the list)")
	wait := fs.Duration("wait", fleetInitWait, "how long to wait for a running Tenant to take the fleet")
	cli, err := openFleetCLI(fs, args)
	if err != nil {
		return err
	}
	if len(cli.pos) != 0 {
		return errors.New("usage: tenant fleet init [--listen ADDR] [--address HOST:PORT]...")
	}
	id, created, err := initFleet(cli.ops, cli.c.lc, *listen, addresses, "")
	if err != nil {
		return err
	}
	fmt.Println(initSaid(cli.ops, cli.c.lc, id, created))
	recErr := ensureRecovery(cli.store)
	if rec, proven, err := cli.store.Recovery(); err == nil && rec != "" {
		fmt.Printf("  recovery key   proven %s; its identity is yours to keep, Tenant has only its public half\n", fleetTime(proven))
	}
	fmt.Println("  " + holderLine(cli.c.cfgDir, *wait))
	return recErr
}

// ensureFleetKey makes the fleet key if the fleet has none: sealed by the
// operating system's secret protector where there is one, an owner-only file
// otherwise (shadow.CreateKey). A key that is there is left as it is: a
// second one would orphan every snapshot made under the first.
func ensureFleetKey(cfgDir string) error {
	k, err := shadow.CreateKey(fleet.KeyPath(cfgDir))
	if errors.Is(err, shadow.ErrKeyExists) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the fleet key: %w", err)
	}
	return k.Close()
}

// fleetKeyWords says how the fleet key is kept, for init's summary.
// It reads nothing of the file: the key is never read but into locked memory.
func fleetKeyWords(cfgDir string) string {
	if _, err := os.Lstat(fleet.KeyPath(cfgDir)); err != nil {
		return "missing: " + err.Error()
	}
	if _, err := secrets.NewUserProtector(); err == nil {
		return "sealed by this system's secret protector (" + fleet.KeyPath(cfgDir) + ")"
	}
	return "an owner-only file (" + fleet.KeyPath(cfgDir) + "): this system has no secret protector Tenant can use"
}

// recoveryTerminal is where init shows the recovery identity, once, and
// reads it back. Tests replace openRecoveryTerminal.
type recoveryTerminal struct {
	// out is the terminal's screen.
	out io.Writer
	// read is one line typed at the terminal, not echoed, moved into locked
	// memory; nil when the line was empty.
	read func() (*secrets.LockedBuffer, error)
}

var openRecoveryTerminal = func() (*recoveryTerminal, error) {
	isTTY := func(f *os.File) bool { return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()) }
	if !isTTY(os.Stdin) || !isTTY(os.Stdout) {
		return nil, errors.New("standard input and output are not both a terminal")
	}
	return &recoveryTerminal{out: os.Stdout, read: func() (*secrets.LockedBuffer, error) {
		line, err := xterm.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(os.Stdout)
		if err != nil || len(line) == 0 {
			clear(line)
			return nil, err
		}
		return secrets.NewLockedBuffer(line) // which zeroes line
	}}, nil
}

// errRecoveryNotSetUp ends an init that did all the rest and could not make
// the recovery key: init exits 1, so nothing takes the fleet for finished.
var errRecoveryNotSetUp = errors.New("the fleet is set up but its machines' memory is not: the recovery key needs a person at a terminal. Run `tenant fleet init` there")

// recoveryTries is how many times the operator may give the identity back.
const recoveryTries = 3

// ensureRecovery makes the fleet's recovery key if it has none, at a
// terminal (gap6-fleet-plan.md section 6, "Encryption"): the identity is
// shown once, from locked memory and nowhere else, and the operator gives it
// back; only once a test decryption opens with what they gave is the public
// half recorded in fleet.json. Without a terminal nothing is shown, nothing
// is recorded, and errRecoveryNotSetUp says so: the identity is never
// written to a pipe or a log. A wrong identity or a cancel records nothing,
// and the identity shown is void.
func ensureRecovery(store *fleet.Store) error {
	rec, proven, err := store.Recovery()
	switch {
	case err != nil:
		return err
	case rec != "" && proven.IsZero():
		return proveRecovery(store, rec)
	case rec != "":
		return nil
	}
	tty, err := openRecoveryTerminal()
	if err != nil {
		fmt.Fprintf(fleetNotes, "note: the recovery key is not made: %v.\n", err)
		return errRecoveryNotSetUp
	}
	identity, recipient, err := shadow.NewRecovery()
	if err != nil {
		return err
	}
	defer identity.Release()
	fmt.Fprint(tty.out, "\nThe fleet's RECOVERY IDENTITY is below. With it, and only with it, your machines' memory can be read\n"+
		"again if this master's fleet key is lost (a master rebuilt from nothing, or from a plain backup).\n"+
		"Tenant does not keep it and will not show it again. Save it now, away from this computer\n"+
		"(a password manager, or on paper), then clear this terminal's scrollback.\n\n    ")
	if err := identity.With(func(p []byte) error {
		_, err := tty.out.Write(p)
		return err
	}); err != nil {
		return err
	}
	fmt.Fprint(tty.out, "\n\n")
	for try := 1; try <= recoveryTries; try++ {
		fmt.Fprint(tty.out, "Type or paste it back, to prove you saved it (it is not shown as you type; Enter alone cancels): ")
		given, err := tty.read()
		if err != nil || given == nil {
			fmt.Fprintln(fleetNotes, "note: cancelled; nothing was recorded, and the identity shown above is void.")
			return errRecoveryNotSetUp
		}
		err = shadow.ProveRecovery(given, recipient)
		_ = given.Release()
		if err == nil {
			return store.SetRecovery(recipient, time.Now())
		}
		if !errors.Is(err, shadow.ErrNotRecovery) {
			return err
		}
		fmt.Fprintf(tty.out, "That is not the recovery identity shown above (%d of %d tries).\n", try, recoveryTries)
	}
	fmt.Fprintln(fleetNotes, "note: the identity was not given back; nothing was recorded, and the identity shown above is void.")
	return errRecoveryNotSetUp
}

// proveRecovery is init on a fleet whose recovery key was recorded and never
// proven (doctor warns of it): the operator gives the identity they kept, and
// only a test decryption that opens with it records the proof. Nothing new
// is made and nothing is shown.
func proveRecovery(store *fleet.Store, recipient string) error {
	tty, err := openRecoveryTerminal()
	if err != nil {
		fmt.Fprintf(fleetNotes, "note: the recovery key is not proven: %v.\n", err)
		return errRecoveryNotSetUp
	}
	for try := 1; try <= recoveryTries; try++ {
		fmt.Fprint(tty.out, "This fleet's recovery key was never proven. Type or paste the recovery identity you kept (it is not shown as you type; Enter alone cancels): ")
		given, err := tty.read()
		if err != nil || given == nil {
			fmt.Fprintln(fleetNotes, "note: cancelled; the recovery key is still not proven.")
			return errRecoveryNotSetUp
		}
		err = shadow.ProveRecovery(given, recipient)
		_ = given.Release()
		if err == nil {
			return store.SetRecovery(recipient, time.Now())
		}
		if !errors.Is(err, shadow.ErrNotRecovery) {
			return err
		}
		fmt.Fprintf(tty.out, "That is not this fleet's recovery identity (%d of %d tries).\n", try, recoveryTries)
	}
	fmt.Fprintln(fleetNotes, "note: the identity given does not open this fleet's recovery key; it is still not proven.")
	return errRecoveryNotSetUp
}

// fleetRecoverCmd is `tenant fleet recover <name> [--was ID]`: the operator
// brings the fleet's recovery identity (of the fleet the machine's copies
// were made under) so that the machine's memory, made under another fleet
// key, is read and becomes its memory here. The machines' links and memory
// are held by the running Tenant, so this hands the identity to it, as
// `tenant fleet update` hands it an update: POST /fleet/recover with the
// dashboard's token, the identity the request's body. The identity is read
// at a terminal without being shown, or from the first line of standard
// input; never from an argument.
func fleetRecoverCmd(args []string) error {
	fs := flag.NewFlagSet("fleet recover", flag.ContinueOnError)
	was := fs.String("was", "", "the id the machine had under the master that made its copies (`tenant child status` on a re-homed machine names it)")
	hub := fs.String("hub", "", "the running Tenant's dashboard address, when it is not where config.json says")
	cli, err := openFleetCLI(fs, args)
	if err != nil {
		return err
	}
	if len(cli.pos) != 1 {
		return errors.New("usage: tenant fleet recover <name> [--was ID] [--hub ADDR]   (the recovery identity is read at the terminal or from standard input, never from an argument)")
	}
	m, err := cli.ops.machine(cli.pos[0])
	if err != nil {
		return err
	}
	// Where it would go is settled before the identity is read, and before
	// anything is sent: plain HTTP only to this computer.
	base := attachBaseURL(*hub, cli.c.lc)
	if err := recoverHubSafe(base); err != nil {
		return exitError{2, err.Error()}
	}
	identity, err := readRecoveryInput()
	if err != nil {
		return err
	}
	defer clear(identity)
	token, _ := dashboardClientToken(cli.c.cfgDir, cli.c.lc, true)
	q := url.Values{"machine": {m.ID}, "confirm": {"yes"}}
	if *was != "" {
		q.Set("was", *was)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/fleet/recover?"+q.Encode(), bytes.NewReader(identity))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 3 * time.Minute, Transport: fleetRecoverTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("no running Tenant answers at %s, and only the Tenant that holds the fleet (tenant serve) can read a machine's memory: nothing was sent (%v)", base, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		return fmt.Errorf("the running Tenant at %s answered %s: nothing changed", base, resp.Status)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc == nil {
		return errors.New("the running Tenant gave no answer")
	}
	if e := loc.Query().Get("err"); e != "" {
		return exitError{1, fleetName(m.Name) + ": " + e}
	}
	fmt.Println(loc.Query().Get("msg"))
	return nil
}

// fleetRecoverTransport is what fleet recover sends with; nil is the
// default. Tests replace it.
var fleetRecoverTransport http.RoundTripper

// recoverHubSafe allows the recovery identity to go to base only where
// nobody on the way can read it: an https:// address, or plain http to this
// computer (an IP that is loopback, or a name every address of which is).
// It reads every machine's memory, and with it a copy could be forged.
func recoverHubSafe(base string) error {
	u, err := url.Parse(base)
	if err == nil && u.Scheme == "https" && u.Host != "" {
		return nil
	}
	refuse := fmt.Errorf("nothing was sent: the recovery identity would go to %s in clear text, over plain http to an address that is not this computer. "+
		"Run `tenant fleet recover` on the master itself, or give --hub the running Tenant's https:// address", base)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		return refuse
	}
	var ips []net.IP
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		ips = []net.IP{ip}
	} else {
		addrs, err := net.LookupHost(u.Hostname())
		if err != nil {
			return refuse
		}
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
	}
	if len(ips) == 0 {
		return refuse
	}
	for _, ip := range ips {
		if ip == nil || !ip.IsLoopback() {
			return refuse
		}
	}
	return nil
}

// readRecoveryInput reads the recovery identity for fleet recover: at a
// terminal without echo, else the first line of standard input. Tests
// replace it.
var readRecoveryInput = func() ([]byte, error) {
	var line []byte
	if isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		fmt.Fprint(fleetNotes, "The fleet's recovery identity (it is not shown as you type): ")
		b, err := xterm.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(fleetNotes)
		if err != nil {
			return nil, err
		}
		line = b
	} else {
		r := bufio.NewReaderSize(io.LimitReader(os.Stdin, 1<<10), 1<<10)
		b, err := r.ReadSlice('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read the recovery identity: %w", err)
		}
		line = bytes.Clone(b)
		clear(b)
	}
	if len(bytes.TrimSpace(line)) == 0 {
		clear(line)
		return nil, exitError{2, "no recovery identity: type it at the terminal, or give it on standard input (never as an argument)"}
	}
	return line, nil
}

func fleetEnrollCmd(args []string) error {
	fs := flag.NewFlagSet("fleet enroll", flag.ContinueOnError)
	ttl := fs.Duration("ttl", fleet.DefaultEnrollTTL, "how long the code can be spent (at most 24h)")
	adopt := fs.String("adopt", "", "give the new machine the id of a revoked one, and with it the approval scheme kept under that id")
	wait := fs.Duration("wait", fleetInitWait, "how long to wait for a running Tenant to take the fleet")
	cli, err := openFleetCLI(fs, args)
	if err != nil {
		return err
	}
	if len(cli.pos) != 1 {
		return errors.New("usage: tenant fleet enroll <name> [--ttl 15m] [--adopt ID]")
	}
	name := cli.pos[0]
	if _, _, err := initFleet(cli.ops, cli.c.lc, "", nil, ""); err != nil {
		return err
	}
	e, err := cli.ops.issueCode(name, *ttl, *adopt, "")
	if err != nil {
		return err
	}
	fmt.Println(e.Code)
	w := fleetNotes
	fmt.Fprintln(w, "\n"+enrollText(e))
	if *adopt == "" {
		// A new machine under an old name starts clean unless the operator adopts the old id.
		if all, err := cli.store.Machines(); err == nil {
			for _, old := range all {
				if old.State == fleet.StateRevoked && strings.EqualFold(old.Name, name) {
					fmt.Fprintf(w, "note: a machine revoked on %s was named %s too (id %s). This one starts clean. To give it that id and what was kept under it, make the code with:  tenant fleet enroll %s --adopt %s\n",
						fleetTime(old.RevokedAt), fleetName(old.Name), old.ID, fleetName(name), old.ID)
				}
			}
		}
	}
	if rec, _, err := cli.store.Recovery(); err == nil && rec == "" {
		fmt.Fprintln(w, "note: the fleet has no recovery key yet, so no machine's memory is kept: run `tenant fleet init` at a terminal")
	}
	fmt.Fprintln(w, holderLine(cli.c.cfgDir, *wait))
	return nil
}

func fleetListCmd(args []string) error {
	cli, err := openFleetCLI(flag.NewFlagSet("fleet list", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if _, err := fleetIdentity(cli.c.cfgDir); err != nil {
		return err
	}
	machines, err := cli.store.Machines()
	if err != nil {
		return err
	}
	fmt.Println("Fleet: " + holderLine(cli.c.cfgDir, 0))
	fmt.Println(listText(machines, nil))
	fmt.Println("  Which machines are online is known to the process that holds the fleet: /fleet there.")
	return nil
}

func fleetShowCmd(args []string) error {
	cli, err := openFleetCLI(flag.NewFlagSet("fleet show", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(cli.pos) != 1 {
		return errors.New("usage: tenant fleet show <name>")
	}
	m, err := cli.ops.machine(cli.pos[0])
	if err != nil {
		return err
	}
	fmt.Println(cli.ops.show(m, nil))
	return nil
}

// show is showText with the machine's scheme looked up.
func (o fleetOps) show(m fleet.Machine, link *fleet.MachineStatus) string {
	var text string
	if f, err := o.schemes(); err != nil {
		text = showText(m, link, nil, err)
	} else if s, err := f.Scheme(m.ID); err != nil {
		text = showText(m, link, nil, err)
	} else {
		text = showText(m, link, &s, nil)
	}
	if m.State == fleet.StatePending {
		return text
	}
	// Its memory (TEN-532), above what the machine reports about itself.
	const reported = "\n  Reported by the machine"
	if i := strings.Index(text, reported); i >= 0 {
		return text[:i] + "\n" + memoryLine(o.dataDir, m.ID) + text[i:]
	}
	return text + "\n" + memoryLine(o.dataDir, m.ID)
}

func fleetConfirmCmd(args []string) error {
	fs := flag.NewFlagSet("fleet confirm", flag.ContinueOnError)
	var pairs stringList
	fs.Var(&pairs, "set", "category=mode for this machine alone, e.g. exec=allow (repeatable)")
	cli, err := openFleetCLI(fs, args)
	if err != nil {
		return err
	}
	if len(cli.pos) != 1 {
		return errors.New("usage: tenant fleet confirm <name> [--set category=mode]...   (you are asked for the six digits the machine shows)")
	}
	set, err := parseSet(pairs)
	if err != nil {
		return err
	}
	m, err := cli.ops.machine(cli.pos[0])
	if err != nil {
		return err
	}
	if m.State != fleet.StatePending {
		return fmt.Errorf("%s is %s, not waiting for confirmation", fleetName(m.Name), m.State)
	}
	_, p, err := cli.ops.preview(m, set)
	if err != nil {
		return err
	}
	fmt.Println(confirmScreen(m, p))
	// The digits are typed by a person, at a terminal, after reading the
	// screen above. There is no flag for them: a command a script or the agent
	// runs cannot confirm a machine.
	code, err := fleetAsk(fmt.Sprintf("\nType the six digits %s shows on its own screen (Enter alone cancels): ", fleetName(m.Name)))
	if err != nil {
		return err
	}
	if code == "" {
		return errors.New("not confirmed")
	}
	if _, _, err := cli.ops.confirmMachine(m.ID, code, set); err != nil {
		return err
	}
	fmt.Printf("confirmed %s (%s). The master may call it from now on; its scheme: tenant fleet permissions %s\n", fleetName(m.Name), m.ID, fleetName(m.Name))
	return nil
}

func fleetRenameCmd(args []string) error {
	cli, err := openFleetCLI(flag.NewFlagSet("fleet rename", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if len(cli.pos) != 2 {
		return errors.New("usage: tenant fleet rename <name> <new-name>")
	}
	m, err := cli.ops.rename(cli.pos[0], cli.pos[1])
	if err != nil {
		return err
	}
	fmt.Printf("renamed %s to %s (id %s; its scheme and everything else kept under the id stay)\n", fleetName(m.Name), fleetName(cli.pos[1]), m.ID)
	return nil
}

func fleetRevokeCmd(args []string) error {
	fs := flag.NewFlagSet("fleet revoke", flag.ContinueOnError)
	keep := fs.Bool("keep-scheme", false, "keep the machine's approval scheme under its id, for a machine enrolled later to adopt")
	del := fs.Bool("delete-scheme", false, "delete the machine's approval scheme and its record")
	cli, err := openFleetCLI(fs, args)
	if err != nil {
		return err
	}
	if len(cli.pos) != 1 || *keep && *del {
		return errors.New("usage: tenant fleet revoke <name> [--keep-scheme | --delete-scheme]")
	}
	m, err := cli.ops.machine(cli.pos[0])
	if err != nil {
		return err
	}
	if !*keep && !*del {
		answer, err := fleetAsk(fmt.Sprintf("Revoke %s (%s). Keep its approval scheme under its id, or delete it? [keep/delete, Enter alone cancels]: ", fleetName(m.Name), m.ID))
		switch answer = strings.ToLower(answer); {
		case err != nil:
			return fmt.Errorf("%w; say --keep-scheme or --delete-scheme", err)
		case answer == "keep" || answer == "k":
		case answer == "delete" || answer == "d":
			*del = true
		default:
			return errors.New("not revoked")
		}
	}
	if _, err := cli.ops.revoke(m.ID, *del); err != nil {
		return err
	}
	if *del {
		fmt.Printf("revoked %s (%s): its certificate is refused, its record and approval scheme are deleted, and %s\n", fleetName(m.Name), m.ID, memoryGoneWords(cli.c.dataDir, m.ID))
	} else {
		fmt.Printf("revoked %s (%s): its certificate is refused. Its approval scheme is kept under its id (adopt it: tenant fleet enroll <name> --adopt %s)\n", fleetName(m.Name), m.ID, m.ID)
	}
	return nil
}

// permEdit applies one edit of a scheme, as /permissions spells them: set
// <category> <mode>, add <pattern> <mode>, rm <pattern>. A pattern may hold
// spaces (a command rule), so the mode is the last word. cats is nil for a
// rule set with no categories (the fleet's rules).
func permEdit(cats *approvalBroker, rules tui.ToolRuleControl, surface string, args []string) (string, error) {
	usage := errors.New("want: set <category> <mode> | add <pattern> <mode> | rm <pattern>")
	if len(args) < 2 {
		return "", usage
	}
	// The answer names the mode as it was saved, not as it was typed: a mode is
	// read leniently (case, surrounding white space), and what was typed is not
	// echoed to the terminal.
	saved := func(mode string) string {
		m, _ := approval.ParseMode(mode)
		return m.String()
	}
	switch verb := strings.ToLower(args[0]); {
	case verb == "set" && len(args) == 3 && cats != nil:
		cat, mode := strings.ToLower(args[1]), strings.ToLower(args[2])
		ok, err := cats.SetPermissionFrom(surface, cat, mode)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no such category %s (one of %s)", fleetQuote(args[1]), strings.Join(approval.Categories, ", "))
		}
		return cat + " is now " + saved(mode), nil
	case (verb == "add" || verb == "rule") && len(args) >= 3:
		pattern, mode := strings.Join(args[1:len(args)-1], " "), strings.ToLower(args[len(args)-1])
		if err := rules.SetRule(pattern, mode); err != nil {
			return "", err
		}
		return "rule " + fleetRule(strings.TrimSpace(pattern)) + " is now " + saved(mode), nil
	case verb == "rm" || verb == "remove" || verb == "del":
		pattern := strings.Join(args[1:], " ")
		if removed, err := rules.RemoveRule(pattern); err != nil || !removed {
			return "", cmp.Or(err, fmt.Errorf("no rule %s", fleetQuote(pattern)))
		}
		return "removed rule " + fleetRule(strings.TrimSpace(pattern)), nil
	}
	return "", usage
}

// fleetRuleSet is the fleet's rules for new machines as an editor takes them.
// With o, each change is recorded (fleetOps.auditSettings): a rule that lets a
// tool run on every machine confirmed from now on is not a quiet change.
type fleetRuleSet struct {
	r *toolRules
	o *fleetOps
}

func (s fleetRuleSet) Rules() []tui.ToolRuleInfo { return s.r.Rules() }

func (s fleetRuleSet) SetRule(pattern, mode string) error {
	if err := s.r.Set(pattern, mode); err != nil {
		return err
	}
	if s.o != nil {
		m, _ := approval.ParseMode(mode)
		s.o.auditSettings(defaultsWhat, "rule set", strings.TrimSpace(pattern)+" "+m.String())
	}
	return nil
}

func (s fleetRuleSet) RemoveRule(pattern string) (bool, error) {
	had := s.r.Remove(pattern)
	if had && s.o != nil {
		s.o.auditSettings(defaultsWhat, "rule removed", strings.TrimSpace(pattern))
	}
	return had, nil
}

func fleetPermissionsCmd(args []string) error {
	fs := flag.NewFlagSet("fleet permissions", flag.ContinueOnError)
	defaults := fs.Bool("defaults", false, "the fleet's categories and rules for new machines, not one machine's scheme")
	from := fs.String("from", "", "defaults: lay the fleet's rules and the machine's groups' rules over it again; master: copy the master's local scheme again")
	group := fs.String("group", "", "make the change to each member of this group")
	all := fs.Bool("all", false, "make the change to every confirmed machine")
	cli, err := openFleetCLI(fs, args)
	if err != nil {
		return err
	}
	switch {
	case (*group != "" || *all) && (*defaults || *group != "" && *all):
		return errors.New("--group, --all and --defaults go one at a time")
	case *group != "" || *all:
		return fleetBulkPermissions(cli, fleetSelection{Group: *group, All: *all}, *from)
	}
	f, err := cli.ops.schemes()
	if err != nil {
		return err
	}
	if *defaults {
		if *from != "" {
			return errors.New("usage: tenant fleet permissions --defaults [set <category> <mode> | unset <category> | add <pattern> <mode> | rm <pattern>]")
		}
		const later = ". Machines confirmed or copied again from now on start with it; it moves none: tenant fleet permissions <name> --from master"
		verb := ""
		if len(cli.pos) > 0 {
			verb = strings.ToLower(cli.pos[0])
		}
		switch {
		case verb == "set" || verb == "unset":
			said, err := defaultEdit(cli.ops, cli.pos)
			if err != nil {
				return err
			}
			fmt.Println(said + later)
			return nil
		case len(cli.pos) > 0:
			done, err := permEdit(nil, fleetRuleSet{f.Rules(), &cli.ops}, surfaceCLI, cli.pos)
			if err != nil {
				return fmt.Errorf("fleet rules: %w", err)
			}
			fmt.Println("fleet rules: " + done + ". Machines confirmed from now on start with it; one confirmed already: tenant fleet permissions <name> --from defaults")
			return nil
		}
		text, err := defaultsText(f, "tenant fleet permissions --defaults")
		if err != nil {
			return err
		}
		fmt.Println(text)
		return nil
	}
	if len(cli.pos) == 0 {
		return errors.New("usage: tenant fleet permissions <name> [set <category> <mode> | add <pattern> <mode> | rm <pattern>] [--from defaults|master]   |   tenant fleet permissions --group <group> | --all ...   |   tenant fleet permissions --defaults")
	}
	m, err := cli.ops.machine(cli.pos[0])
	if err != nil {
		return err
	}
	name := fleetName(m.Name)
	switch {
	case *from != "" && len(cli.pos) > 1:
		return errors.New("--from takes no edit beside it")
	case *from != "":
		said, err := cli.ops.recopy(f, m, *from)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", name, said)
	case len(cli.pos) > 1:
		done, err := cli.ops.editMachine(f, m, cli.pos[1:])
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Printf("%s: %s (this machine only)\n", name, done)
		return nil
	}
	fmt.Println(cli.ops.show(m, nil))
	return nil
}

func fleetRelayCmd(args []string) error {
	cli, err := openFleetCLI(flag.NewFlagSet("fleet relay", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	out, err := fleetRelaySwitch(cli.c.cfgDir, cli.c.lc, cli.pos)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// fleetRelaySwitch shows or sets the relays' fleet switches (relay.allow_fleet,
// imessage.allow_fleet): args is nothing, a relay, or a relay and on|off.
func fleetRelaySwitch(cfgDir string, lc *launchConfig, args []string) (string, error) {
	state := func(on bool) string {
		if on {
			return "on: its turns may reach the fleet, each call checked by the relay's own rules and by the machine's scheme"
		}
		return "off: its turns reach no fleet machine, reads included"
	}
	if len(args) == 0 {
		return "discord   " + state(lc.Relay.AllowFleet) + "\nimessage  " + state(lc.IMessage.AllowFleet), nil
	}
	relay := strings.ToLower(args[0])
	if relay != surfaceDiscord && relay != surfaceIMessage || len(args) > 2 {
		return "", errors.New("usage: fleet relay [discord|imessage [on|off]]")
	}
	if len(args) == 2 {
		on, err := parseOnOff(args[1])
		if err != nil {
			return "", err
		}
		if err := persistLaunchConfig(cfgDir, lc, func(lc *launchConfig) {
			if relay == surfaceDiscord {
				lc.Relay.AllowFleet = on
			} else {
				lc.IMessage.AllowFleet = on
			}
		}); err != nil {
			return "", err
		}
		_ = fleet.RelayChanged.Audit(context.Background(), "relay", relay, "state", map[bool]string{true: "reach", false: "not reach"}[on])
	}
	on := lc.Relay.AllowFleet
	if relay == surfaceIMessage {
		on = lc.IMessage.AllowFleet
	}
	return relay + " is " + state(on), nil
}

func fleetRemoteConfirmCmd(args []string) error {
	cli, err := openFleetCLI(flag.NewFlagSet("fleet remote-confirm", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	// Turning it on widens who may confirm a machine, so it takes a person at
	// a terminal, as confirming one does: a command a script or the agent runs
	// has none, and there is no flag that answers for them (terminalAsk).
	// Turning it off needs nobody: it only closes.
	if on, err := parseOnOff(strings.Join(cli.pos, " ")); len(cli.pos) == 1 && err == nil && on {
		answer, err := fleetAsk("This lets a signed-in dashboard browser on another computer confirm a pending machine, once it gives the sign-in key again.\n" +
			"Whoever holds that key can then confirm a machine from anywhere the dashboard is reached.\nType on to turn it on (Enter alone cancels): ")
		if err != nil {
			return err
		}
		if !strings.EqualFold(answer, "on") {
			return errors.New("not turned on")
		}
	}
	out, err := fleetRemoteConfirmSwitch(cli.ops, cli.c.lc, cli.pos)
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// fleetRemoteConfirmSwitch shows or sets fleet.remote_confirm (TEN-545):
// whether a signed-in dashboard browser that is not on this computer may
// confirm a pending machine, once it gives the sign-in key again. It is set
// at this computer, by a person: the command line asks at a terminal before
// it turns it on (fleetRemoteConfirmCmd), the terminal app takes it from a
// typed line only, and the dashboard has no switch, so a session elsewhere
// cannot open the door it then walks through. The file is no better guarded
// than config.json is: whatever can write that file as this account can set
// it. The running Tenant reads config.json at each confirmation.
func fleetRemoteConfirmSwitch(o fleetOps, lc *launchConfig, args []string) (string, error) {
	state := func(on bool) string {
		if on {
			return "remote confirmation is on: a signed-in dashboard browser on another computer can confirm a pending machine, after giving the sign-in key again. So can anyone else who holds that key, from wherever the dashboard is reached"
		}
		return "remote confirmation is off: a machine is confirmed at this computer only (a browser signed in at the dashboard's 127.0.0.1 address, or the terminal)"
	}
	switch len(args) {
	case 0:
		return state(fleetRemoteConfirmOn(o.cfgDir)), nil
	case 1:
		on, err := parseOnOff(args[0])
		if err != nil {
			return "", err
		}
		// Written whatever lc says it is: lc is this process's copy, and
		// another process may have switched the file since.
		if err := persistLaunchConfig(o.cfgDir, lc, func(lc *launchConfig) { lc.Fleet.RemoteConfirm = on }); err != nil {
			return "", err
		}
		o.audit(fleet.Machine{Name: "(this master)"}, "remote confirmation turned "+map[bool]string{true: "on", false: "off"}[on],
			"whether a signed-in dashboard browser on another computer may confirm a machine (fleet.remote_confirm)")
		// What the file says now, not what was asked: config.json has no lock
		// between processes, and another save may have landed on top of this one.
		if now := fleetRemoteConfirmOn(o.cfgDir); now != on {
			return "", fmt.Errorf("config.json was written, and now says remote confirmation is %s: another process saved it at the same moment. Run this again", map[bool]string{true: "on", false: "off"}[now])
		}
		return state(on), nil
	}
	return "", errors.New("usage: fleet remote-confirm [on|off]")
}

// fleetRemoteConfirmOn is fleet.remote_confirm as config.json holds it now.
// A file that cannot be read is off: the door is not left open on the
// strength of a file nobody can read.
func fleetRemoteConfirmOn(cfgDir string) bool {
	lc, err := loadLaunchConfig(cfgDir)
	return err == nil && lc.Fleet.RemoteConfirm
}
