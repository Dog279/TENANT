# Judge D: independent model (the DGX stack), no lens

**Who this judge is.** A different model from judges A, B and C: `DeepSeek-V4.1-Flash-1017-NVFP4`, served by vLLM on the
operator's DGX machine (the `dgx` provider in Tenant's own configuration). It was added on 2026-11-13 so that one
scorecard comes from a model that did not write the dossiers or the other scorecards. There is no Judge D in the
2026-10-02 review.

**Lens.** None. All 14 capabilities carry equal weight.

**Method.** One call to the model's chat API with the judging rubric and the three 2026-11-13 dossiers in full
(38,765 prompt tokens; 11,957 completion tokens, of which 9,917 were reasoning; temperature 0; 682 seconds). It saw no
other judge's scores and no scores from 2026-10-02, so it is not anchored to the previous run. The text from "Scores"
down is the model's reply, unedited.

**Limits.** This judge could not open the code or the web, so it made no spot-checks: it scores only what the dossiers
say. Its three means were re-added from its own table and are right (83, 115 and 113 out of 140).

## Scores

| # | Dimension | Tenant | OpenClaw | Hermes | Decisive evidence |
|---:|---|---:|---:|---:|---|
| 1 | Messaging channels and surfaces | 5 | 9 | 9 | Tenant has polished TUI/CLI/dashboard, Discord, iMessage, MCP and a JSON API, but only two chat channels. OpenClaw catalogues 33 channels plus native apps and voice, though long-tail quality lags the top five. Hermes bundles 26 adapters with a 92-entry platform catalog, voice and Webapp, but iMessage depends on BlueBubbles or a catalog plugin. |
| 2 | Tool execution (shell, files, code, background processes) | 7 | 9 | 9 | Tenant added workdir, timeout, background processes, paged reads, atomic writes and command rules, but still lacks PTY and a dedicated code runner. OpenClaw is very dense: PTY, durable background, process tool, apply_patch, Code Mode, node_exec and cloud workers. Hermes has PTY, background notify, persistent kernels, six core backends plus bubblewrap/Singularity plugins, though its default backend is local and the no-overall-deadline regression remains open. |
| 3 | Browser and computer use | 5 | 9 | 8 | Tenant does text/CSS browser control and screenshot vision input, but no coordinate clicking and no desktop computer use. OpenClaw has dedicated and user Chrome profiles, remote CDP, sandboxed browser, Chrome extension, and computer-use nodes/cloud desktops. Hermes moved to browser_exec with vault sign-in, cloud browsers, cua computer use and Bot Screen, but the breaking removal of the old browser tools is a maturity caveat. |
| 4 | Memory and personalization | 7 | 8 | 6 | Tenant has tiered memory, FTS plus embeddings, privacy controls and memory tools, but embeddings need a separate endpoint and vector search is brute force. OpenClaw has Markdown memory, active memory, dreaming on by default and the Knowledge plugin, though three overlapping memory plugins compete. Hermes’s built-in memory is deliberately tiny at 2200/1375 characters, alongside FTS5 session search and a large pluggable catalog. |
| 5 | Extensibility (skills, plugins, MCP) | 6 | 9 | 9 | Tenant’s MCP client/server is solid, but it has no third-party plugin API, no skills hub/registry and no skills progress. OpenClaw has 186 bundled plugins, 52 skills, MCP relay/events and ClawHub, though ClawHub has a bad security history. Hermes has 44 bundled and 141 optional skills, a 2,311-entry plugin catalog, skills hub and MCP client, although plugins load in-process by default. |
| 6 | Autonomy and scheduling | 6 | 9 | 9 | Tenant has cron, goals, serve and service install, but no event triggers, webhook/file-watch or heartbeat. OpenClaw has automations, webhooks, Gmail/IMAP triggers, MCP events, heartbeat migration and Advisor. Hermes has robust cron with cancel, missed-run recovery and self-healing, plus kanban, curator, goals/loops, webhooks and unattended hard stops. |
| 7 | Multi-agent and delegation | 6 | 9 | 9 | Tenant has spawn_agent/team_await event-driven delegation, but no per-subagent workspace/sandbox and run limits are off by default. OpenClaw has subagents, Swarm on by default, A2A/Reef, UI panels and device-folder sessions. Hermes has pluggable delegation, worktrees, live steer/stop, mixture-of-agents and Group Chats, with a known open cross-gateway collaboration gap. |
| 8 | Model flexibility and local LLMs | 8 | 9 | 9 | Tenant supports vLLM/OpenAI-compatible, Anthropic, Claude Code and many local presets/tool formats, but lacks first-class Gemini, OpenRouter, Bedrock, Vertex and Azure. OpenClaw has 76 provider plugins, many named models, local runtimes and failover/routing. Hermes has 39 provider plugins, managed llama.cpp, LM Studio, hot-swap and fallback chains, though prompt overhead remains high. |
| 9 | Security and safe defaults | 6 | 3 | 4 | Tenant approvals ask on local/Discord and deny on iMessage, with audit and a hardened opt-in container, but interactive surfaces and Linux/Windows cron run on the host by default. OpenClaw defaults sandbox off, host exec to full and ask off, and has 722 GHSAs despite extensive opt-in defenses. Hermes defaults to local execution, smart LLM approval and in-process plugins, with one new CVE and a reported auto-approved destructive command. |
| 10 | Observability and ops | 9 | 7 | 8 | Tenant has log book, dashboard, doctor, backups, hash-chained security log, self-update rollback, service installer and shutdown recovery, though update is unexercised against a public release. OpenClaw has Control UI, doctor, diagnostics/OTel/Prometheus and PostgreSQL, but nine stable-line releases in the window fix blocked/stuck updates. Hermes has doctor/status/logs/prompt-size, backups, stable updates and hermes pm, but Desktop idle burn remains open. |
| 11 | Install, setup UX and cross-platform | 3 | 8 | 8 | Tenant’s install scripts, service units, single binary and three-OS packaging exist, but the repo is private and the “Latest” release lacks scripts, so public one-liners 404. OpenClaw has one-line installers, npm, onboard daemon, desktop/mobile apps and many platform guides, but Windows remains Alpha and pipe-to-shell persists. Hermes has one-liners, signed MSIX/macOS packages, guided onboarding and locales, but support is narrower and recent release slips occurred. |
| 12 | Engineering quality and reliability | 8 | 7 | 7 | Tenant’s Go suite passed 4,488 tests with 0 failures on Windows, 647 test files, clean vet/build and architecture tests, though it is one-operator and field defects surfaced. OpenClaw has 129 workflows and about 22k test files, but main is 17.5k commits ahead of stable and self-rated maturity remains 68% Alpha. Hermes has about 58k test functions and a healthier CI sample, but open issues and bugs grew sharply and one stable release was built from the wrong commit. |
| 13 | Docs, community and ecosystem | 3 | 10 | 9 | Tenant has a credible 43-page public docs site, but the GitHub repo is private, there are no community files or users, and docs link to unavailable installs. OpenClaw has a huge docs tree, 394k stars, about 3.8k contributors, Foundation/ClawHub and a community forum. Hermes has 435 docs pages, 263k stars, about 6.3k contributors, Discord and a large catalog, though docs drift persists. |
| 14 | Self-improvement loop | 4 | 9 | 9 | Tenant’s improve code is byte-identical, skill induction lacks outcome scoring, and SoulNudge/reflection remain off by default with no fitness baseline shipped. OpenClaw defaults self-learning to auto and Skill Workshop 2.0 adds version history, undo and guard rails. Hermes has a code-verified LLM background review, curator, locks, optional write approval and a mutation ledger, but no evaluation gate and about 30K tokens per review. |

