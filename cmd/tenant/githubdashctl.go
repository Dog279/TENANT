package main

// githubdashctl.go adapts the GitHub watch list to the dashboard's Integrations
// page, the same way dashCron adapts the cron manager. The storage, parsing and
// per-repo write semantics all live in githubTUIControl - this is a view-type
// translation, so the web page and /github can never drift apart in behavior.

import (
	"tenant/internal/dashboard"
)

type dashGitHubRepos struct{ c githubTUIControl }

func (a dashGitHubRepos) Repos() ([]dashboard.GitHubRepoRow, error) {
	rows, err := a.c.Repos()
	if err != nil {
		return nil, err
	}
	out := make([]dashboard.GitHubRepoRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, dashboard.GitHubRepoRow{Slug: r.Slug, Write: r.Write, Primary: r.Primary})
	}
	return out, nil
}

func (a dashGitHubRepos) SetWrite(repo string, write bool) error {
	_, err := a.c.SetWrite(repo, write)
	return err
}

func (a dashGitHubRepos) Add(repo string, write bool) error {
	_, err := a.c.Add(repo, write)
	return err
}

func (a dashGitHubRepos) Remove(repo string) error {
	ok, err := a.c.Remove(repo)
	if err != nil {
		return err
	}
	if !ok {
		// Report it rather than silently succeeding: on a web form a no-op
		// "Stop watching" looks identical to a successful one.
		return errNotWatched(repo)
	}
	return nil
}

type errNotWatched string

func (e errNotWatched) Error() string { return string(e) + " was not being watched" }
