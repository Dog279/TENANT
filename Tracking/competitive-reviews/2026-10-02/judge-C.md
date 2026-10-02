# Judge C: industry analyst / buyer's guide lens

Lens: a technical newcomer choosing an agent to self-host **today**. I weight real-world usability, total cost of ownership (token overhead, maintenance and upgrade pain) and maturity. Tenant is scored as the local build (main plus unpushed branches), but "can a newcomer get and run it" still counts against it.

Spot checks: `cmd/tenant/execprofiles.go:303-313` confirms interactive exec runs on the host on the local branch, with cron staying confined. `hermes/cli-config.yaml.example:422` confirms Hermes defaults to `backend: "local"`. The dossiers held up on both.

## Scores (1–10)

| # | Dimension | Tenant | OpenClaw | Hermes | Justification (decisive evidence) |
|---|---|---|---|---|---|
| 1 | Messaging channels/surfaces | 4 | 10 | 9 | Tenant has two chat channels (Discord, iMessage) plus a TUI, web dashboard and CLI. There is no Telegram, Slack, WhatsApp, Signal or voice. OpenClaw has about 48 channel docs plus native iOS/Android apps; Hermes has 20+ adapters, voice and Termux. |
| 2 | Tool execution | 5 | 9 | 9 | Tenant has no background processes, PTY or stdin, and 64 KB file reads cannot be paged. Workdir and timeout are only on an unpushed branch. OpenClaw has `exec` with pty, yield and background plus a `process` tool. Hermes has PTY, a process registry, a persistent code kernel and 7 terminal backends. |
| 3 | Browser & computer use | 4 | 9 | 8 | Tenant's chromedp browser is text and DOM only: the model never sees screenshots and there is no desktop control. Both rivals have vision browsing, real-profile attach and computer-use tools. |
| 4 | Memory & personalization | 8 | 8 | 6 | Tenant has T0–T5 tiers, hybrid FTS5 and vector retrieval, and privacy controls (retention, forget-everywhere, export). Its vector search is brute force and the embedding endpoint is a separate setup step. OpenClaw has Active Memory, Dreaming and standing intents. Hermes's built-in memory is about 1.3K tokens by design and Honcho is now an external plugin. |
| 5 | Extensibility (skills/plugins/MCP) | 6 | 9 | 9 | Tenant's MCP client is solid (OAuth 2.1, live `mcp_add`, per-connector trust), but there is no hub, skill scripts are not executed, and it exposes only 2 MCP server tools. The rivals have a plugin SDK, a 163-plugin or 210-skill catalogue and a public hub (ClawHub's malware history is scored under #9). |
| 6 | Autonomy & scheduling | 6 | 9 | 8 | Tenant has cron, `/goal` with an LLM judge, and an improvement scheduler, with Discord-only delivery and no heartbeat or event triggers. OpenClaw has cron, webhooks, Gmail and IMAP triggers, a heartbeat and exec-completion wakes. Hermes has cron with delivery anywhere, Kanban workers and a curator. |
| 7 | Multi-agent/delegation | 6 | 8 | 8 | Tenant has `spawn_agent`, a team bus, 5 specialists, budgets and info-flow propagation across agents, but no per-sub-agent workspace or sandbox. Hermes has worktree-isolated children; OpenClaw has isolated agents and swarm fan-out. |
| 8 | Model flexibility & local LLM | 7 | 9 | 9 | Tenant is best of the three for weak local models: text-protocol parsers by model family, model profiles, a fallback chain and a Supra router. It has no Gemini, OpenRouter or Bedrock presets and no multimodal input. The rivals have 40–60 providers, and their local support works but strains small context windows. |
| 9 | Security & safe defaults | 6 | 4 | 4 | Tenant runs exec on the host but every exec, write and send goes through approval prompts, plus info-flow exfiltration checks, a hash-chained audit log and signed releases. Against it: no Linux confinement without Docker, an unauthenticated legacy SSE gateway, and no external scrutiny yet. OpenClaw defaults to `exec.security=full`, `ask=off`, sandbox off, and has 722 GHSAs including a 1-click RCE. Hermes runs local by default with unsandboxed plugins and 6+ CVEs. |
| 10 | Observability & ops | 8 | 8 | 8 | Tenant has an excellent log book, dashboard, a 1.5K-line doctor and encrypted verified backups, but **no self-update**. OpenClaw has OTel, Prometheus and `openclaw update` with rollback, but upgrades are its top complaint. Hermes has doctor, Langfuse and a large update subsystem, but Desktop burns resources while idle. |
| 11 | Install/setup UX & cross-platform | 4 | 8 | 9 | Tenant is a clean static Go binary for 3 OSes, but the repo is private. It has no installer, brew, Docker image or systemd unit, and Google Workspace needs your own GCP client. OpenClaw has one-liners, an onboarding wizard and apps, with immature native Windows. Hermes has one-liners, MSIX, Termux, Nix, Desktop and `--portal` key-less setup. |
| 12 | Engineering quality & reliability | 7 | 6 | 5 | Tenant has 4,149 passing tests and 0 failures, a 3-OS `-race` CI and arch tests. Its reliability evidence comes from one operator, and several headline fixes sit on unpushed branches. OpenClaw self-rates as Alpha (68%), with broken-upgrade and data-loss reports and about 9K open issues and PRs. Hermes's main CI is often red, it has "[Audit regression]" issues, and about 48K open issues and PRs. |
| 13 | Docs/community/ecosystem | 2 | 9 | 9 | Tenant has good internal docs (ARCHITECTURE, ADRs, runbooks) but no public repo, users, forum or contributors, and its docs depend on TEN-xxx IDs. The rivals have 250K–390K stars, thousands of contributors, Discord communities and docs sites of about 450 pages. |
| 14 | Self-improvement loop | 5 | 7 | 8 | Tenant has distillation on, but skill induction is heuristic and human-gated, SoulNudge is off and fails closed (no fitness baseline shipped), and outcome scoring is unmerged. Hermes's background-fork skill and memory review plus its curator are verified in code (about 30K tokens per review). OpenClaw's Skill Workshop `auto` mode bypasses rollback. |

