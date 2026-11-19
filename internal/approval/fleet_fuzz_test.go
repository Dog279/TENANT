package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"tenant/internal/control"
)

// dice reads small choices off a fuzz input; past its end every choice is 0.
type dice struct {
	b []byte
	i int
}

func (d *dice) n(max int) int {
	if d.i >= len(d.b) {
		return 0
	}
	v := int(d.b[d.i]) % max
	d.i++
	return v
}

var (
	fuzzModes   = []string{"ask", "allow", "deny"}
	fuzzActions = []string{"os_exec", "os_exec_dangerous", "os_write", "web_interact", "gsuite_send", "mcp:h:1:tool", "os_exec_unknown", explicitAction, scopedAction}
	fuzzCmds    = []string{"uptime", "ls -la", "rm -rf x", "systemctl status nginx", "git push", "ls && rm x", "df -h | tee log", "FOO=1 make test"}
	fuzzRules   = []string{
		"os_exec", "os_exec_dangerous", "os_write", "web_interact", "gsuite_send", "mcp:h:1:tool", "tool", "os_*", "*:tool", "*_send",
		"os_exec(rm *)", "os_exec(ls *)", "os_exec(uptime)", "os_exec(systemctl status *)", "os_exec(systemctl *)", "os_exec(git push)",
		"os_exec(df *)", "os_exec(make test *)", "os_exec(tee *)", explicitAction, scopedAction,
	}
	fuzzAnswers = []control.ApprovalDecision{control.DenyOnce, control.ApproveOnce, control.ApproveSession, control.ApproveAlways}
)

// scheme rolls a scheme: a mode for every category and up to five rules.
func (d *dice) scheme() Scheme {
	s := Scheme{Permissions: map[string]string{}, ToolRules: map[string]string{}}
	for _, c := range Categories {
		s.Permissions[c] = fuzzModes[d.n(3)]
	}
	for range d.n(6) {
		s.ToolRules[fuzzRules[d.n(len(fuzzRules))]] = fuzzModes[d.n(3)]
	}
	return s
}

