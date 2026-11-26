// Package alerts is the log book's alert rules (docs/design/logging-v2.md
// §3, Event Viewer's "Attach Task To This Event"; TEN-399). A rule is a
// filter in the filter language, an optional threshold (N events in a
// window), an action and a throttle.
//
// The Evaluator runs as a housekeeping task, so exactly one process, the
// housekeeper-lease holder, evaluates the rules, over the events committed
// since its cursor. It keeps the cursor and each rule's last firing in the
// store's meta table, so a lease handover neither fires twice nor misses an
// event.
//
// Every firing is an event (alerts 1001), which the dashboard shows, and a
// banner on the evaluating process's status feed. An action that leaves the log book (a Discord or iMessage message, a
// cron prompt) gets an Alert, which holds none of the event's data fields,
// only its message, scrubbed again (§11); a cron prompt not even that.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
)

// The actions.
const (
	Dashboard = "dashboard" // the alert event alone: the dashboard shows it (the default)
	Discord   = "discord"   // and a DM to the operator
	IMessage  = "imessage"  // and a text to the operator (imessage.operator)
	Cron      = "cron"      // and a one-off prompt job; Target is the prompt
	// Investigate is an agent turn over the event and its timeline, read-only
	// and within a budget, once the operator approves it (TEN-400,
	// investigate.go); Target is where its result also goes: "", Discord or
	// IMessage.
	Investigate = "investigate"
)

const (
	// DefaultThrottle is a rule's throttle when it sets none.
	DefaultThrottle = 15 * time.Minute
	// minThrottle keeps a rule from messaging the operator every tick.
	minThrottle = time.Minute
	// lookback is how far back a first evaluation reads, with no cursor
	// for this store yet (a new log book, a restore, a quarantine).
	lookback = 5 * time.Minute
	// msgMax cuts an alert's message, in runes.
	msgMax = 300
	// metaKey holds the evaluator's state in the store's meta table.
	metaKey = "alerts"
)

// Spec is a rule as configured: an entry of config.json's logs.alerts.
type Spec struct {
	Name     string
	Filter   string // the filter language (docs/LOGGING.md)
	Count    int    // threshold: fire once Count events match within Window; <= 1 fires on every match
	Window   string // a duration: 10m, 1h
	Action   string // Dashboard (""), Discord, IMessage, Cron or Investigate
	Target   string // Cron's prompt; Investigate's channel
	Throttle string // fire at most once per this long; "" is DefaultThrottle
	Disabled bool
}

// Builtin are the rules every install has, until a logs.alerts entry of the
// same name changes or disables them. When two rules would send one event
// to the same place, the earlier one does.
var Builtin = []Spec{
	{Name: "unexpected-exit", Filter: "provider:system id:3,4"}, // a crash or a kill, found at the next start
	// TEN-347: a refusal (or, in a dry run, a command that ran unconfined)
	// on a cron surface, where nobody could be asked to approve it.
	{Name: "sandbox-unattended", Filter: "provider:sandbox id:2001,2002 data.surface:cron-*"},
	{Name: "audit-failures", Filter: "tag:audit.failure", Count: 5, Window: "10m"},
	{Name: "logbook-full", Filter: "provider:logbook id:2004 data.percent>=95"},
	// TEN-555: the embedder has given no embedding for five minutes; memory
	// recall and wiki search are failing behind a Tenant that looks up.
	{Name: "embedder-silent", Filter: "provider:system id:3110"},
	{Name: "critical", Filter: "level:critical"},
}

// Rule is a compiled Spec.
type Rule struct {
	Spec
	filter           query.Filter // with -log:alerts
	window, throttle time.Duration
}

// Merge lays specs over Builtin: an entry named like a built-in rule
// overrides the fields it sets and its Disabled, keeping its place; any
// other is added after them. Disabled rules are kept.
func Merge(specs []Spec) []Spec {
	out := append([]Spec(nil), Builtin...)
	for _, s := range specs {
		i := slices.IndexFunc(out[:len(Builtin)], func(b Spec) bool { return b.Name == s.Name })
		if i < 0 {
			out = append(out, s)
			continue
		}
		b := &out[i]
		b.Disabled = s.Disabled
		if s.Filter != "" {
			b.Filter = s.Filter
		}
		if s.Count != 0 || s.Window != "" {
			b.Count, b.Window = s.Count, s.Window
		}
		if s.Action != "" {
			b.Action, b.Target = s.Action, s.Target
		}
		if s.Throttle != "" {
			b.Throttle = s.Throttle
		}
	}
	return out
}

// IsBuiltin reports whether name is a built-in rule's.
func IsBuiltin(name string) bool {
	return slices.ContainsFunc(Builtin, func(s Spec) bool { return s.Name == name })
}

