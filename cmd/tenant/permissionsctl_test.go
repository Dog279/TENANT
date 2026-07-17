package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestComposeEffectivePermissions(t *testing.T) {
	cfgDir := t.TempDir()
	c := &commonFlags{
		agent: "alpha", cfgDir: cfgDir,
		lc: &launchConfig{
			Relay: relayConfig{
				AllowExec:   false,
				Permissions: map[string]string{catExec: "allow", catSend: "allow"},
			},
			IMessage: imessageConfig{Permissions: map[string]string{catSend: "ask"}},
		},
	}
	stg := &settings{
		Tools:       map[string]bool{"web_navigate": true},
		Permissions: map[string]string{catExec: "deny", catWeb: "allow"},
	}
	if err := stg.save(cfgDir, c.agent); err != nil {
		t.Fatal(err)
	}

	local, err := composeEffectivePermissions(c, &pluginFlags{osEnable: true}, "tui")
	if err != nil {
		t.Fatal(err)
	}
	if local.Surface != permissionSurfaceLocal || local.Categories[catExec].Mode != "deny" || local.Categories[catWeb].Mode != "allow" {
		t.Errorf("local composition wrong: %+v", local)
	}
	if local.Preset != "custom" || !local.ToolOverrides["web_navigate"] {
		t.Errorf("local effective metadata wrong: %+v", local)
	}
	if !containsString(local.ConfiguredTools, "web") || !containsString(local.ConfiguredTools, "os") {
		t.Errorf("configured tools do not compose launch state and saved overrides: %v", local.ConfiguredTools)
	}

	discord, err := composeEffectivePermissions(c, &pluginFlags{}, "relay")
	if err != nil {
		t.Fatal(err)
	}
	if discord.Categories[catExec].Mode != "deny" || discord.Categories[catExec].Source != "relay.allow_exec=false" {
		t.Errorf("discord outer gate not composed: %+v", discord.Categories[catExec])
	}
	if discord.Categories[catSend].Mode != "allow" {
		t.Errorf("discord send override = %+v", discord.Categories[catSend])
	}

	imessage, err := composeEffectivePermissions(c, &pluginFlags{}, "imessage")
	if err != nil {
		t.Fatal(err)
	}
	if imessage.Categories[catExec].Mode != "deny" || imessage.Categories[catSend].Mode != "ask" {
		t.Errorf("imessage composition wrong: %+v", imessage.Categories)
	}
}

func TestPermissionPresetsAvailableAndPersist(t *testing.T) {
	for _, name := range []string{"safe", "balanced", "full"} {
		modes := permissionPresets[name]
		if len(modes) != len(catOrder) {
			t.Errorf("preset %s has %d categories, want %d", name, len(modes), len(catOrder))
		}
	}

	cfgDir, dataDir := t.TempDir(), t.TempDir()
	if err := cmdPermissions(context.Background(), []string{
		"preset", "safe", "--agent", "alpha", "--surface", "local", "--config", cfgDir, "--data", dataDir,
	}); err != nil {
		t.Fatal(err)
	}
	stg, err := loadSettings(cfgDir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, category := range catOrder {
		if stg.Permissions[category] != "deny" {
			t.Errorf("safe %s = %q, want deny", category, stg.Permissions[category])
		}
	}
}

func TestPermissionsEffectiveJSON(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	out, err := captureTenantStdout(t, func() error {
		return cmdPermissions(context.Background(), []string{
			"effective", "--agent", "alpha", "--surface", "dashboard", "--config", cfgDir, "--data", dataDir, "--json",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var report effectivePermissions
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode effective JSON: %v\n%s", err, out)
	}
	if report.Agent != "alpha" || report.Surface != "local" || len(report.Categories) != len(catOrder) {
		t.Fatalf("effective report wrong: %+v", report)
	}
	if report.Preset != "balanced" || len(report.AvailablePresets) != 3 {
		t.Errorf("preset metadata wrong: %+v", report)
	}
}
