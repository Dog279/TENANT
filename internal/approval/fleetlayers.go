package approval

// fleetlayers.go is what the fleet lays over a machine's scheme on purpose
// (TEN-546): the fleet's default categories for new machines
// ("fleet_permissions") and its machine groups ("fleet_groups"). Neither is
// ever read when a call is decided: a call is decided by the machine's own
// stored scheme (For, Child), and a layer reaches that scheme only when it is
// laid, at a named moment, by the operator: a machine confirmed, copied again
// (Fresh), or the fleet's and its groups' rules applied (Reapply).
//
// A machine's fresh copy is, each layer winning over the one before it:
//
//  1. a copy of the master's local scheme, clamped (Inherit);
//  2. the fleet's default categories, each replacing the copy's mode;
//  3. the fleet's rules (Scheme.Overlay);
//  4. the rules of each group it is a member of, in ascending name order
//     (compared without case), each laid with Overlay: a later group wins
//     over an earlier one, and any group over a fleet rule.
//
// After that the scheme is the machine's: its own later edit stands until a
// layer is laid over it again.

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Bounds on "fleet_groups": over one, the key cannot be read.
const (
	maxGroups       = 256
	maxGroupMembers = 4096
	maxGroupRules   = 512
)

var (
	groupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	// A group name must not pass for a machine id, which a target may also be.
	idLike = regexp.MustCompile(`(?i)^m[0-9a-f]{16}$`)
)

// Group is one of the fleet's machine groups: a name, its member machines
// (ids, never names) and rules laid over each member's scheme when the group
// is applied. A group holds no categories.
type Group struct {
	Name      string            // as first written; unique without regard to case
	Machines  []string          // member ids, sorted
	ToolRules map[string]string // pattern -> mode, as tool_rules
}

// groupEntry is one group as "fleet_groups" holds it, under its name.
type groupEntry struct {
	Machines  []string          `json:"machines,omitempty"`
	ToolRules map[string]string `json:"tool_rules,omitempty"`
}

// ValidGroupName says why name cannot name a group, or nil: 1 to 63 of
// A-Z a-z 0-9 . _ -, starting with a letter or a digit; not "all" (a bulk
// target) and not the shape of a machine id.
func ValidGroupName(name string) error {
	switch {
	case !groupName.MatchString(name):
		return fmt.Errorf("%q is not a group name: 1 to 63 letters, digits, dots, dashes or underscores, starting with a letter or a digit", plain(name, 64))
	case strings.EqualFold(name, "all"):
		return errors.New(`"all" is not a group name: it means every machine`)
	case idLike.MatchString(name):
		return fmt.Errorf("%q is not a group name: it has the shape of a machine id", name)
	}
	return nil
}

func (g Group) clone() Group {
	return Group{Name: g.Name, Machines: slices.Clone(g.Machines), ToolRules: maps.Clone(g.ToolRules)}
}

// decodeDefaults reads "fleet_permissions": category -> mode, only the
// categories the operator set for new machines. A file without the key has
// none.
func decodeDefaults(raw json.RawMessage) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 {
		return out, nil
	}
	var m map[string]string
	if err := strictJSON(raw, &m, false); err != nil {
		return nil, err
	}
	for cat, mode := range m {
		md, ok := ParseMode(mode)
		switch {
		case !isCategory(cat):
			return nil, fmt.Errorf("%q is not a category", cat)
		case !ok:
			return nil, fmt.Errorf("%s is %q, not ask, allow or deny", cat, mode)
		}
		out[cat] = md.String()
	}
	return out, nil
}

