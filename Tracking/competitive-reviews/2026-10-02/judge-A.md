# Judge A: power-user lens (most capable agent today: breadth, depth, autonomy)

Tenant is scored as the local build (main plus unpushed branches). Spot-checks: Tenant `internal/model` has no image/content-part types (so it has no vision input); Tenant `go test ./...` log is green; Hermes `tools/process_registry.py` and `tools/delegate_tool*.py` exist, with 22 platform adapters under `plugins/platforms`.

| # | Dimension | Tenant | OpenClaw | Hermes | Decisive evidence |
|---|---|---|---|---|---|
| 1 | Messaging channels/surfaces | 4 | 10 | 9 | Tenant: Discord + iMessage + TUI/web, no Telegram, Slack, WhatsApp or voice. OpenClaw: about 48 channel docs plus native iOS/Android apps. Hermes: 20+ adapters plus voice and desktop. |
| 2 | Tool execution | 4 | 9 | 9 | Tenant has no background processes, PTY, stdin or paged reads, and workdir/timeout exist only on a local branch. OpenClaw: pty, yieldMs auto-background, process tool, completion wake. Hermes: PTY, process registry, persistent code kernel, 7 terminal backends. |
| 3 | Browser and computer use | 3 | 9 | 8 | Tenant's chromedp is text/DOM only: screenshots never reach the model and there is no desktop control. OpenClaw: real-profile attach, remote CDP, `computer` tool on nodes. Hermes: Playwright + vision, cloud browsers, cua computer use. |
| 4 | Memory and personalization | 8 | 9 | 6 | Tenant: six tiers, hybrid FTS5 + vectors, profile, privacy/forget controls (brute-force vector scan). OpenClaw: active-memory recall agent, dreaming, standing intents. Hermes: small built-in memory (~800 + 500 tokens) plus FTS session search, with depth from plugins. |
| 5 | Extensibility (skills/plugins/MCP) | 6 | 9 | 9 | Tenant: solid MCP client with live `mcp_add`/trust, but no plugin API, no hub, and bundled skill scripts are not run. OpenClaw: typed plugin SDK, 163 extensions, ClawHub, ACP. Hermes: 210 skills, multi-source hub, polished MCP client/server. |
| 6 | Autonomy and scheduling | 6 | 9 | 8 | Tenant: cron + `/goal` with an LLM judge, delivery to Discord only, no heartbeat or webhook triggers. OpenClaw: heartbeat, webhook/Gmail/IMAP triggers, standing orders, exec-completion wakes. Hermes: cron delivering anywhere, Kanban orchestrator, curator. |
| 7 | Multi-agent and delegation | 6 | 8 | 8 | Tenant: in-process spawn plus a team bus and research fan-out, but no per-agent workspace or isolation. OpenClaw: isolated agents with bindings, swarm, ACP harnesses. Hermes: up to 10 parallel children, per-subagent git worktrees, MoA. |
| 8 | Model flexibility and local LLMs | 7 | 9 | 9 | Tenant has the best small-local-model tooling (family tool-call parsers, profiles, Supra router, fallback chain) but no multimodal input and about 10 presets. OpenClaw and Hermes: about 60 and about 40 provider plugins, first-class local runtimes, failover. |
| 9 | Security and safe defaults | 7 | 4 | 4 | Tenant: approval broker, info-flow taint tracking with adversarial tests, hash-chained log, no CVE record; but the local branch runs host exec by default. OpenClaw: sandbox off with no exec prompts by default, 722 GHSAs, 1-click RCE, ClawHub malware. Hermes: host by default, unsandboxed plugins, six or more CVEs. |
| 10 | Observability and ops | 8 | 8 | 7 | Tenant: log book with alerts and an LLM investigator, doctor --fix, encrypted backups, security audit, but no self-update. OpenClaw: OTel/Prometheus, doctor, `update` with rollback (upgrade breakage is common). Hermes: doctor/status/logs, Langfuse, update subsystem. |
| 11 | Install/setup UX and cross-platform | 5 | 8 | 8 | Tenant: static binary for 3 OSes and a setup wizard, but no installer, brew tap, systemd unit or Docker image, and Google needs your own OAuth client. OpenClaw and Hermes: one-line installers, onboarding, Docker/Nix, desktop/mobile apps (Termux and MSIX for Hermes). |
| 12 | Engineering quality and reliability | 8 | 5 | 4 | Tenant: about 152K LOC with 116K test LOC, 0 failing tests, -race CI on 3 OSes, arch tests, near-zero TODOs. OpenClaw: self-rated "Alpha 68%", about 9k open issues/PRs, broken upgrades. Hermes: main CI often red, 34k open PRs, "[Audit regression]" stream. |
| 13 | Docs, community and ecosystem | 2 | 9 | 9 | Tenant: private repo, 0 stars, one user, docs keyed to internal Jira IDs. OpenClaw: about 391k stars, a foundation, a huge doc site. Hermes: about 251k stars, about 4k contributors, about 450 doc pages. |
| 14 | Self-improvement loop | 5 | 7 | 8 | Tenant: distillation is on, but heuristic skill induction is human-gated, SoulNudge is off and lacks its fitness baseline, and outcome scoring exists only on a stale branch. OpenClaw: Skill Workshop in auto mode plus dreaming. Hermes: background-fork skill/memory review after every N tool iterations, plus the curator. |

## Overall (power-user weighting)

Weights (sum 100): Tool execution 12, Browser/computer use 10, Channels 9, Extensibility 9, Autonomy 9, Memory 8, Multi-agent 8, Model flexibility 8, Self-improvement 7, Security 5, Observability 5, Engineering 4, Install 3, Docs/community 3.

| Product | Weighted overall (/10) |
|---|---|
| **OpenClaw** | **8.4** |
| **Hermes** | **7.8** |
| **Tenant** | **5.5** |

Rationale: a power user wants the most capability right now. OpenClaw and Hermes both deliver background exec, vision browsing and computer use, more than 20 channels and a skill ecosystem. Under this lens their weak security defaults and churn cost them only a little, which is why those dimensions carry low weight.

## Where Tenant sits competitively and the 3 highest-leverage gaps

Tenant is the best-engineered and most security-conscious of the three: it has disciplined tests, real info-flow defenses, deep operator tooling and the strongest small-local-model handling. On raw capability, though, a power user gets a clearly narrower agent, roughly a generation behind OpenClaw and Hermes on what the agent can actually do. The three highest-leverage gaps are:
1. **Execution parity:** ship background processes, PTY and stdin, paged file reads, and land exec-parity on main.
2. **Multimodal and computer use:** feed screenshots and images to vision-capable models, so browsing stops being DOM-only, then add desktop control.
3. **Reach:** add Telegram/Slack/WhatsApp channels and a way to install community skills/plugins (including running bundled skill scripts) instead of compiled-in Go plugins only.
