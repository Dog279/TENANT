# Judge B: Security and Reliability Engineer

**Review date:** 2026-11-13. Previous run: 2026-10-02.

**The question for this lens:** which of these would I trust on a home server, with access to email, files and a shell, running unattended for months?

**Method.** I read the rubric, my 2026-10-02 scorecard and all three new dossiers in full. I started every cell from my previous score and moved it only where the evidence changed. Tenant is scored as `main` at `8b9bbf7` (last run it was main plus unpushed local branches; those are now merged). I spot-checked the claims that decide my scores. The rival clones were read and searched only; nothing in them was run.

Spot-checks, all confirmed unless noted:

- **Tenant, exec default.** Interactive surfaces (TUI, dashboard, Discord, iMessage) resolve to the `host` backend unless the operator writes an `exec_profiles` entry. Source: `cmd/tenant/execprofiles.go:364-374`.
- **Tenant, cron default (corrects my last card).** The built-in backend is macOS Seatbelt in dry run, and the host on Linux and Windows. Dry run logs a refusal and re-runs on the host. So "cron stays confined", which I wrote last time, was too kind: by default cron is not confined on any OS. Source: `cmd/tenant/execprofiles.go:14-18, 38-45, 427-450`.
- **Tenant, cron approver.** Unattended exec jobs do not get a blanket yes. The approver denies the destructive category, denies any re-run outside a sandbox, and denies writes into Tenant's config and data folders. It approves the rest. Source: `cmd/tenant/cronmgr.go:287-322`.
- **Tenant, approvals.** Every category starts at `ask` on the host and Discord brokers, and at `deny` on the iMessage broker. Source: `internal/approval/broker.go:100-102, 124-126, 147-149`. `os_exec` is off unless a launch flag turns it on (`cmd/tenant/plugin_os.go:75`).
- **Tenant, read-only prompt skip.** Commands such as `ls`, `cat`, `env`, `printenv` and `git status` run with no prompt on the host and Discord. The code calls this a convenience, not a boundary. Source: `internal/plugins/osys/classify.go:80-142`; `cmd/tenant/approval.go:41-63`.
- **Tenant, SSE gateway.** It now needs a bearer token on every request, compared in constant time, and checks Host and Origin. Source: `cmd/tenant/ssegateway.go:11-59`. This closes the item I checked last time.
- **Tenant, run limits.** Token, cost, time and tool-call limits are off by default. Only machine guards remain (4 helpers, 4 browsers, 1,000 bus messages). Source: `internal/orchestra/budget.go:86-94`.
- **Tenant, background processes.** They run on the host only; a confined backend keeps kill-at-timeout. Source: `internal/plugins/osys/process.go:1-13`.
- **Tenant, update and service.** Both commands are registered. Source: `cmd/tenant/cliregistry.go:87, 116`. The live container tests are gated on an environment variable and do not run in CI (`internal/sandbox/container_live_test.go:43-44`).
- **OpenClaw, exec default.** `DEFAULT_SECURITY = "full"`, `DEFAULT_ASK = "off"`, ask fallback `deny`. Source: `src/infra/exec-approvals-config.ts:94-96`; `docs/tools/exec-approvals.md:277-290`.
- **OpenClaw, sandbox default.** Mode falls back to `"off"`. Source: `src/agents/sandbox/config.ts:220`; `docs/gateway/sandboxing.md:9`.
- **OpenClaw, self-learning.** On by default in `auto`. "Every change applies immediately and can be undone." Source: `docs/tools/self-learning.md:12-30`.
- **OpenClaw, update fixes.** The patch notes for 2026.10.6, 2026.10.9 and 2026.11.2 each describe fixes for problems that could stop installs, updates or starts. Source: `CHANGELOG/2026.10.6.md`, `2026.10.9.md`, `2026.11.2.md`.
- **Hermes, terminal default.** Backend is `local`. Source: `hermes_cli/config_defaults.py:285`; `cli-config.yaml.example:438`.
- **Hermes, approvals default.** Mode is `smart` (an auxiliary model auto-approves what it judges low risk). Cron, single-query and unattended platforms default to `deny`. Source: `hermes_cli/config_defaults.py:1636-1661`.
- **Hermes, plugins and skills.** `plugins.isolation` defaults to `in_process`. Skills the agent writes are not scanned by default. Source: `hermes_cli/config_defaults.py:1714, 1456`.
- **Hermes, stated boundary.** "The only security boundary against an adversarial LLM is the operating system." A sandboxed terminal backend does not confine code execution, MCP subprocesses, plugins, hooks or skills. Source: `SECURITY.md:58-119`.

