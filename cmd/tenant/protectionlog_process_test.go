package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A process protection that is not in force leaves a record for every
// command that opens Tenant's data, not only serve and the TUI (decision
// A40-bis): one event (system 2030) in the log book per start, and one line
// on stderr at a terminal. A command that opens nothing (version, doctor)
// writes nothing, and with every control in force nothing is written.
func TestAMissingControlIsLoggedByEveryCommand(t *testing.T) {
	first, _ := forcedControls(t) // skips where there are no controls
	bin := protectTestBinary(t)
	removeTestExclusion(t, bin) // the starts with every control in force write it
	u := newTenantUnderTest(t, bin)
	if err := os.WriteFile(launchConfigPath(u.cfg), []byte(`{"dashboard":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dirs := []string{"--data", u.data, "--config", u.cfg}
	forced := []string{"TENANT_TEST_FORCE_FAIL=" + first + "=forced by the test", "TENANT_TEST_TERMINAL=1"}
	line := "process protection: running without " + first + ": forced by the test. `tenant doctor` says what that leaves open"

	for _, tc := range []struct {
		args   []string
		typed  string
		writes bool
	}{
		{append([]string{"chat", "--backend", "echo"}, dirs...), "hello\n", true}, // runs an agent turn
		{append([]string{"log", "test535", "a line"}, dirs...), "", true},         // opens the data and does something else
		{[]string{"version"}, "", false},                                          // prints, opens nothing
		{append([]string{"doctor"}, dirs...), "", false},                          // reports, opens nothing for writing
	} {
		_, _, stderr := u.exits(t, forced, tc.typed, tc.args...)
		if n := strings.Count(stderr, line); tc.writes && n != 1 || !tc.writes && n != 0 {
			t.Errorf("tenant %s with %s forced off: the line is on stderr %d times; want %v:\n%s", tc.args[0], first, n, tc.writes, stderr)
		}
	}
	// Every control in force: no line, no event.
	if _, _, stderr := u.exits(t, []string{"TENANT_TEST_TERMINAL=1"}, "hello\n", append([]string{"chat", "--backend", "echo"}, dirs...)...); strings.Contains(stderr, "process protection: running without") {
		t.Errorf("tenant chat with every control in force says:\n%s", stderr)
	}

	// What `tenant logs` reads back: one event per start that ran without it.
	_, out, stderr := u.exits(t, nil, "", append([]string{"logs", "query", "--format", "json", "--limit", "500"}, dirs...)...)
	byCommand := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		var e struct {
			ID   int               `json:"id"`
			Data map[string]string `json:"data"`
		}
		if json.Unmarshal([]byte(l), &e) != nil || e.ID != 2030 {
			continue
		}
		if e.Data["control"] != first || e.Data["reason"] != "forced by the test" || e.Data["acknowledged"] != "" {
			t.Errorf("event 2030 carries %v", e.Data)
		}
		byCommand[e.Data["command"]]++
	}
	if len(byCommand) != 2 || byCommand["chat"] != 1 || byCommand["log"] != 1 {
		t.Errorf("events 2030 by command: %v; want chat 1 and log 1, and none for version, doctor or the start with every control in force\n%s%s", byCommand, out, stderr)
	}
}

// The rule the owner's decision implies (decision 3 of 2026-11-19): every
// command that loads a secret or runs an agent turn leaves the record. These
// are the commands of OPEN-DECISIONS A61, each run once with a control off;
// those that need an account they do not have here (gsuite, imessage, x)
// fail after the point where the record is written, which is the first
// thing each does. Not run: web, which starts a browser (it passes the same
// c.resolve() first, cmd/tenant/commands.go cmdWeb). eval's smoke subset
// loads no key and runs no model, and writes nothing.
func TestEveryCommandThatRunsATurnLeavesTheRecord(t *testing.T) {
	first, _ := forcedControls(t)
	bin := protectTestBinary(t)
	removeTestExclusion(t, bin)
	u := newTenantUnderTest(t, bin)
	if err := os.WriteFile(launchConfigPath(u.cfg), []byte(`{"dashboard":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wiki, plan := t.TempDir(), filepath.Join(t.TempDir(), "plan.md")
	for path, body := range map[string]string{filepath.Join(wiki, "a.md"): "# a note\n", plan: "# Plan\nDo a thing.\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	forced := []string{"TENANT_TEST_FORCE_FAIL=" + first + "=forced by the test"}
	flags := []string{"--backend", "echo", "--data", u.data, "--config", u.cfg}
	want := map[string]int{}
	for _, tc := range []struct {
		name   string
		args   []string // "*" is where the flags go: the plugin commands read their task first
		writes bool
	}{
		{"chat", []string{"*"}, true},
		{"os", []string{"list the files here", "*"}, true},
		{"sql", []string{"how many tables", "*", "--db", filepath.Join(t.TempDir(), "x.db")}, true},
		{"wiki", []string{"what is in the note", "*", "--dir", wiki}, true},
		{"gsuite", []string{"list my mail", "*"}, true},
		{"imessage", []string{"list my chats", "*"}, true},
		{"x", []string{"read my timeline", "*"}, true},
		{"orchestrate", []string{"*", "say hi"}, true},
		{"team", []string{"*", "say hi"}, true},
		{"research", []string{"*", "what is hi"}, true},
		{"goal", []string{"*", "--max-turns", "1", "the answer is hi"}, true},
		{"review", []string{"*", "--reviewers", "eng", plan}, true},
		{"tool-test", []string{"*"}, true},
		{"eval", []string{"*", "--subset", "fitness", "--gate-only"}, true},
		{"eval", []string{"*", "--subset", "smoke"}, false},
	} {
		args := []string{tc.name}
		for _, a := range tc.args {
			if a == "*" {
				args = append(args, flags...)
			} else {
				args = append(args, a)
			}
		}
		u.exits(t, forced, "hello\n", args...)
		if tc.writes {
			name := map[string]string{"team": "orchestrate"}[tc.name] // an alias is recorded by its command's name
			if name == "" {
				name = tc.name
			}
			want[name]++
		}
	}
	_, out, _ := u.exits(t, nil, "", append([]string{"logs", "query", "--format", "json", "--limit", "500"}, "--data", u.data, "--config", u.cfg)...)
	got := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		var e struct {
			ID   int               `json:"id"`
			Data map[string]string `json:"data"`
		}
		if json.Unmarshal([]byte(l), &e) == nil && e.ID == 2030 {
			got[e.Data["command"]]++
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events 2030 by command: %v\nwant %v", got, want)
	}
}

// A record that cannot be written is not lost in silence (round four of the
// independent test: a program of the user denied itself writes on
// <data>\logs, and chat then ran with a control off and said nothing). The
// command says so on stderr, terminal or not, and the record goes to the
// system's log (here the test build's stand-in file).
func TestAnUnwritableLogBookIsSaid(t *testing.T) {
	first, _ := forcedControls(t)
	bin := protectTestBinary(t)
	u := newTenantUnderTest(t, bin)
	if err := os.WriteFile(launchConfigPath(u.cfg), []byte(`{"dashboard":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	chat := []string{"chat", "--backend", "echo", "--data", u.data, "--config", u.cfg}
	forced := []string{"TENANT_TEST_FORCE_FAIL=" + first + "=forced by the test"}
	line := "process protection: running without " + first + ": forced by the test. `tenant doctor` says what that leaves open"

	// Written: not a terminal, so nothing on stderr and nothing in the system's log.
	if _, _, stderr := u.exits(t, forced, "hello\n", chat...); strings.Contains(stderr, line) {
		t.Errorf("a record that was written, not at a terminal, is on stderr:\n%s", stderr)
	}
	if b, _ := os.ReadFile(u.systemLog); len(b) != 0 {
		t.Errorf("a record that was written went to the system's log too: %s", b)
	}

	logs := filepath.Join(u.data, "logs")
	denyWrites(t, logs)
	if b, err := os.ReadFile(filepath.Join(logs, "logbook.db")); err != nil || len(b) == 0 {
		t.Fatalf("the first start left no logbook.db to deny: %v", err)
	}
	code, _, stderr := u.exits(t, forced, "hello\n", chat...)
	if n := strings.Count(stderr, line+"; the record could not be written to the log book ("); code != 0 || n != 1 || !strings.Contains(stderr, "and is in the system's log") {
		t.Errorf("chat with the log folder unwritable: exit %d, the line %d times; want 0 and once, saying it is in the system's log:\n%s", code, n, stderr)
	}
	b, _ := os.ReadFile(u.systemLog)
	if got := strings.Split(strings.TrimSpace(string(b)), "\n"); len(got) != 1 ||
		!strings.Contains(got[0], "(chat) is running without a process protection") || !strings.Contains(got[0], "("+first+") could not be applied: forced by the test") ||
		!strings.Contains(got[0], "could not be written to its log book") {
		t.Errorf("the system's log: %q", got)
	}
	t.Logf("stderr:\n%s\nsystem log: %s", stderr, b)
}
