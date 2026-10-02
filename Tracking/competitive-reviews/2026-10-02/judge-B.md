# Judge B: Security and Reliability Engineer

**The question for this lens:** which of these would I trust on a home server, with access to email, files and a shell, running unattended for months?

**Method.** I read all three dossiers in full and spot-checked the decisive claims myself:

- **Tenant, interactive exec.** On the local branch, interactive surfaces run on the host. Cron stays confined. Source: `cmd/tenant/execprofiles.go:303-313`.
- **Tenant, unauthenticated SSE.** The legacy SSE gateway has no auth. It refuses to bind anything but loopback unless you pass `--insecure-lan`. Source: `commands.go:358-386`.
- **Tenant, dashboard.** It listens on `127.0.0.1:8770` by default.
- **OpenClaw, exec.** `exec.security` defaults to `full` and `ask` to `off`. Source: `docs/tools/exec-approvals.md:229-238`.
- **OpenClaw, advisories.** 722 published GHSAs. Source: `ghsa.tsv`.
- **Hermes, terminal.** The default backend is `local`. Source: `cli-config.yaml.example:422`.

Tenant was scored as the local build: main plus the unpushed branches.

## Scores

| # | Dimension | Tenant | OpenClaw | Hermes | Justification (decisive evidence) |
|---|---|---|---|---|---|
| 1 | Messaging channels and surfaces | 3 | 10 | 9 | **OpenClaw:** about 48 channel docs, with Discord rated Stable. **Hermes:** more than 20 adapters plus voice. **Tenant:** only Discord and iMessage (native iMessage is macOS-only), plus TUI, web and CLI. No Telegram, Slack, Signal or voice. |
| 2 | Tool execution (shell, files, code, background processes) | 4 | 9 | 9 | **OpenClaw and Hermes:** both have PTY, a background-process registry with poll and kill, and code runners. Hermes adds 7 terminal backends. **Tenant:** no background processes, no PTY, and file reads cap at 64 KB with no paging. The 120s/600s timeout and workdir support exist only on an unpushed branch; main is fixed at 60s. |
| 3 | Browser and computer use | 3 | 9 | 8 | **OpenClaw and Hermes:** both offer vision-driven browsing and desktop computer use. **Tenant:** chromedp text/DOM browsing only. The model cannot see `web_screenshot` output and there is no desktop control. |
| 4 | Memory and personalization | 7 | 8 | 6 | **Tenant:** tiers T0–T5, an append-only archive, hybrid FTS and vector search, and per-class retention with "forget everywhere". That is the best privacy story, but vector search is a brute-force scan and it is proven with only one user. **OpenClaw:** richest design (active recall, dreaming, standing intents), but one reviewer reports losing 8 months of memory in an upgrade. **Hermes:** tiny built-in memory, and CVE-2026-10223 was an RCE through the memory scan. |
| 5 | Extensibility (skills, plugins, MCP) | 5 | 8 | 8 | **Tenant:** a solid MCP client (OAuth 2.1, live `mcp_add`, per-connector trust), and agent-written skills start untrusted. It has no hub, and bundled skill scripts are listed but never run. **OpenClaw:** a huge plugin SDK, but ClawHub shipped 341 or more malicious skills. **Hermes:** a hub with a regex guard, but plugins run in-process and unsandboxed. |
| 6 | Autonomy and scheduling | 6 | 9 | 8 | **Tenant:** cron with wall-clock caps and untrusted-data prompts, plus `/goal` with a judge. It has no heartbeat and no event triggers. **OpenClaw:** heartbeat, webhook/Gmail/IMAP triggers and approval grants. **Hermes:** cron that delivers anywhere, plus Kanban and a curator. |
| 7 | Multi-agent and delegation | 5 | 8 | 8 | **Tenant:** in-process goroutines with budgets and info-flow tracking that crosses agents, but no per-sub-agent workspace or sandbox. **OpenClaw:** isolated agents and workspaces, plus swarm. **Hermes:** worktree per sub-agent; sub-agent approvals auto-deny. |
| 8 | Model flexibility and local LLMs | 7 | 9 | 8 | **Tenant:** best-in-class engineering for small local models (text-protocol tool parsers, model profiles, health-gated fallback, a small-model router). It has few hosted presets and no multimodal input. **OpenClaw:** about 60 providers, but self-rates local models as Alpha. **Hermes:** about 40 providers, but a fixed prompt of about 14K tokens strains local models. |
| 9 | Security and safe defaults | **6** | **3** | **4** | See the security section below. |
| 10 | Observability and ops | 8 | 8 | 7 | **Tenant:** log book with alert rules and an LLM investigator, a 1.5K-line doctor, age-encrypted backups with verify and restore, a hash-chained security log, and `security audit`. It has no self-update and no OpenTelemetry. **OpenClaw:** OpenTelemetry, Prometheus, and `update` with rollback, but upgrades are its most-cited breakage. **Hermes:** doctor, backups and Langfuse; the dashboard is heavy and burns resources while idle. |
| 11 | Install, setup UX and cross-platform | 4 | 8 | 8 | **Tenant:** one static binary for 3 OSes × 2 architectures, with signed checksums and SBOMs. But there is no installer, no Homebrew, no Docker image, no systemd unit, and Gmail needs your own GCP OAuth client. **OpenClaw and Hermes:** one-line installers, desktop and mobile apps, and Docker/Nix. Both rely on pipe-to-shell installs. |
| 12 | Engineering quality and reliability | 7 | 5 | 4 | **Tenant:** about 116K test lines for about 152K code lines, 0 failures across 4,149 local tests, a 3-OS CI with `-race`, architecture tests, and roughly zero TODO debt in code. Reliability evidence comes from one operator, and the latest work sits on unpushed branches. **OpenClaw:** self-rated "Alpha 68%", with widely reported broken upgrades and about 9K open issues and PRs. **Hermes:** `main` CI often red, "[Audit regression]" issues show fixed bugs returning, 1,610 commits landed in 3 days, and about 34K PRs are open. |
| 13 | Docs, community and ecosystem | 2 | 9 | 9 | **Tenant:** a private repo with 0 stars and one user, so no external eyes on its security. Its internal docs are good (as-built ARCHITECTURE, ADRs), but they rely on Jira IDs outsiders cannot open. **OpenClaw:** about 391K stars and a funded foundation. **Hermes:** about 251K stars and about 450 doc pages. |
| 14 | Self-improvement loop | 5 | 6 | 7 | **Tenant:** distillation runs by default. Skill induction is heuristic and gated by the operator, and SoulNudge is off and fails closed. Safe, but weak. **OpenClaw:** `auto` self-learning edits skills directly, with no proposal or rollback snapshot, which is a risk for this lens. **Hermes:** a real background review fork that writes skills, plus a curator. It costs about 30K tokens per review and has no eval gate. |

