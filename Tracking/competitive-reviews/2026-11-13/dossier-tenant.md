# Tenant: capability dossier (audit of the local checkout, 2026-11-13)

Repo: the local checkout. Checked out: `main` at `8b9bbf7`, clean, equal to `origin/main` and to the tag `v0.2.0-rc.2`. Remote: `github.com/Dog279/TENANT`, **private**, 0 stars, 0 forks (`gh repo view`: `visibility: PRIVATE`). Three releases: `v0.1.0` (2026-09-30, still the one GitHub marks "Latest") and two pre-releases published today, `v0.2.0-rc.1` and `v0.2.0-rc.2`. History: about 912 commits on main (GitHub API; the local clone is shallow and starts at 2026-09-23), from 2026-06-04 to 2026-11-13, all by one person under several author names.

Since the previous review's base (`d2497d6`, 2026-10-01): **260 commits** (210 non-merge: 89 docs, 54 feat, 45 fix, 10 test), 33 merged PRs (#38 to #70), 483 files changed, +57,390 / -2,392 lines.

Unless a section says otherwise, the evidence below is from the checkout. There are no unmerged local branches and no open PRs (`git branch --no-merged main` is empty; `gh pr list` is empty), so "Tenant" here means main only.

## Branch state: what happened to the previous run's local-only work

