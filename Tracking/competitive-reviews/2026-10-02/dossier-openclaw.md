# OpenClaw capability dossier (as of 2026-10-02)

Researcher stance: neutral. Primary evidence is a shallow clone of `openclaw/openclaw` at commit `32e30e5` (2026-10-02 12:01 -0400), the GitHub API, and the in-repo docs (`docs/`, mirrored at docs.openclaw.ai). Secondary sources: security press and research, and user reviews. Repo paths below are relative to the clone root.

**Important caveat on "maturity":** OpenClaw publishes its own maturity scorecard (`qa/maturity-scores.yaml`, rendered at `docs/maturity/scorecard.md`, last scored 2026-09-19). It is self-assessed, and the run metadata says `by: codex`, so an AI agent produced the scores. I quote those scores as "self-rated" and set my own maturity call beside them.

## Snapshot facts (dated)

| Fact | Value | Source / date |
|---|---|---|
| Latest stable release | **v2026.9.7**, published 2026-09-30 | GitHub releases API |
| Latest extended-stable ("LTS-equivalent") | **v2026.8.35**, 2026-10-02 (end-of-August code plus security and reliability fixes) | release notes |
| Versioning | CalVer `YYYY.M.N`; channels: stable (`latest`), extended-stable, beta, dev | `docs/install/development-channels.md` |
| Release scale | 2026.9.7 alone: "2,818 pull requests, 518 direct commits, 344 contributors" | `CHANGELOG/2026.9.7.md` |
| Stars / forks | ~391k / ~82k | GitHub API, 2026-10-02 |
| Open issues / open PRs | ~5,918 / ~3,255 (API `open_issues_count` 9,173 = both combined) | GitHub search API, 2026-10-02 |
| Contributors | ~3,470 (anon-inclusive, from API pagination) | GitHub API |
| Repo created | 2025-11-24 (current repo; it carried the earlier Clawdbot/Moltbot names) | GitHub API |
| License | MIT (GitHub shows NOASSERTION because of added notices) | `LICENSE`, `package.json` |
| Language | TypeScript on Node 24.16+/26 (pnpm monorepo), Swift (macOS/iOS, ~1.5k files), Kotlin (Android, ~600 files), small Rust crates (`crates/openclaw-gateway-client`, `openclaw-node-host`) | clone |
| Size | ~51.7k files and ~908 MB checkout; ~22.8k non-test TS files and ~4.3M non-test TS/TSX lines across `src/ extensions/ packages/ ui/`; ~18.9k `*.test.ts` files | clone counts |
| Bundled extensions | 163 plugin manifests under `extensions/` | clone |
| Governance | OpenClaw Foundation, a US 501(c)(3) launched ~2026-07-08. It employs the core team and signs releases. Donors: Amazon, OpenAI, Red Hat, U-Michigan and others; infrastructure sponsors include NVIDIA, GitHub, Vercel and Blacksmith. | `README.md#governance`; explainx.ai |
| History | Built by Peter Steinberger as "Clawdbot", renamed "Moltbot" after Anthropic trademark pressure, then "OpenClaw" (Jan 2026). Steinberger joined OpenAI in Feb 2026; OpenAI says it is "a donor, not an owner". | Fortune 2026-02-15; Neowin; README |

---

## 1. Messaging channels / surfaces

