# Change: GitHub gated merge, part 1: the merge gate (TEN-432)

**Branch:** `feat/ten-432-gated-merge` · **Status:** implemented, tested, not yet deployed.

## What changed
The agent can now ask to merge a pull request, but only through a strict gate. It replaces
the old `github_merge_pr` tool, which always refused.

Two new GitHub tools:
- `github_merge_readiness {number, repo?}` is read-only. It reports one line per merge rule,
  saying what failed and how to fix it.
- `github_merge {number, expected_sha, method?, repo?}` is gated. It merges only when every rule
  passes for the exact head commit the agent checked, and the merge is then approved.

The rules, all checked against the PR's current head commit:

| rule | passes when |
|---|---|
| `config` | the repo has an entry in `github-merge.json` (below) that reads cleanly |
| `ready` | the PR is open, not a draft, from the same repo (not a fork), targets an allowed base, and GitHub reports it `clean` or `has_hooks` |
| `ci` | every required check is a successful **check run from its pinned app** (every such run, if the name reports twice), and nothing else on the commit, including a namesake run or status, is pending or failing |
| `security_review` | **not built yet** (TEN-433): always fails |
| `docs` | **not built yet** (TEN-434): always fails |

Because the last two always fail, **this release merges nothing**. It ships the gate so that the
security review and the docs check can plug into it.

How a merge runs:
1. The repo must be watched `:write`.
2. An approval system must be wired. The `allow_write` setting never approves a merge.
3. Every rule must pass, and `expected_sha` must equal the head. A failure refuses the merge
   before anyone is asked.
4. The approval is **explicit-only**. Only a live yes or an exact `github_merge` tool rule
   answers it. A wildcard rule such as `github_*: allow` doesn't; a matching deny of any
   shape still refuses. A "destructive: allow" category mode or a session grant does not, since those
   were given for other actions. A category deny still refuses. "Always" on the prompt covers
   this time only.
5. After approval, the gate checks again. The head, the base branch, the base commit and the
   rules file must be unchanged, and every rule must still pass.
6. GitHub merges with its `sha` guard set to the checked head. If the answer is lost, or GitHub
   answers with a server error, Tenant reads the PR to report whether it merged. It never
   retries.

Every refusal and every merge is in the `plugin.github` log: Merged (1101), which carries the
approval that allowed it, and MergeRefused (2101).

## Setting it up
Create `github-merge.json` in the config directory, keyed by `owner/name`:

```json
{
  "owner/repo": {
    "allowed_bases": ["main"],
    "required_checks": [
      {"name": "build"},
      {"name": "test", "app_id": 15368}
    ]
  }
}
```

- `allowed_bases` is optional and defaults to the repo's default branch.
- `app_id` defaults to 15368, which is GitHub Actions on github.com. **On GitHub Enterprise
  every required check must name its `app_id`.**
- Unknown fields are an error. The gate then refuses rather than guess, so a typo can't
  switch a rule off.
- The file is read on every check, so edits need no restart. Two keys that differ only in
  case are an error.
- The token needs **Commit statuses: read**, besides Checks: read, so the gate can see every
  status on the commit. Without it, `github_pr_checks` still lists the check runs (with a
  note), and the `ci` rule fails and names the missing permission.

The file is a trust root:
- The agent's file tools can't write it.
- A backup routes it with the config.
- Forgetting GitHub removes it.

## Known limits
- **The gate can't lock the base branch or CI at the moment of merging.** GitHub's `sha`
  guard pins only the PR's head, so the base branch or a check can still change between
  Tenant's last check and GitHub processing the merge. Turn on branch protection for the base
  branch, with the same required checks, so GitHub enforces them at that instant too.
- **Shell commands can still write the rules file.** On the default host exec backend,
  `os_exec` can write it, just as it can the approval settings files. Sandboxing every exec
  surface by default (TEN-347) closes this.
- **Forks aren't merged.** PRs from forks are refused; a person merges them.
- **Unattended surfaces can't merge yet.** Scheduled jobs and alert investigations can't
  merge; that comes with TEN-435.

## Why
The operator decided (2026-09-28) that the agent may merge, but only with proper controls: an
adversarial security review of the new code, updated docs and patch notes, and green CI, all
for the exact change being merged, and then the normal Allow / Ask / Deny approval.
TEN-432 is the gate. TEN-433 (the independent security review), TEN-434 (the docs rule) and
TEN-435 (unattended merge) complete it.
