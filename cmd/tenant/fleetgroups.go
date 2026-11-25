package main

// fleetgroups.go is TEN-546's shared operations: the fleet's machine groups,
// its default categories for new machines, and changes to many machines at
// once (bulk). They are fleetOps methods, so the command line, the terminal
// app and the dashboard run the same code and file the same records, each
// with its own surface. None of them prints: each returns what happened.
//
// A group or a default changes no machine's scheme. It is laid over a scheme
// on purpose, at a named step: a machine confirmed or copied again (the fresh
// copy, approval.Fleet.Fresh), or applied (approval.Fleet.Reapply). See
// docs/fleet-runbook.md, "Groups and default categories".

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"

	"tenant/internal/approval"
	"tenant/internal/fleet"
	"tenant/internal/logs/catalog"
	"tenant/internal/tui"
)

// auditSettings files a change to a group or to the defaults for new
// machines: its Security record and its line in the fleet log. what is
// "group <name>" or "defaults for new machines".
func (o fleetOps) auditSettings(what, change, detail string) {
	_ = catalog.FleetSettingsChanged.Audit(context.Background(), "what", what, "change", change, "detail", detail,
		"actor", actorOperator, "surface", o.surface)
	_ = fleet.SettingsChanged.Audit(context.Background(), "what", what, "change", change, "detail", detail, "surface", o.surface)
}

const defaultsWhat = "defaults for new machines"

// --- the default categories ---

// setDefault makes mode category's mode on every machine confirmed or copied
// again from now on. It moves no machine.
func (o fleetOps) setDefault(category, mode string) error {
	f, err := o.schemes()
	if err != nil {
		return err
	}
	category = strings.ToLower(strings.TrimSpace(category))
	if err := f.SetDefault(category, mode); err != nil {
		return err
	}
	m, _ := approval.ParseMode(mode)
	o.auditSettings(defaultsWhat, "default set", category+" "+m.String())
	return nil
}

// unsetDefault gives category back to the master's copy for new machines;
// false when it was not set.
func (o fleetOps) unsetDefault(category string) (bool, error) {
	f, err := o.schemes()
	if err != nil {
		return false, err
	}
	category = strings.ToLower(strings.TrimSpace(category))
	had, err := f.UnsetDefault(category)
	if had {
		o.auditSettings(defaultsWhat, "default unset", category+" comes from the master's copy again")
	}
	return had, err
}

// defaultEdit is `set <category> <mode>` or `unset <category>` for the
// defaults for new machines, in words.
func defaultEdit(o fleetOps, args []string) (string, error) {
	switch verb := strings.ToLower(args[0]); {
	case verb == "set" && len(args) == 3:
		if err := o.setDefault(args[1], args[2]); err != nil {
			return "", fmt.Errorf("fleet defaults: %w", err)
		}
		m, _ := approval.ParseMode(args[2])
		return "fleet defaults: " + strings.ToLower(args[1]) + " is " + m.String() + " on new machines", nil
	case verb == "unset" && len(args) == 2:
		had, err := o.unsetDefault(args[1])
		switch {
		case err != nil:
			return "", fmt.Errorf("fleet defaults: %w", err)
		case !had:
			return "", fmt.Errorf("fleet defaults: %s is not set for new machines", fleetQuote(args[1]))
		}
		return "fleet defaults: " + strings.ToLower(args[1]) + " comes from the master's copy on new machines", nil
	}
	return "", errors.New("want: set <category> <mode> | unset <category>")
}

// --- groups ---

// fleetGroupInfo is a group with its members as the fleet's records hold
// them.
type fleetGroupInfo struct {
	approval.Group
	Members []fleet.Machine // in name order
	Unknown []string        // member ids no machine in the fleet has
}

func groupWhat(name string) string { return "group " + name }

// groupInfo looks up g's members.
func (o fleetOps) groupInfo(g approval.Group) fleetGroupInfo {
	info := fleetGroupInfo{Group: g}
	for _, id := range g.Machines {
		if m, err := o.store.Machine(id); err == nil {
			info.Members = append(info.Members, m)
		} else {
			info.Unknown = append(info.Unknown, id)
		}
	}
	slices.SortFunc(info.Members, byMachineName)
	return info
}

func byMachineName(a, b fleet.Machine) int {
	return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID, b.ID))
}

