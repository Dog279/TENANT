package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"tenant/internal/agent"
)

func TestUnannounced(t *testing.T) {
	cats := []string{catEgress, catMCP, catSandbox}
	if got := unannounced(cats, []string{catMCP}); !slices.Equal(got, []string{catEgress, catSandbox}) {
		t.Fatalf("unannounced = %v", got)
	}
	if len(cats) != 3 || cats[1] != catMCP {
		t.Fatalf("unannounced must not change its input: %v", cats)
	}
}

func TestNewCategoryNotice(t *testing.T) {
	if got := newCategoryNotice(nil, "Review with /permissions."); got != "" {
		t.Fatalf("no categories: %q", got)
	}
	got := newCategoryNotice([]string{catEgress, catMCP}, "Review with /permissions.")
	want := "2 new safety checks ask you first until you choose: Share private data (egress), Connect remote services (mcp). Review with /permissions."
	if got != want {
		t.Fatalf("notice =\n%q\nwant\n%q", got, want)
	}
	if got := newCategoryNotice([]string{catSandbox}, "x"); !strings.HasPrefix(got, "1 new safety check asks you first") {
		t.Fatalf("one category: %q", got)
	}
}

// launchTUIPermissions wires the host broker from the settings file as cmdTUI
// does, persist hook and one-time notice included, and returns what was told.
func launchTUIPermissions(t *testing.T, cfg string) (*approvalBroker, []string) {
	t.Helper()
	stg, err := loadSettings(cfg, "main")
	if err != nil {
		t.Fatal(err)
	}
	var told []string
	tell := func(s string) { told = append(told, s) }
	broker := newApprovalBroker(nil)
	broker.LoadModes(stg.Permissions, stg.KnownCategories)
	var mu sync.Mutex
	save := func() {
		mu.Lock()
		defer mu.Unlock()
		saveSettingsOrTell(stg, cfg, "main", tell)
	}
	broker.SetPersist(func(modes map[string]string, known []string) {
		mu.Lock()
		stg.Permissions, stg.KnownCategories = modes, known
		mu.Unlock()
		save()
	})
	announceNewCategories(broker, stg, &mu, save, tell)
	return broker, told
}

