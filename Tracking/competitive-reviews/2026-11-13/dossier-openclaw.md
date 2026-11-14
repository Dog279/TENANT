# OpenClaw capability dossier (as of 2026-11-13)

Researcher stance: neutral. Primary evidence is a shallow clone of `openclaw/openclaw` `main` at commit `649ca5f87` (2026-11-14 02:37 +0530, i.e. 2026-11-13 21:07 UTC), the GitHub API, and the in-repo docs (`docs/`, mirrored at docs.openclaw.ai). Secondary sources: security press, CVE databases and user reports dated 2026-10-02 or later. Repo paths below are relative to the clone root. The baseline is the previous dossier (2026-10-02, v2026.9.7, commit `32e30e5`); where a before/after number is given, the "before" was re-read from the GitHub API at `32e30e5` unless marked otherwise.

**Important caveat on "maturity":** OpenClaw publishes its own maturity scorecard (`qa/maturity-scores.yaml`, rendered at `docs/maturity/scorecard.md`). It is self-assessed and every surface's run metadata still says `completed_at: 2026-09-19`, `by: codex`. It has **not been re-scored since the previous dossier**, so the self-rated numbers below are identical to last time and are now about eight weeks old. I quote them as "self-rated" and set my own call beside them.

**Important caveat on "main vs stable":** the clone is `main`. The latest stable tag, v2026.11.3, is on a release branch that has diverged: the GitHub compare API reports `main` **17,523 commits ahead and 1,559 behind** v2026.11.3. Several things visible in the clone are not in any stable release yet. Where that matters I say "main only" or "stable".

## Snapshot facts (dated)

| Fact | Value | Source / date |
|---|---|---|
| Latest stable release | **v2026.11.3**, published 2026-11-10 (a patch release, "no new configuration options or features"). Latest feature release: **v2026.10.10**, 2026-11-03. Latest prerelease: v2026.11.5-beta.2, 2026-11-12. | GitHub releases API [as of 2026-11-13] |
| Latest extended-stable ("LTS-equivalent") | **v2026.9.37**, 2026-11-10 (end-of-September code plus security and reliability fixes). The channel moved from the 2026.8.x line to 2026.9.x on 2026-10-15 (v2026.9.33). The 9.37 notes say it was "published from a retired maintenance line by explicit maintainer decision". | release notes |
| Versioning | CalVer `YYYY.M.N`; channels: stable (`latest`), extended-stable, beta, dev. Unchanged. | `docs/install/development-channels.md` |
| Release scale | 2026.10.5: "2,250 pull requests, 411 direct commits, and 637 contributors". 2026.10.10: "3,028 source PRs · 1,064 direct commits · 454 contributors". 2026.11.3: 56 PRs, 44 direct commits, 36 contributors. | `CHANGELOG/2026.10.5.md`, `2026.10.10.md`, `2026.11.3.md` |
| Release count since last run | 33 GitHub releases between 2026-10-03 and 2026-11-12: 15 on the stable line (9.8, 9.9, 10.1–10.10, 11.1–11.3), 5 extended-stable (9.33–9.37), 13 betas. | GitHub releases API |
| Stars / forks | 394,394 / 82,888 (was ~391k / ~82k) | GitHub API [as of 2026-11-13] |
| Open issues / open PRs | 4,758 / 3,371 (API `open_issues_count` 8,129 = both combined). Was ~5,918 / ~3,255 (9,173). | GitHub search API [as of 2026-11-13] |
| Issue and PR flow since 2026-10-02 | Issues: 7,074 opened, 8,221 closed. PRs: 28,249 opened, 23,041 merged. | GitHub search API [as of 2026-11-13] |
| Contributors | ~3,797 (anon-inclusive, from API pagination; was ~3,470) | GitHub API [as of 2026-11-13] |
| Repo created | 2025-11-24 (unchanged) | GitHub API |
| License | MIT (API now reports `MIT`; last run it showed NOASSERTION) | `LICENSE`, `package.json`, GitHub API |
| Language | TypeScript on Node 24.16+/26.1+ (pnpm monorepo); Node 24.22.1+/26.12.1+ now "strongly recommended" (see §9). Swift (1,794 files), Kotlin (655 files), Rust (94 files in 4 crates: the two from last run plus `openclaw-iroh-ffi` and `openclaw-iroh-node`). | clone; `SECURITY.md`; `crates/` |
| Size | ~64.8k tracked files (was ~51.7k), ~877 MB working tree; ~28.6k non-test TS/TSX files and ~5.14M lines across `src/ extensions/ packages/ ui/` (was ~22.8k / ~4.3M); ~22.0k `*.test.ts` files (was ~18.9k). Counted with `git ls-files`; the previous run's exact exclusions were not recorded, so treat the deltas as approximate. | clone counts |
| Bundled extensions | **186** plugin manifests under `extensions/` on `main` (was 163). The stable tag v2026.11.3 has 171 extension directories (165 at the previous commit). | clone; GitHub contents API |
| Database | SQLite by default. `package.json` now declares `databaseEngines: ["sqlite","postgres"]`. Schema versions: state 40 / agent 42 on `main`, state 22 / agent 27 at v2026.11.3, state 20 / agent 24 at the previous commit. | `package.json` at each ref |
| Governance | Unchanged: OpenClaw Foundation, a US 501(c)(3). New adjacent project: **OpenClaw Enterprise** (`openclaw/openclaw-enterprise`, 467 stars), an agent control plane announced 2026-09-29, started at OpenAI and donated to the Foundation, with Red Hat and NVIDIA involved. It was announced before the last run but not covered there. | `README.md`; The Register 2026-09-30; GitHub API |
| History | Unchanged from the previous dossier. | previous dossier |

---

## What changed since 2026-10-02

One row per capability section. "Before" is the 2026-10-02 dossier or the repo at `32e30e5`.