- **What exists:** about 48 channel docs (`docs/channels/`) covering WhatsApp, Telegram, Slack, Discord, Signal, iMessage (native, plus a BlueBubbles migration path), Google Chat, MS Teams, Matrix, Mattermost, IRC, LINE, Feishu, WeChat/WeCom, QQ, Zalo, Nostr, Twitch, Nextcloud Talk, Synology Chat, SMS, Tlon, voice-call, and A2A. Shared machinery: broadcast groups, group-message policy, pairing of unknown DM senders, and bot-loop protection. Meeting plugins cover Google Meet, Zoom, Teams meetings and Slack huddles. There is also a WebChat/Control UI and a TUI.
- **Evidence:** `docs/channels/*.md`, `extensions/{whatsapp,telegram,slack,discord,signal,imessage,matrix,msteams,...}`.
- **Maturity (self-rated, 2026-09-19):** Discord **Stable** (q73/c87). Telegram, WhatsApp, Slack and iMessage **Beta** (q66–68). Signal, Google Chat, Matrix and Teams **Alpha** (q59–60). Community and regional cohorts **Alpha** (q53–55). Voice-call **Experimental** (q41).
- **My call:** **polished** for the top 5–6 channels and **works/prototype** for the long tail. Breadth is unmatched by any other OSS assistant.
- **Gaps:** Quality drops off sharply after the big five. A reviewer of the August 2026 release reports that subagent orchestration, widgets, forking and automatic skill improvement were **web-app-only** and did not reach Telegram or mobile (openclawdatabase.com, 2026-08-31).

## 2. Tool execution: shell/exec, files, code exec, background processes

- **What exists:**
  - `exec` tool parameters: `workdir`, `env`, `timeoutSeconds`, `pty`, `yieldMs` auto-backgrounding (default 10s), `background`, `host` (auto/sandbox/gateway/node) and `elevated`.
  - `process` tool for background sessions: poll, log, kill, TTL'd retention capped at 50 finished sessions. Completion wake resumes the agent when a background command finishes or fails.
  - File tools: `read`, `write`, `edit`, `apply_patch`.
  - "Code Mode" (experimental): the model writes JS that calls a hidden tool catalog, run in a Node `vm` worker or a QuickJS executor.
  - Remote `code_execution` via xAI (billed by xAI).
  - Remote node execution (`node_exec`) on paired devices, plus cloud workers.
- **Evidence:** `docs/tools/exec.md`, `docs/gateway/background-process.md`, `docs/tools/code-mode.md`, `docs/tools/code-execution.md`, `docs/tools/apply-patch.md`, `docs/gateway/cloud-workers.md`.
- **Maturity:** self-rated "Browser automation, exec, and sandbox tools" **Beta** (q75/c79). My call: **polished** and very feature-dense.
- **Gaps:** Defaults are permissive (see §9). The docs warn that disabling the write/edit tools does not make `exec` read-only. Two different features are both called "Code Mode" (OpenClaw's and Codex's) with the same tool names, `exec` and `wait`, which is a documented source of confusion.

## 3. Browser automation / computer use

- **What exists:**
  - **Browser:** an agent-dedicated Chrome/Brave/Edge/Chromium profile controlled through a loopback service. A `user` profile attaches to your real signed-in Chrome via Chrome DevTools MCP. Also: remote/hosted CDP, a sandboxed browser in Docker, SSRF policy, and a Chrome extension.
  - **Computer use:** one `computer` tool (`computer.act` plus `screen.snapshot`) for the Gateway desktop or paired nodes. On macOS it uses Peekaboo by default or the CUA driver. On Windows/Linux it uses the bundled `cua-computer` plugin. It also works on Crabbox cloud desktops. It needs a vision model.
  - Codex computer-use plugin.
- **Evidence:** `docs/tools/browser*.md`, `docs/nodes/computer-use.md`, `extensions/{browser,cua-computer,crabbox}`, `docs/plugins/codex-computer-use.md`.
- **Maturity:** browser is **works → polished**; computer use **works** (newer, multi-provider). There is a separate CI contract test, `.github/workflows/browser-mcp-contract.yml`.
- **Gaps:** Attaching to the user's real signed-in browser is a large blast radius. There are troubleshooting pages specifically for WSL2/remote CDP and Linux. Computer use needs macOS Accessibility and Screen Recording grants and a vision model.

## 4. Memory & personalization

- **What exists:**
  - Plain-Markdown memory in the workspace: `USER.md` (user model, with superseding directives), `MEMORY.md` (long-term), `memory/YYYY-MM-DD.md` daily notes, and `DREAMS.md`. `SOUL.md` sets personality and `AGENTS.md` holds standing orders.
  - Memory search (builtin, LanceDB, Honcho, memory-wiki).
  - **Active Memory:** an escalating recall sub-agent for temporal and multi-hop questions, citing LongMemEval.
  - **Dreaming:** background consolidation in light, deep and REM phases. It is **on by default** and promotes into `MEMORY.md`, with rewrite preimages kept in SQLite.
  - **Standing intents:** event-triggered prospective memory stored in SQLite.
  - Session search, compaction, and memory provenance.
  - Per-person `USER.md` on multi-user Gateways.
