package main

import (
	"context"
	"os"
	"runtime"
	"slices"
	"strings"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// secretenv.go (TEN-535): the environment variables Tenant reads as a
// secret. macOS shows a user's programs each other's starting environment,
// and nothing Tenant does to its own process changes that, so on macOS a
// secret that arrives in one of them is reported: a warning in the system
// log when serve or the TUI starts, and a finding in `tenant security audit`.
// The supported way is the credentials store (/configure <plugin>, or the
// dashboard's keys page).
//
// A command the agent runs sees none of them unless its exec profile passes
// one by name (envAllowlist in execprofiles.go).

// directSecretEnv are the secrets read from the environment by a fixed name.
// The key catalog (keyscatalog.go) and the launch config name the rest.
var directSecretEnv = []string{"ATLASSIAN_CLIENT_SECRET", "ATLASSIAN_TOKEN", "BLUEBUBBLES_PASSWORD",
	"DISCORD_BOT_TOKEN", "GITHUB_PAT", "GITHUB_TOKEN", passphraseEnv, "X_BEARER_TOKEN"}

// secretEnvNames is every environment variable Tenant reads as a secret,
// sorted: the fixed names, each catalogued key's variables, and the
// variables config.json names for a provider's key (key_env) and for the
// quality model's.
func secretEnvNames(lc *launchConfig) []string {
	names := slices.Clone(directSecretEnv)
	for _, spec := range keyCatalog() {
		names = append(names, spec.EnvVars...)
	}
	if lc != nil {
		for _, p := range lc.Providers {
			if p != nil {
				names = append(names, p.Auth.KeyEnv)
			}
		}
		names = append(names, lc.Improve.QualityModelKeyEnv, lc.Improve.LegacyJudgeKeyEnv)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	return slices.DeleteFunc(names, func(n string) bool { return n == "" })
}

// exposedSecretEnv returns the names, never the values, of the secret
// variables that are set, on a system that shows them to the user's other
// programs: macOS (goos is runtime.GOOS). Linux and Windows keep a protected
// process's environment to itself.
func exposedSecretEnv(goos string, lc *launchConfig, getenv func(string) string) []string {
	if goos != "darwin" {
		return nil
	}
	return slices.DeleteFunc(secretEnvNames(lc), func(n string) bool { return getenv(n) == "" })
}

var sysSecretInEnvironment = catalog.System.Event(5020, event.Warning, "Configuration", "A secret is in an environment variable ({names}), which macOS shows to your other programs", "names").
	Keywords("config").
	Doc("Tenant started on macOS with a variable it reads as a secret set. macOS lets a user's programs read each other's starting environment, and process protection does not change that. names lists the variables, never their values.",
		"A key was exported in the shell, or set in the service's unit, instead of being saved in the credentials store.",
		"Save the key with /configure <plugin> or the dashboard's keys page, remove the variable, and start Tenant again.")

// warnSecretEnvironment files that warning when serve or the TUI starts.
func warnSecretEnvironment(ctx context.Context, lc *launchConfig) {
	logSecretEnvironment(ctx, exposedSecretEnv(runtime.GOOS, lc, os.Getenv))
}

func logSecretEnvironment(ctx context.Context, set []string) {
	if len(set) > 0 {
		sysSecretInEnvironment.Log(ctx, "names", strings.Join(set, ", "))
	}
}