## Scores

| # | Dimension | Tenant | OpenClaw | Hermes | Justification (decisive evidence) |
|---|---|---|---|---|---|
| 1 | Messaging channels and surfaces | 3 | 10 | 9 | **Tenant:** still Discord and iMessage only, plus TUI, web and CLI. Photos now reach the model and the dashboard chat streams, but there is no new channel and no voice. **OpenClaw:** 33 catalogued channels; X added. **Hermes:** 26 bundled adapters, voice, and a new browser Webapp. No cell moved. |
| 2 | Tool execution (shell, files, code, background processes) | 6 | 9 | 9 | **Tenant:** background processes with list, log, wait, kill and stdin, paged file reads, `workdir` and `timeout`, and atomic writes are all on main. Still no PTY, no code runner, and background jobs work on the host only. **OpenClaw and Hermes:** unchanged feature-dense tool sets with PTY and code runners. |
| 3 | Browser and computer use | 4 | 9 | 8 | **Tenant:** the model now sees screenshots and images on the Anthropic and OpenAI-compatible backends. Control is still by text or CSS locator, with no click at coordinates and no desktop control. **OpenClaw:** unchanged; fixes only. **Hermes:** password-manager sign-in and Bot Screen added, but the ten `browser_*` tools were removed in a breaking change. |
| 4 | Memory and personalization | 7 | 8 | 6 | **Tenant:** no material change; brute-force vector scan and one user's evidence still stand. **OpenClaw:** unchanged core; Knowledge plugin is main only. No new data-loss report, but failed updates remain the main risk to stored memory. **Hermes:** built-in memory still tiny; the session auto-prune default flipped on and off within three weeks. |
| 5 | Extensibility (skills, plugins, MCP) | 5 | 8 | 8 | **Tenant:** no change to skills and no hub. SQL tools now run on a read-only connection. **OpenClaw:** 186 bundled plugins; no new ClawHub malware campaign was found, but the history stands. **Hermes:** a catalog of about 2,300 SHA-pinned plugins launched; plugins still load in-process by default and the hub scan is still regex-only. |
| 6 | Autonomy and scheduling | 6 | 9 | 8 | **Tenant:** no new trigger type and no heartbeat. Shutdown now records a cut turn and catches up after boot. A daylight-saving bug that would have hung every daily job reached main before it was fixed. **OpenClaw:** event triggers from MCP servers added. **Hermes:** cron gained cancel, missed-run policy and self-healing re-runs; unattended sessions hard-stop tool loops by default. These harden what was there and add no new kind of autonomy. |
| 7 | Multi-agent and delegation | 5 | 8 | 8 | **Tenant:** `team_await` is now event-driven. Sub-agents still share the parent's exec surface with no workspace or sandbox of their own, and team runs lost their default spend limits. **OpenClaw:** new panels; a sandboxed child could launch a CLI model outside its sandbox until 10.8. **Hermes:** delegation is pluggable; sub-agent approvals still auto-deny. |
| 8 | Model flexibility and local LLMs | 8 | 9 | 8 | **Tenant:** multimodal input landed, with a launch-time vision probe for local models. That closes one of the two gaps I named. Also a GLM tool format and per-provider timeouts that feed failover. Hosted presets are still few. **OpenClaw:** about 76 provider plugins; local models still self-rated Alpha. **Hermes:** 39 providers and a managed llama.cpp runtime; the fixed prompt is lighter but still about 12K tokens by one outside measurement. |
| 9 | Security and safe defaults | **6** | **3** | **4** | See the security section below. No cell moved. |
| 10 | Observability and ops | 9 | 8 | 7 | **Tenant:** `tenant update` (verify, smoke-run, back up, swap, restart, confirm, roll back) and `tenant service` on three OSes close the self-update gap I named. The Security log now records each sandboxed run's boundary. The updater has not been run end to end against a published release, and there is still no OpenTelemetry. **OpenClaw:** strong tooling, but nine of fifteen stable-line releases describe fixes for blocked or failed updates. **Hermes:** stable update channel by default and `hermes prompt-size`; Langfuse left core and the desktop idle-burn tracker is still open. |
| 11 | Install, setup UX and cross-platform | 5 | 8 | 8 | **Tenant:** install scripts that verify checksums, a service installer and a packaging CI on three OSes now exist. The repo is still private, so the documented one-liners cannot work for anyone else, the release marked Latest has no scripts, and the release candidates are unsigned. **OpenClaw and Hermes:** one-line installers and apps as before. Hermes narrowed desktop platform support. |
| 12 | Engineering quality and reliability | 7 | 5 | 4 | **Tenant:** 4,488 passes and 0 failures on Windows, and the unpushed-branch problem is gone. Against that, five defects this period were found in the field and not by tests, `-race` runs on Linux only, and all reliability evidence still comes from one operator. **OpenClaw:** issue backlog down about 20%, but `main` is 17.5K commits ahead of stable, the "Alpha 68%" scorecard was not refreshed, and updates keep breaking. **Hermes:** a tested stable channel and a healthier CI sample, offset by open bugs up 40% (to about 11.1K), a release built from the wrong commit, and breaking changes in back-to-back releases. |
| 13 | Docs, community and ecosystem | 3 | 9 | 9 | **Tenant:** a public docs site with 43 user-facing pages is live. The repo is still private with one user, so there are still no outside eyes on its security, and the site calls the project open source and promises a signature that does not exist yet. **OpenClaw:** about 394K stars and a funded foundation. **Hermes:** about 263K stars and 435 doc pages. |
| 14 | Self-improvement loop | 5 | 7 | 7 | **Tenant:** no change; `internal/improve` is byte-identical to the last base. **OpenClaw:** Skill Workshop 2.0 saves the previous version before every learned-skill change and can undo it. That closes the rollback gap I marked as a risk. `auto` is still the default and changes still apply before a person sees them. **Hermes:** skill locks and an opt-in approval queue were added; defaults and the lack of an eval gate are unchanged. |