// listGroups is every group, in name order.
func (o fleetOps) listGroups() ([]fleetGroupInfo, error) {
	f, err := o.schemes()
	if err != nil {
		return nil, err
	}
	all, err := f.Groups()
	if err != nil {
		return nil, err
	}
	out := make([]fleetGroupInfo, 0, len(all))
	for _, g := range all {
		out = append(out, o.groupInfo(g))
	}
	return out, nil
}

// showGroup is one group, named in any case.
func (o fleetOps) showGroup(name string) (fleetGroupInfo, error) {
	f, err := o.schemes()
	if err != nil {
		return fleetGroupInfo{}, err
	}
	g, err := f.Group(name)
	if err != nil {
		return fleetGroupInfo{}, err
	}
	return o.groupInfo(g), nil
}

func (o fleetOps) createGroup(name string) (approval.Group, error) {
	f, err := o.schemes()
	if err != nil {
		return approval.Group{}, err
	}
	g, err := f.CreateGroup(name)
	if err == nil {
		o.auditSettings(groupWhat(g.Name), "created", "")
	}
	return g, err
}

// deleteGroup removes a group. What it laid on its members stays theirs.
func (o fleetOps) deleteGroup(name string) (approval.Group, error) {
	f, err := o.schemes()
	if err != nil {
		return approval.Group{}, err
	}
	g, err := f.DeleteGroup(name)
	if err == nil {
		o.auditSettings(groupWhat(g.Name), "deleted", fmt.Sprintf("%d machine(s), %d rule(s); their schemes keep what it laid", len(g.Machines), len(g.ToolRules)))
	}
	return g, err
}

// addToGroup puts machines (by name or id) into a group; added are those
// that were not in it. Every machine is found before anything changes.
func (o fleetOps) addToGroup(name string, refs []string) (added []fleet.Machine, err error) {
	f, err := o.schemes()
	if err != nil {
		return nil, err
	}
	byID := map[string]fleet.Machine{}
	for _, ref := range refs {
		m, err := o.machine(ref)
		if err != nil {
			return nil, err
		}
		byID[m.ID] = m
	}
	g, err := f.Group(name)
	if err != nil {
		return nil, err
	}
	ids, err := f.AddMembers(g.Name, slices.Sorted(maps.Keys(byID)))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		added = append(added, byID[id])
	}
	slices.SortFunc(added, byMachineName)
	if len(added) > 0 {
		o.auditSettings(groupWhat(g.Name), "machines added", machineList(added))
	}
	return added, nil
}

// removeFromGroup takes machines out of a group, by name or id; an id no
// machine has any more is taken as a member's id. removed are the ids that
// were in it.
func (o fleetOps) removeFromGroup(name string, refs []string) (removed []string, err error) {
	f, err := o.schemes()
	if err != nil {
		return nil, err
	}
	g, err := f.Group(name)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ref := range refs {
		m, err := o.machine(ref)
		switch {
		case err == nil:
			ids = append(ids, m.ID)
		case slices.Contains(g.Machines, ref):
			ids = append(ids, ref)
		default:
			return nil, err
		}
	}
	if removed, err = f.RemoveMembers(g.Name, ids); err == nil && len(removed) > 0 {
		o.auditSettings(groupWhat(g.Name), "machines removed", o.idNames(removed))
	}
	return removed, err
}

// setGroupRule sets one of a group's rules. It moves no machine.
func (o fleetOps) setGroupRule(name, pattern, mode string) error {
	f, err := o.schemes()
	if err != nil {
		return err
	}
	g, err := f.Group(name)
	if err != nil {
		return err
	}
	if err := f.SetGroupRule(g.Name, pattern, mode); err != nil {
		return err
	}
	m, _ := approval.ParseMode(mode)
	o.auditSettings(groupWhat(g.Name), "rule set", strings.TrimSpace(pattern)+" "+m.String())
	return nil
}

// removeGroupRule removes one of a group's rules; false when it had none.
func (o fleetOps) removeGroupRule(name, pattern string) (bool, error) {
	f, err := o.schemes()
	if err != nil {
		return false, err
	}
	g, err := f.Group(name)
	if err != nil {
		return false, err
	}
	had, err := f.RemoveGroupRule(g.Name, pattern)
	if had {
		o.auditSettings(groupWhat(g.Name), "rule removed", strings.TrimSpace(pattern))
	}
	return had, err
}