- **Evidence:** `docs/concepts/{memory,active-memory,dreaming,user-model,soul,standing-intents,memory-provenance}.md`, `extensions/{memory-core,memory-lancedb,memory-wiki,active-memory}`.
- **Maturity:** self-rated "Session, memory, and state lifecycle" **Beta** (q75/c76). My call: **polished in design**, and the most thought-through part of the product.
- **Gaps:** Memory is model-written Markdown, so its quality depends on the model. Background dreaming spends tokens. A reviewer lost 8 months of memories and skills after a failed upgrade (2026-08-31), which points to upgrade fragility rather than a memory-design flaw.

## 5. Extensibility: skills, plugins, MCP, registry

- **What exists:**
  - **Skills:** `SKILL.md` directories with frontmatter, gated by environment, binaries and config; 50 bundled under `skills/`.
  - **Plugins:** a typed plugin SDK covering channel, provider, tool, harness and runtime plugins, with manifests, permission requests and bundles. 163 bundled.
  - **MCP client:** stdio, SSE and streamable HTTP, managed from the Control UI or CLI, with `openclaw mcp doctor --probe`, hot reload and durable tool grants. **MCP server:** `openclaw mcp serve`.
  - **ACP:** drives external harnesses such as Claude Code, Cursor, Gemini CLI and OpenCode.
  - **ClawHub** (clawhub.ai, `openclaw/clawhub`, ~9.5k stars): the public registry for skills and plugins, with CI workflows for publishing.
- **Evidence:** `docs/tools/{skills,mcp,acp-agents}.md`, `docs/plugins/*`, `.github/workflows/plugin-clawhub-*.yml`.
- **Maturity:** self-rated Plugins **Beta** (q72/c79); ClawHub **Alpha** (q58/c62). My call: plugins and MCP are **polished**; the registry is **works** with a bad security history.
- **Gaps and incidents:**
  - Koi Security (Feb 2026) found **341 malicious skills out of 2,857**. The "ClawHavoc" campaign used fake prerequisites to install the Atomic macOS Stealer, and other skills shipped reverse shells or credential exfiltration from `~/.clawdbot/.env` (The Hacker News, 2026-02).
  - A later audit by RankClaw claimed **1,103 malicious out of 14,706** (Mar 2026). This is a secondary claim from insiderllm.com.
  - Responses: VirusTotal hash and Code Insight scanning with daily rescans (Feb 2026), then NVIDIA SkillSpector checks (itbrief).
  - SECURITY.md explicitly excludes "a malicious plugin after a trusted operator installs it" from scope.
  - Plugin SDK churn: there was a breaking deprecation gate on 2026-09-01 (`CHANGELOG/2026.8.1.md`).

## 6. Autonomy & scheduling

- **What exists:**
  - **Automations:** `openclaw automations`, alias `cron`. Persistent jobs with one-shot or recurring schedules, webhooks, Gmail PubSub and IMAP triggers, and delivery to a channel, a webhook or nowhere.
  - **Heartbeat:** a system-owned periodic main-session turn with rate limits (30s minimum and a flood guard).
  - Event wakes on background-exec completion.
  - **Standing orders** in `AGENTS.md`: scope, triggers, approval gates, escalation.
  - **Goals** (`/goal`): a durable per-session objective with a token budget.
  - Exact-operation approval grants for recurring jobs (added in 2026.8.1).
  - Hooks.
- **Evidence:** `docs/automation/*`, `docs/gateway/heartbeat.md`, `docs/tools/goal.md`.
- **Maturity:** self-rated "Automation and durable work" **Beta** (q72/c79). My call: **polished**.
- **Gaps:** Heartbeats are the main driver of runaway cost. One widely reported example: about 25 overnight Opus calls answering "is it daytime yet?" cost $18.75 (Notebookcheck). Cron output recovery and delivery fixes were still landing in 2026.8.35.