// Rules merges specs over Builtin and compiles the enabled rules. A rule
// that doesn't compile is left out, and bad says why, by name.
func Rules(specs []Spec) (rules []Rule, bad map[string]error) {
	bad, seen := map[string]error{}, map[string]bool{}
	for _, s := range Merge(specs) {
		if seen[s.Name] {
			bad[s.Name] = errors.New("two rules have this name")
			continue
		}
		seen[s.Name] = true
		r, err := Compile(s)
		switch {
		case err != nil:
			bad[s.Name] = err
		case !s.Disabled:
			rules = append(rules, r)
		}
	}
	return rules, bad
}

// Compile checks s and compiles it.
func Compile(s Spec) (Rule, error) {
	r := Rule{Spec: s, throttle: DefaultThrottle}
	var err error
	switch {
	case !event.ValidName(s.Name):
		return r, fmt.Errorf("name %q: want 1–40 of a-z, 0-9, '.', '_' and '-'", s.Name)
	case strings.TrimSpace(s.Filter) == "":
		return r, errors.New("filter: empty; a rule needs a filter")
	}
	if r.filter, err = query.Parse(s.Filter + " -log:" + Events.Log); err != nil {
		return r, fmt.Errorf("filter: %w", err)
	}
	switch {
	case s.Count > 1:
		if r.window, err = time.ParseDuration(s.Window); err != nil || r.window <= 0 {
			return r, fmt.Errorf("threshold.window %q: want a duration such as 10m", s.Window)
		}
	case s.Window != "":
		return r, errors.New("threshold.window needs a threshold.count of 2 or more")
	}
	if s.Throttle != "" {
		if r.throttle, err = time.ParseDuration(s.Throttle); err != nil || r.throttle < minThrottle {
			return r, fmt.Errorf("throttle %q: want a duration of at least %s", s.Throttle, minThrottle)
		}
	}
	switch s.Action {
	case "", Dashboard:
		r.Action = Dashboard
	case Discord, IMessage:
		if s.Target != "" {
			return r, fmt.Errorf("action.target: %s alerts go to the operator; leave it empty", s.Action)
		}
	case Cron:
		if strings.TrimSpace(s.Target) == "" {
			return r, errors.New("action.target: a cron action needs the prompt to run")
		}
	case Investigate:
		if s.Target != "" && s.Target != Discord && s.Target != IMessage {
			return r, fmt.Errorf("action.target %q: where an investigation's result also goes: empty (the dashboard alone), discord or imessage", s.Target)
		}
	default:
		return r, fmt.Errorf("action.kind %q: want dashboard, discord, imessage, cron or investigate", s.Action)
	}
	return r, nil
}

// Alert is one firing: everything an action gets. It holds none of the
// event's data fields; its message is scrubbed again, on one line and cut
// (§11).
type Alert struct {
	Rule, Action, Target string
	Seq                  uint64 // the event's
	Time                 time.Time
	Level                event.Level
	Log, Provider        string
	ID                   uint32
	Msg                  string
	Count                int           // a threshold rule's matches within Window
	Window               time.Duration //
}

func newAlert(r Rule, e event.Event, n int64) Alert {
	return Alert{Rule: r.Name, Action: r.Action, Target: r.Target, Seq: e.Seq, Time: e.Time, Level: e.Level,
		Log: name(e.Log), Provider: name(e.Provider), ID: e.ID, Msg: clip(logs.Scrub(e.Msg), msgMax), Count: int(n), Window: r.window}
}

// name is a log's or a provider's name, which an outside writer may have
// set: one that isn't a valid name could carry text, so it doesn't pass.
func name(s string) string {
	if event.ValidName(s) {
		return s
	}
	return "(unnamed)"
}

