package main

// skillctl_github.go is the /configure github surface (TEN-177): collect the
// repo + a fine-grained PAT, validate the token against the live API, and
// persist to launchConfig.Skills["github"] so the toolmux activator can build
// the real plugin mid-session without a relaunch.
//
// The PAT is a Secret field, so it is masked on entry and stored in
// credentials.json under skill:github:token - never in config.json, never in a
// flag, never echoed back.

import (
	"context"
	"fmt"
	"strings"
	"time"

	githubp "tenant/internal/plugins/github"
)

func githubSkillKind() skillKind {
	return skillKind{
		ID:    "github",
		Label: "GitHub - repo control plane (branches, PRs, issues, CI) via fine-grained PAT",
		Wired: true,
		SetupHint: "Create a FINE-GRAINED token at github.com -> Settings -> Developer settings ->\n" +
			"Personal access tokens -> Fine-grained tokens. Scope it to EXACTLY the repos below and grant:\n" +
			"  Contents R/W, Pull requests R/W, Issues R/W, Checks R, Actions R\n" +
			"Leave Administration, Secrets, Environments and Workflows at No access - Tenant never needs\n" +
			"them, and withholding them means a prompt-injected agent cannot reach them either.",
		Fields: []skillKindField{
			{
				Key: "repo", Required: true, Validate: validateGitHubRepos,
				Prompt: "Repositories to watch - comma separated owner/name. " +
					"Add :write to one the agent may modify (e.g. you/project:write, you/notes)",
			},
			{
				Key: "token", Prompt: "Fine-grained personal access token", Secret: true, Required: true,
				Validate: validateGitHubToken,
			},
			{
				// This is NOT "which repos are writable" - that is the per-repo
				// :write flag. This only decides whether a write on an already
				// writable repo still stops for a per-action approval.
				Key: "allow_write", Default: "false",
				Prompt: "On :write repos, perform writes WITHOUT a per-action approval prompt? " +
					"(no = each write still asks; repos without :write are never writable either way)",
				Options:      []string{"false", "true"},
				OptionLabels: []string{"no - ask before each write (recommended)", "yes - auto-approve writes on :write repos"},
				Validate:     validateOneOf("false", "true"),
			},
			{Key: "api_base", Prompt: "GitHub Enterprise API base (optional)"},
		},
		Probe:        probeGitHub,
		ProbeTimeout: 30 * time.Second,
	}
}

// validateGitHubRepo rejects anything that is not owner/name, so a typo fails
// at configure time rather than as a confusing 404 on the first tool call.
func validateGitHubRepos(v string) error {
	_, err := githubp.ParseRepos(v)
	return err
}

// validateGitHubToken does a shape check only. The real check is the probe,
// which asks GitHub. This just catches an obviously-wrong paste (a URL, a
// username) before we spend a round trip on it.
func validateGitHubToken(v string) error {
	v = strings.TrimSpace(v)
	if len(v) < 20 {
		return fmt.Errorf("that does not look like a token (too short)")
	}
	if strings.Contains(v, " ") || strings.HasPrefix(v, "http") {
		return fmt.Errorf("that looks like a URL or a phrase, not a token")
	}
	return nil
}

// probeGitHub validates the PAT against the live API and reports the repo it
// resolved, so the operator sees exactly what the agent will be scoped to.
func probeGitHub(ctx context.Context, creds *credentials, settings map[string]string, _ func() error) (string, error) {
	repo := strings.TrimSpace(settings["repo"])
	if repo == "" {
		return "", fmt.Errorf("no repo configured")
	}
	token := creds.Get(skillSecretID("github", "token"))
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("no token stored - run `/configure github` again")
	}
	repos, err := githubp.ParseRepos(repo)
	if err != nil {
		return "", err
	}
	svc, err := githubp.Open(githubp.Config{
		Repos:   repos,
		Token:   token,
		APIBase: settings["api_base"],
	})
	if err != nil {
		return "", err
	}
	login, err := svc.Validate(ctx)
	if err != nil {
		return "", err
	}
	// Resolve each repo's default branch: it proves the token can actually SEE
	// every watched repo, which a bare /user check does not. A PAT scoped to
	// only some of them fails here instead of at 08:00 in an unattended brief.
	var lines []string
	for _, r := range svc.Watched() {
		branch, berr := svc.DefaultBranch(ctx, r)
		if berr != nil {
			return "", fmt.Errorf("token is valid for %s but cannot read %s: %w", login, r.Slug(), berr)
		}
		mode := "watch-only"
		if r.Write {
			mode = "WRITABLE (still gated per action)"
		}
		lines = append(lines, fmt.Sprintf("%s @%s - %s", r.Slug(), branch, mode))
	}
	return fmt.Sprintf("authenticated as %s; %d repo(s):\n  %s", login, len(lines), strings.Join(lines, "\n  ")), nil
}
