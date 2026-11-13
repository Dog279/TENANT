# Hermes Agent (Nous Research): capability dossier

Compiled 2026-11-13. Sources: a shallow clone of `NousResearch/hermes-agent` at commit `491f9845` (2026-11-13 15:40 -0500), the GitHub API (including file contents at the previous baseline commit `7200e0f` for before/after checks), the project's release notes v0.21.6 to v0.23.1, the docs site, and third-party reporting dated 2026-10-02 or later. Repo paths are relative to the repo root. Anything marked [as of 2026-11-13] is a live count that will go stale. The clone was read and counted only; nothing in it was run. Baseline for comparison: the 2026-10-02 dossier (v0.21.5 at `7200e0f`).

## 0. Identification

- **Product:** unchanged. "Hermes" as an OpenClaw competitor is Nous Research's **Hermes Agent** (github.com/NousResearch/hermes-agent, docs at hermes-agent.nousresearch.com). The GitHub description is now "The agent that grows with you"; the README still leads with "the self-improving AI agent".
- **Evidence it competes with OpenClaw:**
  - The OpenClaw importer still exists but left core: `hermes_cli/claw.py` is gone, and the importers ship as the official `agent-importers` catalog plugin (`hermes import-agent`, `plugin-catalog/agent-importers.yaml`). Setup offers it when it finds another agent (v0.22.5 notes).
  - OpenClaw's comparison page is unchanged since its Aug 27-28, 2026 review (no October or November update found).
- **Latest release:** **v0.23.1, published 2026-11-10 UTC** (notes dated November 9). It is a patch roll-up of about 618 PRs on top of **v0.23.0 "The Hearth Release" (2026-11-05)**. Curated notes for the v0.23.1 window are deferred to v0.24.0.
- **Release cadence since last run:** 12 stable releases in about five weeks: v0.21.6 (10-08), v0.21.7 (10-12), v0.22.0 "The Threshold Release" (10-14), v0.22.1 to v0.22.7 (10-14 to 11-03), v0.23.0 (11-05), v0.23.1 (11-10). [GitHub API, as of 2026-11-13]
  - Tags moved from date-based (`v2026.9.24`) to SemVer (`v0.21.6` onward). v0.21.6 was the first release cut by a new stable release pipeline.
  - Since v0.21.7, source installs follow tagged stable releases by default instead of every commit on `main` (`hermes update --set-channel main` opts back in). Canary pre-releases are cut from `main` (latest: `v0.23.1+canary.20261113T064447Z`).
  - 48 stable releases in total, up from 36.
  - v0.22.1 "was built from the wrong commit" per the project's own v0.22.2 notes, and was replaced the next day.
- **Business model changes:** still venture-funded with the paid Nous Portal and "Hermes Cloud". New since last run, all per the v0.22.0 and v0.23.0 notes:
  - A free trial model (`nous/welcome`, once per OS user) for the first Desktop launch. The CLI, TUI and gateway get no free model.
  - "Connectors": hosted app integrations (Linear, Notion and others) through the Nous tool gateway, usable with a free connectors-only identity (`tools.connectors.enabled` defaults to true, `hermes_cli/config_defaults.py`).
  - Usage-window subscription plans, a Microsoft Store listing, and opt-in usage telemetry to Nous (`telemetry.shared_metrics`, both switches default off).
  - Self-hosting with your own keys remains supported. The silent default model when none is picked is now GLM-5.3 (v0.23.0 notes).

---

## What changed since 2026-10-02

One row per capability section. "Before" is the 2026-10-02 dossier at `7200e0f`; "now" is `491f9845`.

