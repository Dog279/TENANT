package approval

// fleet.go is the approval side of the fleet (docs/plans/gap6-fleet-plan.md,
// section 5). An action that lands on a child machine is checked twice, and
// both checks must pass: by the origin of the turn (local, a relay, cron),
// and by that machine's own scheme. Deny beats ask beats allow, and from an
// origin with nobody to ask, ask is deny.
//
// A machine's scheme is what the master's is: category modes, tool rules and
// command rules. So a machine has a Broker and a Rules of its own, and the
// decision is Broker.Confirm's own ladder rather than a second copy of it.
// What a fleet call changes is small and lives in the broker it decides
// through (Decider.view): the card and the records name the machine; the
// read-only command judgement is never asked, so every shell command asks
// until "always" saves a command rule; and "always" saves into that
// machine's scheme and nowhere else.
//
// The schemes persist in settings.<agent>.json under "fleet_schemes", keyed
// by the machine's immutable id, beside the fleet's own rules for new
// machines ("fleet_rules"). The name of a machine keys nothing: it is a
// label, and possibly the machine's own words.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"tenant/internal/control"
	"tenant/internal/logs"
)

// Machine names a fleet child. ID is the id the master gave it: it never
// changes and it is what a scheme is kept under. Name is a label: it can
// change, and the machine may have chosen it, so a card and a record show it
// escaped (safe).
type Machine struct{ ID, Name string }

// A machine id is short and plain, so it can be a key of the settings file
// and stand on a card unquoted.
var machineID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const (
	// maxNameRunes is how much of a machine's name a card shows.
	maxNameRunes = 48
	// maxDetail is how long a machine's request may be. A longer one is
	// refused, not cut: what is approved is what was read.
	maxDetail = 16 << 10
	maxAction = 200 // as long as a rule's pattern may be
)

// plain is s, cut to max characters, as printable ASCII: every control
// character, every escape sequence and every character outside ASCII shows
// as its escape, so the text can draw nothing, hide nothing and pass for
// nothing else by look-alike letters. Brackets and quotes are escaped too:
// the text can then neither close the label it stands in nor start one.
func plain(s string, max int) string {
	cut := len(s) > utf8.UTFMax*max
	if cut {
		s = s[:utf8.UTFMax*max] // more than max characters either way; the rest is never read
	}
	r := []rune(s)
	if len(r) > max {
		r, cut = r[:max], true
	}
	q := strconv.QuoteToASCII(string(r))
	q = strings.NewReplacer("[", `\x5b`, "]", `\x5d`).Replace(q[1 : len(q)-1])
	if cut {
		q += "..."
	}
	return q
}

// safe is m as a card and a record may show it: the name escaped (plain),
// and so is an id that is not one.
func (m Machine) safe() Machine {
	if !machineID.MatchString(m.ID) {
		m.ID = "?" + plain(m.ID, 64)
	}
	m.Name = plain(m.Name, maxNameRunes)
	return m
}

// label starts the text of a safe machine's request: the id the master gave
// it, then its name in quotes it cannot close.
func (m Machine) label() string { return "[host " + m.ID + ` "` + m.Name + `"] ` }

// Scheme is one machine's approval scheme as the settings file keeps it,
// under the same two keys as the master's own: category modes, and tool
// rules with the command rules among them.
type Scheme struct {
	Permissions map[string]string `json:"permissions,omitempty"`
	ToolRules   map[string]string `json:"tool_rules,omitempty"`
}

// normal is s as the fleet holds it: every category spelled out (one the
// scheme leaves out asks) and every mode in its one spelling. Unlike the
// master's own settings, which skip an entry they cannot read, a machine's
// scheme with such an entry is no scheme: the entry may have been a deny.
func (s Scheme) normal() (Scheme, error) {
	out := Scheme{Permissions: make(map[string]string, len(Categories))}
	for cat, mode := range s.Permissions {
		m, ok := ParseMode(mode)
		switch {
		case !isCategory(cat):
			return Scheme{}, fmt.Errorf("permissions: %q is not a category", cat)
		case !ok:
			return Scheme{}, fmt.Errorf("permissions: %s is %q, not ask, allow or deny", cat, mode)
		}
		out.Permissions[cat] = m.String()
	}
	for _, c := range Categories {
		if out.Permissions[c] == "" {
			out.Permissions[c] = ModeAsk.String()
		}
	}
	var err error
	out.ToolRules, err = strictRules(s.ToolRules)
	return out, err
}

