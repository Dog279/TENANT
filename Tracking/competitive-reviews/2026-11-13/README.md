# Competitive review: Tenant vs OpenClaw vs Hermes Agent (2026-11-13)

**Overall (average of judges A, B and C, with the referee's two rulings): Tenant 6.2 · OpenClaw 7.5 · Hermes 7.2.**
On 2026-10-02 it was 5.7 · 7.5 · 7.1. Tenant gained half a point, most of it from running commands, vision and
operations. The rivals barely moved. Tenant is still third, and the largest thing holding it back is no longer a
missing feature: nobody outside can download it.

## What was compared

- **Tenant:** `main` at `8b9bbf7`, which is also the tag `v0.2.0-rc.2`. There are no unmerged local branches this
  time, so "Tenant" means main only. The repo is still private, and the release GitHub marks "Latest" is still `v0.1.0`.
- **OpenClaw:** stable v2026.11.3 (2026-11-10); latest feature release v2026.10.10 (2026-11-03). Repo checked at
  `649ca5f87`, which is `main`. Main is about 17,500 commits ahead of stable, so the report marks main-only items.
- **Hermes Agent** (Nous Research): v0.23.1 (2026-11-10), repo checked at `491f9845`.

## Method

The same as 2026-10-02, with the three changes listed below.

1. Three researchers each wrote an evidence report on one product: [Tenant](dossier-tenant.md) (from the code, with a
   full test run), [OpenClaw](dossier-openclaw.md) and [Hermes](dossier-hermes.md) (from shallow clones plus the GitHub
   API and primary and critical sources). Each report has the same 14 sections as last time and a new table,
   "What changed since 2026-10-02".
2. Judges scored all three products 1–10 on the same 14 capabilities, each without seeing the others' scores:
   - [A: power user](judge-A.md), who wants the most capable agent today
   - [B: security and reliability](judge-B.md), trusting it with a home server unattended
   - [C: newcomer's guide](judge-C.md), choosing one to self-host today
   - [D: a different model](judge-D.md), with no lens and equal weights (new this run)
3. A [referee](referee.md) ruled on any score where the judges were more than 2 points apart. Two were.

**Changes to the method this run**

- **Judges A, B and C were anchored.** Each was given its own 2026-10-02 scorecard and told to move a score only
  where the evidence changed. Their weights are unchanged. This keeps run-to-run changes tied to evidence, at the cost
  of carrying forward anything they misjudged last time.
- **Judge D is new.** It is `DeepSeek-V4.1-Flash-1017-NVFP4` on the DGX machine, called once with the rubric and the
  three reports. It saw no earlier scores, so it is not anchored. It could not open the code or the web, so it made no
  spot-checks. Its scores are reported but are not in the tracked overall, so that the history stays like for like.
- **The referee ran for the first time.** Its rulings replace the judges' scores for those two cells in the tracked
  numbers. Both figures are shown.

**Caveats.** The judges all read the same three reports, so their agreement mostly shows how consistent the reports
are. Tenant's tests ran on Windows without `-race` this time (macOS last time), so the pass counts are not like for
like. Last run scored Tenant as main plus unpushed branches; those branches are now merged.

## Scores (average of judges A, B and C)

A change from 2026-10-02 is shown in brackets. No bracket means no change.

| # | Capability | Tenant | OpenClaw | Hermes |
|---|---|---|---|---|
| 1 | Messaging channels | 3.7 | 10 | 9 |
| 2 | Running commands (shell, files, background jobs) | 6.3 (+2.0) | 9 | 9 |
| 3 | Browser & computer control | 4.7 (+1.3) | 9 | 8.3 (+0.3) |
| 4 | Memory & personalization | 7.7 | 8.3 | 6 |
| 5 | Extensibility (skills, plugins, MCP) | 5.7 | 8.7 | 8.7 |
| 6 | Autonomy & scheduling | 6 | 9 | 8 |
| 7 | Multi-agent / delegation | 5.7 | 8 | 8 |
| 8 | Model choice & local LLMs | 8 (+1.0) | 9 | 8.7 |
| 9 | Security & safe defaults | **6.7** (+0.3) | 3.7 | 4 |
| 10 | Monitoring & operations | **8.7** (+0.7) | 8 | 7.3 |
| 11 | Install & cross-platform | 4 † (−0.3) | 8 | 8.3 |
| 12 | Engineering quality & reliability | **7.3** | 5.3 | 5 † (+0.7) |
| 13 | Docs, community & ecosystem | 3.3 (+1.3) | 9 | 9 |
| 14 | Self-improvement loop | 5 | 7.7 (+1.0) | 7.7 |
| | **Simple average** | **5.9** (+0.4) | **8.0** | **7.6** |

† Referee ruling. The judges' own average was 5.3 for Tenant's install and 4.3 for Hermes's engineering; see
"The two refereed scores" below.

Each judge's overall score uses their own weighting. Last run's figure is in brackets.

| Judge | Tenant | OpenClaw | Hermes |
|---|---|---|---|
| A: power user | 6.1 (5.5) | 8.5 (8.4) | 7.8 (7.8) |
| B: security & reliability | 6.4 (6.0) | 6.4 (6.4) | 6.1 (6.1) |
| C: newcomer's guide | 6.3 (5.5) | 7.7 (7.7) | 7.5 (7.5) |
| Average of A, B and C, as scored | 6.3 | 7.5 | 7.2 |
| **With the referee's two rulings (the tracked figure)** | **6.2** (5.7) | **7.5** (7.5) | **7.2** (7.1) |
| D: a different model, equal weights | 5.9 | 8.2 | 8.1 |
| All four judges, with the rulings | 6.1 | 7.7 | 7.4 |

To two decimals the tracked figures moved from 5.66 · 7.50 · 7.14 to 6.18 · 7.53 · 7.24. Tenant's distance to
OpenClaw fell from 1.84 to 1.35. Under Judge B's weights Tenant is now level with OpenClaw (6.40 and 6.42 as scored).

## What changed since 2026-10-02

**Tenant** (260 commits, 33 merged pull requests)

- **Running commands:** background processes with `os_process`, paged file reads, and `workdir` and `timeout` are on
  main. Read-only commands skip the prompt, and rules can be written per command. Still no PTY and no code runner, and
  background jobs run only on the host.
- **Vision:** screenshots and image files now reach the model, on the Anthropic and OpenAI-compatible backends. The
  browser still clicks by text or CSS selector, and there is no desktop control.
- **Operations and install:** `tenant update` (verified, backed up, with rollback), `tenant service` on three OSes,
  and `install.sh` / `install.ps1`. None of it can be used by anyone outside while the repo is private.
- **Security:** the container sandbox finished its first hardening wave and ran on a real Docker engine for the first
  time (Windows, 14 of 14 cases). The old SSE gateway now needs a token. The defaults did not get stricter:
  interactive surfaces run commands on the host, and so does cron on Linux and Windows.
- **Docs:** a public docs site is live with 43 pages.
- **No change:** channels, memory, skills, autonomy triggers, sub-agent workspaces and the learning loop. The skills
  and learning-loop code is byte-identical to last run.
- **Tests:** `go build`, `go vet` and `go test ./...` pass: 89 packages, 4,488 passes, 81 skips, 0 failures.

**OpenClaw**

- No default changed: the sandbox is still off, and commands still run on the host with no prompt.
- Learned-skill changes are now versioned and can be undone, which closes the gap the judges named last time.
- Nine of the fifteen stable-line releases in the period fixed blocked, stuck or failed updates.
- The published advisory count is flat at 722. No batch was published in the period, though several releases carry
  boundary-bypass fixes that have no advisory yet.
- One new channel (X), about 76 model providers on `main`, and stars up to about 394k.

**Hermes**

- No default changed: commands still run on the host, approvals are still judged by a model, and plugins still load
  in-process. A bubblewrap sandbox and plugin isolation are new but opt-in.
- Installs now follow a tested stable channel, and a sample of CI runs on `main` was mostly green. Against that, open
  bugs rose from 7,953 to 11,149, and one stable release was built from the wrong commit.
- One new high-severity CVE (a webhook authorization bypass, since fixed). The outside posture review from June is
  still open and was lowered in priority.
- A plugin catalog launched with about 2,300 entries. The ten `browser_*` tools were replaced by one, a breaking change.
- A third party measured the fixed prompt at about 12.2K tokens per call, down from about 13.9K.

## Where Tenant leads

- **Security and safe defaults (6.7 against 3.7 and 4).** Approvals are on by default, and neither rival changed a
  default. The lead comes from prompts and tracking, not confinement: see "Found along the way".
- **Monitoring and operations (8.7 against 8 and 7.3).** Self-update and the service installer were added to the log
  book, doctor, backups and security log. Three of the four judges put Tenant first here; Judge A has it level with
  OpenClaw.
- **Engineering quality (7.3 against 5.3 and 5).** No failing tests and almost no deferred work in code, against
  OpenClaw's update breakages and Hermes's growing bug backlog.
- **Token cost: not re-measured.** Last run quoted about 8.3K fixed tokens per call for Tenant. That figure is not
  recorded anywhere in the repo and could not be reproduced, so this lead is unconfirmed this run.

## Where Tenant trails

| Gap | Score then | Score now | Tracked as | Status in Jira |
|---|---|---|---|---|
| Running commands | 4.3 vs 9 | 6.3 vs 9 | TEN-488 | Done. Left over: PTY, a code runner, background jobs in the sandbox |
| Vision | 3.3 vs 8–9 | 4.7 vs 8.3–9 | TEN-493 | Done for what the model sees. Left over: clicking at coordinates, desktop control |
| Channels: Discord and iMessage only | 3.7 vs 9–10 | 3.7 vs 9–10 | TEN-494 | To do. Four tickets, none started |
| Install and upgrade | 4.3 vs 8+ | 4 vs 8+ | TEN-495 | In progress. All six tickets done; waiting on a public release |
| Skills ecosystem and learning loop | 5–5.7 vs 7–8.7 | 5–5.7 vs 7.7–8.7 | TEN-496 | To do. No tickets yet |
| Autonomy and delegation | 5.7–6 vs 8–9 | 5.7–6 vs 8–9 | TEN-497 | To do. Two fixes done, no new triggers or workspaces |
| Public repo, community, docs | 2 vs 9 | 3.3 vs 9 | TEN-442 / TEN-449 | To do. The docs site is live; the repo is not public |

The Docker sandbox (TEN-453) is in progress: 4 tickets done, 11 in progress, 9 to do.

**What the judges named as the highest-leverage next steps**

1. **Release it publicly** (judges B, C and D, and the referee). A public repo plus a non-pre-release that carries
   the install scripts. The referee expects this alone to lift install from 4 to 6 or 7, and it also unlocks outside
   security review and the docs and community score.
2. **Channels** (judges A, C and D). Telegram and Slack first. This score has not moved and is the widest gap.
3. **Skills ecosystem and learning loop** (judges A, C and D). Run bundled skill scripts, install from public hubs
   with a scan, and score skills by outcome.

Judge B's first pick was different: make confinement the default for unattended work, starting with Linux.

## The two refereed scores

| Score | A | B | C | D | Ruling |
|---|---|---|---|---|---|
| Install: Tenant | 6 | 5 | 5 | 3 | **4** |
| Engineering quality: Hermes | 4 | 4 | 5 | 7 | **5** |

- **Tenant install, 4.** The installer, service and updater are real and pass CI on three OSes, but no outsider can
  obtain them. The referee found that judges A and B had never charged the private repo to this score, last run or
  this one, so their starting point was too high. This is why the score reads as a small fall despite the new work.
- **Hermes engineering, 5.** Judge D gave credit for test volume and workflows; the outcomes (bugs up 40% in six
  weeks, a mis-built stable release) do not support a 7. Judges A and B held a 4 whose main reason, a red `main` that
  installs followed, no longer applies.

## Where the second model disagreed

Judge D ranked the three in the same order and agreed closely on security (6, 3, 4) and on Tenant's lead in
operations. It differed in three places:

- **Harder on Tenant where delivery is missing:** install 3 (judges 5.3) and the learning loop 4 (judges 5).
- **Kinder to the rivals on engineering:** OpenClaw 7 (judges 5.3) and Hermes 7 (judges 4.3).
- **Kinder to the rivals on the learning loop and delegation:** 9 for both on each (judges 7.7 and 8).

The net effect is a wider gap: Tenant 5.9 against 8.2 and 8.1, where the anchored judges have 6.2 against 7.5 and 7.2.

## Found along the way

These are not scores, but they came out of the checks and need attention.

- **The public install page does not work for anyone outside.** It prints one-liners that return 404, calls the
  project open source under MIT, and promises a signature that the release does not carry. Going public will not fix
  the one-liner by itself, because the release marked "Latest" (`v0.1.0`) has no install scripts.
- **Cron is not confined by default.** It runs on the host on Linux and Windows, and in a dry run on macOS. Last
  run's review said "cron stays confined", which was too generous.
- **Team runs have no default spending limit** since the run limits were turned off by default.
- **The release smoke job has never run.** It covers install, service and update, and is skipped for private repos.
- **Tenant's fixed token cost is unrecorded.** Measure it and keep the figure in the repo, so the next review can
  compare it.

**Corrections to the 2026-10-02 record** (the old files are left as they were)

- Tenant's MCP server could already expose every tool with `--tools`, not only two memory tools.
- CI runs `-race` on Linux only, not on all three OSes.
- Judge B's printed unweighted means for OpenClaw and Hermes were added wrongly (7.8 and 7.4, not 8.1 and 7.6). The
  weighted scores, which the summary used, were right.

## Next review

Re-run the same method against the same 14 capabilities. Keep judges A, B and C anchored to their previous
scorecards and keep Judge D unanchored, so the tracked figure and the second opinion both stay comparable. Add a new
dated folder next to this one and a row to [the index](../README.md).
