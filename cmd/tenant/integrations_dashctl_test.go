package main

import (
	"regexp"
	"strings"
	"testing"
)

// C-6: the Integrations page shows every integration's own words (its name,
// setup hint, field prompts and choices). None may tell the operator to run a
// backticked command or a slash command, or to press Enter (a terminal's
// words), whatever the terminal app says around them.
func TestIntegrationWords_NoTerminalCommands(t *testing.T) {
	slash := regexp.MustCompile(`(?:^|\s)/[a-z][a-z-]*\b`)
	if len(skillKinds) == 0 {
		t.Fatal("no integrations in the catalog")
	}
	for id, k := range skillKinds {
		texts := []string{k.Label, k.SetupHint}
		for _, f := range k.Fields {
			texts = append(texts, f.Prompt)
			texts = append(texts, f.OptionLabels...)
		}
		for _, s := range texts {
			switch {
			case strings.Contains(s, "`"):
				t.Errorf("%s: a backticked command: %q", id, s)
			case slash.MatchString(s):
				t.Errorf("%s: a slash command: %q", id, s)
			case strings.Contains(s, "Enter to skip"):
				t.Errorf("%s: terminal words: %q", id, s)
			case strings.Contains(strings.ToLower(s), "bearer token"):
				t.Errorf("%s: jargon: %q", id, s)
			}
		}
	}
}
