package main

import (
	"encoding/json"
	"os"
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