## 7. Multi-agent / subagents

- **What exists:**
  - Multiple isolated agents per Gateway, each with its own workspace, auth profiles, model registry and SQLite session store. Channel-account **bindings** route traffic to agents.
  - **Sub-agents:** background runs in their own session that announce results back. They get no session or message tools by default, nesting depth is configurable, and they can be given a cheaper model.
  - **Swarm:** fan-out from Code Mode JS with `Promise.all`, collector children, an `outputSchema` and bounded concurrency. On by default.
  - Parallel specialist lanes, subagent yield/handoff, ACP external harnesses, and an A2A channel.
- **Evidence:** `docs/concepts/multi-agent.md`, `docs/tools/{subagents,swarm,acp-agents}.md`.
- **Maturity:** **works → polished**.
- **Gaps:** CVE-2026-32915 (fixed in 2026.3.11) let low-privilege leaf subagents reach the subagent control surface. A reviewer reported a subagent task stuck after 27 minutes (2026-08-31). Each subagent pays for its own context.

## 8. Model flexibility

- **What exists:**
  - About 60 provider plugins in `extensions/`: OpenAI/Codex (including GPT-6.1 Sol as of 2026.8.35), Anthropic (plus Vertex and Bedrock), Google, xAI, Mistral, DeepSeek, Groq, Cerebras, OpenRouter, LiteLLM, Vercel/Cloudflare gateways, GitHub Copilot, NVIDIA, Qwen/Kimi/Moonshot/MiniMax/Zai, and others.
  - **Local:** llama.cpp managed server with hardware-aware recipes, Ollama, LM Studio, vLLM, SGLang, MLX, ds4, llmman and ONNX.
  - **Failover:** same-model bounded retry, then auth-profile rotation, then the `model.fallbacks` chain.
  - Per-subagent and per-agent model overrides, plus `clawrouter`.
  - Agent "harnesses": Codex app-server and the Claude CLI backend.
- **Evidence:** `docs/concepts/{model-providers,model-failover}.md`, `docs/gateway/local-models.md`, `extensions/*`.
- **Maturity:** self-rated OpenAI **Beta** (q74), Anthropic **Beta** (q71), local models **Alpha** (q61/c68). My call: **polished** for hosted providers and **works** for local.
- **Gaps:** The project leans toward OpenAI (OpenAI is a donor, ships the Codex harness by default, and has "Sign in with ChatGPT" in 2026.9.7). Local models are explicitly second-tier, and the docs warn that a large system prompt plus the tool set strains small-context local models.

## 9. Security

- **What exists:**
  - Optional sandbox backends: **Docker (default backend)**, Podman, SSH, OpenShell and Crabbox. Modes are `off`, `non-main` and `all`; scopes are agent, session and shared.
  - **Exec approvals:** modes `deny`, `allowlist`, `ask`, `auto` (LLM auto-reviewer) and `full`, with allowlists, executable hash/realpath binding and `askFallback=deny`. Elevated mode is required to break out of a sandbox.
  - DM pairing for unknown senders. Gateway binds to **loopback by default** with auth required. Operator roles can force sandboxing.
  - Secrets: an egress proxy, masked credential requests, and 1Password/Vault plugins.
  - Prompt-injection guidance, `openclaw security audit`, a hardened baseline, a threat model (MITRE ATLAS), notes on formal verification, and an incident-response plan.
  - CI includes CodeQL, opengrep and a dependency audit.
- **Evidence:** `docs/gateway/sandboxing*`, `docs/tools/{exec-approvals,permission-modes,elevated}.md`, `docs/gateway/security/*`, `docs/security/*`, `SECURITY.md`.
- **Critical defaults (from the docs, 2026-10-02):**
  - `agents.defaults.sandbox.mode` defaults to **`off`**.
  - Host `exec.security` defaults to **`full`** and `exec.ask` to **`off`**.
  - So out of the box the main session runs shell commands **on the host with no prompts**. The README states this: "Tools run on the host for the main session unless you configure sandboxing."
