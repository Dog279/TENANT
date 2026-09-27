package catalog

import (
	"strings"
	"testing"

	"tenant/internal/logs/event"
)

// Every built-in definition passes the registration checks again and keeps
// the conventions, documentation included.
func TestTheCatalogIsValidAndDocumented(t *testing.T) {
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	specs := Specs("")
	if len(specs) == 0 {
		t.Fatal("no definitions registered")
	}
	if err := Check(specs, ""); err != nil {
		t.Fatal(err)
	}
}

func spec(provider string, id uint32, lvl event.Level) event.Spec {
	return event.Spec{Provider: provider, Log: provider, ID: id, Level: lvl, Task: "T", Template: "x",
		Meaning: "m", Cause: "c", Action: "a"}
}

func TestCheckHoldsTheIDConventions(t *testing.T) {
	diag := spec("p", 9001, event.Verbose)
	diag.Channel = event.Diagnostic
	retired := spec("p", 2002, event.Info) // wrong for its block, but retired
	retired.Retired = true
	ok := []event.Spec{spec("p", 3, event.Critical), spec("p", 1001, event.Info), spec("p", 2001, event.Warning),
		spec("p", 3001, event.Critical), spec("p", 4103, event.Warning), spec("p", 5001, event.Info), diag, retired}
	if err := Check(ok, ""); err != nil {
		t.Fatalf("conforming specs: %v", err)
	}

	undocumented := spec("p", 1, event.Info)
	undocumented.Meaning = ""
	opDiag := spec("p", 1002, event.Info)
	opDiag.Channel = event.Diagnostic
	for _, c := range []struct {
		s    event.Spec
		want string
	}{
		{undocumented, "not documented"},
		{spec("p", 7001, event.Info), "in no block"},
		{spec("p", 10001, event.Info), "in no block"},
		{spec("p", 2001, event.Error), "the 2000s are warnings"},
		{spec("p", 3001, event.Warning), "the 3000s are errors"},
		{spec("p", 9001, event.Verbose), "the diagnostic channel"}, // operational
		{opDiag, "the diagnostic channel"},
	} {
		if err := Check([]event.Spec{c.s}, ""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d: Check = %v, want %q", c.s.ID, err, c.want)
		}
	}
}

// An ID the committed reference lists can't disappear: retiring it keeps it.
func TestCheckRefusesADroppedID(t *testing.T) {
	specs := []event.Spec{spec("mcp.web", 1001, event.Info), spec("mcp.web", 3001, event.Error)}
	prev := Markdown(specs)
	if err := Check(specs, prev); err != nil {
		t.Fatal(err)
	}
	if err := Check(specs[:1], prev); err == nil || !strings.Contains(err.Error(), "mcp.web/3001") || !strings.Contains(err.Error(), "Retired()") {
		t.Fatalf("dropping 3001: %v", err)
	}
	specs[1].Retired = true
	if err := Check(specs, prev); err != nil {
		t.Fatalf("retiring 3001: %v", err)
	}
	if md := Markdown(specs); !strings.Contains(md, "| [3001](#mcpweb-3001) | error (retired) |") || !strings.Contains(md, "**Retired:**") {
		t.Fatalf("the retired entry:\n%s", md)
	}
}

// Discovery question 5: "What does this event mean, and what should I do?"
// The catalog answers it for any (provider, ID) an event carries. It also
// gives question 7 ("Has this happened before?") the filter the store's
// statistics are read with.
func TestHelpSaysWhatAnEventMeansAndWhatToDo(t *testing.T) {
	d, ok := event.Lookup("system", 3101)
	if !ok {
		t.Fatal("system/3101 not found")
	}
	help := Help(d.Spec())
	for _, want := range []string{
		"### system 3101", "`{component} failed: {err}`", "**Level:** error", "**Log:** system",
		"**What it means.** A subsystem stopped with an error.",
		"**Why it happens.** Something it depends on failed",
		"**What to do.** Open the component's log",
		"`provider:system id:3101`",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("Help lacks %q:\n%s", want, help)
		}
	}
	if _, ok := event.Lookup("system", 9999); ok {
		t.Error("Lookup found an ID nobody declared")
	}
	if _, ok := event.Lookup("nosuch", 3101); ok {
		t.Error("Lookup found an unknown provider's event")
	}
	if d, ok := event.Lookup("security", 4103); !ok || !strings.Contains(Help(d.Spec()), "**Keywords:** `audit.failure`") {
		t.Errorf("security/4103's keywords: %v", ok)
	}
}

func TestMarkdownKeepsATemplateInItsCell(t *testing.T) {
	s := spec("p", 1001, event.Info)
	s.Template = "a | b `c`"
	if md := Markdown([]event.Spec{s}); !strings.Contains(md, "| T | `` a \\| b `c` `` |\n") {
		t.Fatalf("the index row:\n%s", md)
	}
}

func TestGroupPlacesEveryKindOfLog(t *testing.T) {
	for log, want := range map[string]string{
		"system": "Tenant", "turns": "Tenant", "discord": "Subsystems/Channels", "cron": "Subsystems/Scheduler",
		"mcp.github": "Subsystems/Integrations/MCP", "plugin.web": "Subsystems/Integrations/Plugins",
		"route": "Subsystems/Model", "improve": "Subsystems/Memory", "privacy": "Subsystems/Backup and Privacy",
		"ext.deploy": "External", "myfeature": "Subsystems",
	} {
		if got := strings.Join(Group(log), "/"); got != want {
			t.Errorf("Group(%q) = %q, want %q", log, got, want)
		}
	}
}
