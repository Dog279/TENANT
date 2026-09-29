package github

// dispatch.go exposes the GitHub control plane to the agent and enforces the
// blast-radius boundary (TEN-174/175). Three tiers:
//
//	reads   - always allowed (search / get / list / read / CI status / changes)
//	writes  - deny-by-default; need AllowWrite or a per-action Confirm
//	merge   - the merge gate (merge.go): every rule must pass for the exact
//	          commit, then an explicit approval; AllowWrite never approves it
//
// Some targets are refused in Go REGARDLESS of policy, Confirm, or model intent
// (deleting the default branch). A gate that lives only in a prompt is a
// suggestion; a compromised or merely over-eager model talks its way past it.
// These are if statements, so prompt injection cannot reach them.
//
// Every tool takes an optional repo argument resolved through the configured
// allowlist (Service.Resolve). Omitted means the primary repo; a repo that is
// not configured is refused. That matters more than usual here: this plugin
// reads issue bodies, PR descriptions and commit messages, which are attacker-
// controlled text, so "the model asked for it" is not a safe basis for scope.
//
// Never exposed at all (absent from Tools, so the model cannot even name them):
// repo deletion, settings/branch-protection, secrets/environments, and
// workflow-file edits - an agent must not be able to rewrite the CI that judges
// its own work.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v74/github"
	"tenant/internal/model"
)

// Policy gates GitHub writes by blast radius. Same shape as the Atlassian
// plugin so the toolmux wiring and the shared confirm broker are reused as-is.
type Policy struct {
	AllowWrite bool
	Confirm    func(ctx context.Context, action, detail string) bool
}

type actionClass int

const (
	classRead actionClass = iota
	classWrite
)

func (p Policy) gate(ctx context.Context, c actionClass, detail string) error {
	if c == classRead || p.AllowWrite {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, "github_write", detail) {
		return nil
	}
	return fmt.Errorf("blocked: this would modify the GitHub repo (%s) and was not approved - "+
		"enable the write flag or confirm. This is a blast-radius boundary, not a bug", detail)
}

// Dispatcher implements the agent tool-dispatcher for GitHub.
type Dispatcher struct {
	svc    *Service
	policy Policy
}

func NewDispatcher(svc *Service, policy Policy) *Dispatcher {
	return &Dispatcher{svc: svc, policy: policy}
}

const repoArgDesc = `"repo":{"type":"string","description":"owner/name; omit for the primary repo. Must be one of the configured repositories."}`

// gateWrite applies the per-repo authorization BEFORE the shared Policy gate.
// Order matters: a repo the operator marked read-only is refused in code, so no
// amount of AllowWrite or Confirm can reach it. The Policy gate then decides
// whether THIS action, on a writable repo, is approved.
func (d *Dispatcher) gateWrite(ctx context.Context, r RepoRef, detail string) error {
	if !r.Write {
		return fmt.Errorf("refused: %s is watched read-only - reporting works, writing does not. "+
			"Mark it writable with `/github write %s` if that is intended", r.Slug(), r.Slug())
	}
	return d.policy.gate(ctx, classWrite, detail)
}