- **Trust model:** the Gateway assumes a single trusted operator. Shared agents are explicitly not an isolation boundary. Prompt injection on its own is out of scope for security reports.
- **Incidents and CVEs:**
  - **722 published GitHub Security Advisories** (14 critical, 249 high, 390 medium, 69 low). By month: 220 in Feb, 204 in Mar, 108 in Apr, then 45–75 per month through Sep 2026.
  - **CVE-2026-25253** (CVSS 8.8, fixed in 2026.1.29): 1-click RCE. The Control UI trusted a `gatewayUrl` query parameter and leaked the gateway token, so even loopback-only installs were exploitable.
  - Other examples: CVE-2026-32038 (sandbox network namespace bypass), CVE-2026-32026 (sandbox media path escape), CVE-2026-32063 (systemd unit injection) and CVE-2026-42435 (shell-wrapper environment injection, fixed in 2026.4.12).
  - **Mass exposure** in early 2026: estimates range from ~21.6k (Censys) and ~40k (SecurityScorecard) to ~42.7k (Shodan), with a 135k figure in one aggregator. Attributed to older defaults binding `0.0.0.0` without auth and to exploitation of 25253. These exposure figures come from secondary sources with inconsistent methods.
  - ClawHub malware: see §5.
- **Maturity:** self-rated **Beta** (q72/c79). My call: the defenses are now **extensive and well documented**, but defaults favor capability over safety, and the track record is the worst of any comparable project.

## 10. Observability & ops

- **What exists:**
  - **Control UI** (Vite + Lit SPA served by the Gateway): chat, activity, nodes, config, settings, MCP and dashboards/widgets. Also a TUI.
  - `openclaw doctor` (seven doc pages: health checks, config migrations, `--fix`), `openclaw gateway status`, and `openclaw mcp doctor`.
  - Logging, diagnostics, **OpenTelemetry** and **Prometheus** plugins, usage tracking, trajectories, and an audit log.
  - `openclaw update` with channels, backups and rollback protection (improved in 2026.9.7). Restart recovery. Gateway lock against duplicate gateways.
- **Evidence:** `docs/web/control-ui.md`, `docs/gateway/{doctor,logging,opentelemetry,prometheus,diagnostics,audit,restart-recovery}.md`, `extensions/diagnostics-{otel,prometheus}`.
- **Maturity:** self-rated Gateway **Stable** (q80), CLI **Stable** (q83), Observability **Beta** (q75), Control UI **Beta** (q74), TUI **Alpha** (q59). My call: **polished**.
- **Gaps:** **Upgrades** are the most common complaint.
  - "Update fatigue": 18 releases between 2026-04-24 and 2026-05-12 (betterclaw.io).
  - A reviewer claims about 70% of updates broke their install, and the August 2026 ("OpenClaw 2.0") in-place upgrade hung silently.
  - Gateway-crash reports after an update (piunikaweb, 2026-04-29).
  - Doctor-driven breaking migrations, for example the 2026.8.1 OpenProse removal and the `codex/*` to `openai/*` route rename.
  - The extended-stable channel exists to address this.

## 11. Install/setup UX & cross-platform

- **What exists:**
  - One-line installers: `curl … install.sh | bash` and `iwr … install.ps1 | iex`. They provision Node. Also `npm i -g openclaw` and `openclaw onboard --install-daemon` (a wizard that checks model access, creates the workspace and installs the daemon).
  - Deployment guides for Docker, Podman, Nix, Kubernetes, Fly, Render, Railway, Hetzner, Raspberry Pi and more.
  - Migration importers from Claude and Hermes (`extensions/migrate-*`).
  - **Companion apps:** macOS, iOS (including HealthKit), Android, Linux app, Windows Hub, and watchOS surfaces.