// The terminal tells the operator about a new category once, and the
// seen-marker survives a later broker persist that saves the same settings.
func TestAnnounceNewCategories_OnceAndMarkerSurvivesPersist(t *testing.T) {
	cfg := t.TempDir()
	body := `{"permissions":{"exec":"ask","write":"allow","destructive":"allow","web":"allow","send":"allow","egress":"ask","mcp":"ask","sandbox":"allow"}}`
	if err := os.WriteFile(filepath.Join(cfg, "settings.main.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	broker, told := launchTUIPermissions(t, cfg)
	all := strings.Join(told, "\n")
	if !strings.Contains(all, "Share private data (egress)") || !strings.Contains(all, "Connect remote services (mcp)") || strings.Contains(all, "sandbox") {
		t.Fatalf("first launch told %q, want a notice naming egress and mcp only", told)
	}
	for _, line := range told {
		if n := len([]rune("⚙ " + line)); n > 40 { // a third of a 120-column terminal
			t.Errorf("notice line %q is %d columns; the terminal feed cuts it", line, n)
		}
	}
	// A later choice persists the modes through the same settings object.
	if _, err := broker.SetPermission(catWeb, "ask"); err != nil {
		t.Fatal(err)
	}
	stg, err := loadSettings(cfg, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stg.AnnouncedCategories, []string{catEgress, catMCP}) {
		t.Fatalf("announced_categories = %v after a persist, want egress and mcp", stg.AnnouncedCategories)
	}
	if !slices.Contains(stg.KnownCategories, catWeb) || slices.Contains(stg.KnownCategories, catEgress) {
		t.Fatalf("known_categories = %v, want web and not egress", stg.KnownCategories)
	}

	// Relaunch: still new, but already told.
	broker, told = launchTUIPermissions(t, cfg)
	if len(told) != 0 {
		t.Fatalf("second launch told %q, want nothing", told)
	}
	if got := broker.NewCategories(); !slices.Equal(got, []string{catEgress, catMCP}) {
		t.Fatalf("NewCategories = %v after relaunch, want egress and mcp", got)
	}
}

// serve restores each offsite broker's modes with its own marker, and a choice
// on one saves that surface's marker and leaves the other's alone.
func TestServeOffsiteBrokersKeepTheirOwnMarker(t *testing.T) {
	cfg := t.TempDir()
	lc := &launchConfig{}
	lc.Relay.Permissions = map[string]string{catExec: "ask", catWrite: "ask", catDestructive: "ask", catWeb: "ask", catSend: "ask"}
	lc.IMessage.Permissions = map[string]string{}
	for _, cat := range []string{catExec, catWrite, catDestructive, catWeb, catSend, catEgress, catMCP, catSandbox} {
		lc.IMessage.Permissions[cat] = "ask"
	}
	lc.IMessage.KnownCategories = []string{catExec, catWrite, catDestructive, catWeb, catSend, catEgress}
	if err := lc.Save(cfg); err != nil {
		t.Fatal(err)
	}
	c := &commonFlags{agent: "main", cfgDir: cfg, lc: lc}
	var told []string
	notify := func(s string) { told = append(told, s) }

	// With no bot token Access has no Discord rows to choose, so serve says
	// nothing about them (C-3); with one it tells the operator.
	relayDeps := serveRelayDeps{
		c: c, stores: &stores{}, log: testLog(), degraded: &degradedState{},
		emit: func(agent.Event) {}, notify: notify,
	}
	if buildServeRelay(context.Background(), relayDeps); len(told) != 0 {
		t.Fatalf("with no Discord token serve told %q", told)
	}
	creds, _ := loadCredentials(cfg)
	creds.Set(skillSecretID("discord", "token"), "tok-test-token")
	if err := creds.Save(cfg); err != nil {
		t.Fatal(err)
	}
	relay := buildServeRelay(context.Background(), relayDeps)
	imsg := buildServeIMessage(context.Background(), serveIMessageDeps{
		c: c, stores: &stores{}, log: testLog(), degraded: &degradedState{}, broker: newApprovalBroker(testLog()),
		emit: func(agent.Event) {}, notify: notify,
	})
	if got := relay.broker.NewCategories(); !slices.Equal(got, []string{catEgress, catMCP, catSandbox}) {
		t.Fatalf("Discord New = %v, want egress, mcp and sandbox", got)
	}
	if len(told) == 0 || !strings.HasPrefix(told[0], "Discord conversations: 3 new safety checks") ||
		strings.Count(strings.Join(told, "\n"), "new safety check") != 1 { // the rest: no router to build the relay's agent here
		t.Fatalf("serve told %q, want the one Discord notice", told)
	}
	var imsgNew []string
	for _, in := range imsg.perms.Permissions() {
		if in.New {
			imsgNew = append(imsgNew, in.Category)
		}
	}
	if !slices.Equal(imsgNew, []string{catMCP, catSandbox}) {
		t.Fatalf("iMessage New = %v, want mcp and sandbox", imsgNew)
	}

	if _, err := relay.broker.SetPermission(catSandbox, "deny"); err != nil {
		t.Fatal(err)
	}
	if _, err := imsg.perms.SetPermission(catMCP, "deny"); err != nil {
		t.Fatal(err)
	}
	saved, err := loadLaunchConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if k := saved.Relay.KnownCategories; !slices.Contains(k, catSandbox) || slices.Contains(k, catMCP) || slices.Contains(k, catEgress) {
		t.Errorf("relay.known_categories = %v, want sandbox and not egress or mcp", k)
	}
	if k := saved.IMessage.KnownCategories; !slices.Contains(k, catMCP) || !slices.Contains(k, catEgress) || slices.Contains(k, catSandbox) {
		t.Errorf("imessage.known_categories = %v, want egress and mcp, not sandbox", k)
	}
	if saved.Relay.Permissions[catSandbox] != "deny" || saved.IMessage.Permissions[catMCP] != "deny" {
		t.Errorf("modes not saved: relay %v, imessage %v", saved.Relay.Permissions, saved.IMessage.Permissions)
	}
}