// decodeGroups reads "fleet_groups", keyed by each name in lower case. One
// entry it cannot read spoils the whole: a group rule may have been a deny.
func decodeGroups(raw json.RawMessage) (map[string]Group, error) {
	out := map[string]Group{}
	if len(raw) == 0 {
		return out, nil
	}
	var top map[string]json.RawMessage
	if err := strictJSON(raw, &top, true); err != nil { // true: two names that differ only in case are one name twice
		return nil, err
	}
	if len(top) > maxGroups {
		return nil, fmt.Errorf("%d groups, over the %d the fleet keeps", len(top), maxGroups)
	}
	for name, entry := range top {
		if err := ValidGroupName(name); err != nil {
			return nil, err
		}
		var e groupEntry
		if err := strictJSON(entry, &e, true); err != nil {
			return nil, fmt.Errorf("group %s: %w", name, err)
		}
		g, err := e.group(name)
		if err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = g
	}
	return out, nil
}

// group checks one entry and makes it a group.
func (e groupEntry) group(name string) (Group, error) {
	if len(e.Machines) > maxGroupMembers {
		return Group{}, fmt.Errorf("group %s: %d machines, over the %d a group holds", name, len(e.Machines), maxGroupMembers)
	}
	if len(e.ToolRules) > maxGroupRules {
		return Group{}, fmt.Errorf("group %s: %d rules, over the %d a group holds", name, len(e.ToolRules), maxGroupRules)
	}
	g := Group{Name: name, Machines: slices.Clone(e.Machines)}
	for _, id := range g.Machines {
		if !machineID.MatchString(id) {
			return Group{}, fmt.Errorf("group %s: %q is not a machine id", name, plain(id, 64))
		}
	}
	slices.Sort(g.Machines)
	if len(slices.Compact(slices.Clone(g.Machines))) != len(g.Machines) {
		return Group{}, fmt.Errorf("group %s: a machine is listed twice", name)
	}
	var err error
	if g.ToolRules, err = strictRules(e.ToolRules); err != nil {
		return Group{}, fmt.Errorf("group %s: %w", name, err)
	}
	return g, nil
}

// encodeGroups is groups as "fleet_groups" holds them.
func encodeGroups(groups map[string]Group) map[string]groupEntry {
	out := make(map[string]groupEntry, len(groups))
	for _, g := range groups {
		out[g.Name] = groupEntry{Machines: g.Machines, ToolRules: g.ToolRules}
	}
	return out
}

