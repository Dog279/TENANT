# Hermes Agent (Nous Research): capability dossier

Compiled 2026-10-02. Sources: a shallow clone of `NousResearch/hermes-agent` at commit `7200e0f` (2026-10-02 11:06 -0500), the GitHub API, the docs site, and third-party reporting. Repo paths are relative to the repo root. Anything marked [as of 2026-10-02] is a live count that will go stale.

## 0. Identification

- **Product:** "Hermes" as an OpenClaw competitor is Nous Research's **Hermes Agent** (github.com/NousResearch/hermes-agent, docs at hermes-agent.nousresearch.com). I found no other agent called "Hermes" that is a plausible match. Nous's Hermes *LLM* model family is a separate thing.
- **Evidence:**
  - The README markets it as "the self-improving AI agent".
  - It ships `hermes claw migrate`, which imports OpenClaw settings, memories, skills and keys (`hermes_cli/claw.py`).
  - OpenClaw's own docs have a comparison page (docs.openclaw.ai/start/why-openclaw/openclaw-and-hermes-agent, Aug 2026).
  - Trade press consistently pairs the two (Composio, Hostinger, HackerNoon, 2026).
- **Latest release:** **v0.21.5 (tag v2026.9.24), published 2026-09-24.** It rolls up about 460 PRs. Curated notes are deferred to v0.22.0.
  - Tags are date-based and roughly weekly: there are 36 releases since v2026.3.12.
  - The repo was created 2025-07-22. It is MIT licensed and mostly Python, with a TypeScript TUI, desktop app and web dashboard.
- **Business model:** venture-funded. Optional paid **Nous Portal** subscription (300+ models plus a "Tool Gateway" for search, image, TTS and cloud browser) and "Hermes Cloud" hosted deployments. Self-hosting with your own keys stays fully supported.

---

## 1. Messaging channels and surfaces

- **What exists:** a single `hermes gateway` process with platform adapters. It routes messages, approvals, cron delivery and cross-platform continuity.
  - **Platform adapters in `plugins/platforms/`:** telegram, discord, slack, whatsapp, signal (plus `gateway/platforms/signal.py`), email, sms, matrix, mattermost, teams, google_chat, irc, line, simplex, feishu, dingtalk, wecom, homeassistant, ntfy, a2a, buzz, photon, raft.
  - **Adapters in `gateway/platforms/`:** BlueBubbles (iMessage via a Mac relay), WhatsApp Cloud API, WeChat (weixin), QQ bot, Yuanbao, MS Graph webhook, generic webhooks, and an OpenAI-compatible `api_server`.
  - **Other surfaces:** full TUI (`ui-tui/`), a desktop app (`apps/desktop`, "Hermes Desktop"), a web dashboard (`web/`, `hermes_cli/web_server*.py`), ACP adapter for IDEs (`acp_adapter/`), MCP server mode (`mcp_serve.py`), Android/Termux APT package, and voice (wake word, STT/TTS, realtime voice).
- **Maturity:** Telegram, Discord, Slack and CLI are polished. The long tail ranges from prototype to working. Example: #97065 (open since 2026-08-28) is a gateway setup crash on one platform.
- **Gaps:**
  - iMessage needs BlueBubbles on a Mac; there is no native path.
  - Breadth is comparable to or greater than OpenClaw's roughly 23 channels, but per-adapter quality varies.
  - A path-traversal CVE hit the WeCom adapter (CVE-2026-7396, v0.8.0).

## 2. Tool execution

- **Shell:** `tools/terminal_tool*.py`.
  - Supports PTY, background processes with a registry, notifications and checkpoints (`process_registry*.py`), sudo handling, and `read_terminal` / `close_terminal`.