| # | Section | Change | Evidence |
|---|---|---|---|
| 1 | Messaging channels | One new channel: **X / Twitter** (mentions and allowlisted public replies; no DMs). Channel catalog 32 → 33 entries; channel-declaring plugin manifests 27 → 28; `docs/channels/` top-level entries 55 → 56. Phone-call tasks can now carry a brief and return an outcome report. Self-rated channel maturity unchanged (not re-scored). | `docs/channels/x.md`, `docs/channels/index.md` (generated catalog), `extensions/x`; `CHANGELOG/2026.10.5.md` "Channels and calls" |
| 2 | Tool execution | No default change. New optional tools: `deep_research` (main only, default-enabled plugin with a lazy tool) and `autoreview` (main only, opt-in). Code Mode `store`/`load` values persist across restarts (10.1). Exec gained a `durable` background option on Linux/systemd. Several restriction-bypass fixes (see §9). | `docs/tools/deep-research.md`, `docs/tools/autoreview.md`, `docs/tools/exec.md:86`, `CHANGELOG/2026.10.1.md` |
| 3 | Browser and computer use | No material capability change found. Reliability fixes only (Chrome extension uploads under 47 MiB without local-file permission, tab cleanup scoped per agent, lazy desktop-helper start, Code Mode can read browser observations). One relevant fix: agents could bypass a denied browser tool through the paired node's browser proxy (#179880, fixed in 10.8). | `CHANGELOG/2026.10.5.md` "Browser and computer use"; `CHANGELOG/2026.10.8.md` |
| 4 | Memory | Dreaming still **on by default**. New **Knowledge** plugin (main only, off until selected as the memory provider): an evidence-backed vault with a Control UI explorer, 13 doc pages. Memory search fixes (accented and Korean text, embedding fallbacks). Self-rated maturity unchanged. | `src/memory-host-sdk/dreaming.ts:281`; `docs/plugins/knowledge.md`; `CHANGELOG/2026.10.5.md` "Memory and learning" |
| 5 | Extensibility | Bundled plugin manifests **163 → 186** on `main` (171 directories in stable). Bundled skills 50 → 52. New: **MCP Relay** (connect ChatGPT/Claude to a Gateway through `mcp.openclaw.ai`, off by default), **MCP Events**, MCP Apps can call tools while open. ClawHub: **no new malware campaign found** in sources dated after 2026-10-02; a ClawHub archive-download SSRF/redirect fix landed in 11.1. Plugin SDK churn continues: 22 deprecations listed in 10.1, removal deadlines of 2027-01-12 and 2027-01-15 announced in 10.5. | manifests in clone; `docs/plugins/mcp-relay.md`; `docs/automation/mcp-events.md`; `CHANGELOG/2026.10.1.md` "Upcoming deprecations"; `CHANGELOG/2026.11.1.md` |
| 6 | Autonomy and scheduling | **Event-triggered automations** from MCP servers (10.10, opt-in, needs a public HTTPS callback). **Heartbeat is being retired as a separate mechanism**: on `main` (PR #179387, 2026-10-21) Doctor converts heartbeat monitors into ordinary automation jobs. This is **not in v2026.11.3**, where heartbeat is still a system-owned automation. New experimental **Advisor** (off by default) reviews running work. | `docs/automation/mcp-events.md`; `docs/gateway/heartbeat.md` on `main` vs at tag v2026.11.3; `extensions/advisor/openclaw.plugin.json:11` |
| 7 | Multi-agent | Control UI gained Subagents and Processes panels (10.5) and swarm-worker transcripts (10.10). Sessions can run in a folder on a paired computer (10.10). "Experimental fleet management" was **removed** (PR #165825, 2026-10-08) and the team docs were rewritten as "multiplayer" (`start/teams.md`, `gateway/team-server.md`, `gateway/multi-tenant-hosting.md`, `cli/fleet.md`, `cli/claws.md` removed; `start/multiplayer.md` added). Security fix: sandboxed child agents could launch a CLI model outside their sandbox (#178742, fixed in 10.8). | `CHANGELOG/2026.10.5.md`, `2026.10.10.md`, `2026.10.8.md`; docs tree diff |
| 8 | Model flexibility | Provider plugin manifests **~65 → 76** on `main` by a same-method recount (the "before" and a stable figure of 66 are derived by subtracting the newly added plugin directories; the previous dossier said "about 60"). New models: GPT-6.1 Sol in the Codex list, Claude Haiku 5.5, Gemini 4 Argon, Gemini 3.8 Flash. New harness: Muse Code CLI for Meta models. New "Decision model" provider class (main only). New local runtimes: oMLX, Liquid AI (main only). | manifest count script; `CHANGELOG/2026.9.9.md`, `2026.10.1.md`, `2026.10.5.md`; `docs/plugins/muse.md` |
| 9 | Security | **Defaults unchanged**: sandbox `off`, `exec.security=full`, `exec.ask=off`, `askFallback=deny`. **Published GHSAs: 722 → 722** (no new batch; last batch 2026-09-11). GitHub's global advisory database gained 1 reviewed entry (CVE-2026-62197, a June-era bug). Releases 10.6–11.3 shipped many security fixes without advisories yet. Response to the third-party **eMudhra CA key compromise** within about five days. New doc text on delegated self-administration (`tools.openclaw.autoApproveChanges`). New exposure surfaces, all opt-in: public conversation links, MCP Relay, OpenClaw ID remote access (main only). | `src/infra/exec-approvals-config.ts:94-96`; `src/agents/sandbox/config.ts:220`; `docs/tools/exec-approvals.md:277-286`; GitHub advisories API; `CHANGELOG/2026.10.6.md`–`2026.11.3.md` |
| 10 | Observability and ops | Control UI: new sidebar (10.5), Chat Details panel (10.10). **PostgreSQL** is a supported alternative store on `main`. Update recovery was the dominant theme of the period: 2026.9.8, 9.9, 10.2, 10.3, 10.4, 10.6, 10.7, 10.9, 11.2 (nine of fifteen stable-line releases) and 11.5-beta.2 all describe fixes for blocked, stuck or failed updates. | `CHANGELOG/2026.10.*.md` intros; `docs/gateway/postgresql.md` |
| 11 | Install and platforms | New standalone ("fat binary") Bun build path and an experimental AOT build (main only). New Jetson guide. Windows-specific startup and path fixes in 10.6, 11.2 and 9.37. Self-rated platform maturity unchanged (Windows app still q19/c21). | `docs/install/standalone.md`, `docs/install/jetson.md`; `CHANGELOG/2026.10.6.md` |
| 12 | Engineering quality | Workflows **111 → 129** on `main` (117 in stable). Non-test TS lines ~4.3M → ~5.14M. Open issues 5,918 → 4,758; open PRs 3,255 → 3,371. Schema versions doubled on `main` (state 20 → 40). Self-rated overall maturity unchanged at **68% "Alpha"** (not re-scored). | clone counts; GitHub API; `qa/maturity-scores.yaml` |
| 13 | Docs and community | Docs tree 1,406 → 1,490 files (101 added outside release notes, 32 removed). Official support moved from Discord to community.openclaw.ai. Stars +3.4k, contributors +~330. VISION.md now lists "reliable updates" as a named priority. | docs tree diff; `README.md`; `VISION.md` |
| 14 | Self-improvement | **Skill Workshop 2.0** (10.5): every learned-skill change saves the previous version and can be undone. This closes the gap noted last time (auto mode had no rollback snapshots). Default is still `auto`. Proposals were replaced by drafts in `propose` mode. On `main` it moved into its own `skill-workshop` plugin and gained a daily "skill diet". | `docs/tools/skill-workshop.md` (Undo, Drafts); `docs/tools/self-learning.md:11-13`; same file at tag v2026.11.3 |

Counts that did not change and were re-verified: exec and sandbox defaults, 722 GHSAs with the same severity split, 50 scored surfaces with identical scores, MIT license, no paid bug bounty (`SECURITY.md:34`).

---

## 1. Messaging channels / surfaces

- **What exists:** the generated channel catalog lists 33 channels (`docs/channels/index.md`): A2A, Buzz, ClickClack, Discord, Feishu, Google Chat, iMessage, IRC, LINE, Matrix, Mattermost, MS Teams, Nextcloud Talk, Nostr, QQ, Raft, Reef, Signal, Slack, SMS, Synology Chat, Telegram, Tlon, Twitch, WebChat, WhatsApp, **X / Twitter (new)**, Zalo and Zalo personal, plus five external plugins (WeChat, WeCom, Yuanbao, Zalo ClawBot). 28 bundled plugin manifests declare a channel. Voice-call is listed as a related plugin. Shared machinery is unchanged: broadcast groups, group policy, pairing of unknown DM senders, bot-loop protection, access groups.
- **New since last run:** the X plugin answers mentions from allowlisted authors with public replies; DMs, original posts, likes and follows are not supported (`docs/channels/x.md`). Phone-call tasks accept a brief, mid-call instructions and return an outcome report (10.5). Split speech mode for browser Talk (10.10).
- **Evidence:** `docs/channels/*.md`, `extensions/*/openclaw.plugin.json` (`channels` field).
- **Maturity (self-rated, still dated 2026-09-19):** Discord **Stable** (q73/c87). Telegram, WhatsApp, Slack and iMessage **Beta** (q66–68). Signal, Google Chat, Matrix and Teams **Alpha** (q59–60). Community and regional cohorts **Alpha** (q53–55). Voice-call **Experimental** (q41). X is not scored.
- **My call:** unchanged. **Polished** for the top 5–6 channels, **works/prototype** for the long tail. Breadth is still unmatched by any other OSS assistant.
- **Gaps:** Quality still drops off after the big five. The period's release notes carry a steady stream of delivery fixes (missing or repeated replies, wrong-conversation follow-ups, Telegram queue import skipping entries older than 24 hours). The previous reviewer claim that some features were web-app-only was not re-tested.

## 2. Tool execution: shell/exec, files, code exec, background processes

- **What exists:** unchanged core.
  - `exec` tool: `workdir`, `env`, `timeoutSeconds` (default 1800), `pty`, `yieldMs` (default 10000), `background`, `host` (auto/sandbox/gateway/node), `elevated`, and now `durable` (keeps a background command running across Gateway restarts; Linux with systemd 250+ only).
  - `process` tool for background sessions, with completion wake.
  - File tools: `read`, `write`, `edit`, `apply_patch` (multi-file patches now pre-check mismatches before writing any file).
  - Code Mode (still "experimental"): `store`/`load` values now persist across cells, replies and Gateway restarts (10.1).
  - `node_exec` on paired devices, cloud workers, and (10.10) sessions that work directly in a folder on a paired computer.
  - New on `main`: `deep_research` (portable engine or Google's managed service) and opt-in `autoreview`.
- **Evidence:** `docs/tools/exec.md`, `docs/gateway/background-process.md`, `docs/tools/code-mode.md:12`, `docs/tools/deep-research.md`, `docs/tools/autoreview.md`.
- **Maturity:** self-rated "Browser automation, exec, and sandbox tools" **Beta** (q75/c79), unchanged. My call: **polished** and very feature-dense.
- **Gaps:** Defaults are still permissive (see §9). Host `apply_patch` "permits paths outside the workspace by default" (`docs/gateway/permission-modes.md`). The "two features called Code Mode" confusion was not re-checked.

## 3. Browser automation / computer use

- **What exists:** unchanged in shape.
  - **Browser:** agent-dedicated Chromium-family profile, a `user` profile that attaches to the real signed-in Chrome, remote CDP, a sandboxed browser in Docker, SSRF policy, and a Chrome extension.
  - **Computer use:** one `computer` tool for the Gateway desktop or paired nodes (Peekaboo or CUA on macOS, bundled `cua-computer` elsewhere, Crabbox cloud desktops).
  - Codex computer-use plugin.
- **New since last run:** fixes only. Chrome extension uploads under 47 MiB no longer need local-file read permission; cleanup leaves other agents' tabs alone; desktop helpers start lazily; headless pages can dismiss invisible passkey prompts; Code Mode can consume browser observations.
- **Evidence:** `docs/tools/browser*.md`, `docs/nodes/computer-use.md`, `extensions/{browser,cua-computer,crabbox}`, `CHANGELOG/2026.10.5.md` lines 1161–1204.
- **Maturity:** browser **works → polished**; computer use **works**. No change.
- **Gaps:** The real-browser attach is still a large blast radius. Two policy bypasses were fixed in the period: a denied browser tool reachable through the node's browser proxy (#179880), and Discord attachment downloads inheriting broad private-network permission (#191458). CVE-2026-62197 (CDP discovery accepting blocked WebSocket URLs) was reviewed into GitHub's database on 2026-10-21 but affects versions before 2026.6.6.

## 4. Memory & personalization

- **What exists:** unchanged core.
  - Markdown memory in the workspace (`USER.md`, `MEMORY.md`, daily notes, `DREAMS.md`, `SOUL.md`, `AGENTS.md`).
  - Memory search backends (builtin, LanceDB, Honcho, memory-wiki).
  - **Active Memory:** default mode `escalate`, which runs the blocking recall sub-agent only when rule-based recall finds no strong match. Cross-conversation recall "defaults on for personal installs" (`docs/concepts/active-memory/enabling.md`).
  - **Dreaming:** still **on by default** (`parseBoolean(dreaming?.enabled) ?? true`, `src/memory-host-sdk/dreaming.ts:281`; `docs/concepts/dreaming.md:14`).
  - Standing intents, session search, compaction, provenance, per-person `USER.md`.
- **New since last run:**
  - **Knowledge plugin** (main only): agents save pages, claims and typed relationships as Markdown with retained source evidence; read-only explorer in the Control UI; off until chosen as the memory provider (`docs/plugins/knowledge.md` and 12 sub-pages).
  - Incognito sessions are excluded from self-learning; ChatGPT transcript import exists (mentioned in release notes).
  - Memory search fixes for accented and Korean text, and keyword fallback when local embeddings fail (10.5).
- **Maturity:** self-rated "Session, memory, and state lifecycle" **Beta** (q75/c76), unchanged. My call: **polished in design**.
- **Gaps:** Memory is still model-written Markdown. Dreaming still spends tokens in the background by default. Three overlapping memory plugins (memory-core, memory-wiki, Knowledge) now compete for one slot. No new data-loss report was found for this period, but update failures (§10) remain the main risk to stored memory; 10.3 specifically "preserves saved sign-ins and memory files during affected Doctor repairs".

## 5. Extensibility: skills, plugins, MCP, registry

- **What exists:**
  - **Skills:** `SKILL.md` directories; **52** bundled under `skills/` on `main` (50 before and in stable; `create-template` and `creative-project` added).
  - **Plugins:** typed SDK; **186** bundled manifests on `main` (163 before). By manifest category: 68 models, 25 channels, 16 voice, 10 each for web, infrastructure and developer tools, 8 security, 7 each for media and agent orchestration, 6 memory, 5 agent runtimes. 94 are `enabledByDefault` (mostly model providers).
  - **MCP client and server:** as before, plus MCP Apps that can call their tools while their view is open (10.1).
  - **MCP Relay (new, 10.10):** an outbound WebSocket to `mcp.openclaw.ai` lets ChatGPT, Codex or Claude reach the Gateway without an open port; disabled by default; per-app grants; "the relay can read content passing through it".
  - **MCP Events (new, 10.10):** see §6.
  - **ACP** and **ClawHub** (`openclaw/clawhub`, 9,555 stars) as before. ClawHub's README now also describes experimental "whole-agent Template packages".
- **Evidence:** `extensions/*/openclaw.plugin.json`, `docs/plugins/mcp-relay.md`, `docs/tools/skills.md:396-407`, GitHub API.
- **Maturity:** self-rated Plugins **Beta** (q72/c79); ClawHub **Alpha** (q58/c62). Unchanged. My call: plugins and MCP **polished**; the registry **works** with a bad security history.
- **Gaps and incidents:**
  - **ClawHub malware status:** I found no report of a new malicious-skill campaign dated after 2026-10-02. Pages that surface with October 2026 dates are re-surfaced February coverage (for example a GBHackers article with a site-header date that is actually dated 2026-02-18 and cites 1,184 historical malicious skills, attributed to Antiy CERT). A Cloud Security Alliance note (2026-07) says four research efforts documented separate waves since February. So the history stands and nothing new was verified.
  - Scanning: skill pages show VirusTotal, ClawScan and static-analysis state before install, and `openclaw skills verify` exits non-zero on a failed verification (`docs/tools/skills.md`). ClawScan is now its own repo (`openclaw/clawscan`).
  - 11.1 blocks ClawHub archive downloads and redirects to private origins (#190939).
  - SECURITY.md still excludes "a malicious plugin after a trusted operator installs or enables it" (`SECURITY.md:55`).
  - **SDK churn:** 10.1 lists 22 deprecations, most of which "reached" their removal target between 2026-09-08 and 2026-10-03 but remain "removal-pending". 10.5 sets hard dates of 2027-01-12 and 2027-01-15 for two more.
  - The current total number of ClawHub skills was **not verified** (the landing page shows only featured items).

## 6. Autonomy & scheduling

- **What exists:**
  - **Automations** (`openclaw automations`, alias `cron`): one-shot and recurring jobs, webhooks, Gmail PubSub and IMAP triggers, delivery to a channel or webhook. Webhooks can now reject work when capacity is unavailable.
  - **Event-triggered automations (new, 10.10):** a job can start when a connected MCP server reports an event. Opt-in `mcp-events` plugin; only the OpenAI webhook profile is supported; needs a public HTTPS callback route.
  - **Heartbeat:** in stable v2026.11.3 it is still a system-owned automation running periodic main-session turns. On `main` it has been **retired into ordinary automation jobs** that Doctor creates from the old config and `HEARTBEAT.md` (PR #179387, 2026-10-21; not referenced in any numbered changelog through 2026.11.3). The migration notes say heartbeat-specific duplicate suppression and no-route skipping are removed.
  - **Advisor (new, 10.10, experimental, off by default):** reviews an agent's ongoing work and can suggest one short correction; uses extra model calls.
  - Standing orders, Goals, exact-operation approval grants (now with optional `tools.exec.grantExpiryDays`), hooks. Unchanged.
- **Evidence:** `docs/automation/mcp-events.md`, `docs/gateway/heartbeat.md` (main and at tag), `extensions/advisor/openclaw.plugin.json`, `docs/gateway/config-tools/built-in-tools.md:36`.
- **Maturity:** self-rated "Automation and durable work" **Beta** (q72/c79), unchanged. My call: **polished**.
- **Gaps:** No new cost anecdote was found for this period; the heartbeat cost complaints in the previous dossier date from February to April 2026. A reported bug had queued cron work starving heartbeat ticks for all agents (#167844, closed). 11.2 fixes "scheduled reminders missing"; 11.3 fixes queued cron storage failures exiting the Gateway. If the heartbeat retirement ships, it will be a doctor-driven migration with a schema bump that blocks downgrade without a backup.

## 7. Multi-agent / subagents

- **What exists:** unchanged core (isolated agents per Gateway, bindings, sub-agents, Swarm on by default, ACP harnesses, A2A and Reef channels).
- **New since last run:**
  - Control UI **Subagents** and **Processes** panels beside the parent chat, with targeted Stop (10.5); swarm workers and their transcripts visible (10.10).
  - **Device-folder sessions** (10.10): the Gateway on a server can work in a folder on a paired laptop, governed by `nodeHost.workerRuns.folders`.
  - "Multiplayer claw" is the new name for a shared team Gateway (`docs/start/multiplayer.md`); the project says it runs its own development on one (team.openclaw.ai).
  - Experimental fleet management was removed from the main repo (PR #165825). Enterprise-style multi-tenancy now appears to live in the separate OpenClaw Enterprise project (my inference from timing, not stated in the repo).
- **Evidence:** `docs/concepts/multi-agent.md`, `docs/tools/{subagents,swarm}.md`, `docs/tools/swarm.md:13` ("enabled by default"), `CHANGELOG/2026.10.5.md`, `2026.10.10.md`.
- **Maturity:** **works → polished**. No change.
- **Gaps:** Fixes in the period show where it still breaks: private subagent completions leaking into streamed replies (11.3), cancelled children blocking handoff (11.3), sandboxed child agents launching a host CLI model (#178742), child approvals missing for parents on custom stores (11.1). VISION.md states "a shared Gateway is for people who trust each other".

## 8. Model flexibility

- **What exists:**
  - **76** plugin manifests declare at least one model provider on `main` (96 provider ids); 11 of them are new since the last commit, so about 65 by the same method then, and 66 at the stable tag. `docs/providers/` has 104 pages (89 before).
  - New providers on `main`: Abliteration, Bifrost, Celeris, Liquid AI, Nace, oMLX, Particle, Requesty, plus "Decision" providers (Fastino, Levanto, Rerank) that return typed yes/no, choice or score results and are not chat models. `docs/providers/` also adds Google Vertex, Neon and Poolside pages.
  - New models named in release notes: GPT-6.1 Sol (Codex list), Claude Haiku 5.5, Gemini 4 Argon, Gemini 3.8 Flash.
  - New harness: Muse Code CLI for `meta/*` models (10.x). Codex app-server and Claude CLI harnesses remain.
  - A curated global "recommended models" list (`docs/concepts/recommended-models.md`).
  - Failover, per-agent overrides and `clawrouter` unchanged.
- **Evidence:** manifest scan of `extensions/*/openclaw.plugin.json` (`providers` field), `docs/providers/*`, `CHANGELOG/2026.9.9.md`, `2026.10.5.md` "Models and local runtimes".
- **Maturity:** self-rated OpenAI **Beta** (q74), Anthropic **Beta** (q71), local models **Alpha** (q61/c68), unchanged. My call: **polished** for hosted providers and **works** for local.
- **Gaps:** The OpenAI lean is unchanged and arguably stronger (MCP Events implements only the OpenAI webhook profile; MCP Relay targets ChatGPT first). Setup can silently change defaults: "Rerunning Google API-key setup chooses Argon as the primary model" (10.5). A reported bug shipped a Codex version that could not serve the advertised GPT-6.1 Sol (#164095, closed).

## 9. Security

- **What exists:** the defense set from the last run is intact (sandbox backends Docker/Podman/SSH/OpenShell/Crabbox; exec approval modes `deny`/`allowlist`/`ask`/`auto`/`full`; session permission modes `read-only`/`guarded`/`workspace`/`full`; DM pairing; loopback bind with auth; secrets egress proxy; `openclaw security audit`; threat model; CodeQL, opengrep and dependency audit in CI).
- **Critical defaults (re-verified in code and docs, 2026-11-13):**
  - `agents.defaults.sandbox.mode` defaults to **`off`**: `mode: agentSandbox?.mode ?? globalSandbox?.mode ?? "off"` (`src/agents/sandbox/config.ts:220`); "Sandboxing is off by default" (`docs/gateway/sandboxing.md:9`).
  - Host `exec.security` defaults to **`full`** and `exec.ask` to **`off`**: `DEFAULT_SECURITY = "full"`, `DEFAULT_ASK = "off"`, `DEFAULT_EXEC_APPROVAL_ASK_FALLBACK = "deny"` (`src/infra/exec-approvals-config.ts:94-96`); `docs/tools/exec-approvals.md:277` and `:286`. The same text is in the docs at tag v2026.11.3.
  - README still says: "Tools run on the host for the main session unless you configure sandboxing" (`README.md:81`).
  - The docs recommend `tools.exec.mode auto` for coding agents (`docs/tools/permission-modes.md`), but it is a recommendation, not the default.
  - **No default moved** in either direction since the last run.
- **New in the trust model:**
  - Delegated self-administration: explicit Full Access or `tools.openclaw.autoApproveChanges: true` "grants standing authorization for permitted system-agent changes, including changes that can enable host code execution even when direct execution or editing tools are denied" (`SECURITY.md:247-256`). When the key is omitted, the docs say "effective Full Access" is enough to auto-apply (`docs/gateway/config-tools/built-in-tools.md:39-48`). My reading is that a plain install with the exec defaults above qualifies, but I did not test it. Setting the key to `false` forces human approval.
  - New opt-in exposure surfaces: public conversation links ("exposes existing and future user messages and final replies to anyone who has it", 10.5), MCP Relay (10.10), MCP Events callback (10.10), and OpenClaw ID remote access over an iroh tunnel (main only, experimental Lab; the `openclaw-id-access` plugin manifest is `enabledByDefault`, but remote access itself must be turned on).
  - VISION.md on `main` adds default-on "bounded terminal update-outcome reports" next to the daily update check; both are disabled by `update.checkOnStart: false`. Not in v2026.11.3.
- **Incidents and CVEs:**
  - **722 published GitHub Security Advisories**, the same as last run (14 critical, 249 high, 390 medium, 69 low). Counted with `gh api "repos/openclaw/openclaw/security-advisories?state=published&per_page=100" --paginate` and tallied by `severity`. 39 carry a CVE id.
  - Publication is in batches, which the last dossier's "45–75 per month" wording hid. By publish month: Jan 3, Feb 220, Mar 204, Apr 108, May 67 (all on 05-28), Jun 45 (all on 06-30), Jul 0, Aug 0, Sep 75 (all on 09-11), Oct 0, Nov 0. **A flat count therefore means "no batch since 2026-09-11", not "no vulnerabilities".**
  - GitHub's global advisory database lists 762 reviewed advisories for npm `openclaw` (26 critical, 266 high, 394 medium, 76 low; `gh api "advisories?ecosystem=npm&affects=openclaw"`). One was published after the last run: GHSA-3x84-qq85-fj65 / CVE-2026-62197 (browser CDP discovery accepted blocked WebSocket URLs; fixed in 2026.6.6; CVE originally published 2026-07-13). The two counts use different severity assignments and should not be added together.
  - Security fixes shipped in the period without an advisory yet (from release notes): sandboxed child agents launching a CLI model outside the sandbox (#178742); browser-tool denial bypass through the node proxy (#179880); POSIX allowlists auto-trusting `test -v` (#180880); IMAP sender check spoofable through nested From comments (#187302); workspace-only limits not applied to Gateway-built delegation and media tools (#187276); workspace-controlled mise/XDG paths added to child `PATH` (#191102); warm CLI turns retaining MCP tool grants (#191980); several credential and transcript leaks into logs and error messages (10.8, 11.2, 11.3).
  - **eMudhra CA incident (third party):** a researcher reported on 2026-10-17 that private keys of eight eMudhra intermediate CAs were recoverable; browsers removed the emSign ECC Root CA - G3 (press reports, 2026-10-20 onward). OpenClaw 2026.10.6 (2026-10-22) raised the recommended Node to 24.22.1/26.12.1, and 10.8 and 11.1 removed the root from the secret egress proxy's trust bundle. This is a fast response to someone else's incident, not an OpenClaw flaw.
  - **Exposure and exploitation:** no new measurement dated after 2026-10-02 was found. Older figures not in the last dossier: Cyera (May 2026) reported about 65,000 (Shodan) and 180,000 (ZoomEye) reachable instances and a "Claw Chain" of four flaws (e.g. CVE-2026-44112, CVE-2026-44113). Secondary, not verified.
  - **Outside assessments** quoted by The Register (2026-09-30): Gartner called OpenClaw an "unacceptable cybersecurity risk" for business users, and China's CERT warned of weak default configurations. Both predate this period.
  - ClawHub malware: see §5.
- **Maturity:** self-rated **Beta** (q72/c79), unchanged. My call: unchanged. The defenses are **extensive and well documented**, and the fix rate is high, but defaults still favor capability over safety and the track record is still the worst of any comparable project. The quiet advisory count this period is a publication artifact.

## 10. Observability & ops

- **What exists:**
  - **Control UI:** new sidebar with agent search and pinning, Subagents and Processes panels (10.5); **Chat Details** panel collecting session info, progress, pull requests and automations (10.10); retry/fallback progress and failure notices with recovery actions; YouTube and local video playback inline.
  - `openclaw doctor`, `openclaw gateway status`, `openclaw mcp doctor`; logging, diagnostics, OpenTelemetry and Prometheus plugins; usage tracking (silent replies and quiet heartbeats now counted in cost monitoring); trajectories; audit log.
  - **PostgreSQL (main only as a documented, supported engine):** five store families (`state`, `agent`, `workboard`, `logbook`, `team-reports`) can live in PostgreSQL 16–18; SQLite stays the default; daily CI on PG 17 and weekly on 16 and 18 (`docs/gateway/postgresql.md`, `docs/reference/database-engines.md`, `.github/workflows/postgres-conformance-weekly.yml`). The stable `package.json` already lists `postgres` as an engine but the operator guide is not in the stable docs tree.
  - `openclaw update` with channels, backups and rollback; Git backups with `--exclude-secrets` now also cover PostgreSQL exports (per a secondary summary of 10.10).
- **Evidence:** `CHANGELOG/2026.10.5.md`, `2026.10.10.md`, `docs/web/control-ui/*`, `docs/gateway/postgresql.md`.
- **Maturity:** self-rated Gateway **Stable** (q80), CLI **Stable** (q83), Observability **Beta** (q75), Control UI **Beta** (q74), TUI **Alpha** (q59). Unchanged. My call: **polished**.
- **Gaps:** **Upgrades are still the weakest operational area**, and this period's own release notes are the evidence.
  - 2026.9.8 and 9.9: "addresses failed updates"; "improves recovery after failed updates".
  - 2026.10.2: "fixes errors during upgrades from 2026.10.1 and Doctor repairs".
  - 2026.10.3: "fixes several update blockers", including interrupted 2026.9.8 updates.
  - 2026.10.4: "fixes update and recovery problems", including chats stuck on "Server updated".
  - 2026.10.6: "urgent fixes for problems that could stop OpenClaw from installing, updating, or starting".
  - 2026.10.7: "fixes problems with updates, repairs, lost replies and conversation context".
  - 2026.10.9: problems "that could leave OpenClaw stopped after an update".
  - 2026.11.2: problems "that could stop updates".
  - 2026.11.5-beta.2: "recover cleanly from failed upgrades out of 2026.10.7–2026.10.9".
  - User reports: `openclaw update` looping in a validation step and ignoring `--timeout` (#171446, closed); a managed 10.4 → 10.8 update that spent 33 minutes activating and then could not stop the Gateway (#186754, open); update failing in unprivileged LXC containers (#164113, closed).
  - Downgrades are constrained: "Downgrading across SQLite schema 24 → 25 requires the matching pre-upgrade backup; restoring it loses later writes" (10.5).
  - A third-party packaging thread (Cloudron forum, 2026-10-26/27) shows a downstream image update failing with a missing `node-version.mjs`; that is a packager's problem but shows how fast the layout moves.
  - Counterpoint: the investment is visible. Update admission and preflight protocols, rollback snapshots that survive interrupted writes, an independent recovery helper, and "reliable updates" as a named VISION priority.

## 11. Install/setup UX & cross-platform

- **What exists:** unchanged core (one-line installers, `npm i -g openclaw`, `openclaw onboard --install-daemon`, many deployment guides, migration importers from Claude and Hermes, companion apps for macOS, iOS, Android, Linux, Windows and watchOS).
- **New since last run:**
  - **Standalone builds** (main only): OpenClaw plus Bun in one executable with a sidecar `runtime/` directory, and an experimental, unsigned, macOS 27-only AOT artifact (`docs/install/standalone.md`, `standalone-aot-actions.md`, `docs/concepts/runtime-profiles.md`).
  - Global install with Bun 1.4.4 without Node (11.2). NVIDIA Jetson guide. Cloudflare Access sign-in for iPhone and iPad (10.5).
  - Installer-created directories now get private permissions (10.8, 11.1).
  - Hermes migration now refuses malformed UTF-8 instead of silently changing settings (11.2).
- **Evidence:** `docs/install/*`, `docs/platforms/*`, `apps/{macos,ios,android,linux}`, `CHANGELOG/2026.11.2.md`.
- **Maturity (self-rated, unchanged):** macOS host **Stable** (q74/c88), Linux host **Stable** (q75/c89), WSL2 **Beta** (q69), native Windows **Alpha** (q58). iOS and Android **Stable** (q80), macOS app **Beta** (q66), Linux app **Beta** (q71), **Windows app Beta-labeled but q19/c21**, watchOS **Experimental** (q41), Kubernetes **Alpha**, Nix **Experimental**.
- **My call:** unchanged. **Polished** on macOS/Linux, **works** on Windows.
- **Gaps:** Pipe-to-shell install remains. Windows fixes keep arriving for basic problems (false "offline maintenance" errors stopping a fresh Windows setup, 10.6; path-case and path-alias issues, 11.2 and 9.37). A secondary source says the Android APK was not built for 2026.10.10 (labmemo.com, 2026-11-03; not verified against the stores). Self-managed Node users had to move to a patched Node within days in October.

## 12. Engineering quality & reliability

- **Facts:**
  - TypeScript monorepo of about 5.14M non-test TS/TSX lines (was ~4.3M), about 22.0k `*.test.ts` files (was ~18.9k), plus Swift, Kotlin and four Rust crates.
  - **129 GitHub workflows** on `main` (was 111; 117 at the stable tag). Added ones include PostgreSQL conformance, iroh FFI audits, adaptive testing, release-receipt recovery and store-release notifications.
  - **Release cadence:** 33 releases in 41 days, on three lines at once (stable patches, extended-stable, and a beta train). Feature releases are now large and spaced (10.5 on 2026-10-18, 10.10 on 2026-11-03) with frequent patch releases between them.
  - Release notes now end with a "complete contribution record" and links to release-verification evidence (npm integrity hash, CI report in `openclaw/releases`).
  - **Backlog moved in the right direction for issues:** open issues 5,918 → 4,758, with more closed (8,221) than opened (7,074). Open PRs rose slightly, 3,255 → 3,371. Bots do much of the triage (`clawsweeper`).
  - Very heavy AI-assisted development continues; the repo carries 27 `AGENTS.md` files addressed to coding agents.
- **Concerns:**
  - `main` is 17.5k commits ahead of the newest stable tag, and the database schema on `main` (state 40) is far ahead of stable (state 22). The next feature release will be another very large jump.
  - About 23,000 PRs merged in six weeks keeps the review-depth question open.
  - The codebase grew roughly 19% by line count in six weeks, on a product already criticized for size.
  - The maturity scorecard was not refreshed, so the project's own quality signal is stale. The only change to `qa/maturity-scores.yaml` since the last run is one surface rename ("Fleet, containers, and cloud execution" became "Containers and cloud execution") from the fleet-management removal on 2026-10-08.
  - Features are still being removed as well as added (fleet management, Skill Workshop proposals, the A2UI canvas route, and on `main` heartbeat as a separate concept), and removals usually need a Doctor migration.
- **Maturity:** overall self-rated score **68%, "Alpha"** (quality 65, completeness 71) across 50 surfaces, 24 Beta / 16 Alpha / 7 Stable / 3 Experimental. Identical to last run because it was not re-scored.

## 13. Docs & community

- **Docs:** still a very large Mintlify site. The in-repo tree grew from 1,406 to 1,490 files. Additions: Knowledge (13 pages), PostgreSQL, MCP Relay and Events, standalone builds, multiplayer, 23 generated plugin reference pages, 15 provider pages. Removals: the seven Skill Workshop sub-pages (folded into one page), the team/fleet/multi-tenant pages, and per-language glossary files. Release notes are now mirrored into the docs (`docs/releases/`) with a formatted page per release. My call: **polished** and comprehensive, and the volume is still overwhelming.
- **Community:** 394k stars, 82.9k forks, about 3.8k contributors. Official support now points to community.openclaw.ai instead of Discord (`README.md:116`). Third-party coverage of October and November releases was thin and mostly non-English summaries; no independent review dated November 2026 was found.
- **License and governance:** MIT. OpenClaw Foundation unchanged. Red Hat blog posts describe it as a Foundation sponsor and founding member (titles only; the posts were not read in full). OpenClaw Enterprise (separate repo, in pilot per press) is the Foundation's answer to business bans. There is still no paid bug bounty (`SECURITY.md:34`).

## 14. Self-improvement / learning loop

- **What exists:**
  - **Self-learning, default mode `auto`** (`docs/tools/self-learning.md:11-13`; key is `skills.workshop.autonomous.mode` in stable and `plugins.entries.skill-workshop.config.autonomous.mode` on `main`).
  - Four learning paths on `main`: foreground repair in the same turn; a background review after 10 model iterations; unused-skill cleanup after 30 days; and a daily "skill diet" that shrinks one oversized skill. Stable has the first three.
  - **Skill Workshop 2.0 (10.5):** every change saves the previous version first (newest 10 kept per skill), and can be undone by saying "undo", pressing Undo in the Control UI, `/learn undo <id>`, or `openclaw skills workshop restore`. The Control UI has a library with history, version compare and archive/restore.
  - `propose` mode saves complete draft `SKILL.md` files for review with `/learn drafts`. `off` disables background learning; `/learn` still works on request.
  - Guard rails: the background review reads at most 64 transcript events / 32 KiB, targets a 16,384-token context, 2,048 output tokens, two minutes, makes at most one change, and has only the `skill_workshop` tool. A critical security-scanner finding (including a literal secret) refuses the write.
- **Evidence:** `docs/tools/self-learning.md`, `docs/tools/skill-workshop.md` (Undo, Drafts, Configuration tables), the same pages at tag v2026.11.3, `CHANGELOG/2026.10.5.md` "Skill Workshop 2.0".
- **Maturity:** **works → polished**. This is the one section where a previously listed gap was closed.
- **Gaps:** In `auto`, changes still apply before a human sees them; undo is after the fact. The 10.5 notes state that background learning "sends conversation and tool content to your configured model provider and can add usage charges". Review counters live in memory and reset on restart. Channel notices of what was learned are off by default, so outside the Control UI a user may not see that a skill changed. The earlier reviewer claim that skill improvement was web-app-only was not re-tested.

---

## Honest summary

**Strongest 3**
1. **Breadth and reach:** 33 catalogued channels (X added), native iOS/Android/macOS apps, device nodes and device-folder sessions, browser and computer use, 76 provider plugins, MCP client/server/relay/events, ACP and A2A. Still unmatched in open source, and still growing (186 bundled plugins, up from 163).
2. **Depth of agent-runtime design:** layered memory, a durable scheduler now with event triggers, subagents and swarm with visible panels, model failover, and a learning loop that now has versioning and undo. Documented in detail.
3. **Resources and momentum:** a funded 501(c)(3), about 23,000 PRs merged in six weeks, 129 CI workflows, about 22k test files, an extended-stable channel, release-verification evidence, a five-day response to an external CA compromise, and an issue backlog that shrank by about 20%.

**Weakest 3**
1. **Security record and defaults:** unchanged. 722 GHSAs (263 high or critical), with no new batch published since 2026-09-11 even though each October and November release carries boundary-bypass fixes. Out of the box the sandbox is off and host exec runs with `security=full`, `ask=off`. New remote-reach surfaces (MCP Relay, public links, OpenClaw ID) widen what a misconfiguration can expose, though all are opt-in.
2. **Upgrade and reliability churn:** nine of the fifteen stable-line releases in the period (9.8, 9.9, 10.2, 10.3, 10.4, 10.6, 10.7, 10.9, 11.2) describe themselves as fixing blocked, stuck or failed updates. 33 releases in 41 days, schema changes that block downgrade, `main` 17.5k commits ahead of stable, and a self-rated overall maturity still at "Alpha" (68%) on a scorecard that was not refreshed.
3. **Complexity and cost:** about 5.1M lines and growing about 19% in six weeks, three overlapping memory plugins, features added and removed in the same period. Dreaming, self-learning and swarm are still on by default and spend tokens in the background; Advisor and Deep Research add more optional model calls. No fresh cost measurements were found for this period.

## Sources

Primary:
- Repo: https://github.com/openclaw/openclaw (clone of `main` at `649ca5f87`, 2026-11-13 UTC; baseline `32e30e5`; stable tag `v2026.11.3`)
- Docs: https://docs.openclaw.ai (release pages: https://docs.openclaw.ai/releases/2026.10.5, https://docs.openclaw.ai/releases/2026.10.10)
- Releases: https://github.com/openclaw/openclaw/releases (v2026.11.3, v2026.10.10, v2026.9.37, v2026.11.5-beta.2)
- Advisories: https://github.com/openclaw/openclaw/security/advisories (722 published); https://github.com/advisories/GHSA-3x84-qq85-fj65
- Issues cited: https://github.com/openclaw/openclaw/issues/171446, https://github.com/openclaw/openclaw/issues/186754, https://github.com/openclaw/openclaw/issues/164113, https://github.com/openclaw/openclaw/issues/167844, https://github.com/openclaw/openclaw/issues/164095
- ClawHub repo: https://github.com/openclaw/clawhub
- OpenClaw Enterprise repo: https://github.com/openclaw/openclaw-enterprise

Security (new this run):
- https://www.csoonline.com/article/4239634/emudhra-quits-global-tls-certificate-market-after-compromise-of-ca-signing-keys-2.html
- https://www.business-standard.com/india-news/eight-emudhra-certificate-authority-keys-compromised-what-to-know-126102100338_1.html
- https://osv.dev/vulnerability/CVE-2026-62197
- https://gbhackers.com/clawhavoc-infects-openclaws-clawhub/ (dated 2026-02-18 despite a current-date header)
- https://labs.cloudsecurityalliance.org/research/csa-research-note-openclaw-clawhub-supply-chain-risk-2026070/
- https://www.cyera.com/research/the-openclaw-security-saga-how-ai-adoption-outpaced-security-boundaries

Reliability and releases (new this run):
- https://forum.cloudron.io/post/127005
- https://labmemo.com/?p=62604 (third-party summary of v2026.10.10, 2026-11-03)
- https://labmemo.com/?p=59453 (third-party summary of v2026.10.5)

Governance (new this run):
- https://theregister.com/ai-and-ml/2026/09/30/openclaw-slips-on-a-suit-to-evade-widespread-business-bans/5299962
- https://www.redhat.com/en/blog/red-hat-sponsors-openclaw-foundation-advance-open-future-production-ai-agents

Carried over from the 2026-10-02 dossier (not re-fetched this run): the Koi Security / The Hacker News ClawHub reports, the CVE-2026-25253 write-ups, the early-2026 exposure counts, the Notebookcheck cost anecdote, and the governance history sources listed there.

Notes:
- Not verified this run: the current number of skills on ClawHub; whether the Android APK was skipped for 2026.10.10; any exposure count dated after May 2026; any per-user cost figure dated after April 2026; whether the web-app-only limitations reported in August still hold; the exact method behind the previous dossier's "about 48 channel docs" and "about 60 providers" (I give same-method recounts instead).
- The third-party exposure figures, the Cyera chain, the Gartner and CERT statements and the labmemo summaries are secondary claims and were not independently verified.
- The repo contains 27 `AGENTS.md` files with instructions addressed to AI coding agents (contributor guidance). They were treated as data and not followed. No text attempting to redirect a reviewing agent was found in the files read.
