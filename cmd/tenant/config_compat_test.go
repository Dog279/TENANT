package main

import (
	"log/slog"
	"strings"
	"testing"

	"tenant/internal/logs"
)

// A persist hook's failed config save reaches the operator's feed, and every
// failed save is in the log book, whoever made it.
func TestPersistChoice_FailureToldAndLogged(t *testing.T) {
	hub := logs.NewMemory(0)
	prev := logs.Default()
	logs.Install(hub)
	defer logs.Install(prev)

	var told []string
	persistChoice(notADir(t), &launchConfig{}, func(s string) { told = append(told, s) },
		"relay: the permission change", func(lc *launchConfig) { lc.Relay.Permissions = map[string]string{"exec": "allow"} })
	if len(told) != 1 || !strings.Contains(told[0], "relay: the permission change applies now but was not saved") {
		t.Fatalf("the operator was told %q", told)
	}
	entries, _ := hub.Snapshot()
	if len(entries) != 1 || entries[0].Source != "system" || entries[0].Level != slog.LevelWarn {
		t.Fatalf("want one system warning, got %+v", entries)
	}
}