// FuzzFleetDecision rolls a master, two machines, an origin and a few
// requests in a row, and holds every decision to the fleet's invariants:
//
//   - deny dominance: an origin that is closed or refuses, or a machine whose
//     scheme denies, means no, with nobody asked;
//   - ask beats allow: an origin that asks never lets an action through unasked;
//   - nobody to ask means nobody is asked, and an ask is a no;
//   - nothing runs unasked on a machine without a rule or a mode in that
//     machine's own scheme that allows it (never the master's rules, modes,
//     flags or read-only judgement, never an earlier "for this session");
//   - no leakage: whatever is answered on a machine's card, only that
//     machine's scheme can change: not the other machine's, not the master's
//     rules or modes, not the origin's own settings; and it changes only on
//     "always".
func FuzzFleetDecision(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 3})
	f.Add([]byte{1, 1, 1, 1, 1, 1, 1, 1, 2, 10, 1, 11, 2, 1, 1, 1, 1, 1, 1, 1, 1, 0, 1, 1, 3, 4, 0, 1, 2, 3, 0, 3, 1, 3})
	f.Add([]byte{2, 2, 2, 2, 2, 2, 2, 2, 5, 0, 1, 12, 1, 13, 1, 14, 1, 15, 2, 2, 1, 2, 0, 0, 0, 0, 0, 0, 1, 3, 3, 0, 3, 3, 1, 3})
	f.Add([]byte("\x01\x00\x01\x00\x01\x00\x01\x00\x03\x0a\x02\x0b\x01\x0c\x00 a relay that asks, a machine with rules \x02\x01\x03\x03\x00\x01\x03"))
	f.Add([]byte("\x00\x01\x02\x00\x01\x02\x00\x01\x04\x00\x02\x01\x02\x02\x02\x03\x02 cron with a list \x03\x01\x01\x00\x03\x03\x03\x03\x03\x03"))
	f.Add(bytes.Repeat([]byte{3, 1, 4, 1, 5, 9, 2, 6, 5, 3, 5, 8, 9, 7, 9}, 8))
	f.Add(bytes.Repeat([]byte{1}, 120))
	f.Add(bytes.Repeat([]byte{255, 254, 253, 7, 3}, 30))

	f.Fuzz(func(t *testing.T, data []byte) {
		d := &dice{b: data}
		// The master: its own modes, rules, a launch-flag grant, a session grant
		// and a read-only judgement that calls everything read-only. None of it
		// may decide for a machine.
		host := newTestBroker(nil)
		master := NewRules()
		host.SetRules(master)
		host.SetReadOnly(func(string) bool { return true })
		ms := d.scheme()
		host.LoadModes(ms.Permissions, Categories)
		master.Load(ms.ToolRules)
		host.Allow(Categories[d.n(len(Categories))])
		host.session[Categories[d.n(len(Categories))]] = true

		fleet := NewFleet(testCategorize)
		for _, id := range []string{"aaaa", "bbbb"} {
			if err := fleet.Put(id, d.scheme()); err != nil {
				t.Fatal(err)
			}
		}
		for range d.n(4) { // some fleet rules, which must move no machine either
			_ = fleet.Rules().Set(fuzzRules[d.n(len(fuzzRules))], fuzzModes[d.n(3)])
		}

		// The surface that asks, with the operator's answers counted.
		var shown []control.ApprovalRequest
		answer := control.DenyOnce
		asker := newTestDiscordBroker(nil)
		asker.SetRules(NewRules())
		asker.SetAsk(func(_ context.Context, req control.ApprovalRequest) control.ApprovalDecision {
			shown = append(shown, req)
			return answer
		})
		rs := d.scheme()
		asker.LoadModes(rs.Permissions, Categories)
		if d.n(4) == 0 {
			asker.SetLock(func(_, cat string) bool { return cat == CatExec || cat == CatWrite })
		}

		var o Origin
		attended := true
		switch d.n(5) {
		case 0, 1: // this computer: no check of its own
			o = Origin{Name: "local", Open: true, Asker: asker}
		case 2: // a relay, with its switch
			o = RelayOrigin(asker, d.n(5) != 0)
		case 3: // a relay with nobody behind it
			silent := newTestDiscordBroker(nil)
			silent.LoadModes(rs.Permissions, Categories)
			o, attended = RelayOrigin(silent, d.n(5) != 0), false
		default: // a cron job
			var list []string
			for i, id := range []string{"aaaa", "bbbb", "gone"} {
				if d.n(3) != 0 || i == 2 {
					list = append(list, id)
				}
			}
			refuses := fuzzActions[d.n(len(fuzzActions))]
			o, attended = CronOrigin(list, func(action, _ string) bool { return action != refuses }), false
		}

		for range 1 + d.n(4) {
			action := fuzzActions[d.n(len(fuzzActions))]
			detail := fuzzCmds[d.n(len(fuzzCmds))]
			if action == "os_exec_dangerous" {
				detail += "  [DANGER: test]"
			}
			answer = fuzzAnswers[d.n(len(fuzzAnswers))]
			cat := testCategorize(action)

			// What must hold, read off the state before the call.
			a, _ := fleet.Scheme("aaaa")
			child, _ := fleet.Child("aaaa")
			mayReach := o.Open && (o.Machines == nil || slices.Contains(o.Machines, "aaaa")) // worked out here, not asked of the code under test
			reachErr := fleet.For(o, Machine{ID: "aaaa"}).Reach()
			origin := ModeAllow
			if o.Verdict != nil {
				origin = o.Verdict(action, detail)
			}
			toolMode, toolPat, toolHit := child.rules.Lookup(action)
			cmd, isExec := execCommand(action, detail)
			cmdMode, cmdHit := "", false
			if isExec {
				cmdMode, _, cmdHit = child.rules.LookupCommand(cmd)
			}
			explicit := requiresExplicit(action)
			mode := a.Permissions[cat]
			targetDenies := (toolHit && toolMode == "deny") || cmdMode == "deny" || (mode == "deny" && !toolHit && !cmdHit)
			reason := (action == "os_exec" && cmdMode == "allow") ||
				(toolHit && toolMode == "allow" && (!explicit || toolPat == action)) ||
				(!explicit && mode == "allow") // no session grant counts: a machine has none

			// Everything a machine's card must never change.
			others := func() map[string]string {
				return map[string]string{
					"machine bbbb":        schemeJSON(mustScheme(t, fleet, "bbbb")),
					"the master's rules":  schemeJSON(Scheme{ToolRules: master.Snapshot()}),
					"the master's modes":  schemeJSON(Scheme{Permissions: host.Modes()}),
					"the origin's rules":  schemeJSON(Scheme{ToolRules: asker.rules.Snapshot()}),
					"the origin's modes":  schemeJSON(Scheme{Permissions: asker.Modes()}),
					"the fleet's rules":   schemeJSON(Scheme{ToolRules: fleet.Rules().Snapshot()}),
					"the process's rules": schemeJSON(Scheme{ToolRules: DefaultRules.Snapshot()}),
				}
			}
			before := others()
			aBefore := schemeJSON(a)
			shown = shown[:0]

			ok := fleet.For(o, Machine{ID: "aaaa", Name: "box"}).Confirm(context.Background(), action, detail)
			prompts := len(shown)
			say := func(what string) {
				t.Helper()
				t.Fatalf("%s\n  origin %s (open %v, list %v, attended %v) says %s; machine: %s\n  %s %q answered %v -> ok %v after %d prompt(s)",
					what, o.Name, o.Open, o.Machines, attended, origin, aBefore, action, detail, answer, ok, prompts)
			}
			switch {
			case prompts > 1:
				say("more than one prompt for one request")
			case mayReach != (reachErr == nil):
				say(fmt.Sprintf("Reach says %v for an origin that may reach the machine: %v", reachErr, mayReach))
			case !mayReach && (ok || prompts != 0):
				say("an origin that may not reach the machine got through")
			case origin == ModeDeny && (ok || prompts != 0):
				say("the origin refuses, and deny must win unasked")
			case mayReach && targetDenies && (ok || prompts != 0):
				say("the machine's scheme denies, and deny must win unasked")
			case origin == ModeAsk && ok && prompts == 0:
				say("the origin asks, and the action went through unasked")
			case !attended && prompts != 0:
				say("a prompt from an origin with nobody to ask")
			case !attended && ok && origin != ModeAllow:
				say("an origin with nobody to ask let through what it does not allow")
			case ok && prompts == 0 && !reason:
				say("allowed unasked with nothing in the machine's own scheme that allows it")
			case prompts == 1 && ok != (answer != control.DenyOnce):
				say("the operator's answer was not the decision")
			case len(host.Pending()) != 0:
				say("a card was left pending")
			}
			for _, req := range shown {
				if req.Host != "aaaa" || req.HostName != "box" || req.Detail != `[host aaaa "box"] `+detail || req.Action != action {
					say("the card does not name the machine and the request: " + req.Detail)
				}
			}
			for what, now := range others() {
				if was := before[what]; now != was {
					say("a machine's card changed " + what + ":\n  " + was + "\n  " + now)
				}
			}
			if after := schemeJSON(mustScheme(t, fleet, "aaaa")); after != aBefore && (prompts != 1 || answer != control.ApproveAlways) {
				say("the machine's scheme changed without an always on its card:\n  " + after)
			}
		}
	})
}