## Overall

| Product | Mean |
|---|---:|
| Tenant | 5.9 |
| OpenClaw | 8.2 |
| Hermes | 8.1 |

## Where Tenant sits and the 3 highest-leverage gaps

Tenant sits last at 5.9, roughly two points behind OpenClaw and Hermes. Its relative strengths are observability/ops, engineering discipline, local-model flexibility and safer default approvals than the two leaders, but it is dragged down by non-deliverability, thin ecosystem/reach and a stalled learning loop.

1. Make Tenant publicly obtainable and installable: publish the repository and a non-prerelease with install assets, and fix docs/install links so outsiders can actually run it.
2. Broaden reach and third-party surface: add major chat channels and a real skills/plugin registry or catalogue; two chat channels plus no hub leaves messaging and extensibility far behind.
3. Close the learning loop: ship the fitness baseline, add outcome-based skill scoring/retrieval gating, and make self-improvement defaults usable rather than leaving the headline jobs off.

## Where I am least sure

- The security-default scores are the hardest: Tenant 6, Hermes 4, OpenClaw 3 requires balancing extensive opt-in defenses and fix rates against very permissive defaults, large advisory history and live incidents.
- Tenant’s install score of 3 is judgment-heavy: the installer/service mechanics are strong in code, but for an independent user the private repo and 404 one-liners dominate the actual experience.
- The browser and extensibility boundaries are close: Hermes browser use at 8 versus OpenClaw at 9, and both extensibility at 9, depend on weighting breaking tool removals and unsandboxed default plugins against massive catalogue breadth.
