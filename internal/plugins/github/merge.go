package github

// merge.go is the merge gate (TEN-432). The agent may merge a pull request only
// when every rule passes for the exact commit being merged, and then only with
// an approval that is either a live yes or an explicit github_merge tool rule
// (the broker's explicit-only mode, not a category allow or a session grant).
//
//	github_merge(repo, n, method, expected_sha)
//	  │ repo :write? ── no ──► refuse
//	  │ Confirm wired? ── no ──► refuse (AllowWrite never approves a merge)
//	  ▼
//	 evaluate#1: config │ ready │ ci │ security_review │ docs   (reads only)
//	  │ head == expected_sha and every rule passes? ── no ──► MergeRefused, refuse
//	  ▼                                                       (no approval, no merge)
//	 Confirm(WithApproval(ctx), "github_merge", detail)  ── denied ──► refuse
//	  ▼
//	 evaluate#2: same base ref, base SHA, head SHA, config digest, rules pass? ── no ──► refuse
//	  ▼
//	 PullRequests.Merge(sha = evaluated head) ── lost answer ──► re-read the PR, report the truth
//	  ▼
//	 Merged (carries the approval id)
//
// The security review (TEN-433) and the docs rule (TEN-434) are not built yet,
// so both fail closed and this gate refuses every merge until they land.
//
// What evaluation cannot close: GitHub pins only the head (the sha parameter),
// so the base branch or a check can still change between the second
// evaluation and GitHub processing the merge. Branch protection on the base
// branch is what enforces the checks at that instant.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	gh "github.com/google/go-github/v74/github"
	"tenant/internal/logs/event"
)

// MergeConfigFile holds the per-repo merge rules, beside the token in the
// config dir. A trust root: the file tools protect it, backup routes it with
// config, and forgetting GitHub removes it.
const MergeConfigFile = "github-merge.json"

// MergeConfigPath is where the merge rules live.
func MergeConfigPath(cfgDir string) string { return filepath.Join(cfgDir, MergeConfigFile) }

// ActionMerge is the approval action a merge raises. The host registers it as
// explicit-only: only a live decision or an exact tool rule answers it.
const ActionMerge = "github_merge"

// githubActionsAppID is the GitHub Actions app on github.com. A required check
// must be a check run from the pinned app; on GitHub Enterprise the id differs,
// so there every required check names its app.
const githubActionsAppID = 15368

// RequiredCheck names a check that must pass, and the app that must report it.
type RequiredCheck struct {
	Name  string `json:"name"`
	AppID int64  `json:"app_id,omitempty"`
}

// MergeRepoConfig is one repo's merge rules in github-merge.json.
type MergeRepoConfig struct {
	// AllowedBases are the branches a PR may merge into. Empty: the repo's
	// default branch only.
	AllowedBases   []string        `json:"allowed_bases,omitempty"`
	RequiredChecks []RequiredCheck `json:"required_checks"`
}