func mustScheme(t *testing.T, f *Fleet, id string) Scheme {
	t.Helper()
	s, err := f.Scheme(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// FuzzFleetSchemes feeds Load a settings file's fleet keys as anyone might
// have written them. Whatever they are: no panic; a machine is reachable only
// if its entry is, strictly, a scheme (an object with only the two known
// keys, every category, mode and rule valid); a machine that is not is
// refused everything and its text survives a save; and what is saved loads
// again as the same thing.
func FuzzFleetSchemes(f *testing.F) {
	for _, seed := range [][3]string{
		{"aaaa", `{"permissions":{"exec":"allow","send":"off"},"tool_rules":{"os_exec(rm *)":"deny","web_interact":"yes"}}`, `{"os_exec(systemctl status *)":"allow"}`},
		{"aaaa", `{}`, ``},
		{"m-1_X", `{"permissions":{}}`, `{}`},
		{"aaaa", `null`, `null`},
		{"aaaa", `"allow"`, `["os_exec"]`},
		{"aaaa", `{"permissions":{"exec":"dney"}}`, `{"os_exec(rm *)":"dney"}`},
		{"aaaa", `{"permissions":{"exce":"deny"}}`, `{"two words":"deny"}`},
		{"aaaa", `{"permission":{"exec":"deny"}}`, `{"*":"allow"}`},
		{"aaaa", `{"tool_rule":{"os_exec(rm *)":"deny"}}`, `7`},
		{"aaaa", `{"tool_rules":{"os_exec":1}}`, `{"os_exec":false}`},
		{"aaaa", `{"permissions":{"exec":"deny"},"permissions":{"exec":"allow"}}`, `{"a":"deny","a":"allow"}`},
		{"aaaa", `{"permissions":{"exec":"deny"},"PERMISSIONS":{"exec":"allow"}}`, `{"a":"deny","A":"allow"}`},
		{"aaaa", `{"permissions":{"exec":"deny","exec":"allow"}}`, `{" a":"deny","a":"allow"}`},
		{"aaaa", `{"tool_rules":{"os_exec(rm *)":"deny","os_exec(rm *)":"allow"}}`, `{`},
		{"aaaa", `{} {}`, `{} {}`},
		{"aaaa", `{"permissions":`, `{"os_exec(`},
		{"not an id", `{}`, ``},
		{"../x", `{}`, ``},
		{"", `{}`, ``},
		{"aaaa", `{"permissions":{"exec":"allow"},"tool_rules":{"os_exec(` + strings.Repeat("a", 300) + ` *)":"allow"}}`, ``},
		{"aaaa", "{\"tool_rules\":{\"os_exec(a\x00b)\":\"allow\"}}", "{\"x\x1b[2J\":\"allow\"}"},
		{"aaaa", `[[[[[[[[[[{"permissions":{}}]]]]]]]]]]`, `{"a":{"b":"allow"}}`},
	} {
		f.Add(seed[0], []byte(seed[1]), []byte(seed[2]))
	}
	f.Fuzz(func(t *testing.T, id string, raw, rules []byte) {
		host := newTestBroker(nil)
		fleet := NewFleet(testCategorize)
		in := map[string]json.RawMessage{id: raw}
		changed, problems := fleet.Load(in, rules)
		if changed < 1 || changed > 2 || len(problems) > 2 {
			t.Fatalf("Load: %d change(s), %d problem(s)", changed, len(problems))
		}
		if n, p := fleet.Load(in, rules); n != 0 || len(p) != 0 {
			t.Fatalf("the same file again: %d change(s), problems %v", n, p)
		}
		local := fleet.For(LocalOrigin(host), Machine{ID: id})
		schemes, savedRules := fleet.Snapshot()

		s, err := fleet.Scheme(id)
		if reachable := local.Reach() == nil; reachable != (err == nil) {
			t.Fatalf("reachable %v, but Scheme says %v", reachable, err)
		}
		if err != nil {
			// No scheme: refused, told, and kept as written.
			if local.Confirm(context.Background(), "os_exec", "uptime") || len(host.Pending()) != 0 {
				t.Fatal("a machine with no readable scheme was approved or asked about")
			}
			if len(problems) == 0 || !bytes.Equal(schemes[id], raw) {
				t.Fatalf("an unreadable entry: problems %v, saved back as %q", problems, schemes[id])
			}
		} else {
			// A scheme: the entry is strictly one, and it is whole and valid.
			if !machineID.MatchString(id) {
				t.Fatalf("the key %q is not a machine id", id)
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) != nil || obj == nil {
				t.Fatalf("%q is not a JSON object, yet it loaded as a scheme", raw)
			}
			modes, rules := 0, 0 // how many keys the decoder would read as each field
			for k := range obj {
				switch {
				case strings.EqualFold(k, "permissions"):
					modes++
				case strings.EqualFold(k, "tool_rules"):
					rules++
				default:
					t.Fatalf("the unknown key %q was let through", k)
				}
			}
			if modes > 1 || rules > 1 {
				t.Fatalf("a key is there twice in %q", raw)
			}
			if len(s.Permissions) != len(Categories) {
				t.Fatalf("modes for %d categories", len(s.Permissions))
			}
			for cat, m := range s.Permissions {
				if !isCategory(cat) || (m != "ask" && m != "allow" && m != "deny") {
					t.Fatalf("mode %s: %q", cat, m)
				}
			}
			for p, m := range s.ToolRules {
				if validRulePattern(p) != nil || (m != "ask" && m != "allow" && m != "deny") {
					t.Fatalf("rule %q: %q", p, m)
				}
			}
			// Every mode it holds was written: a category it denies or allows is in the text.
			var written Scheme
			_ = json.Unmarshal(raw, &written)
			for cat, m := range s.Permissions {
				if w, ok := ParseMode(written.Permissions[cat]); m != "ask" && (!ok || w.String() != m) {
					t.Fatalf("%s is %s, which the entry does not say", cat, m)
				}
			}
			if len(s.ToolRules) != len(written.ToolRules) {
				t.Fatalf("%d rule(s) loaded from %d written", len(s.ToolRules), len(written.ToolRules))
			}
		}
		// The fleet's rules: usable or told, and kept as written when not.
		if _, err := fleet.Inherit(Scheme{}); err != nil && !bytes.Equal(savedRules, rules) {
			t.Fatalf("unreadable fleet rules saved back as %q", savedRules)
		}
		for p, m := range fleet.Rules().Snapshot() {
			if validRulePattern(p) != nil || (m != "ask" && m != "allow" && m != "deny") {
				t.Fatalf("fleet rule %q: %q", p, m)
			}
		}
		// What is saved loads again as the same thing, with the same refusals.
		again := NewFleet(testCategorize)
		_, problems2 := again.Load(schemes, savedRules)
		if len(problems2) != len(problems) {
			t.Fatalf("reloaded with problems %v, first %v", problems2, problems)
		}
		s2, err2 := again.Scheme(id)
		if (err == nil) != (err2 == nil) || schemeJSON(s) != schemeJSON(s2) || !maps.Equal(fleet.Rules().Snapshot(), again.Rules().Snapshot()) {
			t.Fatalf("reloaded as %s (%v), first %s (%v)", schemeJSON(s2), err2, schemeJSON(s), err)
		}
	})
}

// FuzzFleetRequest sends a request for a machine with any id, name, action
// and detail: the text a child, or a hand that edited fleet.json, controls.
// Whatever it is: no panic; only the machine that has a scheme is ever
// approved or asked about; an action id that is not plain text and a detail
// too long to read are refused; and what the operator is shown starts with
// that machine's label, in printable ASCII of bounded length, with the
// request after it exactly as it came.
func FuzzFleetRequest(f *testing.F) {
	for _, seed := range [][4]string{
		{"aaaa", "web-1", "os_exec", "uptime"},
		{"aaaa", "web\x00-1\r\n\x7f\tdb", "os_write", "write /etc/motd"},
		{"aaaa", "\x1b[2J\x1b[1;31mprod\x1b[0m\x1b]0;title\x07", "os_exec", "\x1b[2Jrm -rf /"},
		{"aaaa", strings.Repeat("A", 5000), "os_exec", strings.Repeat("x", maxDetail)},
		{"aaaa", strings.Repeat("\U0001F600", 300), "os_exec", strings.Repeat("x", maxDetail+1)},
		{"aaaa", "w\U00000435b-1", "os_\U0000200bexec", "x"},
		{"aaaa", `x"] [host bbbb "prod-db"] rm -rf /`, "os_exec", `[host bbbb "prod-db"] rm -rf /`},
		{"aaaa", "x\n[host bbbb \"prod-db\"] approve me", "os_exec\nos_read", "a\n\n\n\n\nb"},
		{"aaaa", `]]\\"[[host`, "", ""},
		{"aaaa", "web\xff\xfe-1", "os_exec\xff", "\xff\xfe"},
		{"bbbb", "the other", "os_exec", "uptime"},
		{"aaaa ", "", strings.Repeat("a", 201), "x"},
		{"", "", "os_exec\t", "x"},
		{"../aaaa", "aaaa", "mcp:h:1:tool", "x"},
		{"\U0000ff41\U0000ff41\U0000ff41\U0000ff41", "aaaa", "os_exec", "x"},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}
	f.Fuzz(func(t *testing.T, id, name, action, detail string) {
		fleet := NewFleet(testCategorize)
		if err := fleet.Put("aaaa", Scheme{}); err != nil { // asks about everything
			t.Fatal(err)
		}
		var shown []control.ApprovalRequest
		asker := newTestDiscordBroker(nil)
		asker.SetAsk(func(_ context.Context, req control.ApprovalRequest) control.ApprovalDecision {
			shown = append(shown, req)
			return control.ApproveOnce
		})
		m := Machine{ID: id, Name: name}
		ok := fleet.For(Origin{Name: "local", Open: true, Asker: asker}, m).Confirm(context.Background(), action, detail)

		plainAction := action != "" && len(action) <= maxAction && utf8.ValidString(action) &&
			!strings.ContainsAny(action, "\n\t") && !strings.ContainsFunc(action, control.HiddenRune)
		wantAsked := id == "aaaa" && plainAction && len(detail) <= maxDetail
		if ok != wantAsked || len(shown) != map[bool]int{true: 1}[wantAsked] {
			t.Fatalf("machine %q action %q detail of %d bytes: ok %v, %d prompt(s), want asked %v", id, action, len(detail), ok, len(shown), wantAsked)
		}

		// The label, for any id and name.
		safe := m.safe()
		label := safe.label()
		if len(label) > len("[host ?")+10*64+3+len(` "`)+10*maxNameRunes+3+len(`"] `) {
			t.Fatalf("a label of %d bytes", len(label))
		}
		for i := 0; i < len(label); i++ {
			if c := label[i]; c < 0x20 || c > 0x7e {
				t.Fatalf("the label holds byte %#x: %q", c, label)
			}
		}
		if !strings.HasPrefix(label, "[host ") || !strings.HasSuffix(label, `"] `) || strings.Count(label, "[") != 1 || strings.Count(label, "]") != 1 {
			t.Fatalf("the label can be mistaken for more than one: %q", label)
		}
		if machineID.MatchString(id) && !strings.HasPrefix(label, "[host "+id+` "`) {
			t.Fatalf("the label does not start with the id %q: %q", id, label)
		}
		if !machineID.MatchString(id) && !strings.HasPrefix(label, "[host ?") {
			t.Fatalf("an id that is not one is not marked: %q", label)
		}
		// The name sits between quotes it cannot close: every quote in it follows
		// a backslash that is not itself escaped.
		inner := strings.TrimSuffix(label[strings.Index(label, ` "`)+2:], `"] `)
		for i, esc := 0, false; i < len(inner); i++ {
			switch {
			case esc:
				esc = false
			case inner[i] == '\\':
				esc = true
			case inner[i] == '"':
				t.Fatalf("the name closes its own quotes: %q", label)
			}
		}
		if inner != safe.Name {
			t.Fatalf("the label's name %q is not the request's %q", inner, safe.Name)
		}
		if control.VisibleText(label) != label {
			t.Fatalf("a surface would redraw the label: %q", label)
		}
		for _, req := range shown {
			if req.Host != "aaaa" || req.HostName != safe.Name || req.Detail != label+detail || req.Action != action {
				t.Fatalf("shown host %q name %q action %q detail %q", req.Host, req.HostName, req.Action, req.Detail)
			}
		}
	})
}