// applyGroup lays the fleet's rules and then the rules of all of its groups,
// in name order, over the scheme of each member that is confirmed and has
// one: the operator's deliberate step that moves the members. Each machine
// changed has its own record; one that does not qualify is a result with the
// reason.
func (o fleetOps) applyGroup(name string) ([]fleetResult, error) {
	ms, err := o.targets(fleetSelection{Group: name})
	if err != nil {
		return nil, err
	}
	return o.bulk(ms, func(f *approval.Fleet, m fleet.Machine) (string, error) { return o.recopy(f, m, "defaults") })
}

// groupRuleSet is a group's rules as an editor takes them (permEdit), each
// change through fleetOps, so it is recorded.
type groupRuleSet struct {
	o    fleetOps
	name string
}

func (s groupRuleSet) Rules() []tui.ToolRuleInfo { return nil }
func (s groupRuleSet) SetRule(pattern, mode string) error {
	return s.o.setGroupRule(s.name, pattern, mode)
}
func (s groupRuleSet) RemoveRule(pattern string) (bool, error) {
	return s.o.removeGroupRule(s.name, pattern)
}

// --- many machines at once ---

// fleetSelection is what a bulk change is made to: these machines (by name or
// id), a group's members, or All (every confirmed machine, as `fleet update
// --all` takes them).
type fleetSelection struct {
	Refs  []string
	Group string
	All   bool
}