### Security and safe defaults (row 9)

**Tenant: 6** (was 6)

What runs out of the box:
- Shell exec is off until a launch flag enables it. Once on, every category (exec, write, destructive, web, send, egress, MCP, sandbox) prompts on local surfaces and Discord, and is denied on iMessage.
- Commands run on the host on every interactive surface, on every OS. Nothing selects a sandbox by default.
- Cron runs on the host on Linux and Windows. On macOS it runs under Seatbelt in dry run, which logs a refusal and then re-runs on the host. A filtered auto-approver stands in for the absent operator: it denies destructive actions, sandbox escapes and writes to Tenant's own config and data.
- Network surfaces bind to loopback. The SSE gateway now needs a bearer token.

Stronger since last run:
- The SSE gateway is authenticated. The dashboard refuses its cookie on cross-site requests and backs off after wrong keys. The TUI strips terminal escapes. SQL tools hold a read-only connection.
- The container sandbox finished its first hardening wave and passed 14 of 14 live cases on a real Docker engine. Approval is bound to a digest of the exact run. Each run leaves a boundary record in the hash-chained Security log.

Weaker, or weaker than I thought:
- The container sandbox is opt-in, has been run live on one Windows machine only, and its Linux and macOS work has not started. A home server is most likely Linux.
- Run limits are now off by default, so an unattended team run has no built-in spend ceiling.
- Read-only commands skip the prompt. That list includes `cat`, `env` and `printenv`.
- Cron is not confined by default on any OS. My last card said it was.
- Still no general spotlighting or delimiting of tool results, no peer rate limiting, and no outside audit. The release candidates are unsigned while the repo is private.

Why the score holds: the gains are real but sit on network surfaces and on an opt-in path. The defaults for what a command can touch did not get stricter, and two got looser. Prompting on everything by default is still the best starting posture of the three.

**OpenClaw: 3** (was 3)

What runs out of the box:
- The sandbox is off. Host exec runs with `security=full` and `ask=off`. The agent runs host shell commands with no prompt at all.
- Self-learning, dreaming and swarm are on by default.

Since last run:
- No default moved in either direction.
- The published advisory count is flat at 722 (263 high or critical). That is a publication gap, not a quiet period: releases 10.6 to 11.3 shipped fixes for sandbox escapes, allowlist bypasses and credential leaks with no advisories yet.
- New documentation says effective Full Access is enough to auto-apply system-agent changes "that can enable host code execution even when direct execution or editing tools are denied". The dossier did not test whether a plain install qualifies.
- New remote-reach surfaces (MCP Relay, public conversation links, OpenClaw ID) are all opt-in, but each widens what a mistake can expose.
- In its favour: the hardening toolkit is still the best of the three (Docker, Podman, SSH and OpenShell sandboxes, hash-bound allowlists, approval grants with expiry), and it removed a compromised third-party CA root within about five days.

**Hermes: 4** (was 4)

