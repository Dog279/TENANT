// Package github gives the agent a GitHub CONTROL plane (branches, PRs, issues,
// CI status, code search) over google/go-github (Apache-2.0, pure Go, CGO-free).
//
// It deliberately does NOT do code I/O. Real coding is multi-file and needs
// local test feedback, so the DATA plane stays with Tenant's existing local
// tools (os_read_file / os_write_file / os_edit_file / os_exec): clone, edit,
// go test, push. GitHub's create/update-file API is single-file and validates
// nothing before CI, which is the wrong shape for that loop. (TEN-174/175.)
//
// Scope is a CONFIGURED SET of repos, not "whatever the model names". Every
// call resolves its target through that allowlist, so the blast radius is the
// operator's list rather than the token's full reach. Writes are additionally
// gated in dispatch.go, and a few actions are refused in Go regardless.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v74/github"
)

// Config configures the GitHub service. Token is resolved by auth.go from the
// config value, the environment, or the on-disk cache - never from a CLI flag,
// so it cannot land in shell history or a process listing.
type Config struct {
	// Repos is the allowlist, "owner/name" each. The FIRST entry is the
	// primary: the default target when a tool omits its repo argument. Every
	// other entry is reachable only by naming it explicitly.
	//
	// The fine-grained PAT should be scoped to exactly this set, so the token
	// and the config agree on blast radius and neither alone is the boundary.
	Repos []string

	// Token is the PAT. Usually empty here - auth.go resolves it.
	Token string

	// APIBase overrides the API endpoint for GitHub Enterprise Server
	// (e.g. https://ghe.example.com/api/v3/). Empty = github.com.
	APIBase string

	// HTTPClient overrides the transport (tests inject an httptest client).
	HTTPClient *http.Client
}

// RepoRef is one resolved repository plus the operator's decision about
// whether the agent may WRITE to it. Read access is implied by being watched at
// all: the point of the watch list is that every repo is reportable, while only
// some are worked on autonomously.
type RepoRef struct {
	Owner, Name string
	// Write is the per-repo authorization. A repo without it refuses writes in
	// code, before the Policy gate is consulted - so approving "create a PR"
	// cannot spill into a repo that was only ever meant to be watched.
	Write bool
}

// Slug is "owner/name".
func (r RepoRef) Slug() string { return r.Owner + "/" + r.Name }

// Service is an allowlist-scoped GitHub client.
type Service struct {
	client  *gh.Client
	primary RepoRef
	allowed []RepoRef
}

// Primary is the default repo for tools that omit a target.
func (s *Service) Primary() RepoRef { return s.primary }

// Watched lists every repo in the allowlist, primary first.
func (s *Service) Watched() []RepoRef { return append([]RepoRef(nil), s.allowed...) }

// Slug reports the primary repo, for log lines and single-repo output.
func (s *Service) Slug() string { return s.primary.Slug() }

// SplitRepo parses "owner/name". Exported so the CLI and skillctl validate
// operator input with the same rule the plugin enforces.
func SplitRepo(full string) (owner, repo string, err error) {
	parts := strings.Split(strings.TrimSpace(strings.Trim(full, "/")), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("github: repo must be \"owner/name\", got %q", full)
	}
	return parts[0], parts[1], nil
}

// ParseRepoEntry reads one watch-list entry: "owner/name" (read-only) or
// "owner/name:write" (writes permitted, still gated per action).
func ParseRepoEntry(entry string) (RepoRef, error) {
	entry = strings.TrimSpace(entry)
	write := false
	if i := strings.LastIndex(entry, ":"); i > 0 {
		switch strings.ToLower(strings.TrimSpace(entry[i+1:])) {
		case "write", "rw", "w":
			write = true
			entry = entry[:i]
		case "read", "ro", "r":
			entry = entry[:i]
		}
	}
	owner, name, err := SplitRepo(entry)
	if err != nil {
		return RepoRef{}, err
	}
	return RepoRef{Owner: owner, Name: name, Write: write}, nil
}