| # | Section | Change | Evidence |
|---|---|---|---|
| 1 | Messaging channels and surfaces | Every messaging adapter is now a bundled plugin. `plugins/platforms/` went from 22 directories to 26: BlueBubbles, QQ Bot, Signal, Weixin and WhatsApp Cloud moved in from `gateway/platforms/`; Home Assistant and Yuanbao moved out to the plugin catalog. Bundled messaging adapters: 28 before, 26 now. (The last dossier's list of 23 plugin names counted Signal, which then lived in `gateway/platforms/`.) New surfaces: a browser **Webapp** served by the gateway (off by default), Bot Mode Group Chats, and one gateway owning every session for all surfaces. | Tree at both commits (GitHub contents API for `7200e0f`); `plugin-catalog/homeassistant.yaml`, `plugin-catalog/yuanbao.yaml`; `hermes_cli/config_defaults.py:982`; v0.22.0 and v0.23.0 notes |
| 2 | Tool execution | **Default terminal backend unchanged: `local`** (`cli-config.yaml.example:438`, was `:422`; `hermes_cli/config_defaults.py:285`). Core backends 7 to 6: Singularity became an official catalog plugin. New official **bubblewrap** backend plugin (Linux, opt-in) that wraps terminal and `execute_code`. Audit regression #127094 (no overall deadline) is still open. | `tools/environments/`, `tools/terminal_tool_backends.py:277-279`, `plugin-catalog/bubblewrap.yaml`, `plugin-catalog/singularity.yaml`, v0.22.4 notes |
| 3 | Browser automation and computer use | Breaking change: the ten `browser_*` tools and agent-browser were removed; automation runs on `browser_exec` only. Camofox moved to a plugin. New: password-manager sign-in (1Password, Bitwarden, own vault), "Bot Screen" live desktop streaming with human takeover, and browser/computer use running inside the sandbox on Docker, SSH or Singularity backends. | v0.23.0 upgrade notes (#143146), v0.22.0 notes, registered tool names in `tools/` (`browser_exec`, `browser_cdp`, `browser_dialog`, `browser_vault`) |
| 4 | Memory and personalization | Built-in limits unchanged (2200 / 1375 chars, nudge 10). Bundled memory plugins 5 to 2 (ByteRover, RetainDB); mem0, Holographic and OpenViking joined Honcho in the catalog, which lists 126 memory entries. Session auto-prune default flipped twice: on at v0.22.0, back to opt-in at v0.22.3. No new memory CVE found. | `hermes_cli/config_defaults.py:1302-1305`, `plugins/memory/`, `hermes_cli/config_defaults.py:2259`, v0.22.0 and v0.22.3 notes |
| 5 | Extensibility | Bundled skills **58 to 44**; optional skills 152 to 141. A reviewed **plugin catalog** launched: 2,311 entry files in the tree (21 official, 13 verified, the rest community); the project claimed 600 at v0.22.0 and 1,050+ at v0.23.0. New opt-in `plugins.isolation: host`; **default stays `in_process`**. `hermes mcp serve` became the `mcp-serve` plugin. tirith scanner removed. | `skills/`, `optional-skills/`, `plugin-catalog/*.yaml`, `hermes_cli/config_defaults.py:1714`, v0.22.0 and v0.23.0 notes |
| 6 | Autonomy and scheduling | Cron gained cancel, per-job missed-run policy, self-healing re-runs and a shared prompt-cache key. Spend limits and price tags on paid MCP tools ask even under YOLO. Unattended sessions hard-stop tool loops by default. | `cron/cancellation.py`, `cron/missed_runs.py`, `hermes_cli/config_defaults.py:544-549`, `tools/mcp_tool_costs.py`, v0.22.0 and v0.23.0 notes |
| 7 | Multi-agent and subagents | Delegation is now pluggable (`delegation.backend`; default strategy is a bundled plugin; official `advanced-delegation` adds model-map and per-profile routing). Live subagent view with Steer/Stop. Group Chats got lead/auto routing and a `max_hops` cap (default 4). Defaults unchanged: 10 concurrent, depth 1, subagent approvals auto-deny. #97681 still open. | `plugins/delegation/default` (absent at `7200e0f`), `tools/delegate_tool_config.py:19-23`, `hermes_cli/config_defaults.py:1356-1372`, v0.23.0 notes |
| 8 | Model flexibility | Bundled provider plugins 38 to 39 (Copilot and Copilot-ACP moved to a labs plugin; LM Studio, Solstice and Tencent TokenHub added). One-click local models through a managed llama.cpp runtime. **Fixed prompt overhead:** third-party figure about 13.9K tokens per call before; a different third party measured about 12,200 on v0.22.2 (2026-10-22). The project claims about 2.1K fewer fixed tokens and halved desktop tool schemas (13,367 to 6,883) through default tool deferral. | `plugins/model-providers/` at both commits, `hermes_cli/config_defaults.py:1968-2003`, v0.22.0 notes, DreamHost guide |
| 9 | Security | **Defaults did not move:** local backend, `approvals.mode: smart`, in-process plugins. SECURITY.md §2.2 is unchanged; the only edit says profiles are not tenants. **One new CVE found:** CVE-2026-114463 (webhook route authorization bypass, High 7.5, published 2026-10-21, fixed v0.22.2). v0.21.6 shipped fixes for four dashboard-auth issues credited to Tenable Research, a git-filter execution issue and an email allowlist spoof. **Posture review #40889 is still open, unassigned, zero comments, and was relabeled P3 to P4 on 2026-11-11.** New user incident #141346: smart approval auto-approved commands that destroyed unsaved work. | Diff of `SECURITY.md` between commits, `hermes_cli/config_defaults.py:1656`, v0.21.6 notes, OpenCVE, GitHub issue timeline |
| 10 | Observability and ops | Langfuse moved to an official catalog plugin (`plugins/observability/` is gone). New: `hermes prompt-size`, summary-first `hermes status`, a unified package manager (`hermes pm`), stable update channel, opt-in telemetry. Desktop idle burn tracker #127647 still open (P2). | Tree, `pm/`, `website/docs/reference/cli-commands.md:1325`, v0.22.0 notes |
| 11 | Install, setup UX and cross-platform | Guided first-run onboarding chat on a free trial model. Signed, self-updating desktop packages. Narrower platform support: Windows desktop is MSIX-only and needs Windows 11 22H2+; macOS desktop needs macOS 13+ and is Apple Silicon only in default builds. Stable releases skip macOS Intel and Termux by default. | v0.22.0 and v0.23.0 upgrade notes |
| 12 | Engineering quality and reliability | **CI on `main` looks healthier:** last 15 push runs of `ci.yaml` were 11 success, 1 failure, 3 cancelled (before: 4 failed, 2 cancelled, 2 running). **Backlog grew:** open issues 14,506 to 19,105; open PRs 33,888 to 34,238; open `type/bug` 7,953 to 11,149. **Audit-regression stream did not move:** no new "[Audit regression]" issues since 2026-09-29; of that batch 13 are open and 5 closed (all 5 closed on the filing day). Test files about 5,400 to 7,971; test functions about 44k to 57.8k. | GitHub Actions and search APIs [as of 2026-11-13]; file counts from the tree |
| 13 | Docs and community | Stars 250.7k to 263.2k; forks 53.7k to 59.3k; contributors (API, anonymous included) about 4,158 to 6,347. Docs pages about 450 to 435. Curated notes shipped for v0.22.0 and v0.23.0, but patch releases still defer notes. README drift on session search and Honcho persists. | GitHub API [as of 2026-11-13], `website/docs`, `README.md:26` |
| 14 | Self-improvement and learning loop | No change to defaults: skill-review code default 10 iterations (the example config ships 15, as it also did at baseline), curator 7 days / 2h idle / stale 14 / archive 30, consolidation off. New guards: `hermes curator lock` and per-skill locks, opt-in `skills.write_approval` staging. Trigger code moved to `agent/turn_finalizer.py:882-888` (was `:737-760`). | `agent/agent_init.py:1236-1238`, `cli-config.yaml.example:1220`, `hermes_cli/config_defaults.py:1465-1489`, v0.23.0 notes |

**Net effect on last run's summary:** the strengths hold. Of the three weaknesses, "security posture by default" is unchanged in defaults but has more opt-in mitigations; "churn and backlog" is worse on backlog and better on CI and release process; "cost and bloat" improved on prompt overhead (project claim plus one third-party measurement) and worsened on code size.

---

## 1. Messaging channels and surfaces

- **What exists:** a single `hermes gateway` process with platform adapters. Since v0.22.0 it is one gateway per machine serving all profiles, and since v0.23.0 that gateway owns every session: CLI, TUI, Desktop, ACP editors, bots and cron attach to it as clients (`gateway/session_authority.py`).
  - **Platform adapters, all bundled plugins in `plugins/platforms/` (26):** a2a, bluebubbles, buzz, dingtalk, discord, email, feishu, google_chat, irc, line, matrix, mattermost, ntfy, photon, qqbot, raft, signal, simplex, slack, sms, teams, telegram, wecom, weixin, whatsapp, whatsapp_cloud.
  - **Left in `gateway/platforms/`:** shared base code plus the generic surfaces: MS Graph webhook, generic webhooks, and the OpenAI-compatible `api_server`.
  - **Moved to the plugin catalog:** Home Assistant and Yuanbao (official or community catalog entries). The catalog lists 92 `platform` entries in total, including iMessage, Microsoft 365, Rocket.Chat, VK and Zalo plugins from third parties. The v0.23.0 notes say Yuanbao ships bundled; the tree at this commit has it only in the catalog.
  - **Other surfaces:** TUI (`ui-tui/`, `tui_gateway/`), Hermes Desktop (`apps/desktop`), a web dashboard (`web/`, `hermes_cli/web_server*.py`), the new **Webapp** (the Desktop workspace in a browser tab, served by the gateway; `gateway/webapp_listener.py`, `hermes_cli/webapp*.py`), ACP adapter (`acp_adapter/`), MCP server mode (now the `mcp-serve` plugin), Termux packages, and voice (wake word, STT/TTS, streaming dictation, full-duplex voice chat).
  - **Bot Mode Group Chats:** rooms with several bots, lead or automatic routing, shared notes, cron delivery into rooms, and bots from other machines (`gateway/hosted_room*.py`).
- **Maturity:** Telegram, Discord, Slack and CLI remain the polished paths. The single-gateway change was large: the v0.23.0 notes list what it broke and then fixed (slash commands, `/retry`, clarify cards, ACP sessions).
- **Gaps:**
  - iMessage still needs BlueBubbles on a Mac, or a third-party catalog plugin.
  - The Webapp is off by default (`webapp.enabled: False`, `hermes_cli/config_defaults.py:982`) and is single-user.
  - #97065 (gateway setup crash for one platform, open since 2026-08-28) is still open.
  - New adapter-level security fixes this window: email allowlist display-name spoofing (#125212), and the webhook gateway route bypass (CVE-2026-114463).

## 2. Tool execution

- **Shell:** `tools/terminal_tool*.py`.
  - PTY, background processes with a registry and notifications, sudo handling, `read_terminal` / `close_terminal`.
  - New: foreground commands over 600 s become background jobs; the agent is notified on exit instead of polling (`terminal.process_wait_auto_notify: true`).
- **Files:** `file_tools*.py` and `file_operations*.py`: read, write, patch, fuzzy-match patching, lint on write, write guards, checkpoints/rollback. LSP-backed edits now return in milliseconds (v0.23.0 notes).
- **Code execution:** `code_execution_tool.py`, `code_kernel.py`, `code_kernel_remote.py`, `code_execution_rpc.py`. A persistent per-conversation kernel is always on locally. Scripts call Hermes tools over RPC.
- **Terminal backends:** six in core (`tools/environments/`): local, docker (with `docker_egress.py`), ssh, modal (plus a managed variant), daytona, vercel_sandbox.
  - Singularity/Apptainer is now an official catalog plugin.
  - **New: bubblewrap** official plugin for Linux. Per its catalog description, terminal and `execute_code` commands and the file tools run in a `bwrap` sandbox with a read-only root and hidden credential stores. It is opt-in, and its default `network` profile shares the host network.
  - Backends can declare `uses_host_filesystem = False` so file tools run inside the sandbox. Third-party sandbox backend plugins exist in the catalog (not reviewed here).
- **Maturity:** polished, still the most mature area.
- **Gaps:**
  - The default backend is still `local` (`cli-config.yaml.example:438`, `hermes_cli/config_defaults.py:285`), which runs directly on the host.
  - SECURITY.md §2.2 still says the code-execution tool and MCP subprocesses run on the host even when a sandboxed terminal backend is selected. The code header in `tools/code_execution_tool.py:6-10` says remote backends ship the script to the backend, with tool calls served by the host agent; the policy document has not been updated to claim that as a boundary, so treat the policy wording as the guarantee.
  - Audit regression #127094 (sequential tool execution has no overall deadline) is still open, labeled P2 and needs-repro, with 2 comments.

## 3. Browser automation and computer use

- **What exists:**
  - **Local browser:** `browser_exec` drives a bundled Chromium (also the fallback when a cloud browser fails). The old `browser_navigate`, `browser_click` and the other eight `browser_*` tools are gone. Companion tools: `browser_cdp`, `browser_dialog`, `browser_vault`.
  - **Engines and fallbacks:** Lightpanda remains in core (`tools/browser_lightpanda.py`). Camofox is now an official plugin, auto-installed for homes that used it.
  - **Cloud browsers:** Browserbase, Browser Use and Firecrawl (`plugins/browser/`).
  - **Logins:** password-manager sign-in through 1Password, Bitwarden or Hermes's own encrypted vault; the model does not see the password. The vault key can live in the OS keychain (opt-in). Verified catalog plugins can register login backends scoped to their own entries.
  - **Computer use:** `tools/computer_use/` with the cua backend (`plugins/computer_use/cua`). It is deferred behind `tool_search` by default.
  - **Bot Screen:** a bot on a Linux gateway can have its own desktop, streamed to Hermes Desktop, with human takeover for logins, 2FA and CAPTCHAs. With Docker, SSH or Singularity backends the screen, computer use and browser run inside the sandbox (v0.22.0 notes).
- **Maturity:** working to polished, with a breaking tool-surface change this window. Computer use is still newer.
- **Gaps:**
  - Prompts, skills and toolset lists that name the removed `browser_*` tools must be edited by hand.
  - Vision and computer-use quality depend on the model.
  - Cloud browser backends need third-party accounts or the Nous Portal.

## 4. Memory and personalization

- **Built-in memory:** `tools/memory_tool.py`, `tools/memory_tool_store.py`.
  - Two small curated files, `MEMORY.md` and `USER.md`. Default limits are still 2200 and 1375 characters (`hermes_cli/config_defaults.py:1302-1303`, `tools/memory_tool_store.py:188`).
  - Injected as a frozen snapshot at session start to keep the prompt cache intact.
  - New: `hermes sessions refresh-prompts` lets continuing sessions pick up edited memory, skills or `SOUL.md` at the cost of one cache miss.
- **Nudges:** memory nudge every 10 user turns (`memory.nudge_interval`, `hermes_cli/config_defaults.py:1305`). A background review fork decides what to save (`agent/background_review.py`).
- **Session search:** `tools/session_search_tool.py` is still SQLite FTS5 with no LLM calls (no auxiliary-model call found in the module). It is deferred behind `tool_search` by default.
- **Pluggable external memory:** only ByteRover and RetainDB remain bundled (`plugins/memory/`). Honcho, Mem0, OpenViking, Holographic, Supermemory and Hindsight are catalog plugins maintained by their makers; existing users are migrated on update (v0.22.0 notes). The catalog lists 126 `memory` entries.
- **Maturity:** works. The design is still deliberately small and bounded.
- **Gaps:**
  - The built-in memory is tiny by design.
  - OpenClaw's comparison (competitor, reviewed Aug 2026, not updated since) says the built-in store has no source-session tombstones; it labels that an inference, not a reproduced failure. Not re-verified here.
  - Session history retention changed twice in three weeks: auto-prune of sessions inactive for 90 days was turned on at v0.22.0 and made opt-in again at v0.22.3 (`sessions.auto_prune: False`, `hermes_cli/config_defaults.py:2259`).
  - Earlier memory CVE (CVE-2026-10223) stands; no new memory CVE found this window.

## 5. Extensibility: skills, plugins, MCP, hub

- **Skills:** agentskills.io / SKILL.md standard. **44 bundled and 141 optional** skills (`SKILL.md` files under `skills/` and `optional-skills/`). Word, PowerPoint and Excel merged into one `microsoft-office` skill.
  - New config: `skills.auto_load`, `skills.create_dir`, project-scoped skills that load only from trusted roots, `inline_shell` off by default.
- **Skills Hub:** `tools/skills_hub*.py`. Sources still include GitHub, ClawHub, skills.sh and the official index.
  - Installs go through `skills_guard.py` (scanner version `skills-guard-v13`), still a regex static scan with trust tiers. The module header still documents a known detection gap for language-level write APIs.
  - An advisory NVIDIA SkillEvaluator scan runs on install when that tool is present; it never blocks.
  - Skills the agent writes itself are not scanned by default (`skills.guard_agent_created: False`, `hermes_cli/config_defaults.py:1456`).
- **Plugins:** `plugins/` with a loader, plus the new **plugin catalog** (`plugin-catalog/`).
  - 2,311 catalog entry files at this commit: 21 official, 13 verified, about 2,266 community. By category: 1,108 tools, 399 desktop, 172 automation, 140 models, 126 memory, 92 platform, 80 web, 55 voice.
  - Each entry is pinned to a commit SHA; `requires_hermes` is enforced at install. Opt-in auto-update exists (`plugins.auto_apply: False`).
  - Plugins still load **in-process, unsandboxed** by default (`plugins.isolation: "in_process"`, `hermes_cli/config_defaults.py:1714`; SECURITY.md §2.5). The new opt-in `plugins.isolation: host` runs third-party plugins in a per-profile host process with only that profile's secrets, and `plugins.host.launcher` can wrap it in a sandbox runner. Bundled plugins stay in-process either way.
  - v0.23.1 fixed a bug where the live catalog exceeded a 2 MiB client cap, so installs silently fell back to the shipped catalog.
- **MCP:**
  - **Client:** stdio and HTTP transports, OAuth including device-code login, sampling, liveness supervision, schema cache, progress notifications, and declared per-call prices with spend limits (`tools/mcp_tool*.py`, `tools/mcp_tool_costs.py`). 66 optional MCP definitions ship in `optional-mcps/`.
  - **Server:** now the `mcp-serve` plugin; the command changed from `hermes mcp serve` to `hermes mcp-serve` and nothing rewrites client configs.
  - **Tool search:** on by default when deferred tools exist (`tools.tool_search.enabled: "auto"`); about 20 built-in tools are deferred by default (`hermes_cli/config_defaults.py:1968-2003`).
- **Maturity:** polished, with a much larger third-party surface than five weeks ago.
- **Gaps:**
  - Plugin and skill code is unsandboxed unless the operator opts into host isolation.
  - The hub scan is regex-only. Catalog review depth for about 2,300 entries is not verified; the v0.22.0 notes say 11 plugins that patched internals were delisted.
  - A Critical supply-chain CVE in the MCP catalog (CVE-2026-82021, mutable branch reference, affected 0.18.2 to before 0.19.0, published 2026-08-28 per a third-party aggregator) predates the last run but was not in the last dossier.

## 6. Autonomy and scheduling

- **Cron:** built-in scheduler (`cron/`) with job definitions, a delivery queue to any platform or Group Chat room, retries, quota holds, incidents, a monitor and detached workers.
  - New: `hermes cron cancel` and a Stop button; per-job missed-run policy (`cron.catch_up_missed: true` by default); self-healing re-runs after 5, 15 and 30 minutes; unpinned jobs follow the main model at fire time; one prompt-cache key per job.
  - Jobs are still created in natural language (`cronjob_manage`, renamed from `cronjob`).
- **Other autonomy:**
  - Kanban worker/orchestrator system (`tools/kanban_tools.py`, `plugins/kanban`) with one unified workflow definition.
  - Idle-triggered curator (`agent/curator.py`), `/goal` with a judge, `/loop`, cron suggestions and outbound webhooks.
  - Loop guardrails: unattended gateway and cron sessions hard-stop tool loops by default (`non_interactive_hard_stop_enabled: True`, `hermes_cli/config_defaults.py:549`); interactive sessions get warnings only.
  - Price tags and spend limits on paid MCP tools; the limit prompt applies even in YOLO mode.
- **Maturity:** polished.
- **Gaps:**
  - Cost visibility: a third-party guide (DreamHost, updated 2026-10-22) cites two user reports where Hermes's own spend records covered only 44% and 58% of the billed cost. Not verified here.
  - Spend limits cover MCP tools that declare a price, not model spend in general.
  - Cron still skips the background review on cost grounds.
  - "Approval and clarify prompts that wait until answered" were merged and reverted in the v0.22.0 window; `approvals.timeout` (300 s) still applies.

## 7. Multi-agent and subagents

- **Delegation:** `tools/delegate_tool*.py`.
  - Isolated child agents with their own toolsets; parallel children default max 10 (`tools/delegate_tool_config.py:19`); flat by default (`MAX_DEPTH = 1`, `:23`), deeper via `max_spawn_depth` (range 1-3).
  - Async delegation with durable records, per-subagent git worktrees, structured outputs, fallback providers for children.
  - **New: pluggable delegation backends.** `delegation.backend` picks the strategy; the default is a bundled plugin (`plugins/delegation/default`); the official `advanced-delegation` plugin routes tasks by model map or into separate profiles. A missing backend fails closed.
  - New: running subagents stay visible in CLI, TUI and Desktop with live transcripts plus Steer and Stop; `/stop` reaches background children; children can receive images.
- **Mixture-of-agents:** `agent/moa_loop.py`.
- **Cross-agent:** A2A platform adapter; Bot Mode Group Chats with lead or automatic routing and a bot-to-bot hop cap (`max_hops`, default 4, per v0.23.0 notes); `hermes peer dm` for reaching a peer's bot. RFC #5257 (generalized ACP client) is still open.
- **Maturity:** works to polished.
- **Gaps:**
  - Bots collaborating across gateways (#97681) is still open, though Group Chats can now seat bots from other machines.
  - Subagent approvals auto-deny by default (`delegation.subagent_auto_approve: False`, `hermes_cli/config_defaults.py:1372`).
  - Parallel group-chat rounds were reverted; rooms run serially.

## 8. Model flexibility

- **Providers:** 39 bundled provider plugins (`plugins/model-providers/`), including Anthropic, OpenAI Codex, Gemini, Vertex, Bedrock, Azure Foundry, OpenRouter, xAI, DeepSeek, Kimi, Qwen, MiniMax, Z.ai, NVIDIA, HuggingFace, Nous, Ollama Cloud, LM Studio, Solstice, Tencent TokenHub, a `router` and a generic `custom` provider.
  - GitHub Copilot moved to a "labs" catalog plugin. The catalog lists 140 further `models` entries from third parties.
- **Local models:**
  - New: a managed llama.cpp runtime with one-click install, hardware-aware model recommendations and resumable GGUF downloads (CPU, CUDA, Metal, Vulkan, ROCm), per the v0.22.0 notes.
  - LM Studio is a first-class provider; Ollama, vLLM and llama.cpp servers map to `custom`.
- **Routing and switching:**
  - `/model provider:model` hot-swaps mid-session; `/model reset` returns to the default; reasoning effort is part of the pick.
  - Credential pools, fallback chains, an auxiliary model for side tasks, and bounded wait-out of transient provider errors.
- **Maturity:** polished.
- **Gaps:**
  - Prompt overhead is lower but still large. The earlier third-party figure was about 13.9K fixed tokens per call. A different third party (DreamHost, v0.22.2, updated 2026-10-22) measured about 12,200 per call from the CLI, about 9,000 of it tool schemas. The project's own claims: about 2.1K fewer fixed prompt tokens and desktop tool schemas cut from 13,367 to 6,883 tokens at v0.22.0, and 1,570 fewer tokens on one-shot runs at v0.23.0. The two third-party numbers come from different methods, so the drop is indicative, not exact. Not measured independently here. `hermes prompt-size` now reports the fixed budget.
  - Small local models still face a large tool surface; deferral helps.

## 9. Security

- **Official stance (SECURITY.md §2.2, a primary source), unchanged:** "The only security boundary against an adversarial LLM is the operating system."
  - Approval gates, redaction, pattern scanners and allowlists are still classed as heuristics, and bypasses of them are out of scope for private disclosure (§3.2).
  - Supported posture for untrusted input is still whole-process wrapping (the project's Docker image or NVIDIA OpenShell).
  - The **default is still the `local` backend** on the host. There is still no bug bounty.
  - The only edit since `7200e0f` adds that profiles under one OS user are not tenants, and that sibling-profile reach is out of scope (§2.6 rule 4, §3.2).
- **Approvals:** `tools/approval*.py`.
  - Default mode is `smart` (`hermes_cli/config_defaults.py:1656`), the auxiliary-LLM risk reviewer, as it was at baseline. Cron, single-query and unattended modes default to `deny` (`:1659-1661`).
  - New detections this window: `curl ... | sudo bash` pipes, zsh/dash pipe-to-shell, `deno eval`, cloud-metadata fetches, credential uploads and invisible Unicode in commands. `approvals.deny` globs block even under YOLO and now apply in sandboxes.
  - Protected instruction files still require approval even under YOLO (`cli-config.yaml.example:604-608`).
  - Shell hooks need recorded consent; an unattended gateway skips an unapproved hook.
  - The bundled `tirith` pre-exec scanner was removed; the notes say its key checks were built in.
  - Credentials are stripped from the environment passed to subprocesses (§2.3); the v0.22.0 notes add that child processes no longer inherit adapter or dashboard secrets.
- **Other hardening shipped this window (project's notes):** owner-only permissions on credentials, `state.db` and browser profiles; wider SSRF guard; broader secret masking; A2A refusing browser and foreign-Host requests without a token; inbound webhook, SMS and WhatsApp Cloud rejecting blank signing secrets.
- **Incidents and criticism (dated, new since last run):**
  - **2026-10-08 (v0.21.6 notes):** four dashboard-authentication issues credited to Tenable Research (TRA-725 to TRA-728): spoofed `X-Forwarded-For` resetting the login rate limit, unbounded audit-log writes, no body-size limit on `/auth/` routes, and login codes sent to a non-loopback redirect allowing session takeover. Also: automatic git calls could run filter programs from an untrusted repository's config before the first prompt, and an email allowlist bypass through a quoted display name. No CVE IDs for these were found; the Tenable advisories themselves were not located.
  - **2026-10-19:** issue #141346 (open, P3). A user reports that on macOS with default smart approval and the local backend, the agent drove PowerPoint through `osascript`, closed the user's unsaved deck without saving and deleted an export, all auto-approved. User-reported; not reproduced here.
  - **2026-10-21:** CVE-2026-114463 (CERT/CC, CVSS 7.5 High, CWE-863). A webhook route name containing a colon let a caller holding one route's HMAC gain a higher-privileged route's toolsets. Fixed in v0.22.2.
  - **2026-11-11:** posture review #40889 relabeled from P3 to P4. It is still open, unassigned, with zero comments, five months after filing.
  - **Dependency advisories:** several issues in October reported pinned dependencies with open advisories (#133435, #134394, #142307); all three are closed, and v0.22.5 notes mention a batch of dependency security bumps.
- **Earlier CVEs not in the last dossier (published before 2026-10-02, per a third-party aggregator, radicalnotion.ai):** CVE-2026-82021 (Critical 9.0, MCP catalog supply chain), CVE-2026-82020 (credential store overwrite via file write), CVE-2026-71963 (RCE via git `core.fsmonitor`, 0.18.2 to 0.21.0), plus about a dozen medium-severity SSRF, DoS and authorization issues from July to September. That aggregator lists 45 CVEs for the product in total [as of 2026-11-13]; only 30 were visible to this review.
- **Advisory databases:** the repository's own published-advisories API returned none. GitHub's reviewed global advisory database lists 10 entries for the package, newest 2026-06-17 [as of 2026-11-13]. 450 issues labeled `type/security` are open; 374 were created since 2026-10-02 (open and closed).
- **OpenClaw's comparison (competitor-authored, reviewed Aug 27-28, 2026, unchanged):** says non-interactive contexts other than cron and single-query can auto-approve, there is no general transcript-store redaction boundary, and Docker `:latest` follows main. The config at this commit has `unattended_mode: deny`; the other two claims were not re-verified.
- **Maturity:** layered heuristics are polished and got broader. The default posture is still permissive.
- **Gaps:**
  - Running on the host is the default.
  - In-process code paths are unsandboxed by default; the new isolation options (bubblewrap backend, plugin host) are opt-in and the plugin host is not described as a security boundary in SECURITY.md.
  - CVEs keep arriving, though only one new one was found in this six-week window against several per month in mid-2026.
  - The external posture review was deprioritized, not answered.

## 10. Observability and ops

- **What exists:**
  - **`hermes doctor`:** config, connectivity, live, platform, state and tools checks (8 `doctor*` modules in `hermes_cli/`). It now also warns when the cron store is unwritable.
  - **`hermes status`:** summary-first, with `--short` and `--full`.
  - **`hermes logs`:** log access (`hermes_logging.py`).
  - **`hermes prompt-size`:** new; reports the fixed prompt budget by block.
  - **Dashboard and Webapp:** the web dashboard (`hermes_cli/web_server_*.py`, `web/`) plus the Desktop workspace in a browser.
  - **Usage and cost:** `/usage` (now with per-allowance bars on usage-window plans) and `/insights`.
  - **Backups:** backup/restore; keeps the newest 3 and refuses damaged archives.
  - **Profiles:** one gateway and one desktop backend serve all profiles.
  - **Tracing:** Langfuse is now an official catalog plugin, not bundled.
  - **Updates:** `hermes update` (42 `update*` modules) follows the stable channel by default; a unified package manager (`pm/`, `hermes pm`) owns pinned, hash-checked tools and Python environments and switches only after a new environment passes its check.
  - **Telemetry:** opt-in shared metrics to Nous, default off, asked once per profile.
  - **Agent self-protection:** startup watchdog, SQLite state repair and WAL tooling (66 `hermes_state*` modules at the repo root).
- **Maturity:** polished but heavy. Desktop idle CPU/GPU/memory burn (#127647) is still an open P2 tracker with 37 comments. The v0.23.0 notes claim real gains for long-lived gateways (closed sessions released from memory, a sidebar refresh that took 121 s on one large store now instant).

## 11. Install, setup UX and cross-platform

- **Install paths:**
  - Linux, macOS and WSL2: one-liner `install.sh`.
  - Windows: PowerShell one-liner for the CLI; the desktop app is a signed MSIX (x64 and ARM64) through App Installer or the Microsoft Store. NSIS and MSI installers are gone.
  - macOS desktop: signed, notarized, Apple Silicon only in default builds.
  - Android: Termux packages; Docker/Compose; Nix flake.
- **Setup:**
  - New: a first Desktop launch opens a guided setup chat on the free trial model, so no key is needed to start.
  - `hermes setup` wizard and Nous sign-in by device login remain. `hermes model` can install a local model.
  - 17 locale files including English (`locales/`; the v0.22.0 notes say 16 bundled languages), with further language packs as plugins.
- **Supporting evidence:** install E2E workflows for macOS, Windows and Termux still exist (`.github/workflows/`), but per the v0.22.0 notes E2E now runs only on release builds, and stable releases skip macOS Intel and Termux by default.
- **Maturity:** polished for new users, and the stable channel is a real improvement for update safety.
- **Gaps:**
  - Narrower desktop support: Windows 11 22H2 or newer, macOS 13 or newer, 64-bit only (v0.22.0 and v0.23.0 upgrade notes).
  - Python 3.14 is required for managed installs.
  - Release slips happened inside the new pipeline: v0.22.1 was built from the wrong commit, and v0.23.0 shipped with the plugin-catalog size cap bug fixed in v0.23.1.
  - The README's antivirus false-positive note for `uv.exe` was not re-checked.

## 12. Engineering quality and reliability [as of 2026-11-13]

- **Code size (counted from the tree, nothing executed):**
  - Python: 11,589 `.py` files and about 2.80M lines in total. About 1.70M of those lines are under `tests/`; non-test Python is about 3,300 files and 1.10M lines.
  - TypeScript: 5,604 `.ts`/`.tsx` files and about 1.19M lines (about 726k excluding test files).
  - About 25,600 files in total, of which 2,311 are catalog entries.
  - Comparability note: the last dossier reported roughly 700k Python lines "including tests". The project's v0.22.0 notes say a simplification pass took source from about 1.06M to 698K lines, which suggests the earlier 700k was source only. On that reading, non-test Python grew from about 0.7M to about 1.1M lines in five weeks. Not verified against the old tree.
- **Tests:** 7,971 Python `test_*.py` files and about 57,800 `def test_` functions; about 2,450 TS test files. 46 workflow files in `.github/workflows/` (OS matrix, install E2E, desktop E2E, Nix, OSV scanner, live providers, sandbox image, stable and canary release). Before: about 5,400 files, 44k functions, about 50 workflows.
- **CI health:** of the last 15 push runs of `ci.yaml` on `main` (all within about two hours on 2026-11-13): 11 succeeded, 1 failed, 3 were cancelled, none still running. Before: 4 failed, 2 cancelled, 2 running. This is one short sample.
- **Release process:** new stable pipeline with tested SemVer tags, signed desktop builds and install/upgrade runs before publish; CODEOWNERS replaced the label gate.
- **Velocity and churn:** still extreme.
  - v0.22.0 (since v0.21.0): about 24,200 commits, 6,080 merged PRs, 2.0M insertions and 1.1M deletions.
  - v0.23.0 (since v0.22.0, about three weeks): about 10,080 commits, 1,950 merged PRs, 725k insertions.
  - v0.23.1 (four days): 2,578 commits, 618 merged PRs.
  - Issue and PR numbers exceed #155,000.
  - "God-file eradication" trackers #78647 and #78645 remain open.
- **Backlog:**
  - 19,105 open issues and 34,238 open PRs (before: 14,506 and 33,888).
  - 11,149 open issues labeled `type/bug` (before: 7,953).
  - Of the issues created since 2026-10-02, 6,447 are still open versus 1,232 closed. The release notes report thousands of issue closures per window, so many closures were of older issues.
  - "[Audit regression]" stream: no new issues with that title since the 2026-09-29 batch. Of the 18 in that batch (including tracker #127076), 13 are open and 5 closed; all 5 were closed on the day they were filed. #127093 to #127105 are mostly still open at P2.
- **Maturity:** feature-rich, heavily tested, and now released through a more disciplined channel. Churn is as high as before and the bug backlog grew by about 40%, so per-release stability is still something to verify with your own soak testing.

## 13. Docs and community [as of 2026-11-13]

- **Popularity:** about 263.2k stars and 59.3k forks.
- **Contributors:** about 6,347 per the contributors API, counting anonymous contributors. The v0.23.0 notes credit 666 contributors for that release.
- **License:** MIT.
- **Docs:** a Docusaurus site with 435 pages under `website/docs` and a Simplified Chinese translation tree. It covers user guides, a developer guide, provider integrations, security, and a per-plugin catalog section. READMEs exist in English, Chinese, Urdu and Spanish; SECURITY and CONTRIBUTING have Spanish versions.
- **Community:** Discord, r/hermesagent, weekly third-party newsletters and release digests, hosting-vendor guides.
- **Gaps:**
  - Docs drift persists: `README.md:26` still says session search uses "LLM summarization" and still lists Honcho in the built-in learning loop, though Honcho is a catalog plugin and session search makes no LLM calls.
  - The docs security page states the approval default but not the default terminal backend (fetched 2026-11-13).
  - Patch releases still defer their notes: v0.23.1 points to v0.24.0. The deferred notes did arrive for v0.22.0 and v0.23.0.
  - The v0.23.0 notes and the tree disagree on whether Yuanbao is bundled.

## 14. Self-improvement and learning loop

Still real, and re-verified in code. It is an LLM-driven nudge loop, not learning in any model-training sense.

- **Skill review:**
  - After each turn, if at least N tool iterations have run since the last skill review, the turn finalizer flags a skill review (`agent/turn_finalizer.py:882-888`) and spawns the background review after the reply is delivered (`agent/turn_finalizer.py:620-634`).
  - N is `skills.creation_nudge_interval`. The code default is 10 (`agent/agent_init.py:1236-1238`); the shipped example config sets 15 (`cli-config.yaml.example:1220`). Both values are the same as at the baseline commit; the last dossier reported only the code default.
  - The review **forks the agent** on the conversation snapshot with a prompt telling it to review the conversation and actively update the skill library (`agent/background_review.py:457`).
  - The fork can create or patch skills through `skill_manage` (`tools/skill_manager_tool.py`, with guards, a ledger of every mutation and rollback).
  - Memory review works the same way on turn intervals.
  - The fork reuses the parent's model and prompt cache and runs under a dispatch-side tool whitelist (`agent/background_review.py:1116-1156`). In-code comments still estimate about 30K tokens per review event; it is skipped for cron (`agent/turn_finalizer.py:621-623`).
  - New per the v0.22.0 notes: the review can improve any skill, and learnings go into the task's own skill.
- **New controls on the loop:**
  - `hermes curator lock` and per-skill locks stop autonomous rewrites of a hand-curated skill, even in YOLO mode (`tools/skill_manager_guards.py:224`).
  - Opt-in `skills.write_approval` stages every `skill_manage` mutation, including the background fork's, for human approval (default off, `hermes_cli/config_defaults.py:1465`).
- **Curator:** `agent/curator.py`, defaults at `hermes_cli/config_defaults.py:1481-1503`.
  - Idle-triggered: every 7 days, after 2h idle.
  - Marks agent-created skills stale after 14 days and archives them after 30 days of non-use; it never deletes automatically.
  - LLM consolidation is still off by default; a snapshot is taken before any consolidation pass.
  - New: bundled skills are no longer pruned by default (`prune_builtins: False`), after a case where 57 shipped skills vanished in one startup tick (comment in the defaults file). Hub-installed skills are never pruned.
- **Other learning features:** `learning_graph.py`, `learn_prompt.py`, `learning_mutations.py`.
- **How real:**
  - Autonomous skill authoring and refinement does happen.
  - Quality still depends on the model and the review prompt. There is still no evaluation gate on whether a new skill helps, beyond guards, locks and the optional approval queue.
  - The cost is non-trivial: about 30K tokens per review by the project's own estimate.
  - "Self-improving" still means accumulating procedural notes and prompts, not changing weights. `trajectory_compressor.py` and `batch_runner.py` remain offline data-generation tools.

---

## Honest summary

**Strongest 3**

1. **Breadth plus depth of the runtime.** 26 bundled messaging adapters plus a 92-entry platform shelf in the catalog, 6 core terminal backends plus Singularity and bubblewrap plugins, polished MCP client with spend limits, cron with delivery anywhere, pluggable subagent delegation, 39 bundled model providers and a managed local-model runtime. Since October it also unified every surface behind one gateway and added a browser Webapp.
2. **A real, code-verified learning loop, now with brakes.** Background-fork skill and memory review, FTS5 session recall and an idle curator are unchanged, and the window added skill locks, an optional approval queue and a mutation ledger with rollback.
3. **Ecosystem momentum and release engineering.** About 263k stars, about 6.3k contributors, a plugin catalog that went from zero to about 2,300 pinned entries, a tested stable release channel with signed packages, healthier CI on `main` in this sample, and an unusually candid SECURITY.md.

**Weakest 3**

1. **Security posture by default.** Nothing moved in the defaults: host execution, LLM-judged smart approval, in-process plugins. Mitigations added this window (bubblewrap, plugin host isolation, more approval patterns) are opt-in or heuristic. One new CVE (webhook route authorization bypass) and a set of dashboard-auth fixes landed, a user reported auto-approved destructive commands, and the external posture review was downgraded to P4 without a reply. The third-party surface an operator must review grew by about 2,300 catalog plugins.
2. **Churn and backlog.** About 19.1k open issues (up 32%), about 11.1k open bugs (up 40%), 34.2k open PRs, and 2,578 commits in the four days before v0.23.1. The audit-regression batch from September is mostly still open. Breaking changes arrived in consecutive releases (browser tools removed, MCP server command renamed, session-prune default flipped twice), and one stable release was built from the wrong commit.
3. **Cost and bloat.** The fixed prompt is lighter but still about 12K tokens per call by one third-party measurement; background reviews still cost about 30K tokens each; non-test Python is about 1.1M lines and the whole tree about 4M lines with tests; Desktop idle resource burn is still an open tracker; README claims still lag the code.

## Sources

- Repo: https://github.com/NousResearch/hermes-agent (commit 491f9845, 2026-11-13; baseline 7200e0f, 2026-10-02); `SECURITY.md`; `cli-config.yaml.example`; `hermes_cli/config_defaults.py`
- Releases: https://github.com/NousResearch/hermes-agent/releases (v0.21.6, v0.21.7, v0.22.0 to v0.22.7, v0.23.0, v0.23.1)
- GitHub API: repository, releases, search, Actions runs for `ci.yaml`, contributors, security advisories, issue timelines [as of 2026-11-13]
- Docs: https://hermes-agent.nousresearch.com/docs/ ; https://hermes-agent.nousresearch.com/docs/user-guide/security
- CVE-2026-114463: https://app.opencve.io/cve/CVE-2026-114463 ; fix PR https://github.com/NousResearch/hermes-agent/pull/115285 ; reporter's advisory https://github.com/pinarsadioglu/security-advisories/blob/main/webhook-route-authz-bypass.md
- CVE aggregator (third party): https://radicalnotion.ai/vendor/nousresearch/hermes-agent
- Smart-approval incident: https://github.com/NousResearch/hermes-agent/issues/141346
- Posture review issue #40889: https://github.com/NousResearch/hermes-agent/issues/40889
- Audit regression examples: https://github.com/NousResearch/hermes-agent/issues/127094 ; https://github.com/NousResearch/hermes-agent/issues/127076
- Desktop idle burn tracker: https://github.com/NousResearch/hermes-agent/issues/127647
- OpenClaw's comparison (competitor, reviewed Aug 2026): https://docs.openclaw.ai/start/why-openclaw/openclaw-and-hermes-agent.md
- Token and cost measurement (third party, updated 2026-10-22): https://www.dreamhost.com/guides/hermes-agent/hermes-agent-monthly-cost/
- Earlier token analysis (third party): https://codesota.com/agentic/hermes-agent
- v0.23.0 coverage (third party): https://runtimewire.com/article/nous-research-hermes-agent-v023-unified-gateway-browser-exec ; https://labmemo.com/?p=62787
- v0.22.1 rebuild coverage (third party): https://labmemo.com/?p=59283
- Earlier sources carried over: CSA note (2026-05-04) https://labs.cloudsecurityalliance.org/research/csa-research-note-hermes-agent-cves-20260504-csa-styled/ ; CVE-2026-10223 https://www.sentinelone.com/vulnerability-database/cve-2026-10223/
