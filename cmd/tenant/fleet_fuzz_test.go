package main

import (
	"encoding/json"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"

	"tenant/internal/approval"
)

// What the operator types on the command line becomes the settings file
// (a machine's scheme, the fleet's rules) and decides which config dir a
// command works on. These fuzz the three places where that text is read.

const fuzzMachine = "m0123456789abcdef"

// fuzzFleet is a fleet with one machine, and the settings file's two keys as
// each change saves them.
func fuzzFleet(t *testing.T) (f *approval.Fleet, saved func() (map[string]json.RawMessage, json.RawMessage)) {
	t.Helper()
	f = approval.NewFleet(categorize)
	var schemes map[string]json.RawMessage
	var rules json.RawMessage
	f.SetPersist(func(snapshot func() (map[string]json.RawMessage, json.RawMessage)) { schemes, rules = snapshot() })
	s, err := f.Inherit(approval.Scheme{Permissions: map[string]string{"web": "allow"}, ToolRules: map[string]string{"os_exec(rm *)": "deny"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Put(fuzzMachine, s); err != nil {
		t.Fatal(err)
	}
	return f, func() (map[string]json.RawMessage, json.RawMessage) { return schemes, rules }
}

// reloads fails the test unless what was saved is a file the master accepts
// whole: every machine's entry a scheme, the rules a rule set.
func reloads(t *testing.T, saved func() (map[string]json.RawMessage, json.RawMessage), after string) {
	t.Helper()
	schemes, rules := saved()
	whole, err := json.Marshal(settings{FleetSchemes: schemes, FleetRules: rules})
	if err != nil {
		t.Fatalf("after %q the settings do not encode: %v", after, err)
	}
	var back settings
	if err := json.Unmarshal(whole, &back); err != nil {
		t.Fatalf("after %q the settings file does not parse: %v", after, err)
	}
	fresh := approval.NewFleet(categorize)
	if _, problems := fresh.Load(back.FleetSchemes, back.FleetRules); len(problems) > 0 {
		t.Fatalf("after %q the master would refuse the file: %v", after, problems)
	}
	if _, err := fresh.Scheme(fuzzMachine); err != nil {
		t.Fatalf("after %q the machine lost its scheme: %v", after, err)
	}
}

// The category=mode pairs of the confirm screen: a pair is taken only if it
// names a category and a mode, and a scheme with the pairs applied is one the
// fleet saves and the master loads.
func FuzzFleetSet(f *testing.F) {
	for _, seed := range []string{
		"exec=allow", "send=ask\nwrite=deny", "EXEC = Allow", "exec=allow\nexec=deny", "", "=", "exec", "exec=", "=allow",
		"root=allow", "exec=always", "exec=allow=deny", "exec\x00=allow", "exec=allow\x1b[2J", "tool_rules=allow", "permissions=deny",
		"exec=allow\n\"}, \"m0\": {\"permissions\": {\"exec\": \"allow", "ехес=allow", strings.Repeat("exec=allow\n", 200),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		pairs := strings.Split(in, "\n")
		set, err := parseSet(pairs)
		if err != nil {
			return
		}
		for cat, mode := range set {
			if !slices.Contains(approval.Categories, cat) || !slices.Contains([]string{"ask", "allow", "deny"}, mode) {
				t.Fatalf("parseSet(%q) took %q=%q", pairs, cat, mode)
			}
		}
		fl, saved := fuzzFleet(t)
		s, err := fl.Scheme(fuzzMachine)
		if err != nil {
			t.Fatal(err)
		}
		for cat, mode := range set {
			s.Permissions[cat] = mode
		}
		if err := fl.Put(fuzzMachine, s); err != nil {
			t.Fatalf("the fleet refused a scheme made from accepted pairs %q: %v", pairs, err)
		}
		reloads(t, saved, in)
	})
}

// The permissions edits (set, add, rm) for a machine and for the fleet's
// rules: an edit is applied or refused, never half, and whatever it saved is
// a settings file the master loads without a complaint.
func FuzzFleetPermEdit(f *testing.F) {
	for _, seed := range []string{
		"set\nexec\nallow", "set\nsandbox\ndeny", "add\nos_exec(systemctl status *)\nallow", "add\nos_exec(git\nlog\n*)\nask",
		"add\ndelete_*\ndeny", "rm\nos_exec(rm *)", "rm\nnothing", "rule\nmcp:host:1:*\nallow", "set\nexec", "set\nroot\nallow", "set\nexec\nsometimes",
		"add\n*\nallow", "add\nos_exec(*)\nallow", "add\nos_exec()\nallow", "add\nx y\nallow", "add\n\x1b[2J\nallow", "add\n\"},\"tool_rules\":{\"*\nallow",
		"add\n" + strings.Repeat("a", 300) + "\nallow", "add\nos_exec(a * b *)\nallow", "del\nos_exec(rm *)", "\n\n", "add\nfleet_schemes\ndeny", "set\npermissions\nallow",
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}
	f.Fuzz(func(t *testing.T, in string, machine bool) {
		args := strings.Split(in, "\n")
		fl, saved := fuzzFleet(t)
		before, _ := json.Marshal(func() any { s, r := saved(); return []any{s, r} }())
		var done string
		var err error
		if machine {
			b, cerr := fl.Child(fuzzMachine)
			if cerr != nil {
				t.Fatal(cerr)
			}
			done, err = permEdit(b, b, surfaceCLI, args)
		} else {
			done, err = permEdit(nil, fleetRuleSet{fl.Rules()}, surfaceCLI, args)
		}
		after, _ := json.Marshal(func() any { s, r := saved(); return []any{s, r} }())
		if err != nil && string(before) != string(after) {
			t.Fatalf("the refused edit %q changed what is saved:\n%s\n%s", args, before, after)
		}
		if err == nil {
			for i := 0; i < len(done); i++ {
				if c := done[i]; c < 0x20 || c > 0x7e {
					t.Fatalf("the answer to %q carries byte %#x: %q", args, c, done)
				}
			}
		}
		reloads(t, saved, in)
	})
}

// Flags anywhere among the arguments: every flag given is applied, wherever
// it stands, and no argument is lost or made up. A --config that was read as
// an argument would send the command to the default config dir.
func FuzzFleetFlagsAnywhere(f *testing.F) {
	for _, seed := range []string{
		"web1", "web1\n--config\nC", "--config\nC\nweb1", "--defaults\nadd\nos_exec(systemctl status *)\nallow\n--config\nC\n--data\nD",
		"web1\n--set\nexec=allow\n--set\nsend=ask\n--config\nC", "--config=C\nweb1\nset\nexec\nallow", "web1\n--from\ndefaults\n--config\nC",
		"a\nb\nc\n--config\nC", "--config\nC", "", "web1\n--bogus", "web1\n--config", "-\n--config\nC", "web1\n--\n--config\nC", "--config\n--data\nD",
		"a\n-config\nC", "--defaults=false\nx", "--defaults=maybe\nx",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		args := strings.Split(in, "\n")
		if slices.Contains(args, "--") || slices.Contains(args, "-") {
			return // the terminator and a lone dash follow the flag package's own rules
		}
		fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		cfg := fs.String("config", "", "")
		data := fs.String("data", "", "")
		defaults := fs.Bool("defaults", false, "")
		from := fs.String("from", "", "")
		var set stringList
		fs.Var(&set, "set", "")
		pos, err := parseInterspersed(fs, args)
		if err != nil {
			return
		}
		// What a plain reading of the same arguments says: a flag takes the
		// next argument as its value unless it carries one or is the switch.
		var wantPos, wantSet []string
		wantCfg, wantData, wantFrom, wantDefaults := "", "", "", false
		for i := 0; i < len(args); i++ {
			a := args[i]
			if len(a) < 2 || a[0] != '-' {
				wantPos = append(wantPos, a)
				continue
			}
			name, value, has := strings.Cut(strings.TrimLeft(a, "-"), "=")
			if name == "defaults" {
				wantDefaults = !has || value == "true" || value == "1" || value == "t" || value == "T" || value == "TRUE" || value == "True"
				continue
			}
			if !has {
				if i++; i >= len(args) {
					t.Fatalf("parse of %q succeeded with flag %q lacking a value", args, a)
				}
				value = args[i]
			}
			switch name {
			case "config":
				wantCfg = value
			case "data":
				wantData = value
			case "from":
				wantFrom = value
			case "set":
				wantSet = append(wantSet, value)
			}
		}
		if !slices.Equal(pos, wantPos) || *cfg != wantCfg || *data != wantData || *from != wantFrom || *defaults != wantDefaults || !slices.Equal([]string(set), wantSet) {
			t.Fatalf("parse of %q:\n  arguments %q, want %q\n  config %q/%q data %q/%q from %q/%q defaults %v/%v set %q/%q",
				args, pos, wantPos, *cfg, wantCfg, *data, wantData, *from, wantFrom, *defaults, wantDefaults, []string(set), wantSet)
		}
	})
}