## Overall

Weights for this lens (sum 100):
- Install/setup: 12
- Engineering reliability and upgrade pain: 12
- Security defaults: 10
- Docs/community: 10
- Channels: 8
- Tool execution: 8
- Model flexibility and local LLM: 8
- Observability: 7
- Memory: 6
- Extensibility: 6
- Autonomy: 5
- Browser: 4
- Multi-agent: 2
- Self-improvement: 2

| | Weighted | TCO adjustment | **Overall** |
|---|---|---|---|
| OpenClaw | 7.91 | −0.2 (dreaming, heartbeat and self-learning are on by default and burn tokens; broken in-place upgrades) | **7.7** |
| Hermes | 7.64 | −0.15 (about 14K fixed prompt tokens per call; about 30K tokens per background review) | **7.5** |
| Tenant | 5.48 | 0 (about 8.3K-token baseline, background LLM jobs mostly off, lazy tool loading; offset by manual binary upgrades) | **5.5** |

Buyer's verdict: today a newcomer should pick **OpenClaw** if reach and channels matter most, but harden its defaults first (sandbox on, `exec.ask`). They should pick **Hermes** for the easiest install and the most real self-improvement. **Tenant is not yet a buyable option** because it is not public.

## Where Tenant sits and the 3 highest-leverage gaps

Tenant is a disciplined, low-overhead, local-model-first appliance. It beats both incumbents on engineering hygiene, operator tooling, token cost and safe-by-prompt defaults, but it trails badly on reach, execution parity and ecosystem, so for a buyer it is a niche "careful operator" choice rather than a mainstream contender. The highest-leverage gaps are:
1. **Ship publicly with a real install path:** a public repo, an install script, a brew tap or Docker image, a systemd unit and self-update. Install, docs and community together carry 32% of this lens's weight and currently score 2–4.
2. **Close execution and vision parity:** merge exec-parity, add background processes, PTY and paged reads, and add multimodal input so the browser can actually see.
3. **Broaden channels:** add Telegram and Slack (and ideally WhatsApp and Signal) so it can reach users outside Discord and macOS iMessage.
