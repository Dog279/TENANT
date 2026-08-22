package main

// githubtuictl.go adapts the GitHub watch list to the TUI's /github command
// (mirrors peertuictl.go for /peer). It owns exactly one setting -
// skills.github.settings.repo - which is the comma-separated watch list where
// each entry may carry a :write suffix.
//
// Why a dedicated command rather than re-running /configure github: the write
// flag is a SAFETY decision made per repo, and the operator changes it far more
// often than the token or the repo set. Making them re-enter the whole list to
// flip one repo invites pasting a shorter list by mistake and silently dropping
// a repo from the watch set.

import (
	"fmt"
	"strings"

	githubp "tenant/internal/plugins/github"
	"tenant/internal/tui"
)

// lc is the LIVE in-memory config. persistLaunchConfig updates it alongside
// disk, so other components reading c.lc see the change without a reload -
// skipping it is how the lost-update class of bug reappears.
type githubTUIControl struct {
	cfgDir string
	lc     *launchConfig
}

// load reads the persisted watch list. A missing skill entry is not an error:
// it just means nothing is configured yet, which the caller reports as a hint.
func (g githubTUIControl) load() ([]githubp.RepoRef, error) {
	lc, err := loadLaunchConfig(g.cfgDir)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	sk, ok := lc.Skills["github"]
	if !ok || sk.Settings == nil || strings.TrimSpace(sk.Settings["repo"]) == "" {
		return nil, nil
	}
	entries, err := githubp.ParseRepos(sk.Settings["repo"])
	if err != nil {
		return nil, err
	}
	refs := make([]githubp.RepoRef, 0, len(entries))
	for _, e := range entries {
		ref, perr := githubp.ParseRepoEntry(e)
		if perr != nil {
			return nil, perr
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// save writes the list back, preserving order (the first entry stays primary).
func (g githubTUIControl) save(refs []githubp.RepoRef) error {
	entries := make([]string, 0, len(refs))
	for _, r := range refs {
		e := r.Slug()
		if r.Write {
			e += ":write"
		}
		entries = append(entries, e)
	}
	return persistLaunchConfig(g.cfgDir, g.lc, func(lc *launchConfig) {
		if lc.Skills == nil {
			lc.Skills = map[string]*skillConfig{}
		}
		sk, ok := lc.Skills["github"]
		if !ok || sk == nil {
			sk = &skillConfig{}
			lc.Skills["github"] = sk
		}
		if sk.Settings == nil {
			sk.Settings = map[string]string{}
		}
		sk.Settings["repo"] = strings.Join(entries, ", ")
	})
}

func (g githubTUIControl) view(refs []githubp.RepoRef) []tui.GitHubRepoInfo {
	out := make([]tui.GitHubRepoInfo, 0, len(refs))
	for i, r := range refs {
		out = append(out, tui.GitHubRepoInfo{Slug: r.Slug(), Write: r.Write, Primary: i == 0})
	}
	return out
}

func (g githubTUIControl) Repos() ([]tui.GitHubRepoInfo, error) {
	refs, err := g.load()
	if err != nil {
		return nil, err
	}
	return g.view(refs), nil
}

// find locates a repo case-insensitively; operators type dog279/tenant.
func find(refs []githubp.RepoRef, slug string) int {
	want := strings.ToLower(strings.TrimSpace(slug))
	for i, r := range refs {
		if strings.ToLower(r.Slug()) == want {
			return i
		}
	}
	return -1
}

func (g githubTUIControl) SetWrite(repo string, write bool) (tui.GitHubRepoInfo, error) {
	ref, err := githubp.ParseRepoEntry(repo)
	if err != nil {
		return tui.GitHubRepoInfo{}, err
	}
	refs, err := g.load()
	if err != nil {
		return tui.GitHubRepoInfo{}, err
	}
	i := find(refs, ref.Slug())
	if i < 0 {
		return tui.GitHubRepoInfo{}, fmt.Errorf("%s is not watched - add it first with `/github add %s`", ref.Slug(), ref.Slug())
	}
	refs[i].Write = write
	if err := g.save(refs); err != nil {
		return tui.GitHubRepoInfo{}, err
	}
	return tui.GitHubRepoInfo{Slug: refs[i].Slug(), Write: write, Primary: i == 0}, nil
}

func (g githubTUIControl) Add(repo string, write bool) (tui.GitHubRepoInfo, error) {
	ref, err := githubp.ParseRepoEntry(repo)
	if err != nil {
		return tui.GitHubRepoInfo{}, err
	}
	// An explicit :write in the argument wins over the flag, so
	// `/github add owner/name:write` does what it reads like.
	if ref.Write {
		write = true
	}
	refs, err := g.load()
	if err != nil {
		return tui.GitHubRepoInfo{}, err
	}
	if i := find(refs, ref.Slug()); i >= 0 {
		return tui.GitHubRepoInfo{}, fmt.Errorf("%s is already watched (use `/github write|readonly` to change it)", ref.Slug())
	}
	ref.Write = write
	refs = append(refs, ref)
	if err := g.save(refs); err != nil {
		return tui.GitHubRepoInfo{}, err
	}
	return tui.GitHubRepoInfo{Slug: ref.Slug(), Write: ref.Write, Primary: len(refs) == 1}, nil
}

func (g githubTUIControl) Remove(repo string) (bool, error) {
	ref, err := githubp.ParseRepoEntry(repo)
	if err != nil {
		return false, err
	}
	refs, err := g.load()
	if err != nil {
		return false, err
	}
	i := find(refs, ref.Slug())
	if i < 0 {
		return false, nil
	}
	refs = append(refs[:i], refs[i+1:]...)
	if err := g.save(refs); err != nil {
		return false, err
	}
	return true, nil
}
