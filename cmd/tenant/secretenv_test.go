package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"tenant/internal/config"
	"tenant/internal/logs/event/eventtest"
)

func secretEnvConfig() *launchConfig {
	lc := &launchConfig{Providers: map[string]*config.Provider{"mine": {Auth: config.Auth{KeyEnv: "MY_PROVIDER_KEY"}}, "keyless": {}}}
	lc.Improve.QualityModelKeyEnv = "MY_QUALITY_KEY"
	return lc
}

func TestSecretEnvNames(t *testing.T) {
	names := secretEnvNames(secretEnvConfig())
	for _, want := range []string{"ANTHROPIC_API_KEY", "ATLASSIAN_CLIENT_SECRET", "ATLASSIAN_TOKEN", "BLUEBUBBLES_PASSWORD",
		"DISCORD_BOT_TOKEN", "GITHUB_PAT", "GITHUB_TOKEN", "MY_PROVIDER_KEY", "MY_QUALITY_KEY", "OPENAI_API_KEY",
		"TAVILY_API_KEY", "TENANT_BACKUP_PASSPHRASE", "X_BEARER_TOKEN"} {
		if !slices.Contains(names, want) {
			t.Errorf("secretEnvNames lacks %s", want)
		}
	}
	if !slices.IsSorted(names) || slices.Contains(names, "") || len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Errorf("secretEnvNames is not sorted, or has an empty or repeated name: %q", names)
	}
	if got := secretEnvNames(nil); slices.Contains(got, "MY_PROVIDER_KEY") || !slices.Contains(got, "GITHUB_TOKEN") {
		t.Errorf("with no config: %q", got)
	}
}

// Only macOS shows a process's environment to the user's other programs, so
// only there is a secret in it reported; and only names are, never values.
func TestExposedSecretEnv(t *testing.T) {
	env := map[string]string{"GITHUB_TOKEN": "ghp_value", "MY_PROVIDER_KEY": "sk-value", "PATH": "/bin", "OPENAI_API_KEY": ""}
	get := func(n string) string { return env[n] }
	if got := exposedSecretEnv("darwin", secretEnvConfig(), get); !slices.Equal(got, []string{"GITHUB_TOKEN", "MY_PROVIDER_KEY"}) {
		t.Errorf("darwin: %q", got)
	}
	for _, goos := range []string{"linux", "windows"} {
		if got := exposedSecretEnv(goos, secretEnvConfig(), get); got != nil {
			t.Errorf("%s: %q, want nothing", goos, got)
		}
	}
	if got := exposedSecretEnv("darwin", nil, func(string) string { return "" }); len(got) != 0 {
		t.Errorf("nothing set: %q", got)
	}

	rec := eventtest.Capture(t)
	logSecretEnvironment(context.Background(), nil)
	if n := len(rec.Events()); n != 0 {
		t.Fatalf("nothing exposed filed %d event(s)", n)
	}
	logSecretEnvironment(context.Background(), []string{"GITHUB_TOKEN", "MY_PROVIDER_KEY"})
	evs := rec.Events()
	if len(evs) != 1 || evs[0].ID != 5020 || !strings.Contains(evs[0].Msg, "GITHUB_TOKEN, MY_PROVIDER_KEY") {
		t.Fatalf("the warning: %+v", evs)
	}
	warnSecretEnvironment(context.Background(), secretEnvConfig()) // on the system the tests run on; it must not panic
}