// strictRules is NormalizeRules for a set where one unusable rule spoils the
// whole: it says which.
func strictRules(rules map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(rules))
	for p, mode := range rules {
		p = strings.TrimSpace(p)
		if err := validRulePattern(p); err != nil {
			return nil, fmt.Errorf("tool rule %q: %w", p, err)
		}
		m, ok := ParseMode(mode)
		if !ok {
			return nil, fmt.Errorf("tool rule %q is %q, not ask, allow or deny", p, mode)
		}
		if _, twice := out[p]; twice {
			return nil, fmt.Errorf("tool rule %q is there twice", p)
		}
		out[p] = m.String()
	}
	return out, nil
}

// strictJSON decodes raw into v as the one JSON value it must be: no unknown
// key (a misspelt "tool_rules" would drop every rule in it, the denies too),
// no key twice, no null, nothing after it. fields says v is a struct, whose
// keys the decoder matches in any case.
func strictJSON(raw json.RawMessage, v any, fields bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("more than one JSON value")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("null")
	}
	return twice(raw, fields)
}

// twice reports a key that one object of raw (valid JSON) holds twice: the
// decoder would quietly keep the second, and the one it dropped may be the
// deny. fold says the outermost object's keys are the same in any case.
func twice(raw json.RawMessage, fold bool) error {
	type object struct {
		keys map[string]bool // nil for an array
		key  bool            // the next token is a key
	}
	var open []object
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil // the end: Decode has already vouched for the grammar
		}
		top := len(open) - 1
		switch d, delim := tok.(json.Delim); {
		case delim && d == '{':
			open = append(open, object{keys: map[string]bool{}, key: true})
			continue
		case delim && d == '[':
			open = append(open, object{})
			continue
		case delim: // } or ]: the value it closes has ended
			open, top = open[:top], top-1
		case top >= 0 && open[top].key:
			k := tok.(string)
			dup := open[top].keys[k]
			if fold && top == 0 {
				for other := range open[top].keys {
					dup = dup || strings.EqualFold(other, k) // as the decoder matches a field's name
				}
			}
			if dup {
				return fmt.Errorf("the key %q is there twice", k)
			}
			open[top].keys[k], open[top].key = true, false
			continue
		}
		if top >= 0 && open[top].keys != nil {
			open[top].key = true // a value ended: in an object, a key comes next
		}
	}
}

// decodeScheme reads one machine's entry of "fleet_schemes".
func decodeScheme(raw json.RawMessage) (Scheme, error) {
	var s Scheme
	if err := strictJSON(raw, &s, true); err != nil {
		return Scheme{}, err
	}
	return s.normal()
}

// decodeRules reads "fleet_rules"; a file without the key has none.
func decodeRules(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := strictJSON(raw, &m, false); err != nil {
		return nil, err
	}
	return strictRules(m)
}

// short is err's text, cut where a hand-written key or value would make it
// long: it is told to the operator.
func short(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = strings.ToValidUTF8(s[:300], "") + "..."
	}
	return s
}