- **Evidence:** `README.md#install`, `docs/install/*`, `docs/platforms/*`, `apps/{macos,ios,android,linux}`, store-release workflows.
- **Maturity (self-rated):**
  - Hosts: macOS **Stable** (q74/c88), Linux **Stable** (q75/c89), WSL2 **Beta** (q69), native Windows **Alpha** (q58).
  - Apps: iOS and Android **Stable** (q80), macOS app **Beta** (q66), Linux app **Beta** (q71), **Windows app Beta-labeled but q19/c21**, watchOS **Experimental** (q41).
  - Other: Kubernetes **Alpha**, Nix **Experimental**.
- **My call:** **polished** on macOS/Linux, **works** on Windows.
- **Gaps:** Pipe-to-shell install. The Windows native app is immature. Reviewers say real productive use still requires sysadmin-level skills (damus.io review).

## 12. Engineering quality & reliability

- **Facts:**
  - TypeScript monorepo (pnpm, tsdown, Vitest) of about 4.3M non-test TS/TSX lines, with Swift, Kotlin and small Rust parts.
  - About 18.9k test files. Live, end-to-end and QA-lab scenario suites (`qa/`, `test/`). Install smoke tests and cross-OS release checks.
  - **111 GitHub workflows**, including CodeQL, opengrep, a performance workflow, Node-runtime conformance and a maturity scorecard check. There is a paid runner sponsor (Blacksmith).
  - **Release cadence:** multiple releases per week. Two lines ship in parallel (2026.9.x stable and 2026.8.x extended-stable, e.g. 2026.8.33–8.35 between 2026-09-29 and 2026-10-02).
  - Very heavy AI-assisted development: "AI-assisted PRs are welcome", automated triage and sweep bots (`clawsweeper`, `pr-ci-sweeper`, `mantis-*`), and maturity scores produced by Codex.
- **Concerns:**
  - Around 5.9k open issues and 3.3k open PRs.
  - Thousands of PRs per release raise review-depth questions. SECURITY.md says they receive "a high volume of AI-generated scanner findings".
  - The codebase is very large for a personal assistant (4M+ lines), and complexity shows in the docs: many topics are split into 7–12 sub-pages.
  - A versioning mistake in 2026.8.1 (a beta mislabeled as 2026.9.1-beta.1).
- **Maturity:** overall self-rated score **68%, "Alpha"** (quality 65, completeness 71) across 50 surfaces.

## 13. Docs & community

- **Docs:** very large Mintlify site (docs.openclaw.ai). Index pages split by "reader job", with per-feature troubleshooting, a configuration reference, a threat model and a maturity taxonomy. Translation workflows and DeepWiki. My call: **polished** and comprehensive, though the volume is overwhelming.
- **Community:** about 391k stars, 82k forks, about 3.5k contributors, a Discord, a newsletter, and a large third-party ecosystem of hosting companies, guides and courses (much of it SEO content of variable accuracy).
- **License and governance:** MIT. OpenClaw Foundation 501(c)(3) since July 2026 employs the core team. OpenAI, NVIDIA, Microsoft and Tencent are named partners, and NVIDIA and Tencent engineers do security work (SECURITY.md). There is no paid bug bounty.

## 14. Self-improvement / learning loop

- **What exists:**
  - **Self-learning, default mode `auto`:** corrections and successful work are turned into skills through **Skill Workshop**, which uses proposals, hash binding, a security scanner and rollback capture.
  - Immediate in-turn repair of a skill that turned out wrong, plus a weekly collection review.
  - The `propose` mode stages drafts for human review. `off` disables learning.
  - Dreaming consolidates memory (§4), and `USER.md` keeps preferences with supersession.
- **Evidence:** `docs/tools/{self-learning,skill-workshop}.md`, `docs/concepts/dreaming.md`.
- **Maturity:** **works** and newer. The reviewer above notes automatic skill improvement was web-app-only in the August release.
- **Gaps:** In `auto` mode the background learning edits the Workshop directory directly with ordinary file tools. The docs say these edits "do not create proposals or automatic rollback snapshots", so autonomous self-modification bypasses the review path unless `propose` is chosen. It also adds background token cost.

---

