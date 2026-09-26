package tui

import (
	"context"
	"strings"
	"testing"
)

// Every name the old monolithic switch handled still dispatches (TEN-303).
var legacySlashNames = strings.Fields(`/? /ack /agent /agents /approve /approve! /cancel /cancel-clarify
/ceiling /clear /cls /compact /compress /config /configure /cron /cronjob /dashboard /deep /deep!
/deny /disable /effort /enable /eval /exit /expand /gh /github /goal /help /imessage /imsg /judge
/loop-ceiling /loops /mcp /mem /memory /model /models /mouse /peer /permissions /perms /quit
/reasoning /reject /relay /research /research! /review /route /setup /skill /skills /tailscale
/tools /ts /undo /who /whoami`)

func TestEveryLegacyCommandStillDispatches(t *testing.T) {
	for _, name := range legacySlashNames {
		if slashCommandNamed(name) == nil {
			t.Errorf("%s no longer dispatches", name)
		}
	}
	if slashCommandNamed("/MEMORY") == nil {
		t.Error("lookup is case-sensitive; commands were case-insensitive before")
	}
}

// A dispatched command cannot be missing from the help (TEN-320): each one
// shows in /help all under its canonical name.
func TestEveryCommandIsInTheHelp(t *testing.T) {
	plain, _ := renderHelp()
	for _, c := range slashRegistry().Commands() {
		found := false
		for _, row := range c.HelpRows() {
			if strings.Contains(plain, row.Syntax) {
				found = true
			}
		}
		if !found || !strings.Contains(plain, c.Names[0]) {
			t.Errorf("%s is dispatched but missing from /help all", c.Names[0])
		}
		for _, row := range c.HelpRows() {
			if !strings.Contains(row.Syntax, c.Names[0]) && !strings.Contains(strings.Join(c.Names, " "), strings.Fields(strings.Split(row.Syntax, ",")[0])[0]) {
				t.Errorf("%s has a help row for something else: %q", c.Names[0], row.Syntax)
			}
		}
	}
}

func TestHelpIndexStartsWithTasks(t *testing.T) {
	plain, _ := renderHelpIndex()
	start := strings.Index(plain, "Start here:")
	cats := strings.Index(plain, "Categories")
	if start < 0 || cats < start {
		t.Fatalf("index lacks the Start here block before the categories:\n%s", plain)
	}
	for _, c := range slashRegistry().Tasks() {
		if !strings.Contains(plain[start:cats], c.Names[0]) || !strings.Contains(plain[start:cats], c.Task) {
			t.Errorf("Start here lacks %s", c.Names[0])
		}
	}
	if len(slashRegistry().Tasks()) < 4 {
		t.Error("the beginner block should name at least four commands")
	}
}

func TestUnknownSlashCommand(t *testing.T) {
	m := newModel(context.Background(), Config{})
	if cmd := m.handleSlash("/frobnicate now"); cmd != nil {
		t.Fatal("an unknown command returned a command")
	}
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "unknown command /frobnicate") {
		t.Fatalf("unknown command message = %q", last)
	}
}

func TestSlashParts(t *testing.T) {
	// submit trims the line before dispatch; the argument keeps its inner
	// spacing, as the old switch did.
	fields, cmd, arg := slashParts("/Memory   search  the thing ")
	if cmd != "/memory" || arg != "search  the thing" || len(fields) != 4 {
		t.Fatalf("slashParts = %q %q %q", fields, cmd, arg)
	}
	if f, c, a := slashParts("   "); f != nil || c != "" || a != "" {
		t.Fatal("blank line not handled")
	}
}