// layerKey is a layer in a form two readings can be compared by (syncKey):
// its value, or its own text when it could not be read.
func layerKey(v any, err error, raw json.RawMessage) string {
	if err != nil {
		return rawKey(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// SetPersistLayers installs the hook that saves the fleet's default
// categories and groups ("fleet_permissions", "fleet_groups") whenever one
// changes, with the same contract as SetPersist: the hook calls snapshot and
// writes what it returned, in one hold of the lock under which the file is
// read back into LoadLayers. A key that could not be read goes back as it
// was written; an empty one is nil (left out of the file).
func (f *Fleet) SetPersistLayers(persist func(snapshot func() (defaults, groups json.RawMessage))) {
	f.mu.Lock()
	f.persistLayers = persist
	f.mu.Unlock()
}

func (f *Fleet) saveLayers() {
	f.mu.Lock()
	persist := f.persistLayers
	f.mu.Unlock()
	if persist != nil {
		persist(f.snapshotLayers)
	}
}

// snapshotLayers is snapshot for the two layer keys, for the hook alone.
func (f *Fleet) snapshotLayers() (defaults, groups json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncedDefaults = layerKey(f.defaults, f.defaultsErr, f.defaultsRaw)
	switch {
	case f.defaultsErr != nil:
		defaults = f.defaultsRaw
	case len(f.defaults) > 0:
		defaults, _ = json.Marshal(f.defaults)
	}
	enc := encodeGroups(f.groups)
	f.syncedGroups = layerKey(enc, f.groupsErr, f.groupsRaw)
	switch {
	case f.groupsErr != nil:
		groups = f.groupsRaw
	case len(enc) > 0:
		groups, _ = json.Marshal(enc)
	}
	return defaults, groups
}

// LoadLayers applies the settings file's "fleet_permissions" and
// "fleet_groups" as Load does the schemes: without saving, taking as an edit
// only what differs from what was last read or handed out to be written.
// A key that cannot be read is kept as written and named in problems; while
// it is, no fresh copy can be worked out and (for the groups) none applied or
// changed. No machine's scheme moves either way.
func (f *Fleet) LoadLayers(defaults, groups json.RawMessage) (changed int, problems []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := decodeDefaults(defaults)
	if key := layerKey(d, err, defaults); key != f.syncedDefaults {
		f.syncedDefaults = key
		changed++
		f.defaults, f.defaultsErr, f.defaultsRaw = d, err, nil
		if err != nil {
			f.defaults, f.defaultsRaw = map[string]string{}, defaults
			problems = append(problems, fmt.Sprintf("fleet_permissions: %s. No machine can be confirmed or copied again until this is fixed.", short(err)))
		}
	}
	g, err := decodeGroups(groups)
	if key := layerKey(encodeGroups(g), err, groups); key != f.syncedGroups {
		f.syncedGroups = key
		changed++
		f.groups, f.groupsErr, f.groupsRaw = g, err, nil
		if err != nil {
			f.groups, f.groupsRaw = map[string]Group{}, groups
			problems = append(problems, fmt.Sprintf("fleet_groups: %s. No machine can be confirmed or copied again, and no group applied or changed, until this is fixed.", short(err)))
		}
	}
	return changed, problems
}

// Defaults are the fleet's default categories for new machines: only those
// the operator set.
func (f *Fleet) Defaults() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.defaultsErr != nil {
		return nil, f.defaultsUnreadable()
	}
	return maps.Clone(f.defaults), nil
}

func (f *Fleet) defaultsUnreadable() error {
	return fmt.Errorf("fleet: the default categories for new machines (fleet_permissions) can't be read: %w", f.defaultsErr)
}

func (f *Fleet) groupsUnreadable() error {
	return fmt.Errorf("fleet: the groups (fleet_groups) can't be read: %w", f.groupsErr)
}

// SetDefault makes mode category's mode on every machine confirmed or copied
// again from now on. It moves no machine.
func (f *Fleet) SetDefault(category, mode string) error {
	m, ok := ParseMode(mode)
	switch {
	case !isCategory(category):
		return fmt.Errorf("no category %q (one of %s)", plain(category, 64), strings.Join(Categories, ", "))
	case !ok:
		return errors.New("mode must be ask|allow|deny")
	}
	f.mu.Lock()
	if f.defaultsErr != nil {
		defer f.mu.Unlock()
		return f.defaultsUnreadable()
	}
	f.defaults[category] = m.String()
	f.mu.Unlock()
	f.saveLayers()
	return nil
}

// UnsetDefault gives category back to the master's copy for new machines;
// false when it was not set.
func (f *Fleet) UnsetDefault(category string) (bool, error) {
	f.mu.Lock()
	if f.defaultsErr != nil {
		defer f.mu.Unlock()
		return false, f.defaultsUnreadable()
	}
	_, had := f.defaults[category]
	delete(f.defaults, category)
	f.mu.Unlock()
	if had {
		f.saveLayers()
	}
	return had, nil
}

// Groups are the fleet's groups, in name order.
func (f *Fleet) Groups() ([]Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.groupsErr != nil {
		return nil, f.groupsUnreadable()
	}
	return f.sortedGroupsLocked(func(Group) bool { return true }), nil
}

func (f *Fleet) sortedGroupsLocked(keep func(Group) bool) []Group {
	var out []Group
	for _, k := range slices.Sorted(maps.Keys(f.groups)) {
		if g := f.groups[k]; keep(g) {
			out = append(out, g.clone())
		}
	}
	return out
}

// GroupsOf are the groups machine id is a member of, in name order: the
// order they are laid in.
func (f *Fleet) GroupsOf(id string) ([]Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groupsOfLocked(id)
}

func (f *Fleet) groupsOfLocked(id string) ([]Group, error) {
	if f.groupsErr != nil {
		return nil, f.groupsUnreadable()
	}
	return f.sortedGroupsLocked(func(g Group) bool { _, in := slices.BinarySearch(g.Machines, id); return in }), nil
}

// Group is the group named name, in any case.
func (f *Fleet) Group(name string) (Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, err := f.groupLocked(name)
	return g.clone(), err
}

func (f *Fleet) groupLocked(name string) (Group, error) {
	if f.groupsErr != nil {
		return Group{}, f.groupsUnreadable()
	}
	g, ok := f.groups[strings.ToLower(name)]
	if !ok {
		return Group{}, fmt.Errorf("no group %q", plain(name, 64))
	}
	return g, nil
}

// editGroup changes the group named name with edit, under the fleet's lock,
// and saves when edit reports a change. A group's change moves no machine.
func (f *Fleet) editGroup(name string, edit func(g *Group) (bool, error)) (Group, error) {
	f.mu.Lock()
	g, err := f.groupLocked(name)
	if err == nil {
		g = g.clone()
		var changed bool
		if changed, err = edit(&g); err == nil && changed {
			f.groups[strings.ToLower(g.Name)] = g
			f.mu.Unlock()
			f.saveLayers()
			return g.clone(), nil
		}
	}
	f.mu.Unlock()
	return g, err
}

// CreateGroup makes an empty group.
func (f *Fleet) CreateGroup(name string) (Group, error) {
	if err := ValidGroupName(name); err != nil {
		return Group{}, err
	}
	f.mu.Lock()
	switch old, have := f.groups[strings.ToLower(name)]; {
	case f.groupsErr != nil:
		f.mu.Unlock()
		return Group{}, f.groupsUnreadable()
	case have:
		f.mu.Unlock()
		return Group{}, fmt.Errorf("there is a group %s already", old.Name)
	case len(f.groups) >= maxGroups:
		f.mu.Unlock()
		return Group{}, fmt.Errorf("the fleet has %d groups, as many as it keeps", maxGroups)
	}
	g := Group{Name: name, ToolRules: map[string]string{}}
	f.groups[strings.ToLower(name)] = g
	f.mu.Unlock()
	f.saveLayers()
	return g.clone(), nil
}

// DeleteGroup removes a group. Its members' schemes keep what it laid.
func (f *Fleet) DeleteGroup(name string) (Group, error) {
	f.mu.Lock()
	g, err := f.groupLocked(name)
	if err == nil {
		delete(f.groups, strings.ToLower(g.Name))
	}
	f.mu.Unlock()
	if err == nil {
		f.saveLayers()
	}
	return g.clone(), err
}

// AddMembers puts machines ids into a group; added are those not in it yet.
func (f *Fleet) AddMembers(name string, ids []string) (added []string, err error) {
	for _, id := range ids {
		if !machineID.MatchString(id) {
			return nil, fmt.Errorf("fleet: %q is not a machine id", plain(id, 64))
		}
	}
	_, err = f.editGroup(name, func(g *Group) (bool, error) {
		for _, id := range ids {
			if i, in := slices.BinarySearch(g.Machines, id); !in {
				g.Machines = slices.Insert(g.Machines, i, id)
				added = append(added, id)
			}
		}
		if len(g.Machines) > maxGroupMembers {
			return false, fmt.Errorf("group %s would hold %d machines, over the %d a group holds", g.Name, len(g.Machines), maxGroupMembers)
		}
		return len(added) > 0, nil
	})
	if err != nil {
		added = nil
	}
	return added, err
}

// RemoveMembers takes machines ids out of a group; removed are those that
// were in it.
func (f *Fleet) RemoveMembers(name string, ids []string) (removed []string, err error) {
	_, err = f.editGroup(name, func(g *Group) (bool, error) {
		for _, id := range ids {
			if i, in := slices.BinarySearch(g.Machines, id); in {
				g.Machines = slices.Delete(g.Machines, i, i+1)
				removed = append(removed, id)
			}
		}
		return len(removed) > 0, nil
	})
	return removed, err
}

// LeaveGroups takes machine id out of every group, as a revoke that deletes
// its scheme does, and names the groups it left.
func (f *Fleet) LeaveGroups(id string) ([]string, error) {
	f.mu.Lock()
	if f.groupsErr != nil {
		defer f.mu.Unlock()
		return nil, f.groupsUnreadable()
	}
	var left []string
	for k, g := range f.groups {
		if i, in := slices.BinarySearch(g.Machines, id); in {
			g.Machines = slices.Delete(slices.Clone(g.Machines), i, i+1)
			f.groups[k] = g
			left = append(left, g.Name)
		}
	}
	f.mu.Unlock()
	slices.SortFunc(left, func(a, b string) int { return cmp.Compare(strings.ToLower(a), strings.ToLower(b)) })
	if len(left) > 0 {
		f.saveLayers()
	}
	return left, nil
}

// SetGroupRule sets one of a group's rules. It moves no machine.
func (f *Fleet) SetGroupRule(name, pattern, mode string) error {
	pattern = strings.TrimSpace(pattern)
	if err := validRulePattern(pattern); err != nil {
		return err
	}
	m, ok := ParseMode(mode)
	if !ok {
		return errors.New("mode must be ask|allow|deny")
	}
	_, err := f.editGroup(name, func(g *Group) (bool, error) {
		if _, have := g.ToolRules[pattern]; !have && len(g.ToolRules) >= maxGroupRules {
			return false, fmt.Errorf("group %s has %d rules, as many as a group holds", g.Name, maxGroupRules)
		}
		if g.ToolRules == nil {
			g.ToolRules = map[string]string{}
		}
		g.ToolRules[pattern] = m.String()
		return true, nil
	})
	return err
}

// RemoveGroupRule removes one of a group's rules; false when it had none.
func (f *Fleet) RemoveGroupRule(name, pattern string) (bool, error) {
	pattern = strings.TrimSpace(pattern)
	had := false
	_, err := f.editGroup(name, func(g *Group) (bool, error) {
		_, had = g.ToolRules[pattern]
		delete(g.ToolRules, pattern)
		return had, nil
	})
	return had && err == nil, err
}

// Fresh is the scheme machine id would start with if it were confirmed now:
// Inherit (the master's copy, clamped, with the default categories and the
// fleet's rules), then the rules of each of id's groups in name order. It
// changes nothing. Where Inherit is refused, or the groups can't be read, so
// is this: a group rule may have been a deny.
func (f *Fleet) Fresh(id string, local Scheme) (Scheme, error) {
	s, err := f.Inherit(local)
	if err != nil {
		return Scheme{}, err
	}
	groups, err := f.GroupsOf(id)
	if err != nil {
		return Scheme{}, err
	}
	for _, g := range groups {
		s = s.Overlay(g.ToolRules)
	}
	return s, nil
}

// Reapply lays the fleet's rules and then the rules of each of machine id's
// groups, in name order, over the scheme it has, and saves: Apply fleet
// rules, and applying a group. Its categories stay, and so does every rule of
// its own that no laid rule answers for. groups names the groups laid. It is
// refused while the fleet's rules or its groups can't be read.
func (f *Fleet) Reapply(id string) (groups []string, err error) {
	f.mu.Lock()
	m := f.machines[id]
	switch {
	case f.rulesErr != nil:
		err = fmt.Errorf("fleet: the fleet's rules (fleet_rules) can't be read: %w", f.rulesErr)
	case m == nil || m.b == nil:
		err = fmt.Errorf("fleet: machine %s has no approval scheme that can be read", plain(id, 64))
	}
	var laid []Group
	if err == nil {
		laid, err = f.groupsOfLocked(id)
	}
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	s := m.scheme().Overlay(f.rules.Snapshot())
	for _, g := range laid {
		s = s.Overlay(g.ToolRules)
		groups = append(groups, g.Name)
	}
	f.setLocked(id, s)
	f.mu.Unlock()
	f.save()
	return groups, nil
}