What runs out of the box:
- The terminal backend is `local`, on the host.
- Approvals are in `smart` mode: an auxiliary model decides what is low risk and approves it without asking. Cron, single-query and unattended platforms deny dangerous commands.
- Plugins load in-process. Code execution, MCP subprocesses, hooks and skills are outside any terminal sandbox, by the project's own policy.

Since last run:
- No default moved.
- One new High CVE (webhook route authorization bypass, fixed in v0.22.2), four dashboard-auth issues credited to Tenable, and a user report (#141346) of smart approval auto-approving commands that destroyed unsaved work.
- The outside posture review (#40889) was downgraded from P3 to P4 with no reply after five months.
- The CVE history is larger than I scored last time. I wrote "about 6 CVEs in 2026"; a third-party aggregator now lists 45, including a Critical supply-chain flaw in the MCP catalog. The third-party plugin surface an operator must review grew to about 2,300 entries.
- In its favour: real opt-in mitigations arrived (a bubblewrap backend, a plugin host process, deny globs that hold under YOLO, credentials stripped from child processes, hard stops for unattended tool loops), and the SECURITY.md is still candid about what is and is not a boundary.

Why the score holds: the new mitigations are opt-in or heuristic and the bad news is heavier than before, but Hermes still gates dangerous commands by default and denies them when nobody is there to answer. That keeps it a step above OpenClaw's no-prompt default.

## Changes from my 2026-10-02 scores

| # | Dimension | Product | Was | Now | Why |
|---|---|---|---|---|---|
| 2 | Tool execution | Tenant | 4 | 6 | Background processes, paged reads, `workdir` and `timeout` are on main. Three of the four gaps I named are closed. No PTY, no code runner, background on the host only. |
| 3 | Browser and computer use | Tenant | 3 | 4 | The model can now see screenshots. It still cannot click on what it sees, and there is no desktop control. |
| 8 | Model flexibility and local LLMs | Tenant | 7 | 8 | Multimodal input landed with a vision probe for local models. Few hosted presets remains. |
| 10 | Observability and ops | Tenant | 8 | 9 | Self-update with backup and rollback, and a service installer on three OSes. Not yet exercised against a published release; no OpenTelemetry. |
| 11 | Install, setup UX and cross-platform | Tenant | 4 | 5 | Install scripts, service install and packaging CI exist. The private repo means nobody else can use them, which caps the gain. |
| 13 | Docs, community and ecosystem | Tenant | 2 | 3 | A real public docs site. No public repo, no community, no outside scrutiny, and the site is ahead of what can be downloaded. |
| 14 | Self-improvement loop | OpenClaw | 6 | 7 | Learned-skill changes are now versioned and can be undone. |

**Hermes:** no cell moved. Its evidence changed in several rows (security, engineering, autonomy), but in each the gains and losses cancel at whole-number resolution.

**Cells I looked at hard and held:**
- Tenant row 9. Better network-surface hardening and a real opt-in container path, against unchanged host defaults, looser run limits and my corrected reading of cron.
- Tenant row 12. Branch sprawl is gone, but five field-found defects show the test suite's reach.
- OpenClaw rows 10 and 12. The update failures are more of the evidence I already scored, not a new kind.
- Hermes row 12. The stable channel is a structural gain; a 40% rise in open bugs offsets it.

**Correction to my previous card.** The unweighted means I printed for OpenClaw (8.1) and Hermes (7.6) were added wrongly. My 2026-10-02 scores give 7.79 and 7.36. The weighted overalls on that card (6.4, 6.1, 6.0) were right; I re-added them. The comparisons below use the corrected figures.

## Overall (weights for an unattended home server)

Weights, unchanged from 2026-10-02:

| Dimension | Weight |
|---|---|
| Security | 25% |
| Engineering and reliability | 20% |
| Observability and ops | 12% |
| Tool execution | 8% |
| Extensibility (supply chain) | 6% |
| Autonomy | 6% |
| Memory | 5% |
| Model flexibility | 5% |
| Install | 4% |
| Docs and community (patch velocity, outside scrutiny) | 3% |
| Channels | 2% |
| Browser | 2% |
| Multi-agent | 1% |
| Self-improvement | 1% |

Weighted sums (score × weight, in points out of 1,000):

| Dimension | Weight | Tenant | OpenClaw | Hermes |
|---|---|---|---|---|
| Security | 25 | 6 → 150 | 3 → 75 | 4 → 100 |
| Engineering and reliability | 20 | 7 → 140 | 5 → 100 | 4 → 80 |
| Observability and ops | 12 | 9 → 108 | 8 → 96 | 7 → 84 |
| Tool execution | 8 | 6 → 48 | 9 → 72 | 9 → 72 |
| Extensibility | 6 | 5 → 30 | 8 → 48 | 8 → 48 |
| Autonomy | 6 | 6 → 36 | 9 → 54 | 8 → 48 |
| Memory | 5 | 7 → 35 | 8 → 40 | 6 → 30 |
| Model flexibility | 5 | 8 → 40 | 9 → 45 | 8 → 40 |
| Install | 4 | 5 → 20 | 8 → 32 | 8 → 32 |
| Docs and community | 3 | 3 → 9 | 9 → 27 | 9 → 27 |
| Channels | 2 | 3 → 6 | 10 → 20 | 9 → 18 |
| Browser | 2 | 4 → 8 | 9 → 18 | 8 → 16 |
| Multi-agent | 1 | 5 → 5 | 8 → 8 | 8 → 8 |
| Self-improvement | 1 | 5 → 5 | 7 → 7 | 7 → 7 |
| **Total** | **100** | **640** | **642** | **610** |

| Product | Overall (weighted) | Last run | Unweighted mean | Last run |
|---|---|---|---|---|
| **OpenClaw** | **6.4** (6.42) | 6.4 (6.41) | 7.9 (7.86) | 7.8 (7.79, printed as 8.1) |
| **Tenant** | **6.4** (6.40) | 6.0 (5.98) | 5.6 (5.64) | 5.1 (5.14) |
| **Hermes** | **6.1** (6.10) | 6.1 (6.10) | 7.4 (7.36) | 7.4 (7.36, printed as 7.6) |

Unweighted totals: Tenant 79 of 140, OpenClaw 110, Hermes 103.

**Reading the result.** Under this lens Tenant and OpenClaw are now level. The 0.02 gap is far inside the noise of whole-number scoring, so I call it a tie, not a ranking. Tenant gained 0.4; neither rival moved by more than 0.01.

The tie has two very different shapes. OpenClaw gets there on capability and tooling, in spite of the worst defaults and track record of the three. Tenant gets there on defaults, engineering and operations, in spite of narrow reach and no outside scrutiny. On the unweighted mean Tenant is still more than two points behind.

Tenant's gain did not come from security. It came from closing exec gaps (16 points), ops (12), and four smaller rows (14 together). Row 9, the heaviest weight, did not move for anyone. That is the main finding of this run: all three kept their defaults exactly where they were.

Hermes stood still on the scorecard. It shipped a great deal, including a stable release channel and several opt-in sandboxes, but the bug backlog, the new CVE and the deprioritised posture review cancel that under this lens.

A caution on the Tenant number. Three of its six moved cells (rows 10, 11, 13) rest on things that exist but that nobody outside the author can use yet. The rubric counts them, with the delivery problem charged to install and docs. If the repo stays private, those gains will not grow.

## Where Tenant sits, and its 3 highest-leverage gaps

Tenant is still the one I would find easiest to reason about on a home server. It prompts for everything by default, tracks sensitive data across tools, keeps a tamper-evident log, has verified backups, and now has an updater with rollback and a service installer. In six weeks it closed the exec parity gap and the SSE gateway gap that I flagged last time, and merged everything that sat on local branches.

What it did not do is change what a command can touch by default. Every surface, cron included, runs on the host. The strong sandbox exists but is opt-in and has been proven on one Windows machine. And its security claims still rest on one author's tests.

The three highest-leverage gaps, in order of weight under this lens:

1. **Make confinement the default for unattended work, starting with Linux.** Cron on Linux and Windows runs on the host, and on macOS in dry run. Finish the Linux and macOS container work, run the live suite in CI, and select the container (or an enforcing profile) for cron automatically when an engine is present. Restore a default spend ceiling for unattended team runs, and add spotlighting of tool results in the main prompt. This is the only route to moving row 9, which carries a quarter of the score.
2. **Go public and get outside eyes.** Publish the repo and a non-pre-release with signed artifacts, so the install scripts, the updater and the docs site describe something people can obtain. Then invite or commission an outside security review. This moves rows 11 and 13 directly and is the precondition for trusting rows 9 and 12 beyond one operator's machines.
3. **Widen the reliability evidence for unattended runs.** Five defects this period were found in the field, including a scheduler hang that would have stopped every daily job. Add a heartbeat or watchdog for `serve`, run `-race` and the updater end to end in CI on more than one OS, and add soak tests for cron and the service path. PTY and background jobs inside a sandbox are the remaining exec gaps and belong here too.