func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	return []model.ToolSpec{
		// --- reads (ungated) ---
		{Name: "github_changes", Description: "What changed recently across the watched repositories: new commits, and issues/PRs updated in the window. Use this for a daily brief or an overnight catch-up. Omit repo to cover every watched repo.",
			Parameters: obj(`"since":{"type":"string","description":"window, e.g. 24h, 7d, or an RFC3339 timestamp (default 24h)"},` + repoArgDesc + `,"max":{"type":"integer","description":"max commits per repo (default 20, cap 50)"}`)},
		{Name: "github_search", Description: "Search issues and pull requests using GitHub search syntax (e.g. `is:pr is:open review:required`, `is:issue label:bug`). Returns number/title/state/author/updated.",
			Parameters: obj(`"query":{"type":"string"},`+repoArgDesc+`,"max":{"type":"integer","description":"max results (default 20, cap 50)"}`, "query")},
		{Name: "github_get_issue", Description: "Get one issue by number: title, state, author, labels, body.",
			Parameters: obj(`"number":{"type":"integer"},`+repoArgDesc, "number")},
		{Name: "github_get_pr", Description: "Get one pull request by number: title, state, draft, branches, mergeable state, body.",
			Parameters: obj(`"number":{"type":"integer"},`+repoArgDesc, "number")},
		{Name: "github_list_files", Description: "List a directory in a repo at a ref (default: the default branch). Omit path for the root. Use github_read_file for contents.",
			Parameters: obj(`"path":{"type":"string"},"ref":{"type":"string","description":"branch, tag or SHA"},` + repoArgDesc)},
		{Name: "github_read_file", Description: "Read one text file from a repo at a ref. Binary files are refused. Truncated at 64KB.",
			Parameters: obj(`"path":{"type":"string"},"ref":{"type":"string"},`+repoArgDesc, "path")},
		{Name: "github_pr_checks", Description: "CI status for a pull request: each check run name, status and conclusion. Poll this after pushing to see whether the build went green.",
			Parameters: obj(`"number":{"type":"integer"},`+repoArgDesc, "number")},
		{Name: "github_check_logs", Description: "Failure detail for a pull request: the output title, summary and annotations (file:line messages) of every check that did not succeed. Use after github_pr_checks reports a failure.",
			Parameters: obj(`"number":{"type":"integer"},`+repoArgDesc+`,"max":{"type":"integer","description":"max annotations (default 20, cap 50)"}`, "number")},

		// --- writes (gated) ---
		{Name: "github_create_branch", Description: "Create a branch from a base ref. GATED. Use this before pushing work; never commit directly to the default branch.",
			Parameters: obj(`"name":{"type":"string"},"from":{"type":"string","description":"base branch (default: the repo default branch)"},`+repoArgDesc, "name"), Gated: true},
		{Name: "github_create_issue", Description: "Create an issue. GATED (visible to collaborators).",
			Parameters: obj(`"title":{"type":"string"},"body":{"type":"string"},`+repoArgDesc, "title"), Gated: true},
		{Name: "github_comment", Description: "Comment on an issue or pull request. GATED (notifies collaborators).",
			Parameters: obj(`"number":{"type":"integer"},"body":{"type":"string"},`+repoArgDesc, "number", "body"), Gated: true},
		{Name: "github_create_pr", Description: "Open a pull request from a head branch into a base branch. GATED. Opens as a DRAFT unless draft=false.",
			Parameters: obj(`"title":{"type":"string"},"head":{"type":"string"},"base":{"type":"string","description":"default: the repo default branch"},"body":{"type":"string"},"draft":{"type":"boolean","description":"default true"},`+repoArgDesc, "title", "head"), Gated: true},
		{Name: "github_update_pr", Description: "Update a pull request title or body. GATED. Cannot merge or mark merged.",
			Parameters: obj(`"number":{"type":"integer"},"title":{"type":"string"},"body":{"type":"string"},`+repoArgDesc, "number"), Gated: true},
		{Name: "github_delete_branch", Description: "Delete a branch. GATED, and REFUSED for a repo default branch regardless of approval.",
			Parameters: obj(`"name":{"type":"string"},`+repoArgDesc, "name"), Gated: true},

		// --- the merge gate (merge.go) ---
		{Name: "github_merge_readiness", Description: "Check whether a pull request may be merged: one line per merge rule (config, ready, ci, security_review, docs) with what failed and how to fix it. Read-only. Run it before github_merge and pass its head commit as expected_sha.",
			Parameters: obj(`"number":{"type":"integer"},`+repoArgDesc, "number")},
		{Name: "github_merge", Description: "Merge a pull request. GATED and strict: every merge rule must pass for the exact head commit (see github_merge_readiness), then the merge must be approved. Refused with the failing rule and its fix otherwise. The security review and docs rules are not available yet, so a person merges for now.",
			Parameters: obj(`"number":{"type":"integer"},"expected_sha":{"type":"string","description":"the head commit github_merge_readiness reported"},"method":{"type":"string","description":"merge, squash or rebase (default: the first the repo allows)"},`+repoArgDesc, "number", "expected_sha"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if d.svc == nil {
		return "github is not configured - run `/configure github` (or set $GITHUB_TOKEN) and `/enable github`", true, nil
	}
	switch call.Name {
	case "github_changes":
		return d.changes(ctx, call.Arguments)
	case "github_search":
		return d.search(ctx, call.Arguments)
	case "github_get_issue":
		return d.getIssue(ctx, call.Arguments)
	case "github_get_pr":
		return d.getPR(ctx, call.Arguments)
	case "github_list_files":
		return d.listFiles(ctx, call.Arguments)
	case "github_read_file":
		return d.readFile(ctx, call.Arguments)
	case "github_pr_checks":
		return d.prChecks(ctx, call.Arguments)
	case "github_check_logs":
		return d.checkLogs(ctx, call.Arguments)
	case "github_create_branch":
		return d.createBranch(ctx, call.Arguments)
	case "github_create_issue":
		return d.createIssue(ctx, call.Arguments)
	case "github_comment":
		return d.comment(ctx, call.Arguments)
	case "github_create_pr":
		return d.createPR(ctx, call.Arguments)
	case "github_update_pr":
		return d.updatePR(ctx, call.Arguments)
	case "github_delete_branch":
		return d.deleteBranch(ctx, call.Arguments)
	case "github_merge_readiness":
		return d.mergeReadiness(ctx, call.Arguments)
	case "github_merge":
		return d.merge(ctx, call.Arguments)
	}
	return "", false, fmt.Errorf("github: unknown tool %q", call.Name)
}

func unmarshalArgs(args json.RawMessage, v any) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	if err := json.Unmarshal(args, v); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	return "", false
}

// fail scrubs the token out of every error before the agent - and the archive
// it is written to - can see it. go-github echoes the request URL on failure.
func fail(what string, err error) (string, bool, error) {
	return what + ": " + Redact(err.Error()), true, nil
}

func clampMax(n, def, capN int) int {
	if n <= 0 {
		return def
	}
	if n > capN {
		return capN
	}
	return n
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// parseSince accepts a Go-ish duration (24h, 90m), a day count (7d), or an
// RFC3339 timestamp. Defaults to 24h, which is the daily-brief window.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return now.Add(-24 * time.Hour), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && n > 0 {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return now.Add(-dur), nil
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a window - use 24h, 7d, or an RFC3339 timestamp", s)
}

// scope returns the repos a read should cover: one when named, all watched when
// omitted. Only reads fan out; every write resolves to exactly one repo.
func (d *Dispatcher) scope(arg string) ([]RepoRef, error) {
	if strings.TrimSpace(arg) == "" {
		return d.svc.Watched(), nil
	}
	r, err := d.svc.Resolve(arg)
	if err != nil {
		return nil, err
	}
	return []RepoRef{r}, nil
}

// changes is the watcher: what landed since `since`, across the watched set.
//
// Stateless on purpose. A stored cursor would have to be correct across
// restarts, catch-up runs and manual invocations, and getting it wrong either
// double-reports or silently swallows a push. The caller says how far back to
// look, so the 08:00 brief asks for 24h and a manual check asks for 1h.
func (d *Dispatcher) changes(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Since string `json:"since"`
		Repo  string `json:"repo"`
		Max   int    `json:"max"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	since, err := parseSince(a.Since, time.Now())
	if err != nil {
		return err.Error(), true, nil
	}
	repos, err := d.scope(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	limit := clampMax(a.Max, 20, 50)

	var b strings.Builder
	fmt.Fprintf(&b, "changes since %s (%d repo(s)):\n", since.UTC().Format(time.RFC3339), len(repos))
	quiet := 0
	for _, r := range repos {
		commits, _, cerr := d.svc.client.Repositories.ListCommits(ctx, r.Owner, r.Name, &gh.CommitsListOptions{
			Since:       since,
			ListOptions: gh.ListOptions{PerPage: limit},
		})
		if cerr != nil {
			fmt.Fprintf(&b, "\n== %s\n   (could not read commits: %s)\n", r.Slug(), Redact(cerr.Error()))
			continue
		}
		// Issues and PRs touched in the window, via search so one call covers both.
		q := fmt.Sprintf("repo:%s updated:>=%s", r.Slug(), since.UTC().Format("2006-01-02"))
		res, _, serr := d.svc.client.Search.Issues(ctx, q, &gh.SearchOptions{
			ListOptions: gh.ListOptions{PerPage: limit},
		})

		if len(commits) == 0 && (serr != nil || res == nil || len(res.Issues) == 0) {
			quiet++
			continue
		}
		fmt.Fprintf(&b, "\n== %s\n", r.Slug())
		if len(commits) == 0 {
			fmt.Fprintf(&b, "   no new commits\n")
		}
		for _, c := range commits {
			author := c.GetCommit().GetAuthor().GetName()
			if author == "" {
				author = c.GetAuthor().GetLogin()
			}
			fmt.Fprintf(&b, "   %s %s (%s, %s)\n", shortSHA(c.GetSHA()), clip(c.GetCommit().GetMessage(), 90),
				author, c.GetCommit().GetAuthor().GetDate().Format("2006-01-02 15:04"))
		}
		if serr != nil {
			fmt.Fprintf(&b, "   (could not read issues/PRs: %s)\n", Redact(serr.Error()))
			continue
		}
		for _, is := range res.Issues {
			kind := "issue"
			if is.IsPullRequest() {
				kind = "pr"
			}
			fmt.Fprintf(&b, "   [%s/%s] #%d %s (by %s)\n", kind, is.GetState(), is.GetNumber(),
				clip(is.GetTitle(), 80), is.GetUser().GetLogin())
		}
	}
	if quiet > 0 {
		fmt.Fprintf(&b, "\n%d repo(s) had no activity in the window.\n", quiet)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) search(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Query string `json:"query"`
		Repo  string `json:"repo"`
		Max   int    `json:"max"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Query) == "" {
		return "query is required", true, nil
	}
	repos, err := d.scope(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	// Pin the repo qualifier ourselves. The model supplies filter terms, never
	// the scope - so a query carrying its own repo: cannot widen the search.
	quals := make([]string, 0, len(repos))
	for _, r := range repos {
		quals = append(quals, "repo:"+r.Slug())
	}
	q := strings.Join(quals, " ") + " " + a.Query
	res, _, err := d.svc.client.Search.Issues(ctx, q, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: clampMax(a.Max, 20, 50)},
	})
	if err != nil {
		return fail("github search failed", err)
	}
	if res == nil || len(res.Issues) == 0 {
		return fmt.Sprintf("no issues or PRs matched %q", a.Query), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d result(s):\n", len(res.Issues))
	for _, is := range res.Issues {
		kind := "issue"
		if is.IsPullRequest() {
			kind = "pr"
		}
		fmt.Fprintf(&b, "- #%d [%s/%s] %s (by %s, updated %s)\n",
			is.GetNumber(), kind, is.GetState(), is.GetTitle(), is.GetUser().GetLogin(),
			is.GetUpdatedAt().Format("2006-01-02"))
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) getIssue(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Number int    `json:"number"`
		Repo   string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	is, _, err := d.svc.client.Issues.Get(ctx, r.Owner, r.Name, a.Number)
	if err != nil {
		return fail("github get issue failed", err)
	}
	labels := make([]string, 0, len(is.Labels))
	for _, l := range is.Labels {
		labels = append(labels, l.GetName())
	}
	return fmt.Sprintf("%s #%d %s\nstate: %s\nauthor: %s\nlabels: %s\n\n%s",
		r.Slug(), is.GetNumber(), is.GetTitle(), is.GetState(), is.GetUser().GetLogin(),
		strings.Join(labels, ", "), is.GetBody()), false, nil
}

func (d *Dispatcher) getPR(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Number int    `json:"number"`
		Repo   string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	pr, _, err := d.svc.client.PullRequests.Get(ctx, r.Owner, r.Name, a.Number)
	if err != nil {
		return fail("github get pr failed", err)
	}
	return fmt.Sprintf("%s #%d %s\nstate: %s (draft=%v)\n%s -> %s\nmergeable_state: %s\nauthor: %s\n\n%s",
		r.Slug(), pr.GetNumber(), pr.GetTitle(), pr.GetState(), pr.GetDraft(),
		pr.GetHead().GetRef(), pr.GetBase().GetRef(), pr.GetMergeableState(),
		pr.GetUser().GetLogin(), pr.GetBody()), false, nil
}

func (d *Dispatcher) listFiles(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Path string `json:"path"`
		Ref  string `json:"ref"`
		Repo string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	file, dir, _, err := d.svc.client.Repositories.GetContents(ctx, r.Owner, r.Name, a.Path,
		&gh.RepositoryContentGetOptions{Ref: a.Ref})
	if err != nil {
		return fail("github list failed", err)
	}
	if file != nil && dir == nil {
		return fmt.Sprintf("%s is a file (%d bytes) - use github_read_file", file.GetPath(), file.GetSize()), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d entr(ies) under %q:\n", r.Slug(), len(dir), a.Path)
	for _, e := range dir {
		fmt.Fprintf(&b, "- %s (%s, %d bytes)\n", e.GetPath(), e.GetType(), e.GetSize())
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

const readFileCap = 64 * 1024

func (d *Dispatcher) readFile(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Path string `json:"path"`
		Ref  string `json:"ref"`
		Repo string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return "path is required", true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	file, _, _, err := d.svc.client.Repositories.GetContents(ctx, r.Owner, r.Name, a.Path,
		&gh.RepositoryContentGetOptions{Ref: a.Ref})
	if err != nil {
		return fail("github read failed", err)
	}
	if file == nil {
		return fmt.Sprintf("%q is a directory - use github_list_files", a.Path), true, nil
	}
	content, err := file.GetContent()
	if err != nil {
		return fmt.Sprintf("%q is not decodable as text (likely binary)", a.Path), true, nil
	}
	if len(content) > readFileCap {
		content = content[:readFileCap] + "\n...[truncated]"
	}
	return content, false, nil
}

func (d *Dispatcher) prHeadSHA(ctx context.Context, r RepoRef, number int) (string, error) {
	pr, _, err := d.svc.client.PullRequests.Get(ctx, r.Owner, r.Name, number)
	if err != nil {
		return "", err
	}
	return pr.GetHead().GetSHA(), nil
}

func (d *Dispatcher) prChecks(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Number int    `json:"number"`
		Repo   string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	sha, err := d.prHeadSHA(ctx, r, a.Number)
	if err != nil {
		return fail("github pr checks failed", err)
	}
	checks, statusesRead, err := d.checksAt(ctx, r, sha)
	if err != nil {
		return fail("github pr checks failed", err)
	}
	runs := 0
	for _, c := range checks {
		if !c.Status {
			runs++
		}
	}
	if len(checks) == 0 {
		return fmt.Sprintf("no check runs reported yet for %s #%d (%s)", r.Slug(), a.Number, shortSHA(sha)), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s #%d (%s) - %d check(s):\n", r.Slug(), a.Number, shortSHA(sha), runs)
	for _, c := range checks {
		if c.Status {
			fmt.Fprintf(&b, "- %s (status): %s\n", c.Name, c.State)
			continue
		}
		concl := c.run.GetConclusion()
		if concl == "" {
			concl = "pending"
		}
		fmt.Fprintf(&b, "- %s: %s / %s\n", c.Name, c.run.GetStatus(), concl)
	}
	if !statusesRead {
		b.WriteString("(commit statuses not shown: the token lacks Commit statuses: read)\n")
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) checkLogs(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Number int    `json:"number"`
		Repo   string `json:"repo"`
		Max    int    `json:"max"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	sha, err := d.prHeadSHA(ctx, r, a.Number)
	if err != nil {
		return fail("github check logs failed", err)
	}
	checks, _, err := d.checksAt(ctx, r, sha)
	if err != nil {
		return fail("github check logs failed", err)
	}
	// Annotations rather than raw Actions logs: the log endpoint redirects to a
	// zip archive, which is megabytes of noise to feed a model. Annotations are
	// already the file:line failure summary.
	limit := clampMax(a.Max, 20, 50)
	var b strings.Builder
	shown := 0
	for _, c := range checks {
		if c.Status {
			continue // a commit status carries no output or annotations
		}
		cr := c.run
		switch cr.GetConclusion() {
		case "success", "neutral", "skipped":
			continue
		}
		fmt.Fprintf(&b, "== %s (%s)\n", cr.GetName(), cr.GetConclusion())
		if t := cr.GetOutput().GetTitle(); t != "" {
			fmt.Fprintf(&b, "   %s\n", t)
		}
		if s := cr.GetOutput().GetSummary(); s != "" {
			fmt.Fprintf(&b, "   %s\n", clip(s, 800))
		}
		anns, _, aerr := d.svc.client.Checks.ListCheckRunAnnotations(ctx, r.Owner, r.Name, cr.GetID(),
			&gh.ListOptions{PerPage: limit})
		if aerr != nil {
			continue
		}
		for _, an := range anns {
			if shown >= limit {
				break
			}
			fmt.Fprintf(&b, "   %s:%d %s: %s\n", an.GetPath(), an.GetStartLine(),
				an.GetAnnotationLevel(), clip(an.GetMessage(), 300))
			shown++
		}
	}
	if b.Len() == 0 {
		return fmt.Sprintf("no failing checks for %s #%d (%s)", r.Slug(), a.Number, shortSHA(sha)), false, nil
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) createBranch(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Name string `json:"name"`
		From string `json:"from"`
		Repo string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return "name is required", true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	base := strings.TrimSpace(a.From)
	if base == "" {
		db, derr := d.svc.DefaultBranch(ctx, r)
		if derr != nil {
			return fail("github create branch failed", derr)
		}
		base = db
	}
	if err := d.gateWrite(ctx, r, fmt.Sprintf("create branch %s from %s in %s", name, base, r.Slug())); err != nil {
		return err.Error(), true, nil
	}
	ref, _, err := d.svc.client.Git.GetRef(ctx, r.Owner, r.Name, "refs/heads/"+base)
	if err != nil {
		return fail("github create branch failed (base ref)", err)
	}
	if _, _, err := d.svc.client.Git.CreateRef(ctx, r.Owner, r.Name, &gh.Reference{
		Ref:    gh.Ptr("refs/heads/" + name),
		Object: &gh.GitObject{SHA: ref.GetObject().SHA},
	}); err != nil {
		return fail("github create branch failed", err)
	}
	return fmt.Sprintf("created branch %s from %s (%s) in %s", name, base, shortSHA(ref.GetObject().GetSHA()), r.Slug()), false, nil
}

func (d *Dispatcher) createIssue(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Repo  string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Title) == "" {
		return "title is required", true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	if err := d.gateWrite(ctx, r, fmt.Sprintf("create issue %q in %s", clip(a.Title, 60), r.Slug())); err != nil {
		return err.Error(), true, nil
	}
	is, _, err := d.svc.client.Issues.Create(ctx, r.Owner, r.Name, &gh.IssueRequest{
		Title: gh.Ptr(a.Title), Body: gh.Ptr(a.Body),
	})
	if err != nil {
		return fail("github create issue failed", err)
	}
	return fmt.Sprintf("created issue #%d: %s", is.GetNumber(), is.GetHTMLURL()), false, nil
}

func (d *Dispatcher) comment(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Number int    `json:"number"`
		Body   string `json:"body"`
		Repo   string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Body) == "" {
		return "body is required", true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	if err := d.gateWrite(ctx, r, fmt.Sprintf("comment on %s #%d", r.Slug(), a.Number)); err != nil {
		return err.Error(), true, nil
	}
	c, _, err := d.svc.client.Issues.CreateComment(ctx, r.Owner, r.Name, a.Number, &gh.IssueComment{
		Body: gh.Ptr(a.Body),
	})
	if err != nil {
		return fail("github comment failed", err)
	}
	return "commented: " + c.GetHTMLURL(), false, nil
}

func (d *Dispatcher) createPR(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft *bool  `json:"draft"`
		Repo  string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Head) == "" {
		return "title and head are required", true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	base := strings.TrimSpace(a.Base)
	if base == "" {
		db, derr := d.svc.DefaultBranch(ctx, r)
		if derr != nil {
			return fail("github create pr failed", derr)
		}
		base = db
	}
	if a.Head == base {
		return fmt.Sprintf("refused: head and base are both %q - a PR needs a separate feature branch", base), true, nil
	}
	// Draft by default. A ready-for-review PR pings reviewers; that should be a
	// deliberate choice, not the fallback when the model omits a field.
	draft := true
	if a.Draft != nil {
		draft = *a.Draft
	}
	if err := d.gateWrite(ctx, r, fmt.Sprintf("open PR %s -> %s in %s", a.Head, base, r.Slug())); err != nil {
		return err.Error(), true, nil
	}
	pr, _, err := d.svc.client.PullRequests.Create(ctx, r.Owner, r.Name, &gh.NewPullRequest{
		Title: gh.Ptr(a.Title), Head: gh.Ptr(a.Head), Base: gh.Ptr(base),
		Body: gh.Ptr(a.Body), Draft: gh.Ptr(draft),
	})
	if err != nil {
		return fail("github create pr failed", err)
	}
	return fmt.Sprintf("opened PR #%d (draft=%v): %s", pr.GetNumber(), draft, pr.GetHTMLURL()), false, nil
}

func (d *Dispatcher) updatePR(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Repo   string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.Title) == "" && strings.TrimSpace(a.Body) == "" {
		return "provide a title or a body to update", true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	if err := d.gateWrite(ctx, r, fmt.Sprintf("update PR %s #%d", r.Slug(), a.Number)); err != nil {
		return err.Error(), true, nil
	}
	// Only title/body are patchable here. Deliberately no State field: setting
	// state is how a merge or a close would sneak through this tool.
	patch := &gh.PullRequest{}
	if a.Title != "" {
		patch.Title = gh.Ptr(a.Title)
	}
	if a.Body != "" {
		patch.Body = gh.Ptr(a.Body)
	}
	pr, _, err := d.svc.client.PullRequests.Edit(ctx, r.Owner, r.Name, a.Number, patch)
	if err != nil {
		return fail("github update pr failed", err)
	}
	return fmt.Sprintf("updated PR #%d: %s", pr.GetNumber(), pr.GetHTMLURL()), false, nil
}

func (d *Dispatcher) deleteBranch(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Name string `json:"name"`
		Repo string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return "name is required", true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	// The default-branch guard runs BEFORE the policy gate on purpose: approval
	// is granted for the action in the abstract, not for this target, so an
	// operator saying yes to "delete a branch" must not delete main.
	db, err := d.svc.DefaultBranch(ctx, r)
	if err != nil {
		return fail("github delete branch failed", err)
	}
	if name == db {
		return fmt.Sprintf("refused: %q is the default branch of %s - deleting it is rejected in code regardless of approval", name, r.Slug()), true, nil
	}
	if err := d.gateWrite(ctx, r, fmt.Sprintf("delete branch %s in %s", name, r.Slug())); err != nil {
		return err.Error(), true, nil
	}
	if _, err := d.svc.client.Git.DeleteRef(ctx, r.Owner, r.Name, "refs/heads/"+name); err != nil {
		return fail("github delete branch failed", err)
	}
	return fmt.Sprintf("deleted branch %s in %s", name, r.Slug()), false, nil
}