| Previous local branch | Now |
|---|---|
| `feat/exec-parity` | Merged (PR #39), followed by PR #46 (background processes, paged reads, read-only commands, command rules) |
| `feat/imessage-modes` | Merged (PR #49) |
| `chore/drop-token-budget` | Superseded by "limits off by default" (PR #53, TEN-508): the caps still exist but are unset by default (`internal/orchestra/budget.go:86-94`) |
| `mod/01-02` (prompt-cache prefix) | Not merged as such. The same goal landed differently as TEN-505: the surfaced tool set is sticky for a session (`internal/config/config.go:145-150`) |
| `mod/04`, `mod/05` (skill outcomes, skill retrieval gating) | **Still not on main.** `internal/improve`, `internal/skillpkg` and `internal/memory/skills` have no diff since `d2497d6`. The branches no longer exist locally |
| `mod/03`, `mod/07` | No trace on main (no `MOD-0x` string in code) |

---

## What changed since 2026-10-02

One row per capability. "Gap" is the workstream from the previous summary's "Where Tenant trails" table.

| # | Capability | Change since 2026-10-02 | Evidence | Prior gap or weakness: status |
|---|---|---|---|---|
| 1 | Messaging channels and surfaces | No new channel. iMessage passive/respond modes and the BlueBubbles responder moved from a local branch to main. Photos sent over Discord and iMessage reach the model. Dashboard chat now streams and renders Markdown, has an activity rail and UI components (tables, steps, choices, forms). TUI rewritten as turn blocks with Markdown. The SSE gateway now needs a bearer token | `cmd/tenant/imessagerelay.go:84-85, 270-273`; `cmd/tenant/discordingest.go:246-298`; `internal/uicomp/uicomp.go`; `cmd/tenant/gatewayauth.go:21`; `cmd/tenant/ssegateway.go:30` | TEN-494 (channels): **still open**. Still Discord and iMessage only. "Unauthenticated SSE gateway": **closed** |
| 2 | Tool execution | `workdir` and `timeout` are on main. New: background processes with `os_process` (list, log, wait, kill, write one stdin line), a command still running at its timeout is moved to the background, paged file reads (`offset`/`limit`, ~100K chars a page), read-only commands skip the prompt, per-command rules (`os_exec(brew install *)`), atomic file writes | `internal/plugins/osys/dispatch.go:107-130`; `osys.go:31-34, 160-209, 253-292`; `process.go:1-13`; `classify.go:80-120`; `internal/approval/rules.go:79-102` | TEN-488 (exec parity): **closed** for the three named gaps (background jobs, paged reads, workdir and timeout). Left: no PTY, background is host-only, no dedicated code runner |
| 3 | Browser automation and computer use | The model now sees images: `web_screenshot` and `os_read_image` attach the picture; the Anthropic and OpenAI-compatible backends send image content; local models are probed for vision at launch. Browser control is unchanged (text or CSS locators) | `internal/model/image.go:12-25`; `internal/model/vision.go`; `backend/anthropic/anthropic.go:137`; `backend/vllm/images.go:37-57`; `backend/vllm/probe.go:52-55`; `internal/plugins/web/screenshot_view.go` | TEN-493 (vision): **closed** for model input. **Partly closed** for the capability as a whole: no coordinate or vision-driven clicking, and still no desktop computer use |
| 4 | Memory and personalisation | No material change. Only image plumbing in the assembler and compaction, and attachments covered by privacy/forget (426 lines changed across `internal/memory` and `internal/privacy`) | `git diff --stat d2497d6 HEAD -- internal/memory internal/privacy` | No gap workstream. Brute-force vector scan and the separate embeddings endpoint: **still open** |
| 5 | Extensibility | Skills: **no change**. No hub or registry. New built-in `ui_render` tool. SQL tools now run on an engine-enforced read-only connection and refuse `ATTACH`. MCP client: 24 lines changed | `git diff --stat d2497d6 HEAD -- internal/skillpkg internal/memory/skills internal/improve` (empty); `cmd/tenant/toolmux_build.go:108` | TEN-496 (skills ecosystem): **still open**, not started on main |
| 6 | Autonomy and scheduling | No new trigger type. Cron: a daylight-saving fall-back hang fixed. New stop safety: an OS shutdown cuts the turn, records it, notifies the operator, and after boot posts a reminder and catches up Discord DMs. Serve checks for a new release daily (logs one line, applies nothing) | `internal/cron/schedule.go` (+21/-15, test in `schedule_test.go`); `internal/lifecycle/lifecycle.go:1-15`; `cmd/tenant/discordmgr.go:246`; `cmd/tenant/update.go:405-412` | TEN-497 (event triggers): **still open**. No heartbeat check-in, no webhook or file-watch triggers |
| 7 | Multi-agent and delegation | `team_await` now returns when a helper finishes or messages (was: block up to 3 minutes and re-call); ceiling without news is 30 minutes. Run limits (tokens, cost, time, tool calls, helpers per run) are off by default; machine guards stay (4 helpers, 4 browsers). A helper on a different backend from the embedder can spawn again | `cmd/tenant/team.go:1022-1028`; `internal/orchestra/budget.go:86-94` | TEN-497 (sub-agent workspaces): **still open**. No worktree or sandbox per sub-agent |
| 8 | Model flexibility | New `glm` tool-call format. Vision input (see 3). `request_timeout` per provider. A named self-hosted model gets its served context length. The surfaced tool set is sticky per session, to keep the server's prefix cache. A new optional "Judge" (a System One decision model) checks answers. **No new provider presets** | `internal/model/toolfmt/glm.go`; `internal/config/config.go:145-150, 358`; `internal/agent/judge.go:11-23`; `internal/config/providers.go:34-145` | "No multimodal input": **closed**. No Gemini, OpenRouter, Bedrock, Vertex or Azure preset: **still open** |
| 9 | Security | The container sandbox finished its wave-1 hardening and was run on a real Docker engine for the first time (Windows, 2026-11-10: 14 of 14 live cases, selftest 4 of 4). Some wave 2 landed (own bridge for `full` network, HOME on tmpfs, per-process CPU limit, trixie image, run record in the Security log, `sandbox stop-all`, approval bound to a digest of the exact run). Dashboard: CSP for images, cross-site cookie refusal, sign-in back-off. TUI strips terminal escapes. **Defaults did not get stricter**: interactive surfaces run on the host by default, now on main | `docs/plans/docker-sandbox-hardening.md:438-525`; `internal/sandbox/container.go:3-28`; `container_network.go:29-34`; `internal/dashboard/auth.go:93-95, 122`; `cmd/tenant/execprofiles.go:364-374` | "Unauthenticated SSE gateway": **closed**. "Default isolation is weak": **still open** (container is opt-in; see section 9). No general tool-result spotlighting: **still open**. Peer rate limiting: **still open**. TEN-453 (Docker sandbox): wave 1 **closed** bar live-only items; waves 2 and 3 **partly open** |
| 10 | Observability and operations | `tenant update` (check, verify, back up, swap, restart, confirm, roll back). `tenant service` on all three OSes. `tenant doctor` gains a `container sandbox` row and a service row. Security log record `4201` now describes the sandbox boundary of each command. Log-book housekeeper lease fix. CHANGELOG grew from 93 to 519 lines | `cmd/tenant/update.go`; `internal/selfupdate/`; `cmd/tenant/service*.go`; `cmd/tenant/cliregistry.go` (`update`, `service` rows) | "No self-update command": **closed** in code (not exercised against a public release; see 11) |
| 11 | Install, setup and cross-platform | `install.sh` and `install.ps1` ship as release assets and verify SHA-256. `tenant service install` writes a LaunchAgent, a systemd user unit or a Windows logon task (or `--system` service). Packaging CI installs a snapshot through both scripts on three OSes. **But the repo is private**, so the documented one-liners cannot work for anyone else, and the release GitHub calls "Latest" (`v0.1.0`) has no install scripts | `scripts/install.sh` (88 lines); `scripts/install.ps1` (64); `.github/workflows/packaging.yml`; `.github/workflows/release.yml:33-38, 55, 67-73`; `gh release view v0.1.0` (13 assets, no scripts) | TEN-495 (install and upgrade): **closed in code, not yet deliverable**. Installer, service and self-update exist. Still no Homebrew, Docker image, notarisation or packages |
| 12 | Engineering quality and reliability | Go lines 268K to 309K (14.7K of the growth is a vendored Markdown engine). Test files 579 to 647. `go test ./...` passes on Windows: 89 packages ok, 4,488 passes, 81 skips, 0 failures. Local branch sprawl gone (about 80 branches then, 10 now, all merged) | Section 12 | No gap workstream. Large files grew (`tui.go` 3,168 to 3,696) |
| 13 | Docs and community readiness | A public docs site is live: 43 pages plus an index, written for users, with no Jira IDs. The repo is still private; no community files. The site and README present the project as open source and link to a repo outsiders cannot open | https://findtime.net/tenant/docs/ (fetched 2026-11-13); `findtime-net/tenant-docs/src/` (44 Markdown files, 3,698 lines) | TEN-442 / TEN-449 (public repo, docs): docs **closed**; public repo and community **still open** |
| 14 | Self-improvement and learning loop | **No material change.** `internal/improve` is byte-identical to the previous base. The new Judge is a per-turn answer check, not a learning loop, and is off by default | `git diff --stat d2497d6 HEAD -- internal/improve` (empty); `cmd/tenant/judgeloop.go:1-7` | TEN-496 (learning loop): **still open**. No skill outcome scoring; SoulNudge and reflection still off by default; `baselines/fitness.json` still not shipped |

---

## 1. Messaging channels and surfaces

| Surface | Evidence | Maturity |
|---|---|---|
| TUI (Bubble Tea, streaming, turn blocks rendered from Markdown, activity feed, slash commands, image paste, mid-turn interject) | `internal/tui/tui.go` (3,696 lines), `internal/tui/cmd_*.go` (43 registered slash commands), `internal/md` (in-house goldmark copy) | Polished |
| CLI: one-shot per plugin (`tenant web/os/sql/...`), `chat` over stdin, `orchestrate`, `research`, `goal`, `review` | `cmd/tenant/cliregistry.go` (38 top-level commands plus 10 plugin commands) | Works |
| Web dashboard: server-rendered, Datastar/SSE, streaming chat with tool cards and an activity rail, UI components, about 20 pages, sign-in with back-off | `internal/dashboard/ssr_chat.go`, `ssr_activity.go`, `auth.go`; 2,081 lines of HTML templates | Works to polished |
| Discord relay: DM the agent, approve with buttons/cards, attachment inbox, photos to the model, catch-up of DMs sent while down | `cmd/tenant/discordrelay.go` (711 lines), `discordingest.go:246-298`, `discordmgr.go:246` | Works; used in production on the operator's server |
| iMessage: native macOS (chat.db read + AppleScript send) or BlueBubbles; responder with passive and respond modes, allowlist per message, Contacts names | `cmd/tenant/imessagerelay.go:84-85, 270-273`; `internal/plugins/imessage/` | Works. Now all on main. Group chats, SMS and RCS never drive the agent. Native path is macOS only |
| MCP server (stdio; SSE gateway now token-authenticated; peer listener over streamable HTTP) | `internal/mcpserver/handler.go:152-184`; `cmd/tenant/commands.go:458-470`; `cmd/tenant/ssegateway.go:30` | Works. Two memory tools by default; `--tools` also exposes the live tool mux (this was already so at the previous review, which under-reported it) |
| JSON API | `internal/dashboard/rest.go`, `rest_logs.go` | Works |

**Not present:** Telegram, Slack, WhatsApp, Signal, Matrix, email-as-a-channel, SMS, voice (no STT or TTS; a search for telegram, whatsapp, signal-cli, matrix, whisper, tts and stt in non-test Go finds nothing relevant). No mobile app. Channel breadth is unchanged: two chat channels plus local UIs.

Maturity: polished local surfaces, narrow reach. No change in channel count.

## 2. Tool execution: shell, files, code, background processes

- **Tools:** `os_sysinfo`, `os_read_file`, `os_read_image`, `os_list_dir`, `os_processes`, `os_write_file`, `os_append_file`, `os_edit_file`, `os_make_dir`, `os_exec`, and new `os_process` (`internal/plugins/osys/dispatch.go:107-130`).
- **Exec:** `workdir` (default: home), `timeout` (default 120s, model cap 600s), head+tail clipping (`osys.go:31-32`; `dispatch.go:128-129`). `os_exec` is off unless enabled, then gated by the `exec` category.
- **Background processes:** `background: true` returns an id at once; a foreground command still running at its timeout is moved to the background instead of killed. `os_process` lists, pages the log, waits, kills, and writes one line to stdin. Logs live under `<data>/proc/`; processes die with Tenant; a finished one is forgotten after an hour (`process.go:1-13`). **Host surfaces only**: a sandboxed surface keeps kill-at-timeout.
- **File reads:** `offset` and `limit` in lines, about 100K characters a page, with `next_offset` (`osys.go:34, 160-209`). The old 64 KB dead end is gone.
- **Writes:** temp file plus rename, so a kill mid-write leaves the original (`osys.go:253-292`).
- **Read-only commands** (`ls`, `cat`, `rg`, `git status` and the like) run without a prompt on the host and Discord when every segment only looks and nothing redirects; iMessage stays deny-by-default (`classify.go:80-120`; `cmd/tenant/approval.go:45-61`).
- **Command rules:** `os_exec(<pattern>)` allow/deny/ask, matched per segment of a compound command (`internal/approval/rules.go:79-102`).
- **Still missing:** no PTY (no pty dependency in `go.mod`), so full-screen or truly interactive programs do not work; stdin is one line at a time. No dedicated Python or JS runner and no notebook; code runs through the shell. No content-search tool of its own (the model uses `rg` through exec).
- **Classifier:** still regex-based and still described as a UX layer, not the boundary (`internal/sandbox/sandbox.go:3-5`). The destructive check was narrowed (fewer false positives, per the changelog; not re-audited here).

Maturity: works, and now at rough parity with peers on the basics. Behind on PTY and on background jobs inside a sandbox.

## 3. Browser automation and computer use

- A chromedp-driven headless Chrome per agent: `web_search`, `web_navigate`, `web_read`, `web_find`, `web_links`, `web_screenshot`, `web_click`, `web_fill`, `web_select` (`internal/plugins/web/dispatch.go:114-135`). Stealth JS, egress policy and proxy unchanged.
- **Vision input now exists.** A message can carry images (`internal/model/image.go:12-25`). `web_screenshot` shows the model the top of the page as a JPEG (`screenshot_view.go`); `os_read_image` shows the file. Images are scaled to 1568 px, kept for the last three exchanges, at most 8 a request (changelog; plan `docs/plans/gap1-vision-plan.md`).
  - Anthropic backend: base64 image blocks (`backend/anthropic/anthropic.go:109-137`).
  - OpenAI-compatible backend: `image_url` parts, with a launch-time probe of whether the model can see (`backend/vllm/images.go:37-57`, `probe.go:52-55`).
  - Claude Code CLI backend: images are stripped to a note with the path (`backend/claudecode/claudecode.go:241`).
- **Browser control is still text and DOM.** `web_click` takes visible text or a CSS selector (`dispatch.go:126`). There is no click-at-coordinates, so a vision model can read a screenshot but cannot act on what it sees except through locators.
- **No desktop computer use:** no hits for xdotool, cliclick, robotgo or "computer use" in the code.
- Eval coverage of vision is one task (`internal/eval/tasks/vision/vision-001-read-digits.yaml`).

Maturity: works for text-centric browsing with screenshots the model can now read; still behind vision-driven browser agents and has no computer use.

## 4. Memory and personalisation

No material change. About 8.8K non-test lines in `internal/memory/` (8,811), plus `internal/improve/` (1,968, unchanged).

- **Tiers:** T0 soul, T1 working window, T2 episodic (SQLite + FTS5 + embeddings), T3 semantic facts with quality and decay, T4 skills, T5 append-only JSONL archive, a user profile injected every turn, per-project SME documents, Markdown import (`internal/memory/*`).
- **Retrieval:** hybrid FTS5 + cosine; the vector search is still a brute-force full-table scan (`internal/memory/episodic/search.go`, unchanged).
- **Compaction:** archive-sourced with a goal header. New: image references survive compaction as paths (`internal/memory/assemble/images.go`).
- **Tools:** `memory_remember`, `memory_recall`, `memory_search`, `memory_history`.
- **Privacy:** retention per data class, forget, export; now also covers saved attachments (`internal/privacy/files.go`, +45 lines).
- **Gap, unchanged:** embeddings need a separate endpoint, usually a local Ollama. Without it retrieval is FTS only and the README says the agent "cannot recall".

Maturity: polished on design and tests; effectiveness still shown only on one operator's deployment.

## 5. Extensibility: skills, plugins, MCP

- **First-party plugins (Go, compiled in):** osys, web, sql (SQLite), gsuite, x, imessage, discord, wiki, github, atlassian, simplefin, crm, cron (`internal/plugins/`, about 20.3K non-test lines). Same set as before. New host tool: `ui_render` (table, steps, choices, form; `internal/uicomp/uicomp.go`, `cmd/tenant/toolmux_build.go:108`), on by default.
- **SQL hardening:** `sql_query` runs on a connection the engine holds read-only; `ATTACH`, `DETACH` and `VACUUM INTO` are refused (changelog; `internal/plugins/sql`).
- **No third-party plugin API by design** (`docs/adr/0001-appliance-not-framework.md`). Extension is through MCP servers and skills.
- **MCP client:** stdio and Streamable HTTP, OAuth 2.1 with DCR and PKCE; the agent adopts servers live with `mcp_add/list/remove/trust` (`cmd/tenant/mcptools.go`). Near-unchanged (24 lines in `internal/plugins/mcpremote`).
- **MCP server:** memory tools, plus the whole tool mux with `--tools` (`cmd/tenant/commands.go:458-470`). The SSE transport now requires a bearer token, checks Origin and Host, and binds a session id (`cmd/tenant/ssegateway.go:30`; `tenant security audit` reports it as authenticated, `securityaudit.go:263`).
- **Agent Skills (SKILL.md):** import, export, version, trust. Bundled `scripts/`, `references/` and `assets/` are still only listed, not run (`internal/skillpkg/skillpkg.go:19-20`). No diff since the previous review.
- **No skills hub, registry or community catalogue.** Unchanged.
- **Lazy tool loading** (`internal/agent/lazytools.go`), now with a sticky surfaced set per session.

Maturity: MCP works well; skills ecosystem absent. No progress on the ecosystem gap.

## 6. Autonomy and scheduling

- **Cron engine:** crontab and `@every`, prompt and exec jobs, run history, delivery to Discord (`internal/cron/`, `cmd/tenant/cronmgr.go`). Fixed: a scheduler spin on a daylight-saving fall-back day that would have hung every daily job from 2026-10-31 (`internal/cron/schedule.go`, regression test in `schedule_test.go`). That it reached main at all is a reliability note for unattended use.
- **`/goal`:** unchanged (loop on a stop condition, LLM judge, turn cap).
- **`serve` daemon:** improvement scheduler, cron, retention, MCP connectors, relays. New: ordered shutdown that cuts a running turn, records it and tells the operator's channel; after boot, a reminder for the cut turn and a catch-up of Discord DMs. Nothing is re-run automatically (`internal/lifecycle/lifecycle.go:1-15`; `cmd/tenant/discordmgr.go:246`).
- **`tenant service`:** runs serve under launchd, systemd (user unit) or a Windows task/service, so 24/7 operation no longer needs a hand-written unit (section 11).
- **Gaps, unchanged:** no general heartbeat check-in. No event triggers beyond cron, inbound messages, log alerts and the alert investigator (no webhook or file-watch code in `cmd/tenant` or `internal/cron`).

Maturity: works; more robust to restarts, but no new kinds of autonomy.

## 7. Multi-agent and delegation

- `spawn_agent` and `team_await`, plus the bus tools; sub-agents run concurrently in-process. Five built-in specialists plus "Main".
- **`team_await` is event-driven** (TEN-529): returns when a helper finishes or messages the orchestrator, and says who is still running; the no-news ceiling is 30 minutes (`cmd/tenant/team.go:1022-1028`). Motivated by a field measurement in the changelog (101 of 127 awaits timing out at 3 minutes).
- **Run limits are off by default** (TEN-508): tokens, cost, wall time, tool calls and helpers per run are unlimited unless set. Machine guards remain: 4 concurrent helpers, 4 browsers, 1,000 bus messages (`internal/orchestra/budget.go:86-94`). This trades a safety default for fewer cut-off runs; an unattended team run has no built-in spend ceiling.
- `research` mode, sensitive-data provenance across agents, Tenant-to-Tenant peering: unchanged. Per-peer rate limiting is still absent (only a pairing limiter, `internal/peering/pair.go:72-98`).
- **Gaps, unchanged:** no isolated workspace or worktree per sub-agent, no per-sub-agent sandbox. Sub-agents inherit the spawning turn's exec surface (`cmd/tenant/execprofiles.go:106-123`).

Maturity: works; coordination is cheaper, isolation is unchanged.

## 8. Model flexibility

- **Backends:** `vllm` (any OpenAI-compatible endpoint), `anthropic`, `claudecode` (spawns `claude -p`), `echo` (`internal/model/backend/`).
- **Provider presets:** vLLM, Ollama, llama.cpp, generic OpenAI-compatible, OpenAI, Grok, Z.ai (four variants; still the default), Sakana, Anthropic, Claude Code CLI, echo (`internal/config/providers.go:34-145`). **No new presets.** Still no first-class Gemini, OpenRouter, Bedrock, Vertex or Azure.
- **Text-protocol tool calling:** qwen, hermes, mistral, llama3_json, and new `glm` for Z.ai's GLM-4.5 family and later (`internal/model/toolfmt/glm.go`, 150 lines, with tests).
- **Vision:** see section 3. `vision` on a provider overrides the probe.
- **Routing:** per-role routing, fallback chain with health gating, the Supra small-model router (`internal/route/supra.go:22`). New: `request_timeout` per provider so a stalled server fails over (`internal/config/config.go:358`); a named self-hosted model gets its real context length from `/v1/models` (changelog TEN-507).
- **Prefix-cache stability:** the surfaced tool array only grows within a session; `per_turn_tool_ranking: true` restores the old behaviour (`config.go:145-150`).
- **The Judge:** an optional decision model (a "System One" server; one kind, `clef`) triages prompts and checks a task's answer, sending work back up to 3 rounds by default. **Off by default**, needs an external server address, fails open when the judge is down (`internal/agent/judge.go:11-23`; `cmd/tenant/judgeloop.go:33-35`; `internal/model/systemone/`). Only one judge kind exists and its live verification is described in a plan, not in CI.
- **Per-call token overhead:** not re-measured. The previous figure (about 8.3K) came from a judge's report and is not recorded anywhere in the repo. A bare `tenant chat --backend echo` on an empty config reports `tokens=243 soul=241` with no tools, which is not comparable. Changes that bear on it: `ui_render` is on by default (one more schema), and the sticky tool set lets servers cache the prefix.

Maturity: polished for local-model use; still one of the strongest areas. Hosted-provider breadth is unchanged.

## 9. Security

- **Approvals:** eight categories (`exec`, `write`, `destructive`, `web`, `send`, `egress`, `mcp`, `sandbox`), each ask/allow/deny per surface; every category defaults to **ask** on local surfaces and Discord, and to **deny** on iMessage (`internal/approval/categories.go:16-38`; `broker.go:101, 107-108, 148`). Per-tool rules and now per-command rules. Read-only shell commands skip the prompt (section 2).
- **Sandbox backends** (`internal/sandbox/sandbox.go:7-21`): `host` (none), `restricted` (macOS Seatbelt only), `jobobject` (Windows resource caps only), `container` (ephemeral Docker container per command).
- **Default posture, as built** (`cmd/tenant/execprofiles.go:364-374, 427-450`):
  - TUI, dashboard, Discord, iMessage: **host**, behind approvals. This is now on main.
  - Cron on macOS: Seatbelt in **dry run** (a refusal is logged and the command re-runs on the host).
  - Cron on Linux and Windows: **host**. `tenant sandbox probe` on this Windows machine with an empty config shows `host`, isolation `none`, on all five surfaces.
  - The container backend is **opt-in** through `exec_profiles` in the settings file. Nothing selects it by default. There is no agent tool that reads or writes a profile.
- **Container backend, state:** wave-1 hardening is closed apart from live-only checks: clean CLI launch on a vetted endpoint, version floors that refuse, fixed argv schema, `--pull never`, canonical mount sources, reaping and a start-up sweep, network modes that fail closed, escalation inside a fresh container with one grant, approval bound to a digest of the exact run (`container.go:3-28`; `docs/plans/docker-sandbox-hardening.md:438-466`). From wave 2: own bridge with inter-container traffic off for `full` (`container_network.go:29-34`), HOME on a tmpfs, per-process CPU limit, `debian:trixie-slim` base pinned by digest, a boundary record in the Security log, `tenant sandbox stop-all`.
  - **Tested on a real engine once, on one machine:** Docker Desktop on Windows, 2026-11-10, 14 of 14 live cases and selftest 4 of 4, after a first run where every live case timed out on a stuck engine; the live run found a real defect (duplicate mount on an approved re-run) that the fake CLI had hidden (`docker-sandbox-hardening.md:468-481`).
  - **Not tested on a real engine:** Linux and macOS hosts (wave 3, not started), Podman, a `tcp://` TLS endpoint, `stop-all`. The live suite is gated on `TENANT_SANDBOX_LIVE=1` and does not run in CI (`container_live_test.go:43-44`); it was skipped in this audit (15 skips).
  - **Open in wave 2:** read-only binds over `.git/hooks` and editor/agent folders, a concurrency cap, a circuit breaker, a kill switch setting, the seccomp delta, image scan and signing, a per-run network scope. Under `full` the host's loopback is still reachable (`docker-sandbox-hardening.md:498-521`).
- **Prompt-injection defences:** sensitive-data information-flow tracking (unchanged, now also labels images, `image.go:22-24`); agent-written skills start untrusted; untrusted-data clauses on cron and log investigation (`cmd/tenant/cronmgr.go:56-61`, `loginvestigate.go:87`). There is still **no general spotlighting or delimiting of tool results in the main agent prompt**. New: the dashboard never fetches an image URL the model wrote (CSP `img-src 'self' data:`, `internal/dashboard/auth.go:122`); the TUI shows control characters as markers in approval prompts and strips escapes elsewhere (`internal/tui/safetext.go`); UI-component messages are marked "via choices/form" and cannot be slash commands.
- **Dashboard:** sign-in back-off after repeated wrong keys from one address (`internal/dashboard/auth_pages.go:126, 177-178`); the cookie is refused on cross-site requests (`auth.go:93-95`).
- **Secrets, logs:** unchanged (owner-only files or DACL, log scrubbing, hash-chained Security log, `tenant security audit`). `SECURITY.md:48-49` now states who is trusted: the local user account and its processes.
- **Supply chain:** CodeQL/security workflow; SBOMs. **Keyless signing and attestation are skipped while the repo is private** (`release.yml:31-38, 55`), so the two release candidates carry checksums and SBOMs but no signature.
- **Gaps:** host execution by default; no confinement on Linux or Windows without Docker; no peer rate limiting; no tool-result spotlighting; team runs have no default spend limit.

Maturity: polished on approvals, audit and the opt-in container path; default isolation is still weak and the strong path has one machine's worth of live evidence.

## 10. Observability and operations

- **Log book:** unchanged in size (about 13.6K non-test lines in `internal/logs/`): query engine, alerts, LLM alert investigator, retention. A housekeeper lease race was fixed (changelog).
- **Dashboard:** approvals, agents, cron, eval, goals, memory inspector, MCP, keys, rules, safety, skills, logs, research, access (`internal/dashboard/`, about 11.4K non-test lines).
- **`tenant doctor [--fix]`:** 1,591 lines; new rows for the container sandbox and the service (`cmd/tenant/doctor.go`).
- **`tenant backup create|verify|restore`**, run traces, token usage ledger: unchanged.
- **`tenant update`** (new): resolves the release, verifies the checksum (and the cosign bundle when present and cosign is installed), smoke-runs the new binary, backs up the data, swaps the binary keeping `tenant.prev`, restarts the service, confirms the version, rolls back on failure. A dev build refuses. Nothing is applied automatically (`cmd/tenant/update.go:162-232`; `internal/selfupdate/`). A private repository needs `GITHUB_TOKEN` (`internal/selfupdate/release.go:58-59`).
- **`tenant service`** (new): `install|uninstall|status|start|stop|restart`; restart waits for the running turn (`cmd/tenant/service.go:3-10`).
- **Security log:** record `4201` holds the image id, mounts, network, limits, env names, output size and duration of each sandboxed command (changelog; `docs/plans/docker-sandbox-hardening.md:516`).
- **Not verified:** the release smoke job that exercises `tenant update` against a real release only runs for public releases (`release.yml:67-73`), so self-update has unit tests and a snapshot packaging job but no end-to-end run against a published release that I could find.

Maturity: polished; still the strongest area relative to peers, and the manual-upgrade weakness is addressed in code.

## 11. Install, setup and cross-platform support

- **Install:** a single CGO-free static binary for linux, darwin and windows on amd64 and arm64 (`.goreleaser.yaml:5-17`). New: `install.sh` and `install.ps1` as release assets; they verify SHA-256 against `checksums.txt` and install per user without sudo or admin (`scripts/`). The `v0.2.0-rc.2` release carries both scripts, six archives, SBOMs and checksums (`gh release view`).
- **What blocks a newcomer today:**
  - The repo is private. The README and docs one-liners point at `github.com/Dog279/TENANT/releases/latest/download/install.sh`, which the workflow's own comment says answers 404 on a private repository (`release.yml:67-69`). Not fetched unauthenticated in this audit.
  - "Latest" is `v0.1.0`, which predates the scripts (13 assets, none of them a script). The scripts exist only on pre-releases.
  - Still **no** Homebrew tap, Docker image, macOS notarisation, .deb/.rpm, winget or scoop (decided in `docs/plans/install-upgrade-plan.md:18-19`).
- **Setup:** `tenant setup` wizard (Modify now opens a section list), `tenant doctor`, echo backend for a no-key trial.
- **Service install** (new, `cmd/tenant/service_*.go`):
  - macOS: a LaunchAgent, logs in `~/Library/Logs/tenant/`.
  - Linux: a systemd **user** unit; it stops at logout unless lingering is enabled, and install prints the command rather than running it (`service_linux.go:6-7, 75`). No system-wide unit.
  - Windows: a per-user logon task registered from XML, no elevation needed; `--system` installs an SCM service and needs an elevated terminal (`service_windows.go:14-17, 147-160`). The no-admin path and a restart race were fixed today (PR #70), which suggests the Windows service path is new and lightly used.
- **CI:** ubuntu/windows/macos matrix with build, vet and test; `-race` on Linux only (`ci.yml:46-52`; this was already so at the previous review, which did not say it); an offline smoke eval. A packaging workflow installs a snapshot through both scripts on three OSes.
- **Platform skew:** iMessage native and Seatbelt are macOS only. The container sandbox has been run live on Windows only.
- **Unchanged gap:** Google Workspace still needs the operator's own GCP OAuth client (`internal/plugins/gsuite/embedded_oauth_client.json` is `{}`).

Maturity: works, and the mechanics are now close to peers; not usable by the public until the repo and a non-pre-release are published.

## 12. Engineering quality and reliability

- **Language:** Go 1.26 (toolchain 1.26.9), 19 direct dependencies (`go.mod`). The Markdown engine is a vendored, trimmed goldmark under `internal/md` so `go.mod` gained nothing.
- **Size:**

  | Measure | 2026-10-02 | 2026-11-13 |
  |---|---|---|
  | Go, all | 268,023 | 309,457 |
  | Go tests | 116,349 (579 files) | 129,612 (647 files) |
  | Go non-test | ~151,700 | ~179,800 (704 files) |
  | of which vendored goldmark (`internal/md/goldmark`) | 0 | 14,686 (1,450 test) |
  | Go non-test, own code | ~151,700 | ~166,600 |
  | `cmd/tenant` non-test files | 162 | 177 (52,590 lines) |
  | Largest package, `internal/plugins` (non-test) | ~19K | ~20.3K |
  | `internal/sandbox` (non-test) | not reported | 6,710 |
  | Dashboard HTML templates | 2,429 | 2,081 (`.html` only; 2,553 with `styles.css`) |
  | Docs in `docs/` (Markdown) | 70 files, 23,765 | 67 files, 23,664 |
  | Markdown, whole repo | not reported | 109 files, 27,517 |
  | Public docs site source | none | 44 files, 3,698 |
  | CHANGELOG | 93 | 519 |

  Test-to-code ratio on own code is about 0.77:1, unchanged.
- **Largest files:** `internal/tui/tui.go` 3,696; `cmd/tenant/commands.go` 3,348; `internal/agent/agent.go` 2,527; `cmd/tenant/research.go` 1,754; `cmd/tenant/doctor.go` 1,591; `internal/toolmux/mux.go` 1,516. All grew.
- **Checks run for this audit** (Windows 11, `go1.26.9 windows/amd64`, from the repo root at `8b9bbf7`):
  - `go build ./...`: exit 0 (11.8s, warm cache).
  - `go vet ./...`: exit 0 (7.1s).
  - `go test -json ./...`: **exit 0. 89 packages ok, 0 failed, 13 with no test files. 4,488 test and subtest passes (3,574 top-level), 81 skips (78 top-level), 0 failures. 269s wall, nothing cached.**
  - **Not run with `-race`**: this machine has no C compiler. CI runs `-race` on Linux only.
  - No test binary was blocked by endpoint security (no `exit status 0xffffffff`).
  - Skips by package: cmd/tenant 26, sandbox 24, osys 10, secrets 4, logs/store 4, others 13. By reason: 15 live-container tests (`TENANT_SANDBOX_LIVE`), about 19 symlink cases that need privileges on Windows, about 20 sh/POSIX-only cases, 5 with no restricted backend on Windows, 4 POSIX-mode cases, the rest live or perf gates and in-test skips.
  - Not comparable one-for-one with the previous run (macOS, 4,149 passes, 23 skips): the OS differs, so the skip set differs.
- **Top-level `func Test`:** 3,716 (was 3,336).
- **Eval harness:** 78 YAML tasks (smoke 5, adversarial 5, route 1, fitness 53, ui 13, vision 1); was 64. Baselines shipped: `full`, `smoke`, `logbook-darwin`. Still no `fitness.json`.
- **TODO/FIXME in own non-test code:** 1 hit. Deferred work lives in Jira and in plan documents with status ledgers (`docs/plans/`, 13 files, 3,799 lines). There is no `TODOS.md` in the tree.
- **Rough edges:**
  - Very large files keep growing.
  - Field-found defects this period that tests had not caught: the cron daylight-saving spin, a lost-stdout race on Windows, a serve start-up hang on a stalled docker CLI, the duplicate-mount escalation bug, the Windows service needing admin. Each has a regression test or a fix note in the changelog; they also show how much rests on one operator's machines.
  - 89 of 210 non-merge commits are docs commits (plans, ledgers, regenerated facts).
  - One stale message: `sandbox.go:375` still says "the container backend once it ships".
- **Architecture tests:** `internal/archtest`, unchanged.
- **Local hygiene:** 10 local branches, all merged, one worktree (was about 80 branches and worktrees).

Maturity: high engineering rigour for a solo project; unchanged in kind, larger in volume.

## 13. Docs and community readiness

- **Public docs site: live.** https://findtime.net/tenant/docs/ loads (fetched 2026-11-13) with six areas and 43 pages plus an index: setup (7), first launch (7), models (10), MCP and tools (7), dashboard (7), reference (5). Spot check: the service page already describes today's Windows change (per-user task, no admin, the 10-second address wait). Source is `findtime-net/tenant-docs/src/` (44 Markdown files, 3,698 lines in the local checkout, which is behind its remote).
  - Written for users, task-first, and free of internal Jira IDs (0 `TEN-` strings in the source). The repo's own docs and changelog still carry about 630.
  - Reference pages for the CLI, slash commands, `config.json`, settings and environment exist (158 to 214 lines each). Some pages are thin (`dashboard/pages.md` 35 lines, `reference/settings.md` 34).
  - The version menu is empty (`public/tenant/docs/versions.js` is `[]`): no release has a frozen copy, though the repo rule says each release gets one.
  - No tutorials beyond the quickstart, no troubleshooting or FAQ section, no screenshots checked.
- **In the repo:** MIT `LICENSE`, `SECURITY.md` (now with a trust statement), README, Quickstart, an as-built `ARCHITECTURE.md` (1,696 lines with CI-generated facts), one ADR, plans and runbooks, and a `CLAUDE.md` rule that every behaviour change ships with its docs change.
- **Not public:** the GitHub repo is private, 0 stars, 0 forks. No `CONTRIBUTING`, `CODE_OF_CONDUCT`, issue or PR templates (`.github/` holds workflows only). No Discord, forum or discussion space. No users besides the author.
- **Docs vs code mismatches:**
  - README: "sanitized public snapshot" while the repo is private. Unchanged.
  - The docs site says Tenant is open source under MIT and links to the GitHub repo and its issues page, and the install page gives one-liners against that repo. An outside reader cannot open any of them. The site is ahead of what is actually available.
  - README Features names only Seatbelt for the sandbox and the repository layout lists "host, restricted, jobobject" (`README.md` Safety bullet and layout block). The container backend is not mentioned there. This is an under-claim.
  - The install text says archives come "with checksums, a cosign signature and SBOMs"; the two release candidates have no signature because signing is skipped while private.

Maturity: user documentation went from none to a credible reference site; community readiness is unchanged at zero.

## 14. Self-improvement and learning loop

No material change. `internal/improve`, `internal/skillpkg` and `internal/memory/skills` are identical to the previous base.

- **Distillation** (episodic to semantic facts), on by default under serve.
- **Skill induction:** mines recurring tool-call sequences and proposes skills that need operator acceptance (or an auto-accept mode). Heuristic, not LLM-authored. The agent can also `skill_save` (untrusted until reviewed).
- **Skill refinement:** upsert drops a skill to untrusted. There is still **no outcome-based skill scoring** and no retrieval gating on main.
- **SoulNudge:** still **off by default**, and still fails closed without `baselines/fitness.json`, which is still not shipped (`cmd/tenant/commands.go:2901-2908`; `baselines/` has `full`, `smoke`, `logbook-darwin`). One fix: the gate now looks for the baseline under the data dir.
- **Reflection/SME synthesis:** still off by default (`commands.go:2918-2925`).
- **Adjacent, not a learning loop:** the Judge checks one turn's answer and sends it back; nothing it finds is stored or learned from. The Quality model (the grader for evals) was renamed and separated from it.
- **Assessment:** unchanged. The loop remains conservative and human-gated, the headline jobs are off by default, and there is no new evidence that they improve outcomes.

Maturity: works in parts; no movement since the previous review.

---

## Stars and users

A private repo with 0 stars, 0 forks, one person behind every commit, and one known deployment (the author's). Two release candidates were published today; neither is reachable by the public. It is still pre-public.

## Honest summary

**Strongest 3**
1. **Operations and observability:** the log book, dashboard, doctor, verified encrypted backup, privacy controls, hash-chained Security log and security audit, now joined by a self-update with backup and rollback, an OS service installer on three platforms, and shutdown handling that records and reports a cut turn.
2. **Local-model engineering:** family-aware text tool calling (now including GLM), model profiles, a launch-time vision probe, lazy and now cache-stable tool loading, per-provider timeouts feeding a health-gated fallback chain, a small-model router, and an optional judge. Single CGO-free binary.
3. **Engineering discipline and safety design:** about 167K lines of own Go with about 128K lines of tests (4,488 passing, 0 failing on Windows in this audit), a three-OS CI matrix, architecture tests, approvals on by default in every category, information-flow tracking, and a container sandbox whose hardening is documented item by item, with its open items stated.

**Weakest 3**
1. **Reach and ecosystem:** unchanged. Discord and iMessage only; no Telegram, Slack, WhatsApp, Signal or voice. No skills hub, no community, still private with one user. The public docs site now describes a product the public cannot download.
2. **Default isolation:** every interactive surface runs commands on the host by default, and on Linux and Windows so does cron. The container sandbox is opt-in, has been run on a real engine on one Windows machine only, and its Linux and macOS work has not started. Team runs no longer have default spend limits.
3. **Learning loop and autonomy depth:** no change in six weeks. No skill outcome scoring, the self-improvement jobs are still off or human-gated, the fitness baseline is still not shipped, and there are no event triggers, heartbeat or per-sub-agent workspaces. Vision reached the model, but the browser still cannot act on what the model sees, and there is no computer use.