// ParseRepos splits an operator-supplied list ("a/b, c/d:write") into entries,
// preserving order and dropping duplicates.
func ParseRepos(list string) ([]string, error) {
	fields := strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t' || r == ';'
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		ref, err := ParseRepoEntry(f)
		if err != nil {
			return nil, err
		}
		if seen[strings.ToLower(ref.Slug())] {
			continue
		}
		seen[strings.ToLower(ref.Slug())] = true
		// Preserve the :write suffix so the round trip through config keeps the
		// per-repo authorization instead of silently downgrading it to read-only.
		entry := ref.Slug()
		if ref.Write {
			entry += ":write"
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, errors.New("github: no repositories configured")
	}
	return out, nil
}

// Resolve maps a tool's repo argument onto the allowlist. Empty means the
// primary. Anything not on the list is refused - this is the boundary that
// stops a model (or an injected instruction inside an issue body) from
// steering a call at a repository the operator never configured.
func (s *Service) Resolve(arg string) (RepoRef, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return s.primary, nil
	}
	owner, name, err := SplitRepo(arg)
	if err != nil {
		return RepoRef{}, err
	}
	want := strings.ToLower(owner + "/" + name)
	for _, r := range s.allowed {
		if strings.ToLower(r.Slug()) == want {
			return r, nil
		}
	}
	slugs := make([]string, 0, len(s.allowed))
	for _, r := range s.allowed {
		slugs = append(slugs, r.Slug())
	}
	sort.Strings(slugs)
	return RepoRef{}, fmt.Errorf("refused: %q is not a configured repository. This agent is scoped to: %s",
		owner+"/"+name, strings.Join(slugs, ", "))
}

// Open builds an allowlist-scoped service. It makes NO network call - use
// Validate for that - so constructing a dispatcher stays cheap and offline.
func Open(cfg Config) (*Service, error) {
	if len(cfg.Repos) == 0 {
		return nil, errors.New("github: no repositories configured - set at least one \"owner/name\"")
	}
	allowed := make([]RepoRef, 0, len(cfg.Repos))
	for _, r := range cfg.Repos {
		ref, err := ParseRepoEntry(r)
		if err != nil {
			return nil, err
		}
		allowed = append(allowed, ref)
	}
	tok := strings.TrimSpace(cfg.Token)
	if tok == "" {
		return nil, errors.New("github: no token configured - set it via `/configure github`, the dashboard keys page, or $GITHUB_TOKEN")
	}

	hc := http.Client{Timeout: 30 * time.Second}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	hc.Transport = Events.Observe(hc.Transport) // API failures reach the plugin's log
	client := gh.NewClient(&hc).WithAuthToken(tok)
	if base := strings.TrimSpace(cfg.APIBase); base != "" {
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		var err error
		client, err = client.WithEnterpriseURLs(base, base)
		if err != nil {
			return nil, fmt.Errorf("github: enterprise base %q: %w", base, err)
		}
	}
	return &Service{client: client, primary: allowed[0], allowed: allowed}, nil
}

// Validate confirms the token works and returns the authenticated login. Any
// error is scrubbed of the token before it escapes (a bad PAT often echoes back
// inside a transport error).
func (s *Service) Validate(ctx context.Context) (string, error) {
	u, _, err := s.client.Users.Get(ctx, "")
	if err != nil {
		return "", errors.New("github: token check failed: " + Redact(err.Error()))
	}
	if u == nil || u.GetLogin() == "" {
		return "", errors.New("github: token check returned no user")
	}
	return u.GetLogin(), nil
}

// DefaultBranch reports a repo's default branch. Deliberately uncached: it is
// the input to a SAFETY decision (never delete the default branch), and a stale
// cached value there is not acceptable.
func (s *Service) DefaultBranch(ctx context.Context, r RepoRef) (string, error) {
	repo, _, err := s.client.Repositories.Get(ctx, r.Owner, r.Name)
	if err != nil {
		return "", errors.New("github: read repo: " + Redact(err.Error()))
	}
	if repo.GetDefaultBranch() == "" {
		return "", errors.New("github: repo reported no default branch")
	}
	return repo.GetDefaultBranch(), nil
}
