package main

import (
	"context"
	"flag"
	"fmt"

	"tenant/internal/model"
	githubp "tenant/internal/plugins/github"
)

// githubPlugin declares GitHub: the repo control plane (branches, PRs,
// issues, CI) over a fine-grained PAT.
func githubPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "github",
		Flags:       bindGitHubFlags,
		On:          func(pf *pluginFlags) bool { return githubFlagsOf(pf).on },
		ApplyConfig: applyGitHubConfig,
		Launch:      launchGitHub,
		Title:       "GitHub",
		Version:     1,
		Tools:       func() []model.ToolSpec { return githubp.NewDispatcher(nil, githubp.Policy{}).Tools() },
		Configure:   githubSkillKind,
		SetupOrder:  7,
		Actions:     map[string]string{"github_write": catDestructive}, // branch, issue, comment, PR writes
		StubHint:    "run `/configure github` (fine-grained PAT), then `/enable github`",
		Activate:    activateGitHub,
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
	allowWrite := githubFlagsOf(e.pf).allowWrite || sk.Settings["allow_write"] == "true"
	return githubp.NewDispatcher(svc, githubp.Policy{AllowWrite: e.gateAllow(allowWrite), Confirm: e.confirm}), func() {}, nil
}

// githubFlags are GitHub's launch flags. Deliberately there is NO
// --github-token flag: a token passed as a flag lands in shell history, ps
// output, and the scheduled-task XML. The token comes from $GITHUB_TOKEN,
// /configure github, or the dashboard keys page.
type githubFlags struct {
	on         bool
	repo       string
	allowWrite bool
	apiBase    string
}

func githubFlagsOf(pf *pluginFlags) *githubFlags { return flagsOf[githubFlags](pf, "github") }

func bindGitHubFlags(fs *flag.FlagSet) any {
	f := &githubFlags{}
	fs.BoolVar(&f.on, "github", false, "enable GitHub tools (control plane: branches/PRs/issues/CI)")
	fs.StringVar(&f.repo, "github-repo", "", "GitHub repo this agent may touch, owner/name (single-repo by design)")
	fs.BoolVar(&f.allowWrite, "github-allow-write", false, "permit github_create_branch/issue/comment/pr (writes to the repo)")
	fs.StringVar(&f.apiBase, "github-api-base", "", "GitHub Enterprise API base, e.g. https://ghe.example.com/api/v3/")
	return f
}

func applyGitHubConfig(pf *pluginFlags, s savedConfig) {
	f := githubFlagsOf(pf)
	if !s.set["github"] {
		f.on = true
	}
	if !s.set["github-repo"] {
		f.repo = firstNonEmpty(f.repo, s.get("repo"))
	}
	if !s.set["github-api-base"] {
		f.apiBase = firstNonEmpty(f.apiBase, s.get("api_base"))
	}
	if !s.set["github-allow-write"] && s.get("allow_write") == "true" {
		f.allowWrite = true
	}
}

// launchGitHub builds the GitHub plugin from the launch flags.
func launchGitHub(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := githubFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	repos, rerr := githubp.ParseRepos(f.repo)
	if rerr != nil {
		return nil, nil, fmt.Errorf("github plugin: %w", rerr)
	}
	svc, err := githubp.Open(githubp.Config{
		Repos:   repos,
		Token:   githubp.ResolveToken("", e.c.cfgDir),
		APIBase: f.apiBase,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("github plugin: %w", err)
	}
	return []plugin{githubp.NewDispatcher(svc, githubp.Policy{AllowWrite: e.gateAllow(f.allowWrite), Confirm: e.confirm})}, nil, nil
}
