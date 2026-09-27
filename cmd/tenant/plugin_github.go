package main

import (
	"context"
	"fmt"

	"tenant/internal/model"
	githubp "tenant/internal/plugins/github"
)

// githubPlugin declares GitHub: the repo control plane (branches, PRs,
// issues, CI) over a fine-grained PAT.
func githubPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "github",
		Title:     "GitHub",
		Version:   1,
		Tools:     func() []model.ToolSpec { return githubp.NewDispatcher(nil, githubp.Policy{}).Tools() },
		Configure: githubSkillKind,
		Setup:     7,
		Actions:   map[string]string{"github_write": catDestructive}, // branch, issue, comment, PR writes
		StubHint:  "run `/configure github` (fine-grained PAT), then `/enable github`",
		Activate:  activateGitHub,
	}
}

// activateGitHub builds the real GitHub plugin from /configure-saved settings
// (TEN-176), so enabling it mid-session needs no relaunch.
func activateGitHub(_ context.Context, e *pluginEnv) (plugin, func(), error) {
	lc, err := loadLaunchConfig(e.c.cfgDir)
	if err != nil {
		return nil, nil, fmt.Errorf("github plugin: load config: %w", err)
	}
	sk, ok := lc.Skills["github"]
	if !ok || sk.Settings == nil || sk.Settings["repo"] == "" {
		return nil, nil, fmt.Errorf("github plugin: %w yet - run `/configure github`", errNotConfigured)
	}
	creds, _ := loadCredentials(e.c.cfgDir)
	tok := githubp.ResolveToken(creds.Get(skillSecretID("github", "token")), e.c.cfgDir)
	if tok == "" {
		return nil, nil, fmt.Errorf("github plugin: %w yet - no token; run `/configure github`", errNotConfigured)
	}
	// "repo" holds the watch list: comma-separated owner/name, each
	// optionally suffixed :write. Repos without the suffix are reported on
	// but never written to (the operator's per-repo choice, enforced in the
	// plugin, not by prompt).
	repos, rerr := githubp.ParseRepos(sk.Settings["repo"])
	if rerr != nil {
		return nil, nil, fmt.Errorf("github plugin: %w", rerr)
	}
	svc, err := githubp.Open(githubp.Config{
		Repos:   repos,
		Token:   tok,
		APIBase: sk.Settings["api_base"],
	})
	if err != nil {
		return nil, nil, fmt.Errorf("github plugin: %w", err)
	}
	// Write permission comes from the SAVED setting as well as the launch
	// flag: an operator who opted into writes via /configure should not have
	// to relaunch with a flag to keep them.
	allowWrite := e.pf.githubAllowWrite || sk.Settings["allow_write"] == "true"
	return githubp.NewDispatcher(svc, githubp.Policy{AllowWrite: e.gateAllow(allowWrite), Confirm: e.confirm}), func() {}, nil
}
