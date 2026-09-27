package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every name the old switch in main dispatched still does (TEN-320).
var legacyCLINames = strings.Fields(`version -v --version setup oauth-setup model models route runs agents agent
chat distill consolidate memory mcp-memory mcp-selftest tool-test web sql wiki gsuite simplefin x imessage os
serve attach log sandbox tui orchestrate team research goal review skills atlassian mcp peer ack undo doctor
backup privacy security permissions eval help -h --help`)

func TestEveryLegacyCLICommandDispatches(t *testing.T) {
	for _, name := range legacyCLINames {
		if cliRegistry().Lookup(name) == nil {
			t.Errorf("tenant %s no longer dispatches", name)
		}
	}
}

// The old usage() left dispatched commands out (oauth-setup, consolidate,
// goal, skills, atlassian, ack, undo). Now every one is in `tenant help all`.
func TestEveryCLICommandIsInTheFullHelp(t *testing.T) {
	var b bytes.Buffer
	writeFullHelp(&b)
	help := b.String()
	for _, c := range cliRegistry().Commands() {
		if !strings.Contains(help, "tenant "+c.Names[0]) {
			t.Errorf("tenant %s dispatches but is missing from `tenant help all`", c.Names[0])
		}
	}
}

func TestBeginnerHelpIsTaskOriented(t *testing.T) {
	var b, full bytes.Buffer
	writeBeginnerHelp(&b)
	writeFullHelp(&full)
	help := b.String()
	for _, c := range cliRegistry().Tasks() {
		if !strings.Contains(help, "tenant "+c.Names[0]) || !strings.Contains(help, c.Task) {
			t.Errorf("beginner help lacks %s", c.Names[0])
		}
	}
	for _, want := range []string{"tenant help all", "tenant setup", "tenant tui"} {
		if !strings.Contains(help, want) {
			t.Errorf("beginner help lacks %q", want)
		}
	}
	if strings.Count(help, "\n") >= strings.Count(full.String(), "\n")/3 {
		t.Error("the beginner help should be a short list of jobs, not the reference")
	}
}

func TestServeDescribesTheWholeHub(t *testing.T) {
	var b bytes.Buffer
	writeCommandHelp(&b, cliRegistry().Lookup("serve"))
	for _, want := range []string{"dashboard", "self-improvement", "cron", "Discord relay", "peer listener", "MCP", "retention"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("tenant help serve does not mention %s:\n%s", want, b.String())
		}
	}
}

func TestHelpForOneCommand(t *testing.T) {
	var b bytes.Buffer
	writeCommandHelp(&b, cliRegistry().Lookup("backup"))
	for _, want := range []string{"tenant backup verify FILE", "tenant backup restore FILE", "tenant backup --help"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("tenant help backup lacks %q", want)
		}
	}
	if err := cmdHelp(context.Background(), []string{"frobnicate"}); err == nil {
		t.Fatal("help for an unknown command succeeded")
	}
}

func TestCompletionCoversEveryCommand(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var b bytes.Buffer
		if err := writeCompletion(&b, shell); err != nil {
			t.Fatal(err)
		}
		for _, c := range cliRegistry().Commands() {
			if !strings.Contains(b.String(), c.Names[0]) {
				t.Errorf("%s completion lacks %s", shell, c.Names[0])
			}
		}
		if !strings.Contains(b.String(), "passphrase-file") || !strings.Contains(b.String(), "verify") {
			t.Errorf("%s completion lacks backup's flags or subcommands", shell)
		}
	}
	if err := writeCompletion(&bytes.Buffer{}, "tcsh"); err == nil {
		t.Fatal("an unknown shell was accepted")
	}
}

// The bash script at least parses, when bash is here to ask.
func TestBashCompletionParses(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	var b bytes.Buffer
	if err := writeCompletion(&b, "bash"); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "tenant.bash")
	if err := writeFileForTest(script, b.Bytes()); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bash, "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func writeFileForTest(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