// loadMergeConfig reads r's rules and the digest of the whole file. Strict: an
// unknown field or a malformed entry is an error, so a typo can never quietly
// switch a rule off. Read on every evaluation, so an edit needs no restart.
func (s *Service) loadMergeConfig(r RepoRef) (MergeRepoConfig, string, error) {
	var cfg MergeRepoConfig
	if s.mergeConfigPath == "" {
		return cfg, "", errors.New("merge is not configured on this install")
	}
	raw, err := os.ReadFile(s.mergeConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, "", fmt.Errorf("merge is not configured for %s (no %s)", r.Slug(), MergeConfigFile)
	}
	if err != nil {
		return cfg, "", fmt.Errorf("read %s: %w", MergeConfigFile, err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	var repos map[string]json.RawMessage
	if err := json.Unmarshal(raw, &repos); err != nil {
		return cfg, digest, fmt.Errorf("%s is not valid JSON: %w", MergeConfigFile, err)
	}
	var entry json.RawMessage
	seen := map[string]bool{}
	for k, v := range repos {
		key := strings.ToLower(strings.TrimSpace(k))
		if seen[key] {
			return cfg, digest, fmt.Errorf("%s lists %s twice (keys differ only in case)", MergeConfigFile, k)
		}
		seen[key] = true
		if key == strings.ToLower(r.Slug()) {
			entry = v
		}
	}
	if entry == nil {
		return cfg, digest, fmt.Errorf("merge is not configured for %s (no entry in %s)", r.Slug(), MergeConfigFile)
	}
	dec := json.NewDecoder(bytes.NewReader(entry))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, digest, fmt.Errorf("%s entry for %s is invalid: %w", MergeConfigFile, r.Slug(), err)
	}
	for i, c := range cfg.RequiredChecks {
		if strings.TrimSpace(c.Name) == "" {
			return cfg, digest, fmt.Errorf("%s entry for %s: required check %d has no name", MergeConfigFile, r.Slug(), i+1)
		}
		if c.AppID == 0 {
			if s.enterprise {
				return cfg, digest, fmt.Errorf("%s entry for %s: required check %q needs an app_id on GitHub Enterprise", MergeConfigFile, r.Slug(), c.Name)
			}
			cfg.RequiredChecks[i].AppID = githubActionsAppID
		}
	}
	return cfg, digest, nil
}

// mergeRule is one rule's verdict, with the fix when it failed.
type mergeRule struct {
	Name   string
	Pass   bool
	Reason string
	Fix    string
}

// mergeReport is one evaluation of a PR against the rules.
type mergeReport struct {
	Repo    RepoRef
	Number  int
	Title   string
	Author  string
	HeadRef string
	HeadSHA string
	BaseRef string
	BaseSHA string
	Digest  string
	Methods []string // merge methods the repo allows
	Rules   []mergeRule
}

func (m mergeReport) failed() []mergeRule {
	var out []mergeRule
	for _, r := range m.Rules {
		if !r.Pass {
			out = append(out, r)
		}
	}
	return out
}

// String is the readiness report the agent sees: one line per rule.
func (m mergeReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s #%d at %s:\n", m.Repo.Slug(), m.Number, shortSHA(m.HeadSHA))
	for _, r := range m.Rules {
		if r.Pass {
			fmt.Fprintf(&b, "- %s: pass\n", r.Name)
			continue
		}
		fmt.Fprintf(&b, "- %s: FAIL - %s", r.Name, r.Reason)
		if r.Fix != "" {
			fmt.Fprintf(&b, " Fix: %s", r.Fix)
		}
		b.WriteString("\n")
	}
	if len(m.failed()) == 0 {
		b.WriteString("ready to merge (needs approval)")
	} else {
		fmt.Fprintf(&b, "not ready: %d rule(s) failing", len(m.failed()))
	}
	return b.String()
}

// mergeableWait is how long evaluation waits for GitHub to compute a PR's
// mergeability, which it does lazily. A var so tests don't sleep.
var mergeableWait = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// getPRSettled reads the PR, re-reading while GitHub hasn't worked out whether
// it can merge.
func (d *Dispatcher) getPRSettled(ctx context.Context, r RepoRef, number int) (*gh.PullRequest, error) {
	pr, _, err := d.svc.client.PullRequests.Get(ctx, r.Owner, r.Name, number)
	for _, wait := range mergeableWait {
		if err != nil || !mergeableUnknown(pr) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		pr, _, err = d.svc.client.PullRequests.Get(ctx, r.Owner, r.Name, number)
	}
	return pr, err
}

func mergeableUnknown(pr *gh.PullRequest) bool {
	s := pr.GetMergeableState()
	return s == "" || s == "unknown"
}

// evaluate checks a PR against every rule. Reads only: it never merges, never
// asks for approval and never posts anything. A config problem is itself a
// failing rule, so readiness and merge report it the same way.
func (d *Dispatcher) evaluate(ctx context.Context, r RepoRef, number int) (mergeReport, error) {
	rep := mergeReport{Repo: r, Number: number}
	cfg, digest, cerr := d.svc.loadMergeConfig(r)
	rep.Digest = digest
	if cerr != nil {
		rep.Rules = []mergeRule{{Name: "config", Reason: cerr.Error() + ".",
			Fix: "an operator adds this repo's rules to " + MergeConfigFile + " in the config dir."}}
		return rep, nil
	}

	pr, err := d.getPRSettled(ctx, r, number)
	if err != nil {
		return rep, err
	}
	repo, _, err := d.svc.client.Repositories.Get(ctx, r.Owner, r.Name)
	if err != nil {
		return rep, err
	}
	rep.Title = pr.GetTitle()
	rep.Author = pr.GetUser().GetLogin()
	rep.HeadRef = pr.GetHead().GetRef()
	rep.HeadSHA = pr.GetHead().GetSHA()
	rep.BaseRef = pr.GetBase().GetRef()
	rep.BaseSHA = pr.GetBase().GetSHA()
	if repo.GetAllowMergeCommit() {
		rep.Methods = append(rep.Methods, "merge")
	}
	if repo.GetAllowSquashMerge() {
		rep.Methods = append(rep.Methods, "squash")
	}
	if repo.GetAllowRebaseMerge() {
		rep.Methods = append(rep.Methods, "rebase")
	}

	checks, statusesRead, err := d.checksAt(ctx, r, rep.HeadSHA)
	if err != nil {
		return rep, err
	}
	ci := ciRule(cfg, checks)
	if !statusesRead && ci.Pass {
		ci = mergeRule{Name: "ci", Reason: "the token can't read this commit's statuses, so a failing status could be missed.",
			Fix: "give the token the Commit statuses: read permission."}
	}
	rep.Rules = []mergeRule{
		{Name: "config", Pass: true},
		readyRule(pr, cfg, repo.GetDefaultBranch()),
		ci,
	}
	rep.Rules = append(rep.Rules, unbuiltRules()...)
	return rep, nil
}

// unbuiltRules are the rules whose checks aren't built yet: the security
// review (TEN-433) and the docs rule (TEN-434). They fail closed, so nothing
// merges until both land. A var so tests can reach the approval and merge path.
var unbuiltRules = func() []mergeRule {
	return []mergeRule{
		{Name: "security_review", Reason: "the independent security review isn't available yet (TEN-433).",
			Fix: "a person reviews and merges this PR."},
		{Name: "docs", Reason: "the docs and patch-notes check isn't available yet (TEN-434).",
			Fix: "a person checks the docs and merges this PR."},
	}
}

// readyRule: open, not a draft, from this repo, into an allowed base, and
// mergeable as GitHub sees it.
func readyRule(pr *gh.PullRequest, cfg MergeRepoConfig, defaultBranch string) mergeRule {
	rule := mergeRule{Name: "ready"}
	bases := cfg.AllowedBases
	if len(bases) == 0 {
		bases = []string{defaultBranch}
	}
	allowed := false
	for _, b := range bases {
		if b == pr.GetBase().GetRef() {
			allowed = true
		}
	}
	head, base := pr.GetHead().GetRepo().GetFullName(), pr.GetBase().GetRepo().GetFullName()
	switch {
	case pr.GetState() != "open" || pr.GetMerged():
		rule.Reason = fmt.Sprintf("the PR is %s.", prState(pr))
	case pr.GetDraft():
		rule.Reason, rule.Fix = "the PR is a draft.", "mark it ready for review."
	case head == "" || !strings.EqualFold(head, base):
		rule.Reason = fmt.Sprintf("the head branch comes from %q, not %s; the agent only merges branches of the same repo.", head, base)
		rule.Fix = "a person reviews and merges PRs from forks."
	case !allowed:
		rule.Reason = fmt.Sprintf("it targets %q; merges are allowed only into %s.", pr.GetBase().GetRef(), strings.Join(bases, ", "))
	default:
		switch s := pr.GetMergeableState(); s {
		case "clean", "has_hooks":
			rule.Pass = true
		case "", "unknown":
			rule.Reason, rule.Fix = "GitHub hasn't worked out whether the PR can merge yet.", "check again in a minute."
		case "blocked":
			rule.Reason = "branch protection is blocking the merge (a required review or status is missing)."
			rule.Fix = "see what the branch protection still needs."
		case "behind":
			rule.Reason, rule.Fix = "the branch is behind its base.", "update the branch from the base."
		case "dirty":
			rule.Reason, rule.Fix = "the branch has merge conflicts.", "resolve them and push."
		case "unstable":
			rule.Reason = "a check on the PR is failing."
		default:
			rule.Reason = fmt.Sprintf("GitHub reports the PR as %q.", s)
		}
	}
	return rule
}

func prState(pr *gh.PullRequest) string {
	if pr.GetMerged() {
		return "already merged"
	}
	return pr.GetState()
}

// ciCheck is one check run or commit status on a commit.
type ciCheck struct {
	Name   string
	AppID  int64  // check runs only
	Status bool   // true: a commit status, not a check run
	State  string // pending, success, failure, neutral, skipped, cancelled, ...
	run    *gh.CheckRun
}

// checksAt reads every check run (the latest run of each) and every commit
// status on sha, all pages. The CI tool, the check-logs tool and the merge
// gate all read CI through here, so they can't disagree. statusesRead is false
// when the token may read check runs but not commit statuses (a 403 or 404):
// the tools still show the runs, and the merge gate refuses.
func (d *Dispatcher) checksAt(ctx context.Context, r RepoRef, sha string) (checks []ciCheck, statusesRead bool, err error) {
	opts := &gh.ListCheckRunsOptions{Filter: gh.Ptr("latest"), ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		res, resp, err := d.svc.client.Checks.ListCheckRunsForRef(ctx, r.Owner, r.Name, sha, opts)
		if err != nil {
			return nil, false, err
		}
		for _, cr := range res.CheckRuns {
			state := cr.GetConclusion()
			if cr.GetStatus() != "completed" || state == "" {
				state = "pending"
			}
			checks = append(checks, ciCheck{Name: cr.GetName(), AppID: cr.GetApp().GetID(), State: state, run: cr})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	lo := &gh.ListOptions{PerPage: 100}
	for {
		st, resp, err := d.svc.client.Repositories.GetCombinedStatus(ctx, r.Owner, r.Name, sha, lo)
		var ge *gh.ErrorResponse
		if errors.As(err, &ge) && ge.Response != nil &&
			(ge.Response.StatusCode == http.StatusForbidden || ge.Response.StatusCode == http.StatusNotFound) {
			return checks, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		for _, s := range st.Statuses {
			checks = append(checks, ciCheck{Name: s.GetContext(), Status: true, State: s.GetState()})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		lo.Page = resp.NextPage
	}
	return checks, true, nil
}

// ciRule: every required check is a successful check run from its pinned app,
// and nothing else on the commit is pending or failing. A commit status never
// satisfies a required check: anyone who can post statuses can post any name.
func ciRule(cfg MergeRepoConfig, checks []ciCheck) mergeRule {
	rule := mergeRule{Name: "ci"}
	if len(cfg.RequiredChecks) == 0 {
		rule.Reason = "no required checks are configured, so there is no CI to trust."
		rule.Fix = "an operator lists this repo's required checks in " + MergeConfigFile + "."
		return rule
	}
	var problems []string
	counted := map[int]bool{} // the pinned-app runs a required check was judged on
	for _, want := range cfg.RequiredChecks {
		var pinned []int
		other, status := int64(0), false
		for i, c := range checks {
			switch {
			case c.Name != want.Name:
			case c.Status:
				status = true
			case c.AppID == want.AppID:
				pinned = append(pinned, i)
			default:
				other = c.AppID
			}
		}
		// Every pinned run of the name must pass: a workflow on both push and
		// pull_request reports two runs of one name, and the failing one counts.
		for _, i := range pinned {
			counted[i] = true
			if checks[i].State != "success" {
				problems = append(problems, fmt.Sprintf("required check %q is %s", want.Name, checks[i].State))
			}
		}
		switch {
		case len(pinned) > 0:
		case other != 0:
			problems = append(problems, fmt.Sprintf("required check %q came from app %d, not the pinned app %d", want.Name, other, want.AppID))
		case status:
			problems = append(problems, fmt.Sprintf("required check %q is only a commit status; it must be a check run from app %d", want.Name, want.AppID))
		default:
			problems = append(problems, fmt.Sprintf("required check %q hasn't reported", want.Name))
		}
	}
	// Everything else on the commit, including a run or a status that only
	// shares a required check's name, must not be pending or failing.
	for i, c := range checks {
		if counted[i] || strings.HasPrefix(c.Name, "tenant/") {
			continue
		}
		switch c.State {
		case "success":
		case "neutral", "skipped":
			if c.Status {
				problems = append(problems, fmt.Sprintf("%q is %s", c.Name, c.State))
			}
		default:
			problems = append(problems, fmt.Sprintf("%q is %s", c.Name, c.State))
		}
	}
	if len(problems) > 0 {
		rule.Reason = strings.Join(problems, "; ") + "."
		rule.Fix = "wait for CI to finish, or fix the failing checks and push."
		return rule
	}
	rule.Pass = true
	return rule
}

// safeText makes attacker-controlled text (a PR title, a branch name) fit to
// show on an approval card: control characters and line breaks become spaces,
// runs of spaces collapse, and it is clipped, so it can't fake a rule line or
// a second field.
func safeText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		// Format characters (a right-to-left override) could reorder the
		// card visually; line and paragraph separators could break it.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "..."
	}
	return s
}

// approvalDetail is what the approver sees: what lands where, and the rules.
func approvalDetail(rep mergeReport, method string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "merge %s #%d %q (%s)\n", rep.Repo.Slug(), rep.Number, safeText(rep.Title, 80), method)
	fmt.Fprintf(&b, "into %q from %q at %s, by %s\n", safeText(rep.BaseRef, 60), safeText(rep.HeadRef, 60),
		shortSHA(rep.HeadSHA), safeText(rep.Author, 40))
	for _, r := range rep.Rules {
		fmt.Fprintf(&b, "%s: pass\n", r.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Merged is a merge the agent made through the gate.
var Merged = Events.Provider.Event(1101, event.Info, "Merge", "Merged {repo} #{number} at {sha}", "repo", "number", "sha", "method").
	Doc("The agent merged a pull request after every merge rule passed and the merge was approved.",
		"The agent called github_merge on a PR that was ready, and a person or an explicit github_merge tool rule approved it.",
		"None. The approval that let it run is on this record.")

// MergeRefused is a merge the gate stopped.
var MergeRefused = Events.Provider.Event(2101, event.Warning, "Merge", "Refused to merge {repo} #{number}: {rule}", "repo", "number", "sha", "rule", "reason").
	Doc("The agent asked to merge a pull request and the merge gate refused it.",
		"A merge rule failed (config, ready, ci, security_review, docs), the PR changed while approval was pending, or the merge wasn't approved.",
		"Read reason. The agent was told the same thing and the fix; a person can merge the PR by hand.")

func (d *Dispatcher) mergeReadiness(ctx context.Context, args json.RawMessage) (string, bool, error) {
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
	rep, err := d.evaluate(ctx, r, a.Number)
	if err != nil {
		return fail("github merge readiness failed", err)
	}
	return rep.String(), false, nil
}

func (d *Dispatcher) merge(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Number      int    `json:"number"`
		Method      string `json:"method"`
		ExpectedSHA string `json:"expected_sha"`
		Repo        string `json:"repo"`
	}
	if msg, bad := unmarshalArgs(args, &a); bad {
		return msg, true, nil
	}
	if strings.TrimSpace(a.ExpectedSHA) == "" {
		return "expected_sha is required: the head commit you checked with github_merge_readiness", true, nil
	}
	r, err := d.svc.Resolve(a.Repo)
	if err != nil {
		return err.Error(), true, nil
	}
	if !r.Write {
		return fmt.Sprintf("refused: %s is watched read-only - the agent can't merge there", r.Slug()), true, nil
	}
	// Never through policy.gate: its AllowWrite shortcut must not approve a merge.
	if d.policy.Confirm == nil {
		return "refused: merging needs an approval system, and none is wired on this surface", true, nil
	}
	refuse := func(sha, rule, reason string) (string, bool, error) {
		MergeRefused.Log(ctx, "repo", r.Slug(), "number", a.Number, "sha", sha, "rule", rule, "reason", reason)
		return "refused: " + reason, true, nil
	}

	rep, err := d.evaluate(ctx, r, a.Number)
	if err != nil {
		return fail("github merge failed", err)
	}
	if bad := rep.failed(); len(bad) > 0 {
		return refuse(rep.HeadSHA, bad[0].Name, "the PR isn't ready to merge.\n"+rep.String())
	}
	if !strings.EqualFold(strings.TrimSpace(a.ExpectedSHA), rep.HeadSHA) {
		return refuse(rep.HeadSHA, "expected_sha", fmt.Sprintf("the PR moved: its head is %s, not %s. Check it again with github_merge_readiness.",
			shortSHA(rep.HeadSHA), shortSHA(a.ExpectedSHA)))
	}
	method := strings.ToLower(strings.TrimSpace(a.Method))
	if method == "" && len(rep.Methods) > 0 {
		method = rep.Methods[0]
	}
	if !contains(rep.Methods, method) {
		return refuse(rep.HeadSHA, "method", fmt.Sprintf("%s allows merging only by %s.", r.Slug(), strings.Join(rep.Methods, ", ")))
	}

	// The approval that decides fills this slot, so Merged names it.
	ctx = event.WithApproval(ctx, "")
	if !d.policy.Confirm(ctx, ActionMerge, approvalDetail(rep, method)) {
		return refuse(rep.HeadSHA, "approval", "the merge was not approved.")
	}

	// An approval can wait a long time: check again that what was approved is
	// still what would merge.
	again, err := d.evaluate(ctx, r, a.Number)
	if err != nil {
		return fail("github merge failed", err)
	}
	if again.HeadSHA != rep.HeadSHA || again.BaseRef != rep.BaseRef || again.BaseSHA != rep.BaseSHA || again.Digest != rep.Digest {
		return refuse(again.HeadSHA, "changed", "the PR, its base or the merge rules changed while the merge waited for approval. Check it again with github_merge_readiness.")
	}
	if bad := again.failed(); len(bad) > 0 {
		return refuse(again.HeadSHA, bad[0].Name, "a rule stopped passing while the merge waited for approval.\n"+again.String())
	}

	res, _, err := d.svc.client.PullRequests.Merge(ctx, r.Owner, r.Name, a.Number, "",
		&gh.PullRequestOptions{MergeMethod: method, SHA: rep.HeadSHA})
	if err != nil {
		var ge *gh.ErrorResponse
		if errors.As(err, &ge) && ge.Response != nil && ge.Response.StatusCode < 500 {
			return refuse(rep.HeadSHA, "github", "GitHub refused the merge: "+Redact(err.Error()))
		}
		// No answer, or a server error: the merge may or may not have
		// happened. Read the truth rather than retry.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		pr, _, rerr := d.svc.client.PullRequests.Get(rctx, r.Owner, r.Name, a.Number)
		if rerr == nil && pr.GetMerged() && pr.GetHead().GetSHA() == rep.HeadSHA {
			Merged.Log(ctx, "repo", r.Slug(), "number", a.Number, "sha", rep.HeadSHA, "method", method)
			return fmt.Sprintf("merged %s #%d (the answer was lost; the PR reads as merged)", r.Slug(), a.Number), false, nil
		}
		if rerr != nil {
			return refuse(rep.HeadSHA, "github", "the merge call failed and whether the PR merged is unknown - check it before trying again: "+Redact(err.Error()))
		}
		return refuse(rep.HeadSHA, "github", "the merge call failed and the PR is not merged: "+Redact(err.Error()))
	}
	if !res.GetMerged() {
		return refuse(rep.HeadSHA, "github", "GitHub did not merge the PR: "+Redact(res.GetMessage()))
	}
	Merged.Log(ctx, "repo", r.Slug(), "number", a.Number, "sha", rep.HeadSHA, "method", method)
	return fmt.Sprintf("merged %s #%d by %s at %s (%s)", r.Slug(), a.Number, method, shortSHA(rep.HeadSHA), shortSHA(res.GetSHA())), false, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