### Security and safe defaults (row 9)

**Tenant: 6**

Strengths:
- Approval categories (exec, write, destructive, send, egress, MCP) gate actions by default.
- Sensitive-data information-flow tracking, with adversarial tests.
- A hash-chained Security log.
- Network surfaces bind to loopback, with a hard refusal for unauthenticated binds.
- Secrets files are 0600 or protected by a DACL.

Weaknesses:
- On the local branch, interactive exec runs on the host.
- Linux has no confinement without Docker.
- Tool results get no general spotlighting or delimiting.
- No outside party has ever audited it.

**OpenClaw: 3**

- Out of the box the sandbox is off, and exec runs as `full` with `ask` off. In other words, it runs host shell commands with no prompt at all.
- 722 GHSAs, 263 of them high or critical, including a one-click RCE.
- Tens of thousands of gateways were exposed, and its registry carried malware.
- In its favour: the hardening toolkit (Docker, Podman and OpenShell sandboxes, hash-bound allowlists) and its incident response are the best of the three.

**Hermes: 4**

- The default terminal backend runs on the host.
- Plugins, skills and code execution are unsandboxed even when the terminal is sandboxed.
- Approvals are heuristic or judged by an LLM.
- About 6 CVEs in 2026, and the posture review (#40889) has been open at P3 since June.
- In its favour: a candid SECURITY.md trust model, and dangerous commands prompt by default.

## Overall (weights for an unattended home server)

Weights:

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

| Product | Overall (weighted) | Unweighted mean |
|---|---|---|
| **OpenClaw** | **6.4** | 8.1 |
| **Hermes** | **6.1** | 7.6 |
| **Tenant** | **6.0** | 5.1 |

**Reading the result.** Even with security and reliability weighted heavily, OpenClaw edges ahead because it can be hardened properly. It has real sandbox backends, approvals bound to executable hashes, and the best operations tooling. Its defaults and track record are the worst of the three, but a careful operator can lock it down.

Tenant has the most trustworthy defaults and engineering of the three. Its narrow capability, single-user evidence and lack of outside scrutiny cancel most of that advantage. I deliberately did not reward its small attack surface, because that surface is small partly because it does less.

Hermes is held back by churn, CI that often goes red on `main`, and in-process plugins and code execution with no sandbox.

## Where Tenant sits, and its 3 highest-leverage gaps

Tenant is the most conservative and best-instrumented agent here. It has approvals on by default, information-flow tracking, a tamper-evident log, verified backups and a strong test suite. For a security-minded operator, that makes it the easiest to reason about. But its capabilities are a generation behind OpenClaw and Hermes, and its security claims have never been tested by anyone outside the author.

The three highest-leverage gaps:

1. **Make confinement real by default without giving up Docker.** Today interactive exec runs on the host. Ship a working Linux confinement path (or turn the Docker backend on automatically when Docker is present), spotlight or delimit tool results in the main prompt, and add a heartbeat watchdog for unattended runs.
2. **Close the exec and browse parity gaps.** Add background processes, PTY, paged file reads and vision input for screenshots. Merge the stale unpushed branches so the reliability evidence covers the code that actually ships.
3. **Go public and get outside eyes.** Ship installers (Homebrew, a Docker image, a systemd unit) and a skills catalog. Then commission or invite an outside security review. Today the security story rests on one author's tests.