// targets are the machines t names, by name. A group's member that no
// machine in the fleet has any more is in the list with its id as its name
// and no state; bulk says so for it.
func (o fleetOps) targets(t fleetSelection) ([]fleet.Machine, error) {
	var out []fleet.Machine
	switch {
	case t.All:
		every, err := o.store.Machines()
		if err != nil {
			return nil, err
		}
		for _, m := range every {
			if m.State == fleet.StateConfirmed {
				out = append(out, m)
			}
		}
	case t.Group != "":
		info, err := o.showGroup(t.Group)
		if err != nil {
			return nil, err
		}
		out = info.Members
		for _, id := range info.Unknown {
			out = append(out, fleet.Machine{ID: id, Name: id})
		}
	default:
		seen := map[string]bool{}
		for _, ref := range t.Refs {
			m, err := o.machine(ref)
			if err != nil {
				return nil, err
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
	}
	slices.SortFunc(out, byMachineName)
	return out, nil
}

// fleetResult is what a bulk change did to one machine: OK, and what was
// said, or why not.
type fleetResult struct {
	Machine fleet.Machine
	OK      bool
	Said    string
}

// bulk runs do, the single-machine operation, on each machine in turn: each
// changed machine gets do's own record, the batch none. It never stops at a
// failure. A machine that does not qualify (pending, revoked, not in the
// fleet) is a result with the reason, and do is not run on it.
func (o fleetOps) bulk(ms []fleet.Machine, do func(f *approval.Fleet, m fleet.Machine) (string, error)) ([]fleetResult, error) {
	f, err := o.schemes()
	if err != nil {
		return nil, err
	}
	out := make([]fleetResult, 0, len(ms))
	for _, m := range ms {
		r := fleetResult{Machine: m}
		switch m.State {
		case "":
			r.Said = "no machine in this fleet has this id"
		case fleet.StatePending:
			r.Said = "it is pending: it has no scheme until it is confirmed"
		case fleet.StateRevoked:
			r.Said = "it is revoked"
		default:
			said, err := do(f, m)
			r.OK, r.Said = err == nil, said
			if err != nil {
				r.Said = err.Error()
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// editMachine makes one edit (permEdit's words) to machine m's own scheme:
// the single-machine path every bulk edit runs. A category change has its
// record from the broker (security/ModeChanged); a rule's is filed here.
func (o fleetOps) editMachine(f *approval.Fleet, m fleet.Machine, args []string) (string, error) {
	b, err := f.Child(m.ID)
	if err != nil {
		return "", err
	}
	done, err := permEdit(b, b, o.surface, args)
	if err == nil && len(args) > 0 && strings.ToLower(args[0]) != "set" {
		o.audit(m, "rule changed", done)
	}
	return done, err
}

// machineList is machines' names, for a record or an answer.
func machineList(ms []fleet.Machine) string {
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = fleetName(m.Name)
	}
	return strings.Join(names, ", ")
}

// resultLines is a bulk change as a screen shows it: one line per machine,
// and how many were not changed.
func resultLines(rs []fleetResult) (string, int) {
	if len(rs) == 0 {
		return "  no machine", 0
	}
	failed := 0
	lines := make([]string, len(rs))
	for i, r := range rs {
		mark := "changed    "
		if !r.OK {
			mark, failed = "NOT changed", failed+1
		}
		lines[i] = fmt.Sprintf("  %-20s %s  %s", fleetName(r.Machine.Name), mark, r.Said)
	}
	return strings.Join(lines, "\n"), failed
}

// notChanged is a bulk command's error when some machine was not changed:
// the command line exits 1 with it.
func notChanged(failed int) error {
	if failed == 0 {
		return nil
	}
	return exitError{1, fmt.Sprintf("%d machine(s) were not changed", failed)}
}

// idNames is machine ids as their names, for an answer; an id no machine
// has shows as itself.
func (o fleetOps) idNames(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id
		if m, err := o.store.Machine(id); err == nil {
			out[i] = fleetName(m.Name)
		}
	}
	return strings.Join(out, ", ")
}

// --- the words, shared by the command line and the terminal app ---

// groupText runs one `group` command (tenant fleet group …, /fleet group …)
// and says what it did. cmd is how the operator runs a fleet command there:
// "tenant fleet" or "/fleet". failed counts the machines an apply did not
// change.
func groupText(o fleetOps, cmd string, args []string) (text string, failed int, err error) {
	usage := fmt.Errorf("usage: %[1]s group [show <group> | create <group> | delete <group> | add <group> <machine>... | remove <group> <machine>... | rules <group> [add <pattern> <mode> | rm <pattern>] | apply <group>]", cmd)
	verb := "list"
	if len(args) > 0 {
		verb = strings.ToLower(args[0])
	}
	unmoved := func(g string) string {
		return fmt.Sprintf(". No machine's scheme changed: %s group apply %s lays the group's rules over its members", cmd, g)
	}
	switch {
	case verb == "list" && len(args) <= 1:
		all, err := o.listGroups()
		if err != nil {
			return "", 0, err
		}
		if len(all) == 0 {
			return fmt.Sprintf("no groups yet: %s group create <group>", cmd), 0, nil
		}
		lines := []string{fmt.Sprintf("Groups (their rules reach a member only when the group is applied: %s group apply <group>)", cmd)}
		for _, g := range all {
			lines = append(lines, fmt.Sprintf("  %-20s %3d machine(s)  %3d rule(s)  %s", g.Name, len(g.Machines), len(g.ToolRules), memberWords(g)))
		}
		return strings.Join(lines, "\n"), 0, nil
	case verb == "show" && len(args) == 2:
		g, err := o.showGroup(args[1])
		if err != nil {
			return "", 0, err
		}
		return groupShowText(g, cmd), 0, nil
	case verb == "create" && len(args) == 2:
		g, err := o.createGroup(args[1])
		if err != nil {
			return "", 0, err
		}
		return fmt.Sprintf("group %s made. Add machines: %s group add %s <machine>...", g.Name, cmd, g.Name), 0, nil
	case verb == "delete" && len(args) == 2:
		g, err := o.deleteGroup(args[1])
		if err != nil {
			return "", 0, err
		}
		return fmt.Sprintf("group %s deleted. No machine's scheme changed: what it laid on its members stays theirs", g.Name), 0, nil
	case (verb == "add" || verb == "remove" || verb == "rm") && len(args) >= 3:
		g, err := o.showGroup(args[1])
		if err != nil {
			return "", 0, err
		}
		if verb == "add" {
			added, err := o.addToGroup(g.Name, args[2:])
			if err != nil {
				return "", 0, err
			}
			if len(added) == 0 {
				return "group " + g.Name + ": each of them is a member already", 0, nil
			}
			return "group " + g.Name + ": added " + machineList(added) + unmoved(g.Name), 0, nil
		}
		removed, err := o.removeFromGroup(g.Name, args[2:])
		if err != nil {
			return "", 0, err
		}
		if len(removed) == 0 {
			return "group " + g.Name + ": none of them is a member", 0, nil
		}
		return "group " + g.Name + ": removed " + o.idNames(removed) +
			". No machine's scheme changed: what the group laid on them stays theirs", 0, nil
	case verb == "rules" && len(args) == 2:
		g, err := o.showGroup(args[1])
		if err != nil {
			return "", 0, err
		}
		return strings.Join(groupRuleLines(g.ToolRules, "  "), "\n"), 0, nil
	case verb == "rules" && len(args) >= 4:
		g, err := o.showGroup(args[1])
		if err != nil {
			return "", 0, err
		}
		done, err := permEdit(nil, groupRuleSet{o, g.Name}, o.surface, args[2:])
		if err != nil {
			return "", 0, fmt.Errorf("group %s: %w", g.Name, err)
		}
		return "group " + g.Name + ": " + done + unmoved(g.Name), 0, nil
	case verb == "apply" && len(args) == 2:
		rs, err := o.applyGroup(args[1])
		if err != nil {
			return "", 0, err
		}
		g, _ := o.showGroup(args[1])
		lines, failed := resultLines(rs)
		return fmt.Sprintf("group %s applied: the fleet's rules, then the rules of each member's groups in name order, laid over its scheme (its categories stay)\n%s", g.Name, lines), failed, nil
	}
	return "", 0, usage
}

// memberWords is a group's members for a one-line listing.
func memberWords(g fleetGroupInfo) string {
	words := machineList(g.Members)
	if len(g.Unknown) > 0 {
		words = strings.TrimPrefix(words+", ", ", ") + fmt.Sprintf("%d id(s) no machine has", len(g.Unknown))
	}
	return words
}

func groupRuleLines(rules map[string]string, indent string) []string {
	return schemeLines(approval.Scheme{ToolRules: rules}, indent, nil, nil)[len(approval.Categories):]
}

// groupShowText is one group: its members and its rules.
func groupShowText(g fleetGroupInfo, cmd string) string {
	lines := []string{"Group " + g.Name}
	if len(g.Members) == 0 && len(g.Unknown) == 0 {
		lines = append(lines, "  machines: none")
	} else {
		lines = append(lines, "  machines:")
	}
	for _, m := range g.Members {
		lines = append(lines, fmt.Sprintf("    %-20s %-10s %s", fleetName(m.Name), m.State, m.ID))
	}
	for _, id := range g.Unknown {
		lines = append(lines, fmt.Sprintf("    %-20s %-10s no machine in this fleet has this id", id, "-"))
	}
	lines = append(lines, groupRuleLines(g.ToolRules, "  ")...)
	return strings.Join(append(lines, fmt.Sprintf("  Its rules are laid over each member's scheme when it is applied (%s group apply %s), after the fleet's rules and after the groups whose names come before it.", cmd, g.Name)), "\n")
}

// defaultsText is the defaults for new machines: the categories the operator
// set, then the rules.
func defaultsText(f *approval.Fleet, cmd string) (string, error) {
	cats, err := f.Defaults()
	if err != nil {
		return "", err
	}
	lines := []string{"Fleet defaults: what every newly confirmed machine starts with, laid over its copy of this master's scheme"}
	if len(cats) == 0 {
		lines = append(lines, fmt.Sprintf("  categories: none set; each comes from the master's copy (%s set <category> <mode>)", cmd))
	} else {
		lines = append(lines, fmt.Sprintf("  categories set for new machines (the others come from the master's copy; %s unset <category>):", cmd))
		for _, c := range approval.Categories {
			if mode := cats[c]; mode != "" {
				lines = append(lines, fmt.Sprintf("    %-12s %s", c, mode))
			}
		}
	}
	lines = append(lines, groupRuleLines(f.Rules().Snapshot(), "  ")...)
	return strings.Join(lines, "\n"), nil
}

// --- the command line ---

func fleetGroupCmd(args []string) error {
	cli, err := openFleetCLI(flag.NewFlagSet("fleet group", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	text, failed, err := groupText(cli.ops, "tenant fleet", cli.pos)
	if err != nil {
		return err
	}
	fmt.Println(text)
	return notChanged(failed)
}

// fleetBulkPermissions is `tenant fleet permissions --group <g> | --all`:
// one edit, or --from, made to each machine through the single-machine path.
func fleetBulkPermissions(cli *fleetCLI, t fleetSelection, from string) error {
	if from == "" && len(cli.pos) == 0 || from != "" && len(cli.pos) > 0 {
		return errors.New("usage: tenant fleet permissions --group <group> | --all  set <category> <mode> | add <pattern> <mode> | rm <pattern> | --from defaults|master")
	}
	if from != "" && from != "defaults" && from != "master" {
		return fmt.Errorf("--from %s: want defaults or master", fleetQuote(from))
	}
	ms, err := cli.ops.targets(t)
	if err != nil {
		return err
	}
	rs, err := cli.ops.bulk(ms, func(f *approval.Fleet, m fleet.Machine) (string, error) {
		if from != "" {
			return cli.ops.recopy(f, m, from)
		}
		return cli.ops.editMachine(f, m, cli.pos)
	})
	if err != nil {
		return err
	}
	lines, failed := resultLines(rs)
	fmt.Println(lines)
	return notChanged(failed)
}