// syncKey is one entry of the file in a form two readings can be compared
// by: the scheme it holds, or its own text when it holds none.
func syncKey(s Scheme, err error) string {
	if err != nil {
		return ""
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// rawKey is syncKey for an entry that holds no scheme.
func rawKey(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return "!" + string(raw)
	}
	return "!" + b.String()
}

// clamped are the categories that start no looser than ask on a machine,
// whatever the master's own scheme says (Q1): the operator loosens one for
// that machine if they want it.
var clamped = map[string]bool{CatExec: true, CatWrite: true, CatDestructive: true, CatSend: true, CatSandbox: true}

// answers reports whether rule pattern p decides everything rule own
// decides, so that beside p, own could only ever contradict it: the same
// pattern; a command prefix that own's command falls under; or a tool rule
// that matches own's pattern read as a tool's name. That last test holds for
// a glob of own's too while p has no "?": own's wildcards can then only lie
// where a "*" of p takes anything. With a "?" in p it is not decided here,
// and a wrong yes would drop a rule (a deny, perhaps) where p does not reach.
func answers(p, own string) bool {
	if p == own {
		return true
	}
	pBody, pCmd := commandRuleBody(p)
	oBody, oCmd := commandRuleBody(own)
	switch {
	case pCmd && oCmd:
		pre, wild := strings.CutSuffix(pBody, " *")
		cmd := strings.TrimSuffix(oBody, " *")
		return wild && (cmd == pre || strings.HasPrefix(cmd, pre+" "))
	case pCmd || oCmd:
		return false // a tool rule and a command rule decide different things
	case strings.Contains(p, "?") && strings.ContainsAny(own, "*?"):
		return false
	}
	_, hit := matchRank(p, own)
	return hit
}

// Overlay lays rules over s, as the fleet's rules are laid over a scheme a
// new machine inherits: where the two disagree, rules win. A rule of s that
// one of them answers for goes (the same pattern, a narrower command, a tool
// or a glob a wider rule covers), then they join the set. s's category modes
// stay as they are: a rule is asked before them. What is left to the usual
// precedence: two globs that only partly overlap, and a tool-level deny of
// os_exec in s, which still binds every command rule.
func (s Scheme) Overlay(rules map[string]string) Scheme {
	out := Scheme{Permissions: maps.Clone(s.Permissions), ToolRules: make(map[string]string, len(s.ToolRules)+len(rules))}
	for own, mode := range s.ToolRules {
		covered := false
		for p := range rules {
			if covered = answers(p, own); covered {
				break
			}
		}
		if !covered {
			out.ToolRules[own] = mode
		}
	}
	maps.Copy(out.ToolRules, rules)
	return out
}

// Drift is one difference between two schemes: a category's mode, or a
// rule's (Rule). A rule only one of them has shows "" on the other side.
type Drift struct {
	Rule            bool
	Key, Have, Want string
}

// Drift lists where s differs from want, categories first and then rules by
// pattern: what a machine's scheme has that a fresh copy would not, and the
// other way round.
func (s Scheme) Drift(want Scheme) []Drift {
	var out []Drift
	have, wanted := SavedModes(s.Permissions), SavedModes(want.Permissions)
	for _, c := range Categories {
		if have[c] != wanted[c] {
			out = append(out, Drift{Key: c, Have: have[c].String(), Want: wanted[c].String()})
		}
	}
	patterns := slices.Collect(maps.Keys(s.ToolRules))
	for p := range want.ToolRules {
		if _, ok := s.ToolRules[p]; !ok {
			patterns = append(patterns, p)
		}
	}
	slices.Sort(patterns)
	for _, p := range patterns {
		if s.ToolRules[p] != want.ToolRules[p] {
			out = append(out, Drift{Rule: true, Key: p, Have: s.ToolRules[p], Want: want.ToolRules[p]})
		}
	}
	return out
}

// Each machine's calls may put askBurst prompts in front of the operator at
// once, and one more every askEvery after that. A machine that asks faster
// is refused: a child that has been taken over must not be able to bury the
// operator in cards until one is approved by mistake. (How many requests one
// call may make at all is the link's to bound: it knows what a call is.)
const (
	askBurst = 10
	askEvery = 6 * time.Second
)

// askLimit is one machine's prompt allowance. The zero value is a full one.
type askLimit struct {
	mu    sync.Mutex
	spent float64 // prompts used and not yet given back
	last  time.Time
}

// take uses one prompt of the allowance, if one is left at now.
func (l *askLimit) take(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if back := now.Sub(l.last).Seconds() / askEvery.Seconds(); back > 0 {
		l.spent = max(0, l.spent-back)
		l.last = now
	}
	if l.spent > askBurst-1 {
		return false
	}
	l.spent++
	return true
}

// Origin is where a turn came from, as a fleet call sees it. The zero Origin
// reaches nothing.
type Origin struct {
	// Name is the origin its records name: local, discord, imessage, cron.
	Name string
	// Open says the origin was opened for the fleet. Without it every call is
	// refused, reads included: a relay's switch is off by default.
	Open bool
	// Machines, when not nil, are the only machine ids the origin may reach:
	// the list a cron job carries. An id on it that no longer has a scheme
	// simply fails.
	Machines []string
	// Verdict is the origin's own check of an action: a relay's lock and
	// category modes, cron's approver. nil checks nothing more, which is the
	// local origin: each machine's scheme began as a copy of the local one and
	// stands in for it there.
	Verdict func(action, detail string) Mode
	// Asker is the broker whose way of asking the origin uses: its backend (a
	// relay's buttons, a text) or the host's own surfaces. nil is an origin
	// with nobody to ask, where ask is deny.
	Asker *Broker
}

// LocalOrigin is a turn from this computer's own surfaces (the terminal, the
// dashboard), which is where host asks. It is always open, and it adds no
// check of its own to the machine's scheme.
func LocalOrigin(host *Broker) Origin { return Origin{Name: "local", Open: true, Asker: host} }

// RelayOrigin is a turn that came through a relay (Discord, iMessage) with
// broker relay. open is that relay's fleet switch. The relay keeps its own
// rules: its lock and its category modes are checked as well as the
// machine's scheme, and a prompt goes where the relay asks.
func RelayOrigin(relay *Broker, open bool) Origin {
	return Origin{Name: relay.origin, Open: open, Asker: relay,
		Verdict: func(action, _ string) Mode { return relay.SurfaceMode(action) }}
}

// CronOrigin is a scheduled job's turn. machines is the list the job
// carries: it reaches those and no other, and with none it reaches nothing.
// allows is the job's own approver (the cron-exec approver's rules); nil is a
// job that may read and never act. Nobody is there to ask, so an action a
// machine's scheme asks about is refused.
func CronOrigin(machines []string, allows func(action, detail string) bool) Origin {
	return Origin{Name: "cron", Open: len(machines) > 0, Machines: append([]string{}, machines...),
		Verdict: func(action, detail string) Mode {
			if allows != nil && allows(action, detail) {
				return ModeAllow
			}
			return ModeDeny
		}}
}

// Fleet holds every machine's approval scheme and the fleet's rules for new
// machines, and decides the actions that land on a machine (For).
type Fleet struct {
	categorize func(action string) string
	log        *slog.Logger

	mu       sync.Mutex
	machines map[string]*machine
	rules    *Rules          // the fleet's rules for new machines
	rulesErr error           // why "fleet_rules" holds no rule set, when it does not
	rulesRaw json.RawMessage // its text then, kept for the operator to fix
	// synced is each entry of the file as last read from it or written to it,
	// and syncedRules the fleet's rules likewise (syncKey, rawKey). Load takes
	// only what differs from them as an edit.
	synced      map[string]string
	syncedRules string
	persist     func(snapshot func() (schemes map[string]json.RawMessage, rules json.RawMessage))
}

// machine is one entry of "fleet_schemes".
type machine struct {
	b   *Broker         // its scheme; nil while the entry holds none
	raw json.RawMessage // the entry's text then, kept for the operator to fix
	err error           // and why it is no scheme
}

// NewFleet returns a fleet with no machine. categorize maps an action id to
// its category, as for NewBroker.
func NewFleet(categorize func(action string) string) *Fleet {
	f := &Fleet{
		categorize: categorizeOrDestructive(categorize), log: logs.For("approvals"),
		machines: map[string]*machine{}, rules: NewRules(), synced: map[string]string{},
	}
	f.syncedRules = syncKey(Scheme{ToolRules: map[string]string{}}, nil)
	f.rules.SetPersist(func(map[string]string) {
		f.mu.Lock()
		f.rulesErr, f.rulesRaw = nil, nil // an edit through Rules replaces text that was no rule set
		f.mu.Unlock()
		f.save()
	})
	return f
}

// SetPersist installs the hook that saves the fleet whenever a scheme or the
// fleet's rules change (nil: nothing is saved). The hook is handed snapshot,
// which returns the values of the settings file's "fleet_schemes" and
// "fleet_rules" as they are when it is called: an entry that held no scheme
// goes back as it was written, for the operator to fix. The hook must call
// snapshot and write the file in one hold of the lock under which that file
// is read back into Load, and write what snapshot returned: it is what Load
// compares the file with next. Then the file never gets an older state after
// a newer one, and Load never mistakes a change this process has not written
// yet for a hand edit that undoes it.
func (f *Fleet) SetPersist(persist func(snapshot func() (schemes map[string]json.RawMessage, rules json.RawMessage))) {
	f.mu.Lock()
	f.persist = persist
	f.mu.Unlock()
}

// Rules is the fleet's rule set for new machines: tool rules and command
// rules laid over the scheme every newly confirmed machine inherits
// (Inherit). Changing it moves no machine that already has a scheme.
func (f *Fleet) Rules() *Rules { return f.rules }

// save tells the persist hook that something changed.
func (f *Fleet) save() {
	f.mu.Lock()
	persist := f.persist
	f.mu.Unlock()
	if persist != nil {
		persist(f.snapshot)
	}
}

// snapshot is the fleet as the settings file keeps it, for the persist hook
// alone (SetPersist): it also notes what it returns as the file's next
// content, so a caller that did not write it would make Load take the file
// for an edit.
func (f *Fleet) snapshot() (schemes map[string]json.RawMessage, rules json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	schemes = make(map[string]json.RawMessage, len(f.machines))
	clear(f.synced)
	for id, m := range f.machines {
		if m.b == nil {
			schemes[id], f.synced[id] = m.raw, rawKey(m.raw)
			continue
		}
		f.synced[id] = syncKey(m.scheme(), nil)
		schemes[id] = json.RawMessage(f.synced[id])
	}
	if f.rulesErr != nil {
		f.syncedRules = rawKey(f.rulesRaw)
		return schemes, f.rulesRaw
	}
	set := f.rules.Snapshot()
	f.syncedRules = syncKey(Scheme{ToolRules: set}, nil)
	if len(set) > 0 { // with none, the key is left out of the file
		rules, _ = json.Marshal(set)
	}
	return schemes, rules
}

// scheme is m's scheme as it stands.
func (m *machine) scheme() Scheme {
	return Scheme{Permissions: m.b.Modes(), ToolRules: m.b.rules.Snapshot()}
}

// Load applies the settings file's "fleet_schemes" and "fleet_rules" to the
// running fleet without saving: at start, and whenever the file changes (in
// the hold of the lock the file was read under: see SetPersist). It takes as
// an edit only what differs from what it last read or handed out to be
// written, so a change of this process that is not in the file yet is not
// undone. changed counts the entries it applied; problems says, for each
// entry that is not a scheme, which and why. Such a machine has no scheme
// until the file is fixed: every call to it is refused. The other machines
// are not touched.
func (f *Fleet) Load(schemes map[string]json.RawMessage, rules json.RawMessage) (changed int, problems []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, raw := range schemes {
		s, err := decodeScheme(raw)
		if err == nil && !machineID.MatchString(id) {
			err = errors.New("its key is not a machine id")
		}
		key := syncKey(s, err)
		if err != nil {
			key = rawKey(raw)
		}
		if old, ok := f.synced[id]; ok && old == key {
			continue
		}
		f.synced[id] = key
		changed++
		if err != nil {
			f.machines[id] = &machine{raw: raw, err: err}
			problems = append(problems, fmt.Sprintf(`fleet_schemes: machine "%s": %s. Every call to it is refused until this is fixed.`, plain(id, 64), short(err)))
			continue
		}
		f.setLocked(id, s)
	}
	for id := range f.synced {
		if _, ok := schemes[id]; !ok {
			delete(f.synced, id)
			delete(f.machines, id)
			changed++
		}
	}
	set, err := decodeRules(rules)
	key := syncKey(Scheme{ToolRules: set}, err)
	if err != nil {
		key = rawKey(rules)
	}
	if key != f.syncedRules {
		f.syncedRules = key
		changed++
		f.rules.Load(set) // none, when they could not be read
		f.rulesErr, f.rulesRaw = err, nil
		if err != nil {
			f.rulesRaw = rules
			problems = append(problems, fmt.Sprintf("fleet_rules: %s. No new machine can be given a scheme until this is fixed.", short(err)))
		}
	}
	slices.Sort(problems)
	return changed, problems
}

// setLocked gives machine id the (normal) scheme s, in place when it has one
// already, so a call that is waiting on its card keeps deciding by the
// machine's present scheme.
func (f *Fleet) setLocked(id string, s Scheme) {
	m := f.machines[id]
	if m == nil || m.b == nil {
		rules := NewRules()
		rules.SetPersist(func(map[string]string) { f.save() })
		b := &Broker{
			mu: new(sync.Mutex), modes: map[string]Mode{}, session: map[string]bool{}, known: knownSet(Categories),
			reg: newRegistry(), origin: "fleet:" + id, log: f.log, rules: rules, categorize: f.categorize,
			limit: &askLimit{},
			// By itself this broker approves nothing: an action reaches a
			// machine only through For, which decides with the origin.
			locked:  func(string, string) bool { return true },
			persist: func(map[string]string, []string) { f.save() },
		}
		m = &machine{b: b}
		f.machines[id] = m
	}
	m.raw, m.err = nil, nil
	m.b.LoadModes(s.Permissions, Categories) // s names every category, so every mode is set
	m.b.rules.Load(s.ToolRules)
}

// Inherit is the scheme a machine confirmed now would start with. It is a
// copy of local, the master's local scheme as its settings file holds it
// (so no launch-flag grant and no session grant is in it), with exec, write,
// destructive, send and sandbox no looser than ask: their category modes,
// and every allow rule that could reach them (a command rule, a glob, a tool
// of one of those categories, a tool no table knows). The fleet's rules are
// then laid over it and win (Scheme.Overlay). It changes nothing: Put makes
// the scheme the machine's.
func (f *Fleet) Inherit(local Scheme) (Scheme, error) {
	f.mu.Lock()
	err := f.rulesErr
	f.mu.Unlock()
	if err != nil {
		return Scheme{}, fmt.Errorf("fleet: the rules for new machines (fleet_rules) can't be read: %w", err)
	}
	s := Scheme{Permissions: make(map[string]string, len(Categories)), ToolRules: map[string]string{}}
	modes := SavedModes(local.Permissions) // as the master reads them: an entry it skips is not copied
	for _, c := range Categories {
		m := modes[c]
		if m == ModeAllow && clamped[c] {
			m = ModeAsk
		}
		s.Permissions[c] = m.String()
	}
	rules, _ := NormalizeRules(local.ToolRules)
	for p, mode := range rules {
		if mode == ModeAllow.String() && (isCommandRule(p) || strings.ContainsAny(p, "*?") || clamped[f.categorize(p)]) {
			mode = ModeAsk.String()
		}
		s.ToolRules[p] = mode
	}
	return s.Overlay(f.rules.Snapshot()), nil
}

// Put makes s the scheme of machine id and saves it: a new machine's at
// confirm, a fresh copy later (Inherit), the fleet's rules applied again
// (Overlay), or a malformed entry replaced. s must be a whole scheme: an
// entry that is not a category, a mode or a rule is an error and nothing
// changes.
func (f *Fleet) Put(id string, s Scheme) error {
	if !machineID.MatchString(id) {
		return fmt.Errorf("fleet: %q is not a machine id", plain(id, 64))
	}
	s, err := s.normal()
	if err != nil {
		return fmt.Errorf("fleet: machine %s: %w", id, err)
	}
	f.mu.Lock()
	f.setLocked(id, s)
	f.mu.Unlock()
	f.save()
	return nil
}

// Delete removes machine id's scheme and saves, as a revoke that does not
// keep it; false when there was none. A revoke that keeps the scheme calls
// nothing: the scheme stays under the id for the machine to be adopted
// again, and whether the machine may be called at all is the link's to say.
func (f *Fleet) Delete(id string) bool {
	f.mu.Lock()
	_, had := f.machines[id]
	delete(f.machines, id)
	f.mu.Unlock()
	if had {
		f.save()
	}
	return had
}

// Child is machine id's own broker: its category modes and rules, as the
// permissions editor reads and sets them (Permissions, SetPermissionFrom,
// Rules, SetRule, RemoveRule), each change saved. It approves nothing itself.
func (f *Fleet) Child(id string) (*Broker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch m := f.machines[id]; {
	case m == nil:
		return nil, fmt.Errorf("fleet: machine %s has no approval scheme", plain(id, 64))
	case m.b == nil:
		return nil, fmt.Errorf("fleet: machine %s: its approval scheme can't be read (%v)", plain(id, 64), m.err)
	default:
		return m.b, nil
	}
}

// Scheme is machine id's scheme as it stands.
func (f *Fleet) Scheme(id string) (Scheme, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.machines[id]; m != nil && m.b != nil {
		return m.scheme(), nil
	}
	return Scheme{}, fmt.Errorf("fleet: machine %s has no approval scheme that can be read", plain(id, 64))
}

// Decider decides the actions of calls from one origin that land on one
// machine.
type Decider struct {
	f *Fleet
	o Origin
	m Machine
}

// For is the decision-maker for calls from o that land on m. The link
// answers a child's fleet/confirm with its Confirm, under the context of the
// call the master started; a fleet tool asks its Reach before any call.
func (f *Fleet) For(o Origin, m Machine) Decider { return Decider{f, o, m} }

// Reach reports why no call from the origin may go to the machine at all, a
// read included: the origin was not opened for the fleet, the machine is not
// one it may reach, or the machine has no scheme. nil when a call may go.
func (d Decider) Reach() error {
	_, err := d.reach()
	return err
}

func (d Decider) reach() (*Broker, error) {
	o, id := d.o, d.m.ID
	switch {
	case d.f == nil:
		return nil, errors.New("fleet: no fleet")
	case !machineID.MatchString(id):
		return nil, fmt.Errorf("fleet: %q is not a machine id", plain(id, 64))
	case !o.Open:
		return nil, fmt.Errorf("fleet: %s is not opened for the fleet", o.Name)
	case o.Machines != nil && !slices.Contains(o.Machines, id):
		return nil, fmt.Errorf("fleet: machine %s is not one this %s job may reach", id, o.Name)
	}
	return d.f.Child(id)
}

// Confirm decides one action a tool on the machine asks about, and reports
// whether it may go ahead. Both checks must pass: the origin's own, and the
// machine's scheme. A deny on either side refuses unasked; an ask on either
// side prompts where the origin asks, or refuses when nobody is there; and
// "always" on that prompt saves into the machine's scheme alone. The prompt
// ends as a denial, and its card goes, when ctx is cancelled: the call
// ended, or the link dropped.
func (d Decider) Confirm(ctx context.Context, action, detail string) bool {
	if d.f == nil {
		return false // the zero Decider
	}
	b, err := d.reach()
	switch {
	case err != nil:
	case !plainAction(action):
		err = errors.New("fleet: the action id is empty, too long or not plain text")
	case len(detail) > maxDetail:
		err = fmt.Errorf("fleet: the request is %d bytes, over the %d a card may hold", len(detail), maxDetail)
	}
	if err != nil {
		return d.refuse(ctx, action, detail, "fleet", err)
	}
	mustAsk := false
	if d.o.Verdict != nil {
		switch d.o.Verdict(action, detail) {
		case ModeAllow:
		case ModeAsk:
			mustAsk = true
		default:
			return d.refuse(ctx, action, detail, "origin", errors.New("the origin's own rules refuse it"))
		}
	}
	return d.view(b, mustAsk).Confirm(ctx, action, detail)
}

// plainAction reports whether action can be an action id: one line of
// visible text, no longer than a rule's pattern. A machine sends it.
func plainAction(action string) bool {
	return action != "" && len(action) <= maxAction && utf8.ValidString(action) &&
		!strings.ContainsAny(action, "\n\t") && !strings.ContainsFunc(action, control.HiddenRune)
}

// refuse records a refusal made before the machine's scheme was asked. What
// it records of a request that was not fit to show is escaped and cut short.
func (d Decider) refuse(ctx context.Context, action, detail, via string, why error) bool {
	if !plainAction(action) {
		action = plain(action, maxAction)
	}
	if len(detail) > maxDetail {
		detail = strings.ToValidUTF8(detail[:512], "") + " [cut]"
	}
	m, cat := d.m.safe(), d.f.categorize(action)
	d.f.log.InfoContext(ctx, "approval refused for a fleet machine", "origin", d.o.Name, "host", m.ID, "category", cat, "action", action, "why", why)
	return verdict(withHost(ctx, &m), cat, action, m.label()+detail, d.o.Name, via, "denied_policy", control.DenyOnce)
}

// view is the broker this one call decides through: machine's scheme and
// grants, shared with its own broker b under b's lock, with the origin's
// name and the origin's way of asking. It has no read-only judgement and no
// lock of its own (the origin's lock is in its Verdict).
func (d Decider) view(b *Broker, mustAsk bool) *Broker {
	m := d.m.safe()
	b.mu.Lock()
	v := &Broker{
		mu: b.mu, modes: b.modes, session: b.session, known: b.known, persist: b.persist,
		rules: b.rules, limit: b.limit, categorize: b.categorize, log: b.log, reg: b.reg,
		origin: d.o.Name, machine: &m, mustAsk: mustAsk,
	}
	b.mu.Unlock()
	if a := d.o.Asker; a != nil {
		a.mu.Lock()
		v.ask, v.requests, v.reg = a.ask, a.requests, a.reg
		a.mu.Unlock()
	}
	return v
}