- **Files:** `file_tools*.py` and `file_operations*.py` cover read, write and patch, fuzzy-match patching, lint on write, write guards, read tracking, and checkpoints/rollback (`checkpoint_manager.py`).
- **Code execution:** `code_execution_tool.py`, `code_kernel*.py` (persistent kernel, remote kernel), and `code_execution_rpc.py`. Python scripts can call Hermes tools over RPC ("collapse multi-step pipelines into zero-context-cost turns").
- **Terminal backends:** seven, in `tools/environments/`: local, docker (with `docker_egress.py`), ssh, singularity, modal (plus a managed variant), daytona, vercel_sandbox. Modal and Daytona hibernate when idle.
- **Maturity:** polished, and the most mature area.
- **Gaps:**
  - The default backend is `local` (`cli-config.yaml.example:422`), which runs directly on the host.
  - The code-execution tool and MCP subprocesses run on the host *even when* a sandboxed terminal backend is selected (SECURITY.md §2.2).
  - Open audit regression #127094 (2026-09-29): sequential tool execution has no overall deadline.

## 3. Browser automation and computer use

- **What exists:**
  - **Local browser:** Playwright/CDP-based `browser_tool*.py`, with snapshot/accessibility, vision, dialogs, frames, a supervisor, and a real-profile mode.
  - **Engines and fallbacks:** Camofox (stealth Firefox) and Lightpanda.
  - **Cloud browsers:** Browserbase, Browser Use and Firecrawl backends (`plugins/browser/`).
  - **Other:** a browser vault tool and an extension router.
  - **Computer use:** desktop control in `tools/computer_use/` (cua backend, permissions, doctor, vision routing).
- **Maturity:** the browser is working-to-polished and actively churning; the latest commit is a `/browser connect` fix. Computer use works but is newer.
- **Gaps:**
  - Vision and computer-use quality depend on the model.
  - Cloud browser backends need third-party accounts (or the Nous Portal).

## 4. Memory and personalization

- **Built-in memory:** `tools/memory_tool.py`.
  - Two small curated files, `MEMORY.md` (agent notes) and `USER.md` (user profile). Default limits are 2200 and 1375 characters (about 800 and 500 tokens).
  - Both are injected as a **frozen snapshot** at session start, so the prompt cache stays intact.
- **Nudges:** periodic memory nudges every N turns (`memory.nudge_interval`, default 10). A background review fork decides what to save (`agent/background_review.py`).
- **Session search:** `tools/session_search_tool.py` does SQLite FTS5 search over all past sessions with discovery, scroll, read and browse modes. Today it makes **no LLM calls**; the README's "LLM summarization" wording is stale.
- **Pluggable external memory:** `plugins/memory/` includes mem0, byterover, holographic, openviking and retaindb.
  - **Honcho** (Plastic Labs dialectic user modeling, the headline README feature) is now an **external catalog plugin**: `hermes plugins install honcho` (`cli-config.yaml.example:1822-1829`).
- **Maturity:** works. The design is deliberately small and bounded.
- **Gaps:**
  - The built-in memory is tiny by design.
  - Per OpenClaw's own comparison (a competitor, Aug 2026), there is no source-session lineage or deletion tombstones on memory entries.
  - Memory-injection RCE: CVE-2026-10223 in `_scan_memory_content`, affecting versions ≤2026.4.30, published 2026-06-04.

## 5. Extensibility: skills, plugins, MCP, hub

