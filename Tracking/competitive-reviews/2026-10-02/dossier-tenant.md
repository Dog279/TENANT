# Tenant: capability dossier (audit of the local checkout, 2026-10-02)

Repo: the local checkout. Checked out: `feat/exec-parity`. Remote: `github.com/Dog279/TENANT`, **private**, 0 stars, 0 forks, one release (`v0.1.0`, 2026-09-30). History runs from 2026-06-04 to 2026-10-01: about 650 commits on main, all by one person under several author names.

Unless a section says otherwise, the evidence below is from the checkout. "Main" means merged; "local" means only on unpushed local branches.

## Branch state: main vs local-only work

| Branch | Ahead of main | What it adds | Pushed? |
|---|---|---|---|
| `feat/exec-parity` (HEAD) | 1 (`ec4ab8c`) | `os_exec` gets `workdir` and `timeout` (default 120s, model cap 600s), head+tail output clipping (30K), PATH fix for launchd, `Setsid` so sudo fails fast, classifier false-positive fixes. **Interactive surfaces now run exec on the host by default, not in the sandbox dry run.** 25 files, +640/-61 | No |
| `feat/imessage-modes` | 5 | iMessage passive/respond modes, a responder over BlueBubbles, Contacts names, live tool enable. 41 files, +998/-153 | No (waiting on the operator's Mac test) |
| `chore/drop-token-budget` | 1 (`17e36f5`) | Removes the team token and cost caps (`internal/orchestra/budget.go`) | No |
| `mod/01-02`, `mod/03`, `mod/04`, `mod/05`, `mod/07` | 1–3 each | Prompt-cache prefix stabilisation, JSONSchema on every backend, skill success/failure outcomes, two-level skill retrieval gating, a cron rename | No. Based on main as of 2026-09-19 and stale. None of it is on main (`git cherry` shows all unmerged; no MOD-0x string on main) |

So skill outcome tracking and skill retrieval gating (MOD-04/05) do **not** exist on main. Background processes (exec-parity PR2) are not implemented anywhere yet (`docs/plans/exec-parity-plan.md`).

---

## 1. Messaging channels and surfaces

| Surface | Evidence | Maturity |
|---|---|---|
| TUI (Bubble Tea, streaming, activity feed, slash commands, mid-turn interject) | `internal/tui/tui.go` (3,168 lines), `internal/tui/cmd_*.go` | Polished |
| CLI: one-shot per plugin (`tenant web/os/sql/...`), `chat` over stdin, `orchestrate`, `research` | `cmd/tenant/commands.go`, `cmd/tenant/main.go` | Works |
| Web dashboard: server-rendered, Datastar/SSE, chat page, about 20 pages, sign-in | `internal/dashboard/ssr_chat.go:1-6`, `internal/dashboard/server.go`, `internal/dashboard/auth.go`; 2,429 lines of templates | Works to polished (browser tests exist, e.g. `*_browser_test.go`) |
| Discord relay: DM the agent, approve with buttons/cards, attachment inbox | `cmd/tenant/discordrelay.go`, `cmd/tenant/discordapprove.go:70-75` (SendComponents/SendCard), `internal/plugins/discord/gateway.go` (own gateway client plus a fuzz test) | Works; used in production on the operator's server |
| iMessage: native macOS (chat.db read + AppleScript send) or BlueBubbles; autonomous responder | `internal/plugins/imessage/{chatdb,applescript,bluebubbles,watch}.go`; responder `cmd/tenant/imessagerelay.go` (on main) | Main has the responder, with "Phase 1 fails CLOSED on dangerous tools" (`imessagerelay.go` header). Modes and the BlueBubbles responder are **local only**. macOS only for native |
| MCP server (memory tools over stdio; peer listener over streamable HTTP) | `internal/mcpserver/handler.go:155-162` (only `memory_search` and `memory_fact_add`), `internal/peering/` | Works, narrow |
| Legacy SSE gateway (`--sse-addr`), **no auth** | `TODOS.md` item 1 | Rough |

**Not present:** Telegram, Slack, WhatsApp, Signal, Matrix, email-as-a-channel (Gmail is a tool, not an inbound surface), SMS, voice (no STT or TTS anywhere; the only "voice" in the code is Discord's channel-type enum, `internal/plugins/discord/client.go:34`). No mobile app. Channel breadth is much narrower than OpenClaw's and Hermes's: two chat channels (Discord, iMessage) plus local UIs.

## 2. Tool execution: shell, files, code, background processes

- **Tools:** `os_sysinfo`, `os_read_file`, `os_read_image` (format and dimensions only), `os_list_dir`, `os_processes`, `os_write_file`, `os_append_file`, `os_edit_file` (exact string replace), `os_make_dir`, `os_exec` (`internal/plugins/osys/dispatch.go:106-124`).
- **Exec:** on the local branch, `os_exec` takes `workdir` and `timeout` (default 120s, model may ask up to 600s, operator ceiling configurable), with head+tail clipping at 30K (`internal/plugins/osys/osys.go:26-29, 100-109, 378`). **On main, the timeout is a fixed 60s with head-only clipping and no cwd parameter** (listed as "walls" in `docs/plans/exec-parity-plan.md`).
- **File read cap:** 64 KB with **no offset/limit** (`osys.go:29, 148`). Big files cannot be paged.
- **Background processes:** **none.** No background registry, no poll or kill, no PTY, no stdin (the plan's PR2 is not started).
- **Code execution:** only through the shell. No dedicated Python or JS runner, no notebook.
- **Classifier:** regex-based destructive and exec classification (`osys/classify.go`), which the plan itself calls a "UX layer, never the boundary" (`internal/sandbox/sandbox.go:4-5`).

Maturity: works, but below Claude Code, OpenClaw and Hermes parity by the project's own written gap analysis.

## 3. Browser automation and computer use

- A chromedp-driven headless Chrome per agent: `web_navigate`, `web_read`, `web_find`, `web_links`, `web_screenshot`, `web_click`, `web_fill`, `web_select` (`internal/plugins/web/dispatch.go:114-130`). It injects stealth JS (`web.go:47-98`) and has an egress policy and proxy (`egress.go`, `proxy.go`). Chrome's own sandbox is on by default.
- Search: DDG scrape (keyless default), with Brave and Tavily as options (`search.go` header).
- **The model gets no vision input.** The backends have no image or multimodal message content (no `image_url` or content parts in `internal/model`), so `web_screenshot` returns a file path the model cannot see. Navigation is text and DOM only.
- **No desktop computer use:** no screen capture, mouse or keyboard control of the OS (no hits for xdotool, cliclick, robotgo or "computer use").

Maturity: works for text-centric browsing; behind any vision-driven browser agent.

## 4. Memory and personalisation

This is the deepest subsystem (about 8.7K non-test lines in `internal/memory/`, plus `internal/improve/`).

- **Tiers:**
  - T0 soul, an editable persona (`memory/soul`)
  - T1 working window (`memory/working`)
  - T2 episodic store, SQLite + FTS5 + embeddings (`memory/episodic`)
  - T3 semantic facts with quality and decay modulators (`memory/semantic/search.go:48-245`)
  - T4 skills (`memory/skills`)
  - T5 append-only JSONL archive, the source of truth (`memory/archive`)
  - A user profile injected every turn (`memory/userprofile`, which explicitly mirrors Hermes's USER.md)
  - Per-project SME documents (`memory/sme`)
  - Markdown import (`memory/mdimport`)
- **Retrieval:** hybrid FTS5 + cosine fusion (`episodic/search.go:52-128`). The vector search is a **brute-force full-table scan** (`episodic/search.go:156-185`), with no ANN index. That is fine at personal scale but grows linearly.
- **Compaction:** archive-sourced and modelled on hermes-agent's ContextCompressor (`memory/compress/compress.go:2-8`), with a goal header.
- **Tools:** `memory_remember`, `memory_recall`, `memory_search`, `memory_history`.
- **Privacy:** retention per data class, "forget everywhere", export (`internal/privacy/`). This is unusually thorough.
- **Gap:** embeddings need a separate embeddings endpoint (`internal/config/config.go:42`), usually a local Ollama. Without it, retrieval is FTS only.

Maturity: polished on design and test coverage; how well it works in practice is not demonstrated beyond the single operator.

## 5. Extensibility: skills, plugins, MCP

- **First-party plugins (Go, compiled in):** osys, web, sql (SQLite only), gsuite (Gmail, Calendar, Drive), x, imessage, discord, wiki, github (including gated merge), atlassian (Jira), simplefin (read-only finance), crm (wraps an external `crm-tool` binary), cron (`internal/plugins/`, about 19K lines).
- **No third-party plugin API by design.** It is an "appliance, not a library" (`docs/adr/0001-appliance-not-framework.md`). You extend it through MCP servers and skills.
- **MCP client:** stdio and Streamable HTTP, OAuth 2.1 with DCR and PKCE via the official go-sdk (`internal/plugins/mcpremote/mcpremote.go:1-6`). The agent can adopt servers live with `mcp_add`, `mcp_list`, `mcp_remove` and `mcp_trust` (`cmd/tenant/mcptools.go:65-78`), with per-connector trust. **Works.**
- **MCP server:** only two memory tools exposed (`internal/mcpserver/handler.go`), plus Tenant-to-Tenant peering (`internal/peering`). The legacy SSE transport is unauthenticated (`TODOS.md`).
- **Agent Skills (SKILL.md):** packages can be imported and exported (`internal/skillpkg/skillpkg.go`, `cmd/tenant/skills_package.go:67, 138`). Bundled `scripts/`, `references/` and `assets/` are **only listed, not run** (`skillpkg.go` header). Skills are injected as recipe text into the operating rules. The agent writes skills with `skill_save` (`cmd/tenant/skills_wire.go:71`), which start untrusted until the operator reviews them. A gstack seed bundle is included (`cmd/tenant/gstackseeds.go`).
- **No skills hub or registry and no community catalogue.** This is a big ecosystem gap compared with OpenClaw's ClawHub and Hermes's skills hub.
- **Lazy tool loading:** a `load_tool` meta-tool plus a catalogue keeps tool-schema tokens flat (`internal/agent/lazytools.go`).

## 6. Autonomy and scheduling

- **Cron engine:** crontab and `@every` schedules, prompt and exec job kinds, a per-run wall-clock ceiling, a job-count cap, and run history (`internal/cron/engine.go`, `schedule.go`). The agent manages it with `cron_list/add/remove/set_enabled/run_now` (`internal/plugins/cron/dispatch.go:82-104`). Delivery goes out to Discord (`cmd/tenant/cronmgr.go:799-800`). Prompt hardening for cron says to treat tool output as untrusted (`cronmgr.go:56-61`). **Works.** The "daily brief" job failed five days running in the field from a tool-registry bug (`cmd/tenant/restricttools.go:14`), which was later fixed.
- **`/goal`:** a loop on a stop condition, with an LLM judge after each turn and a hard turn cap (`cmd/tenant/goalctl.go:1-17`). **Works.**
- **`serve` daemon:** runs the improvement scheduler (distill, consolidation, feedback protection, skill induction, nightly eval) (`cmd/tenant/commands.go:2791-2915`). Peer heartbeats exist (`cmd/tenant/peerheartbeat.go`).
- **Gaps:** there is no general "heartbeat check-in" that proactively messages the operator (only cron-scheduled prompts). There is no event-triggered autonomy beyond cron, inbound messages, log alerts and the alert investigator (`cmd/tenant/loginvestigate.go`).

## 7. Multi-agent and delegation

- `spawn_agent` and `team_await` (`cmd/tenant/team.go:1100-1119`), plus a bus with `team_send`, `team_broadcast`, `team_check`, `team_history` and `team_roster` (`internal/orchestra/tools.go:36-66`). Sub-agents run concurrently in-process (goroutines).
- Five built-in specialists (Programmer, Researcher, Writer, QA, Strategist) plus a "Main" persona (`cmd/tenant/builtinsouls/agents/`).
- Budgets: max concurrent sub-agents, agents per run, tool calls per turn and per run (64/256), tokens, cost and wall time (`internal/orchestra/budget.go:21-34, 89-90`). The token and cost caps are removed on the local `chore/drop-token-budget` branch.
- `research` mode: plan, then fan out web researchers, then a cited report (`internal/research/`).
- Sensitive-data provenance travels across agents (`agent/infoflow.go:269-350`).
- Tenant-to-Tenant federation over MCP with pairing (`internal/peering`); per-peer rate limiting is still a TODO (`TODOS.md`).
- **Gaps:** no isolated workspaces or worktrees per sub-agent, and no per-sub-agent sandbox. Specialists share the primary model unless overridden.

Maturity: works.

## 8. Model flexibility

- **Backends:** `vllm` (any OpenAI-compatible endpoint), `anthropic` (native), `claudecode` (spawns `claude -p` to use a subscription, with tools via text-format parsing and a weekly token ledger, `internal/model/backend/claudecode/claudecode.go:1-30`), and `echo` (offline) (`internal/model/backend/`).
- **Provider presets:** vLLM, Ollama, llama.cpp, generic OpenAI-compatible, OpenAI, Grok, Z.ai (four variants; Z.ai GLM is the **default**), Sakana, Anthropic, Claude Code CLI (`internal/config/providers.go:34-137`).
- **No first-class Gemini, OpenRouter, Bedrock, Vertex or Azure presets.** OpenRouter and some others may work through the generic OpenAI-compatible preset, but that was not verified.
- **Text-protocol tool calling** for weak or local models: a family-aware parser with qwen/hermes/mistral/llama3_json formats (`internal/model/toolfmt/toolfmt.go:66`, `family.go:18-36`). Model profiles are tuned for Qwen3 and Gemma (`internal/model/profiles/*.yaml`). There is an eval suite for format quirks (`internal/eval/tasks/adversarial/`).
- **Routing:**
  - Per-role routing (planner, summariser, embeddings and so on).
  - A FallbackLLM chain across providers on rate-limit, credit-out, unreachable or 5xx errors, with latency health-gating (`internal/model/fallback.go:1-20`, `router.go`).
  - A local 51M-parameter "Supra" prompt classifier that routes small vs big models in shadow or active mode (`internal/route/supra.go:1-22`, `internal/config/route.go:105-196`).
- **No multimodal input** (see section 3).

Maturity: polished for local-model use; this is one of the strongest areas.

## 9. Security

- **Approvals:**
  - Categories (exec, write, destructive, outbound send) with once, session or always grants.
  - Per-tool glob rules that reload live.
  - Per-surface brokers (TUI, dashboard, Discord buttons, iMessage text confirm).
  - An audit trail (`internal/approval/{broker,rules,categories,audit}.go`).
- **Sandbox backends** (`internal/sandbox/sandbox.go:7-20`):
  - `host`: no confinement.
  - `restricted`: **macOS Seatbelt only**.
  - `jobobject`: Windows resource caps, **no filesystem or network confinement**.
  - `container`: an ephemeral hardened Docker container per run on any OS, with `--pull never`, network none by default and a fixed argv schema (`container.go` header).
  - The Linux and Windows bubblewrap backends were removed. **On Linux, the only confinement is Docker.**
- **Default posture:** on main, the default exec profile is the sandbox in **dry run** (enforce off). On the local exec-parity branch, **interactive surfaces run on the host by default** (`cmd/tenant/execprofiles.go:303-313`); only cron stays confined. In practice the approval gate is the main defence. That is honest ("like Claude Code without /sandbox"), but it is not sandbox-first.
- **Prompt-injection defences:**
  - Sensitive-data **information-flow tracking**: recent sensitive tool results are fingerprinted, and outbound calls quoting them (including percent-encoded forms) need approval. This propagates across sub-agents (`internal/agent/infoflow.go`, with adversarial tests in `infoflow_adversarial_test.go`).
  - Agent-written skills start untrusted (`skills_wire.go:55-60`).
  - Discord allowed-mentions safety.
  - Untrusted-data prompt clauses on cron and log investigation.
  - The GitHub merge gate is code, not prompt (`internal/plugins/github/dispatch.go:14`).
  - There is **no general spotlighting or delimiting of tool results in the main agent prompt** (the "treat as untrusted" wording appears only in the cron and log-investigation prompts).
- **Secrets:** verified 0600/0700 files on Unix and an explicit DACL on Windows, with an optional OS data protector (`internal/secrets/`). Log scrubbing (`internal/logs/scrub.go`). A hash-chained Security log (`cmd/tenant/logchain.go`). `tenant security audit` reports each surface's posture.
- **Other:** a release workflow with CodeQL/security scanning (`.github/workflows/security.yml`), and keyless-signed checksums plus SBOMs (`.goreleaser.yaml:51-68`).
- **Gaps:** the unauthenticated legacy SSE gateway; no peer rate limiting; no Linux sandbox without Docker; no FS or network confinement on Windows without Docker.

Maturity: polished on approvals and audit; the default isolation is weak.

## 10. Observability and operations

- **Log book:** per-source plain-text folders that become dashboard tabs, `tenant log`, `POST /api/logs`, retention budgets, a query engine, alert rules and an LLM alert investigator (`internal/logs/`, about 13.6K lines; `cmd/tenant/logalerts.go`, `loginvestigate.go`).
- **Dashboard:** approvals, agents, cron, eval, goals, memory inspector, MCP, keys, rules, safety, skills, logs (`internal/dashboard/`, about 10.6K lines).
- **`tenant doctor [--fix]`:** 1,498 lines, with logbook checks and MCP auth-health surfacing (`cmd/tenant/doctor.go`).
- **`tenant backup create|verify|restore`:** consistent while running, with age encryption (`internal/backup/`). Run traces (`internal/runtrace`). A token usage ledger (`memory/usage`).
- **Upgrade path:** **no self-update command.** You download the new release archive by hand (`docs/migration/deploy-and-rollback.md` is a manual runbook). The CHANGELOG is short (93 lines).

Maturity: polished, and probably the strongest area relative to peers.

## 11. Install, setup and cross-platform support

- **Install:** a single CGO-free static Go binary; GoReleaser builds linux, darwin and windows for amd64 and arm64 (`.goreleaser.yaml:11-17`). There is **no** Homebrew tap, Docker image, install script, macOS notarisation, or package-manager presence.
- **Setup:** a `tenant setup` wizard, `tenant doctor`, and an echo backend for a no-key trial.
- **Service install:**
  - macOS: a sample launchd plist only (`deploy/launchd/com.tenant.serve.plist`).
  - Linux: **no systemd unit** shipped.
  - Windows: service support exists (`cmd/tenant/runas.go`, `scripts/migrate-to-server.ps1`, DACL sharing in PR #18).
- **CI:** an ubuntu/windows/macos matrix with build, vet, `test -race`, test, and an eval smoke baseline (`.github/workflows/ci.yml:14-55`).
- **Platform skew:**
  - iMessage native is macOS only.
  - Seatbelt is macOS only.
  - Windows has a lot of hardening work (TEN-401 Chrome orphaning, DACLs).
- **Gap for "any host":** Google Workspace needs operators to bring their own GCP OAuth client, because the embedded client is a `{}` placeholder (`internal/plugins/gsuite/embedded_oauth_client.json`).

Maturity: works; setup UX is reasonable for developers but not consumer-grade.

## 12. Engineering quality and reliability

- **Language:** Go 1.26 (toolchain 1.26.6), about 20 direct dependencies (chromedp, the MCP go-sdk, modernc sqlite, bubbletea, go-github, go-atlassian, google api, age) (`go.mod`).
- **Size:**

  | Measure | Lines |
  |---|---|
  | Go, all | 268,023 |
  | Go tests (579 files) | 116,349 |
  | Go non-test | ~151,700 |
  | `cmd/tenant` (162 non-test files) | — |
  | Largest package, `internal/plugins` | ~19K |
  | Dashboard HTML templates | 2,429 |
  | Docs (70 Markdown files) | 23,765 |

  The test-to-code ratio is high (about 0.77:1).
- **Tests:** 3,336 top-level `func Test`; **`go test ./...` passes** on macOS (darwin, `feat/exec-parity`): exit 0, 78 packages ok, **4,149 test/subtest passes, 23 skips, 0 failures**, 1m32s wall. Some packages were cached; not run with `-race` locally (CI runs `-race`). The skips are platform- or live-gated: sandbox (5), cmd/tenant (7), logs/store (4). Build tags: `livesearch`, darwin, windows, !windows.
- **Eval harness:** 64 YAML tasks (smoke, adversarial tool-format quirks, route agreement, compaction), pass@k, an LLM judge, and baselines (`internal/eval/`, `baselines/`).
- **TODO/FIXME debt in code:** effectively zero (about 1 hit). Deferred work lives in Jira (TEN-xxx) and `TODOS.md` (2 items).
- **Rough edges:**
  - Some very large files (`commands.go` 3,324 lines, `tui.go` 3,168, `agent.go` 2,225).
  - About 80 local branches and worktrees, many stale.
  - Field incidents are documented in code comments (episodic DB corruption, a five-day daily-brief failure, a silently dead Atlassian MCP refresh token).
  - Reliability evidence is a single operator's deployment (a home server with 60+ day uptimes).
- **Architecture tests:** `internal/archtest` enforces import boundaries.

Maturity: high engineering rigour for a solo project.

## 13. Docs and community readiness

- MIT license (`LICENSE`), `SECURITY.md`, README, Quickstart, an as-built ARCHITECTURE.md (1,684 lines, with docfacts generated by CI, `.github/workflows/docfacts.yml`), ADRs, design docs and runbooks.
- **Not public:** the GitHub repo is private, with 0 stars, 0 forks, one contributor, and no CONTRIBUTING, CODE_OF_CONDUCT or issue templates.
- The README claims it is "a sanitized public snapshot", but the repo is still private. An open-source scrub is in progress (cleanup PRs 1–3 merged); history contains personal data, so a fresh-root public repo is planned.
- Docs lean heavily on internal Jira IDs (TEN-xxx) that outsiders cannot resolve.
- **Community:** none. No users besides the author, no Discord or forum, and no plugin or skill ecosystem.
- **Docs vs code mismatches:**
  - The README says "sanitized public snapshot" while the repo is private.
  - The README Features list implies sandboxing, but by default exec runs on the host or in a dry run.
  - The README says the MCP memory server is a surface; true, but it exposes only two tools.

## 14. Self-improvement and learning loop

- **Distillation** (episodic to semantic facts, resumable; `internal/improve/distilljob.go:12-15`, which explicitly calls itself the Hermes "closed learning loop"). On by default under serve.
- **Skill induction:** mines recurring tool-call sequences from successful turns and proposes skills, disabled with status=proposed, needing operator `/skills accept` or an auto-accept mode (`internal/improve/skilljob.go:15-34`). Heuristic, not LLM-authored. The agent can also save skills itself with `skill_save` (untrusted until reviewed).
- **Skill refinement:** the agent can upsert an existing skill (which drops it to untrusted). There is **no outcome-based skill scoring on main** (MOD-04 is local and stale).
- **SoulNudge:** proposes persona and instruction edits from ack/undo feedback, A/B-gated by eval, with human review only. **Off by default** and fails closed without `baselines/fitness.json`, which is **not shipped** (`baselines/` has full, smoke and logbook-darwin only) (`commands.go:2891-2898`, `improve/souljob.go`).
- **Reflection/SME** synthesis: off by default (`commands.go:2908-2916`). Consolidation and feedback-protection jobs.
- **Assessment:** the loop is more conservative (human-gated) than Hermes's autonomous skill creation and refinement. The pieces exist, but the headline self-improvement jobs are off by default, and the evidence that they improve outcomes is limited to the eval harness.

---

## Stars and users

A private repo with 0 stars, 0 forks, one contributor and one known deployment (the author's). It is pre-public.

## Honest summary

**Strongest 3**
1. **Operations and observability:** the log book, dashboard, doctor, encrypted verified backup, privacy and forget controls, hash-chained security log and security audit. This is more operator tooling than most agent projects ship.
2. **Local-model engineering:** text-protocol tool calling with model-family parsers, model profiles, lazy tool loading, a fallback chain with health-gating, a small-model prompt router, and an eval suite targeting local-model quirks. It is CGO-free with a single binary.
3. **Engineering discipline:** about 152K lines of Go with about 116K lines of tests (4,149 passing, 0 failing locally), a three-OS CI matrix with `-race`, architecture tests, almost no in-code TODO debt, and an approval and info-flow tracking design that has adversarial tests.

**Weakest 3**
1. **Reach and ecosystem:** only Discord and iMessage as chat channels (no Telegram, Slack, WhatsApp, Signal or voice). No skills hub or plugin marketplace, no community, still private with one user.
2. **Execution capability gaps:** no background processes or PTY, no paged reads of large files, and no vision input (screenshots are unseen by the model). Main still has the 60s fixed exec timeout; the fixes are on an unpushed branch.
3. **Default isolation and polish of autonomy:** the sandbox is dry run (main) or host (local branch) by default, Linux has no confinement without Docker, and Windows has none without Docker. The self-improvement jobs are mostly off by default or human-gated, and several "implemented" features (MOD-01..05, iMessage modes, token-budget removal, exec parity) exist only on unpushed local branches.