## Honest summary

**Strongest 3**
1. **Breadth and reach:** 20–50 messaging channels, native iOS/Android/macOS apps, device nodes, browser and computer use, about 60 model providers, MCP, ACP and A2A. Nothing else open-source comes close.
2. **Depth of agent-runtime design:** layered memory (Markdown, active recall, dreaming, standing intents), a durable automation scheduler with heartbeats, goals and approval grants, subagents and swarm, and model failover. All of it is documented in detail.
3. **Resources and momentum:** a funded 501(c)(3) with a paid core team and big-tech sponsors, extreme contributor velocity, 111 CI workflows, about 18.9k test files, an extended-stable channel, and an honest public maturity scorecard.

**Weakest 3**
1. **Security record and defaults:** 722 GHSAs in about 8 months (263 high or critical), a 1-click RCE, tens of thousands of exposed gateways, and a malware-ridden skill registry. Out of the box the sandbox is off and host exec runs without prompts.
2. **Upgrade and reliability churn:** releases almost daily, breaking migrations, widely reported broken upgrades and data loss, about 9k open issues and PRs, and a self-rated overall maturity of "Alpha" (68%).
3. **Complexity and cost:** a 4M+ line codebase with sprawling configuration and docs. Heartbeats, dreaming, active memory and self-learning burn tokens in the background (user reports of $200–500/day bills and $18.75 overnight idle checks). Non-experts struggle to run it safely.

## Sources

Primary:
- Repo: https://github.com/openclaw/openclaw (clone `32e30e5`, 2026-10-02)
- Docs: https://docs.openclaw.ai
- Releases: https://github.com/openclaw/openclaw/releases (v2026.9.7, v2026.8.35)
- Advisories: https://github.com/openclaw/openclaw/security/advisories (722 published)
- ClawHub repo: https://github.com/openclaw/clawhub

Security:
- https://thehackernews.com/2026/02/researchers-find-341-malicious-clawhub.html
- https://thehackernews.com/2026/02/openclaw-bug-enables-one-click-remote.html
- https://www.wiz.io/vulnerability-database/cve/cve-2026-25253
- https://depthfirst.com/post/1-click-rce-to-steal-your-moltbot-data-and-keys
- https://hunt.io/blog/cve-2026-25253-openclaw-ai-agent-exposure
- https://insiderllm.com/guides/openclaw-clawhub-security-alert/
- https://www.openclaw.ai/blog/virustotal-partnership
- https://www.csoonline.com/article/4129393/
- https://itbrief.co.uk/story/openclaw-adds-nvidia-skillspector-to-clawhub-checks
- https://vulnerabilities.ncsc.nl/csaf/v2/2026/cve-2026-32038.json
- https://feed.craftedsignal.io/tags/cve-2026-32915/
- https://mondoo.com/vulnerability-intelligence/vulnerability/CVE-2026-42435

Reliability and cost:
- https://openclawdatabase.com/news/videos/2026-08-31-openclaw-2-upgrade-breakage-review/
- https://www.betterclaw.io/blog/openclaw-update-fatigue-burnout
- https://piunikaweb.com/2026/04/29/openclaw-update-triggering-gateway-crashes-issues
- https://www.notebookcheck.net/18-75-overnight-to-ask-Is-it-daytime-yet-The-absurd-economics-of-OpenClaw-s-token-use.1219925.0.html
- https://clawdhost.net/blog/openclaw-api-costs-what-nobody-tells-you

Governance and history:
- https://fortune.com/2026/02/15/openai-openclaw-ai-agent-developer-peter-steinberg-moltbot-clawdbot-moltbook/
- https://www.neowin.net/news/openclaws-founder-peter-steinberger-is-joining-openai/
- https://www.explainx.ai/blog/openclaw-foundation-501c3-nonprofit-july-2026

Note: the third-party exposure counts, the 1,103-malicious-skill figure and the per-user cost anecdotes are secondary claims and were not independently verified. Some timeline sources (e.g. "mid-2024 Clawdbot launch") conflict with the repo's 2025-11 creation date.