- **Skills:** follow the agentskills.io / SKILL.md standard. 58 bundled and 152 optional skills (`skills/`, `optional-skills/`). Skills become `/skill-name` slash commands.
- **Skills Hub:** `tools/skills_hub*.py`. Sources include GitHub, ClawHub (OpenClaw's registry), skills.sh, LobeHub, well-known URLs and the official index.
  - Installs go through `skills_guard.py`, a regex static scan with trust tiers (builtin, trusted, community). Community skills are blocked on any finding unless `--force` is used.
  - The module itself documents gaps in what the scan can detect.
  - Some third-party write-ups claim Hermes "avoids marketplace risk because it writes its own skills". That is **false**: it can install from ClawHub and other public hubs.
- **Plugins:** `plugins/` with a loader. There is a plugin catalog (`plugin-catalog/`) and a Desktop plugin SDK. Plugins exist for model providers, platforms, memory, observability, image/video generation, kanban, Spotify, Google Meet and others.
  - Plugins are imported **in-process, unsandboxed** (SECURITY.md §2.3).
- **MCP:**
  - **Client:** extensive. Covers stdio and HTTP transports, OAuth (including device flow), sampling, health/liveness supervision, a schema cache and a dashboard OAuth flow (`tools/mcp_tool*.py`).
  - **Server:** `mcp_serve.py`.
  - **Tool search:** a `tool_search` tool exists for deferred tool loading.
- **Maturity:** polished.
- **Gaps:**
  - Plugin and skill code is unsandboxed.
  - The hub scan is regex-only.

## 6. Autonomy and scheduling

- **Cron:** a built-in scheduler (`cron/`) with job definitions, a delivery queue to any platform, retries for unreachable targets, quota holds, incidents, a monitor, detached workers, and scripts.
  - Jobs can be created in natural language through `cronjob_tools.py`.
  - Cron prompt scanning lives in `cronjob_prompt_scan.py`.
- **Other autonomy:**
  - A Kanban worker/orchestrator system (`tools/kanban_tools.py`, `plugins/kanban`) for multi-step autonomous task boards.
  - A periodic scheduler and idle-triggered **curator** (`agent/curator.py`).
  - Cron suggestions (`cron/suggestions.py`) and outbound webhooks.
- **Maturity:** polished.
- **Gaps:**
  - Third-party reports describe runaway unattended cost (for example, "a $47 surprise bill from an overnight run").
  - Cron skips the background review, partly on cost grounds.

## 7. Multi-agent and subagents

- **Delegation:** `tools/delegate_tool*.py`.
  - Isolated child agents with their own toolsets.
  - Parallel children: default max 10 concurrent.
  - Flat by default (`MAX_DEPTH = 1`); deeper nesting via `max_spawn_depth`.
  - Async delegation with durable records, per-subagent git worktrees (`subagent_worktree.py`), structured output schemas, a live log, and fallback providers for children.
- **Mixture-of-agents:** `agent/moa_loop.py`.
- **Cross-agent:** A2A platform adapter, plus an RFC for a generalized ACP client (#5257).
- **Maturity:** works to polished.
- **Gaps:**
  - Bots collaborating across gateways is still an open request (#97681).
  - Subagent approvals auto-deny by default.

## 8. Model flexibility

- **Providers:** about 40 provider plugins (`plugins/model-providers/`), including Anthropic, OpenAI/Codex, Gemini, Vertex, Bedrock, Azure Foundry, OpenRouter, xAI, DeepSeek, Kimi, Qwen, MiniMax, Z.ai, NVIDIA, HuggingFace, Copilot, Nous, Ollama Cloud and a generic `custom` provider.
- **Local models:**
  - LM Studio is first-class.
  - Ollama, vLLM and llama.cpp map to `custom` via an OpenAI-compatible endpoint.
  - Ollama `num_ctx` is auto-detected; parser quirks are documented (`cli-config.yaml.example:94-144`).
- **Routing and switching:**
  - `/model provider:model` hot-swaps models mid-session.
  - Credential pools, fallback cooldowns, an auxiliary model for side tasks, a `router` provider, and fallback chains.
- **Maturity:** polished.
- **Gaps:**
  - Prompt overhead is heavy. A third-party analysis (codesota.com) measured about 13.9K fixed tokens per call, and community threads ask why Hermes uses far more tokens than Claude Code.
  - Small local models struggle with the large tool surface.

## 9. Security

- **Official stance (SECURITY.md §2.2, a primary source):** "The only security boundary against an adversarial LLM is the operating system."
  - Approval gates, redaction, pattern scanners and allowlists are explicitly classed as *heuristics*.
  - The supported posture for untrusted inputs is whole-process wrapping, either with Hermes's Docker image or with NVIDIA OpenShell.
  - The **default is the `local` backend**, which runs on the host.
  - There is no bug bounty.
- **Approvals:** `tools/approval*.py`.
  - Dangerous-command detection plus human approval over CLI or messaging.
  - "Smart approval" uses an auxiliary LLM risk reviewer with injection hardening (`approval_smart.py`).
  - A `--yolo` mode exists.
  - Protected instruction files (AGENTS.md, SOUL.md and similar) always require approval, even under yolo.
  - DM pairing for gateway users.
  - Optional pre-exec scanning with `tirith` (cosign-verified binary).
  - Credentials are stripped from the environment passed to subprocesses (§2.3).
- **Incidents and criticism (dated):**
  - **2026-04-11:** a community audit (@Anic888, filed in public issues) reported 4 Critical and 9 High findings in the default config. Examples: yolo disables checks; the LLM auto-approval is prompt-injectable; the write sandbox is opt-in; hooks and plugins run arbitrary Python; the terminal guard is regex-only. Source: the CSA research note.
  - **2026-05-04:** a Cloud Security Alliance note, "9 CVEs in 4 days", covering Hermes and OpenClaw together. Hermes CVEs:
    - CVE-2026-7396: WeCom path traversal, v0.8.0.
    - CVE-2026-7397: symlink following in file tools, fixed v0.9.0.
    - CVE-2026-6829: path traversal in the separate hermes-webui project, fixed v0.50.34.
  - **Other CVEs:**
    - CVE-2026-7112: API server auth bypass in `check_auth`.
    - CVE-2026-10223: RCE via memory scan, ≤2026.4.30, published 2026-06-04.
    - CVE-2026-14617: `GatewayStreamConsumer._filter_and_accumulate`, ≤2026.4.30.
  - **2026-06-07:** issue #40889, an external posture review of v0.16.0. Findings: unauthenticated API when the key is unset, wildcard CORS, no rate limits, plaintext credentials on Windows, and three dependency CVEs. Labeled P3, unassigned, and still open.
  - **OpenClaw's comparison (Aug 2026, competitor-authored):** says Hermes "may auto-approve in headless contexts", lacks transcript-store redaction, and that Docker `:latest` follows main.
- **Maturity:** layered heuristics are polished. The default posture is permissive.
- **Gaps:**
  - Running on the host is the default.
  - In-process code paths are unsandboxed.
  - Security CVEs have recurred at a steady pace through 2026.

## 10. Observability and ops

- **What exists:**
  - **`hermes doctor`:** config, connectivity, live, platform, state and tools checks (`hermes_cli/doctor*.py`).
  - **`hermes status`:** status reporting.
  - **`hermes logs`:** log access (`hermes_logging.py`).
  - **Dashboard:** a web dashboard covering chat, cron, files, gateway, MCP, memory, messaging, OAuth, profiles and sessions (`hermes_cli/web_server_*.py`, `web/`).
  - **Usage and cost:** `/usage` and `/insights`.
  - **Backups:** backup/restore (`backup*.py`).
  - **Profiles:** multi-profile operation.
  - **Tracing:** Langfuse plugin (`plugins/observability/langfuse`).
  - **Updates:** `hermes update` is a large update subsystem with about 40 `update_*` modules covering fleet updates, restart recovery, Windows Store/AppInstaller channels and lock handling.
  - **Agent self-protection:** a startup watchdog, an e-stop, and SQLite state repair and WAL tooling (`hermes_state_*`).
- **Maturity:** polished but heavy. Desktop idle CPU/GPU/memory burn is an open tracker (#127647, 2026-09-29).

## 11. Install, setup UX and cross-platform

- **Install paths:**
  - Linux, macOS and WSL2: one-liner `curl … install.sh | bash`.
  - Native Windows: PowerShell one-liner, plus an MSIX/App Installer package.
  - Android: Termux signed APT repo with stable and canary channels.
  - Also Docker/Compose, Nix flake and Hermes Desktop bundles.
- **Setup:** the `hermes setup` wizard; `hermes setup --portal` uses OAuth so you skip collecting keys.
- **Supporting evidence:** CI has install E2E jobs for macOS, Windows and Termux.
- **Maturity:** polished.
- **Gaps:**
  - The README documents antivirus false positives on the bundled `uv.exe` on Windows.
  - The install is large: it pulls Python 3.14, Node, ripgrep, FFmpeg and more.

## 12. Engineering quality and reliability [as of 2026-10-02]

- **Code size:** Python, roughly 700k lines of `.py` and roughly 970k lines of TS/TSX, both counts including tests. About 17k files.
  - Issue #40889 measured about 246k LOC at v0.16.0 in June, so the codebase grew several-fold in about four months.
- **Tests:** about 5,400 Python test files and about 44k `def test_` functions. CI has about 50 workflows: OS matrix, install E2E, desktop E2E, Nix, OSV scanner, supply-chain audit, lockfile checks and others.
- **CI health:** the main `ci.yaml` on `main` has frequent red runs. Of the last 15 push runs on 2026-10-02: 4 failed, 2 cancelled, 2 still running.
- **Velocity and churn:** extreme.
  - v0.21.5 notes report **1,610 non-merge commits and 460 merged PRs in the roughly 3 days** after v0.21.4, touching 4,828 files (+164k/−149k).
  - Issue and PR numbers exceed #130,000.
  - Ongoing "god-file eradication" refactors are running (#78647, #78645).
- **Backlog:**
  - 14,506 open issues and 33,888 open PRs.
  - 7,953 open issues labeled `type/bug`.
  - Of the issues created since 2026-09-01, 4,277 are still open versus 3,197 closed.
  - A recurring stream of "[Audit regression]" issues reports previously fixed bugs coming back (for example #127093–#127105, 2026-09-29).
- **Maturity:** the project is feature-rich and heavily tested, but under enormous churn. That makes stability per release a real concern, though not a proven failure.

## 13. Docs and community [as of 2026-10-02]

- **Popularity:** about 250.7k stars and 53.7k forks.
- **Contributors:** about 4,158 per the contributors API, counting anonymous contributors.
- **License:** MIT.
- **Docs:** a Docusaurus site with about 450 pages (`website/docs`). It covers user guides, a developer guide, provider integrations, security and migration ("import from other agents"). READMEs exist in English, Chinese, Urdu and Spanish.
- **Community:** Discord, the r/hermesagent subreddit, and many third-party tutorials and comparisons.
- **Gaps:**
  - Docs drift: the README still says "LLM summarization" for session search and lists Honcho as built-in.
  - Release notes are sometimes deferred: v0.21.1–v0.21.5 point to the upcoming v0.22.0.

## 14. Self-improvement and learning loop

This is real, and it is verified in code. It is an LLM-driven nudge loop, not learning in any model-training sense.

- **Skill review:**
  - After each turn, if at least N tool iterations have run since the last skill review (`skills.creation_nudge_interval`, default 10), `agent/turn_finalizer.py:737-760` spawns a background daemon thread.
  - That thread **forks the agent** (`agent/background_review.py`) on the conversation snapshot with a prompt that begins "Review the conversation above and update the skill library. Be ACTIVE…".
  - The fork can create or patch skills through `skill_manage` (`tools/skill_manager_tool.py`, with linter, provenance, a usage ledger and guards).
  - Memory review works the same way on turn intervals.
  - The fork reuses the parent's model and prompt cache and runs under a tool whitelist. In-code comments estimate about 30K tokens per review event; it is skipped for cron.
- **Curator:** `agent/curator.py`.
  - Idle-triggered: default every 7 days, after 2h idle.
  - Deterministically marks skills stale after 14 days and archives them after 30 days of non-use; it never deletes.
  - Optional LLM consolidation is **off by default**. Pinned skills are exempt.
- **Other learning features:** `learning_graph.py` and `learn_prompt.py` give a visible learning view and a manual "learn" command.
- **How real:**
  - Autonomous skill authoring and refinement does happen.
  - Quality depends on the model and on the review prompt. There is no evaluation gate, beyond lint and guards, on whether a new skill actually helps.
  - The cost is non-trivial: about 30K tokens per review.
  - "Self-improving" means accumulating procedural notes and prompts, not changing weights. The trajectory export (`trajectory_compressor.py`, `batch_runner.py`) is for offline RL/data generation by researchers and is not an online loop.

---

## Honest summary

**Strongest 3**

1. **Breadth plus depth of the runtime.** Over 20 messaging platforms, 7 terminal backends (Docker, SSH, Modal, Daytona and others), polished MCP client and server, a cron system with delivery anywhere, subagents with worktrees, and about 40 model providers including local ones. It is arguably the most complete open personal agent surface as of Oct 2026.
2. **A real, code-verified learning loop.** Background-fork skill and memory review, FTS5 session recall, an idle curator that archives stale skills, and a pluggable memory backend (Honcho, mem0 and others). It is a genuine differentiator against OpenClaw's mostly hand-written skills.
3. **Ecosystem momentum and ops tooling.** About 250k stars and about 4k contributors, weekly releases, cross-platform installers (Windows native, Termux, Nix, Desktop), doctor, dashboard, backups and update tooling, plus an unusually candid SECURITY.md trust model.

**Weakest 3**

1. **Security posture by default.** It runs on the host by default, and in-process plugins, skills and code-exec are unsandboxed. The approval layer is heuristic and LLM-judged. About six or more CVEs were published in 2026 (RCE via memory, auth bypass, path traversals), and an external posture review has been left open at P3.
2. **Churn and backlog.** About 14.5k open issues and 33.9k open PRs. Roughly 1,600 commits landed in 3 days. `main` CI goes red frequently, and "[Audit regression]" issues show fixed bugs returning. Stability of any given release is hard to trust without your own soak testing.
3. **Cost and bloat.** A heavy fixed prompt (about 14K tokens per call, per third-party measurement), background reviews costing about 30K tokens each, an enormous codebase (about 1.7M LOC including tests), Desktop idle resource burn, and docs that lag the code.

## Sources

- Repo: https://github.com/NousResearch/hermes-agent (commit 7200e0f, 2026-10-02); releases page; SECURITY.md; cli-config.yaml.example
- Docs: https://hermes-agent.nousresearch.com/docs/
- CSA note (2026-05-04): https://labs.cloudsecurityalliance.org/research/csa-research-note-hermes-agent-cves-20260504-csa-styled/
- CVE-2026-10223: https://www.sentinelone.com/vulnerability-database/cve-2026-10223/
- CVE-2026-7396: https://www.sentinelone.com/vulnerability-database/cve-2026-7396/
- CVE-2026-7112: https://dbugs.ptsecurity.com/vulnerability/PT-2026-35390
- CVE-2026-14617: https://x.com/CVEnew/status/2073191209829388738
- Posture review issue #40889: https://github.com/NousResearch/hermes-agent/issues/40889
- OpenClaw's comparison (competitor, Aug 2026): https://docs.openclaw.ai/start/why-openclaw/openclaw-and-hermes-agent.md
- Composio comparison: https://composio.dev/blog/openclaw-vs-hermes-agent
- Token cost analysis: https://codesota.com/agentic/hermes-agent ; https://deuts.org/x/866-why-does-hermes-use-so-many-more-tokens-than-claude-code-r-hermesagent/
