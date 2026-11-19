package main

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestAgentCommandsSeeNoneOfTenantsSecretVariables pins the rule process
// protection rests on (TEN-535): a command the agent runs, on any surface,
// gets a clean environment. None of the variables Tenant reads as a secret
// reaches it, nor a secret under a name nothing lists, unless the surface's
// exec profile passes one by name (resolveExecProfile).
func TestAgentCommandsSeeNoneOfTenantsSecretVariables(t *testing.T) {
	resetExecProfiles(t)
	withBuiltinExec(t, "host", "") // the same on every system: the allowlist is the host backend's too
	const value = "ten535-must-not-leak"
	for _, n := range secretEnvNames(secretEnvConfig()) {
		t.Setenv(n, value)
	}
	t.Setenv("TEN535_INNOCENT_NAME", value)

	printEnv, cronPrintEnv := "env", "env"
	if runtime.GOOS == "windows" {
		printEnv, cronPrintEnv = "Get-ChildItem Env: | ForEach-Object { $_.Name + '=' + $_.Value }", "set"
	}
	cfg, data := t.TempDir(), t.TempDir()
	environment := func(surface string) string {
		t.Helper()
		var out string
		var err error
		if surface == surfaceCronShell {
			out, err = runCronShell(context.Background(), cronPrintEnv, cfg, data)
		} else {
			out, err = sandboxRunner(cfg, data, nil)(withExecSurface(context.Background(), surface), printEnv, time.Minute)
		}
		if err != nil || !strings.Contains(strings.ToUpper(out), "PATH=") {
			t.Fatalf("%s: the command did not print its environment: %q, %v", surface, out, err)
		}
		return out
	}
	for _, surface := range execSurfaces {
		if out := environment(surface); strings.Contains(out, value) {
			t.Errorf("%s: a command saw a secret variable:\n%s", surface, linesWith(out, value))
		}
	}

	// Named in the surface's env_pass, one gets through, and only that one.
	defaultExecProfiles.Load(map[string]execProfileSetting{surfaceLocal: {EnvPass: []string{"GITHUB_TOKEN"}}})
	if got := linesWith(environment(surfaceLocal), value); strings.TrimSpace(got) != "GITHUB_TOKEN="+value {
		t.Errorf("with GITHUB_TOKEN in env_pass, the command saw:\n%s\nwant only GITHUB_TOKEN", got)
	}
}

func linesWith(s, sub string) string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, sub) {
			out = append(out, strings.TrimSpace(ln))
		}
	}
	return strings.Join(out, "\n")
}