// clip puts s on one line and cuts it to n runes.
func clip(s string, n int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

// Link is the event's page on the dashboard at base.
func Link(base string, seq uint64) string {
	return strings.TrimRight(base, "/") + "/logs/event/" + strconv.FormatUint(seq, 10)
}

func (a Alert) head() string {
	s := fmt.Sprintf("Tenant alert %s: %s event %s/%d in log %s at %s (event %d)",
		a.Rule, a.Level, a.Provider, a.ID, a.Log, a.Time.UTC().Format(time.RFC3339), a.Seq)
	if a.Count > 1 {
		s += fmt.Sprintf(", the latest of %d within %s", a.Count, a.Window)
	}
	return s
}

// Facts is the alert without the event's message, and link: all a cron
// prompt carries, since log text must never become an agent's
// instructions (§11).
func (a Alert) Facts(link string) string { return a.head() + "\n" + link }

// Text is the operator's message: the facts, the event's message in a code
// span, so text written into a log can't pass for Tenant's own (a link,
// formatting), and link.
func (a Alert) Text(link string) string {
	s := a.head()
	if a.Msg != "" {
		s += "\n`" + strings.ReplaceAll(a.Msg, "`", "'") + "`"
	}
	return s + "\n" + link
}

// Evaluator evaluates the rules. Run is its housekeeping task.
type Evaluator struct {
	Store *store.Store
	// Specs reads logs.alerts, at every run, so a change applies without a
	// restart. nil: the built-in rules alone.
	Specs func() []Spec
	// Act sends an alert that leaves the log book: a Discord, IMessage or
	// Cron action. nil fails them.
	Act func(context.Context, Alert) error
	// Notify, when set, puts every alert's banner on this process's status
	// feed (logs.Hub.Notify): the TUI's, or serve's terminal and system log.
	Notify func(line string)

	now     func() time.Time  // tests
	invalid map[string]string // the rule errors logged, so each is logged once
}

// state is what the evaluator keeps in the store's meta table.
type state struct {
	Cursor string               `json:"cursor"`          // the last event evaluated (store.Cursor)
	Fired  map[string]time.Time `json:"fired,omitempty"` // each rule's last firing
}

// Run evaluates the events committed since the last run. Every rule with a
// match fires, unless its threshold isn't met, it is throttled, or an
// earlier rule sends the same event to the same place. The cursor and the
// firings are saved before the actions run: an action cut short by a crash
// is lost, never sent twice.
func (e *Evaluator) Run(ctx context.Context) error {
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	var specs []Spec
	if e.Specs != nil {
		specs = e.Specs()
	}
	rules, bad := Rules(specs)
	e.report(ctx, bad)

	var st state
	if v, err := e.Store.Meta(ctx, metaKey); err != nil {
		return err
	} else if v != "" {
		_ = json.Unmarshal([]byte(v), &st) // unreadable: start over, as with none
	}
	if st.Fired == nil {
		st.Fired = map[string]time.Time{}
	}
	id := e.Store.StoreID()
	from, err := store.ParseCursor(st.Cursor)
	var since time.Time
	if err != nil || from.StoreID != id { // none for this store yet: the last few minutes only
		from, since = store.Cursor{StoreID: id}, now.Add(-lookback)
	}
	newest, _, _, err := e.Store.Query(ctx, query.Filter{}, store.Cursor{}, 1, true)
	if err != nil || len(newest) == 0 || newest[0].Seq <= from.Seq {
		return err
	}
	head := newest[0].Seq

	var fire []Alert
	sent := map[string]bool{} // seq, action and target: one alert each
	for _, r := range rules {
		f := r.filter
		if !since.IsZero() {
			f = after(f, since)
		}
		evs, _, _, err := e.Store.Query(ctx, f, from, 1, false)
		if err != nil {
			return err
		}
		if len(evs) == 0 || evs[0].Seq > head {
			continue
		}
		var n int64
		if r.Count > 1 {
			// ponytail: the window ends now, so after downtime a burst older
			// than the window goes unseen; anchor it at each match if that matters.
			if n, err = e.Store.Count(ctx, after(r.filter, now.Add(-r.window))); err != nil {
				return err
			}
			if n < int64(r.Count) {
				continue
			}
		}
		if last, ok := st.Fired[r.Name]; ok && now.Sub(last) < r.throttle {
			continue
		}
		key := fmt.Sprint(evs[0].Seq, "\x00", r.Action, "\x00", r.Target)
		if sent[key] {
			continue
		}
		sent[key] = true
		st.Fired[r.Name] = now
		fire = append(fire, newAlert(r, evs[0], n))
	}
	st.Cursor = store.Cursor{StoreID: id, Seq: head}.String()
	b, _ := json.Marshal(st)
	if err := e.Store.SetMeta(ctx, metaKey, string(b)); err != nil {
		return err
	}
	for _, a := range fire {
		e.fire(ctx, a)
	}
	return nil
}

// after is f limited to events at or after t.
func after(f query.Filter, t time.Time) query.Filter {
	terms := append(f.Terms[:len(f.Terms):len(f.Terms)], query.Term{Field: "since", Op: query.Match, Values: []query.Value{{Text: t.Format(time.RFC3339Nano), Time: t}}})
	return query.Filter{Terms: terms}
}

// fire files the alert, then sends it when its action leaves the log book.
func (e *Evaluator) fire(ctx context.Context, a Alert) {
	Fired.Log(ctx, "rule", a.Rule, "action", a.Action, "seq", a.Seq, "source", a.Provider+"/"+strconv.FormatUint(uint64(a.ID), 10),
		"source_level", a.Level.String(), "message", a.Msg, "count", a.Count)
	if e.Notify != nil {
		e.Notify(fmt.Sprintf("⚠ alert %s: %s (%s event %d)", a.Rule, a.Msg, a.Level, a.Seq))
	}
	if a.Action == Dashboard {
		return
	}
	err := errors.New("no action runs in this process")
	if e.Act != nil {
		err = e.Act(ctx, a)
	}
	if err != nil {
		ActionFailed.Log(ctx, "rule", a.Rule, "action", a.Action, "seq", a.Seq, "err", err)
	}
}

// report logs each rule error once, until it changes.
func (e *Evaluator) report(ctx context.Context, bad map[string]error) {
	seen := map[string]string{}
	for name, err := range bad {
		seen[name] = err.Error()
		if e.invalid[name] != seen[name] {
			RuleInvalid.Log(ctx, "rule", name, "err", err)
		}
	}
	e.invalid = seen
}
