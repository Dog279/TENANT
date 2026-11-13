# Judge C: industry analyst / buyer's guide lens (2026-11-13)

Lens: a technical newcomer choosing an agent to self-host **today**. I weight real-world usability, total cost of ownership (token overhead, maintenance and upgrade pain) and maturity. Same weights as my 2026-10-02 scorecard. Tenant is scored as `main` only this run (`8b9bbf7`, the branches from last run are merged). "Can a newcomer get and run it" still counts against it.

Method: I started from my 2026-10-02 score for each cell and moved it only where this run's evidence changed, or where the new dossier showed my old evidence was wrong.

## Spot checks

All run on 2026-11-13. I checked that install endpoints answer. I did not download or run any installer, and I ran nothing from the rival clones.

1. **Can a newcomer get Tenant? No.** `gh repo view` reports `PRIVATE`, 0 stars. An unauthenticated request for the repo page returns 404. So does `releases/latest/download/install.sh`, which is the exact one-liner printed on the public install page (https://findtime.net/tenant/docs/setup/install.html, HTTP 200). That page also says "Tenant is open source under the MIT license". `gh release list` shows `v0.1.0` as "Latest" with no install script among its assets. `v0.2.0-rc.2` (a pre-release) carries `install.sh`, `install.ps1`, six archives, SBOMs and `checksums.txt`, and no signature file. `internal/selfupdate/release.go:58-59` confirms self-update needs a token against a private repo. The dossier held up.
2. **Tenant's default exec posture.** `cmd/tenant/execprofiles.go:364-374` on main: any interactive surface without an `exec_profiles` entry gets `backend = "host"`. Cron keeps the built-in backend, which is Seatbelt on macOS only (`:427-450`). The dossier held up.
3. **Tenant's exec parity.** `os_process` is registered (`internal/plugins/osys/dispatch.go:130`, with list, log, wait, kill, write). Paged reads exist (`osys.go:34`, `:158-174`, with `next_offset`). `go.mod` has no pty dependency. `internal/config/providers.go` has no Gemini, OpenRouter or Bedrock preset. I found no Telegram or Slack channel adapter. The dossier held up.
4. **OpenClaw's defaults and availability.** `src/infra/exec-approvals-config.ts:94-96`: `DEFAULT_SECURITY = "full"`, `DEFAULT_ASK = "off"`, fallback `deny`. `src/agents/sandbox/config.ts:220`: sandbox mode falls through to `"off"`. `src/memory-host-sdk/dreaming.ts:281`: dreaming defaults to on. The npm registry serves `openclaw` 2026.11.3 as latest, `https://openclaw.ai/install.sh` answers 200, and GitHub's latest release is v2026.11.3 (2026-11-10). A newcomer can get it. The dossier held up.
5. **Hermes's defaults and availability.** `hermes_cli/config_defaults.py:285` `"backend": "local"`; `:1656` approvals `"mode": "smart"`; `:1714` plugins `"isolation": "in_process"`; `:982` webapp off; `:1977` tool search `"auto"`. `https://hermes-agent.nousresearch.com/install.sh` answers 200 and GitHub's latest release is v0.23.1 (2026-11-10). A newcomer can get it. The dossier held up.

Two corrections to my own 2026-10-02 justifications, from this run's dossier. Neither moves a score. I wrote that Tenant "exposes only 2 MCP server tools"; `--tools` already exposed the whole tool mux then. I wrote that Tenant has "a 3-OS `-race` CI"; `-race` runs on Linux only.

## Scores (1–10)

| # | Dimension | Tenant | OpenClaw | Hermes | Justification (decisive evidence) |
|---|---|---|---|---|---|
| 1 | Messaging channels/surfaces | 4 | 10 | 9 | Tenant still has two chat channels (Discord, iMessage) plus a TUI, dashboard and CLI. Photos now reach the model and the dashboard chat streams, but there is no new channel and no voice. OpenClaw lists 33 channels (X added) plus native apps. Hermes bundles 26 adapters, voice, a new browser Webapp and Group Chats. |
| 2 | Tool execution | 7 | 9 | 9 | Tenant closed the three gaps I named: background processes with `os_process`, paged reads, and `workdir`/`timeout` on main, plus per-command rules and atomic writes. It still has no PTY, no code runner, and background jobs only on host surfaces. OpenClaw adds `durable` background exec to an already dense tool. Hermes keeps PTY, a process registry, a persistent kernel and six core backends plus bubblewrap. |
| 3 | Browser & computer use | 5 | 9 | 9 | Tenant's model can now see screenshots and image files, but it can only act through text or CSS locators and has no desktop control. OpenClaw is unchanged: vision browsing, real-profile attach and a `computer` tool. Hermes added password-manager sign-in, Bot Screen with human takeover, and browser and computer use inside a sandbox. |
| 4 | Memory & personalization | 8 | 8 | 6 | No material change for any of the three. Tenant keeps its tiers, hybrid retrieval and privacy controls, with brute-force vector search and a separate embeddings endpoint. OpenClaw's Knowledge plugin is main only and off by default. Hermes's built-in memory keeps its 2200 and 1375 character limits. |
| 5 | Extensibility (skills/plugins/MCP) | 6 | 9 | 9 | Tenant's skills code has no diff since last run: no hub, and skill scripts are still not executed. Its MCP client remains solid. OpenClaw has 186 bundled plugins on main, MCP Relay and MCP Events, and ClawHub. Hermes launched a catalog of about 2,300 pinned plugins, loaded in-process by default. |
| 6 | Autonomy & scheduling | 6 | 9 | 8 | Tenant added shutdown handling and a service installer but no new trigger type, and a daylight-saving cron hang reached main before it was fixed. OpenClaw added opt-in event triggers from MCP servers to cron, webhooks, Gmail and IMAP. Hermes added cron cancel, missed-run policy and self-healing re-runs, which is robustness, not new reach. |
| 7 | Multi-agent/delegation | 6 | 8 | 8 | Tenant's `team_await` is now event-driven, but sub-agents still share the parent's workspace and exec surface, and run limits are off by default. OpenClaw added sub-agent and process panels and removed fleet management. Hermes made delegation pluggable and keeps worktree-isolated children. |
| 8 | Model flexibility & local LLM | 8 | 9 | 9 | Tenant closed "no multimodal input" (Anthropic and OpenAI-compatible backends, with a launch-time vision probe) and added a GLM tool format, per-provider timeouts and a cache-stable tool set. It still has no Gemini, OpenRouter or Bedrock preset. OpenClaw has 76 provider plugins on main. Hermes has 39 plus a one-click managed llama.cpp runtime. |
| 9 | Security & safe defaults | 7 | 4 | 4 | Tenant still asks before every exec, write and send, and it closed the unauthenticated SSE gateway, hardened the dashboard and TUI, and hardened an opt-in container sandbox that passed 14 of 14 live cases on one Windows machine. Interactive exec still runs on the host by default, the release candidates are unsigned, and there is no outside scrutiny. OpenClaw's defaults are verified unchanged (`security=full`, `ask=off`, sandbox off) with 722 GHSAs and more bypass fixes each release. Hermes still defaults to the local backend, LLM-judged approvals and in-process plugins, and took one new High CVE. |
| 10 | Observability & ops | 9 | 8 | 8 | Tenant closed the gap I named last time: `tenant update` verifies, backs up, swaps and rolls back, and `tenant service` covers three OSes, on top of the log book, doctor and verified backups. The update path has not been run end to end against a public release. OpenClaw has OTel, Prometheus and a richer Control UI, but nine of fifteen stable releases fixed blocked or failed updates. Hermes added `prompt-size`, a stable channel and `hermes pm`, with Desktop idle burn still open. |
| 11 | Install/setup UX & cross-platform | 5 | 8 | 9 | Tenant now has checksum-verifying install scripts, a service installer and a wizard, tested by a packaging CI on three OSes. A newcomer still cannot get it: the repo is private, the documented one-liner returns 404, and "Latest" predates the scripts. There is no brew tap or Docker image. OpenClaw's one-liners and npm package work, with native Windows still weak. Hermes has working one-liners, signed desktop packages and a key-less first run, on a narrower set of platforms. |
| 12 | Engineering quality & reliability | 7 | 6 | 5 | Tenant has 4,488 passing tests and 0 failures, and the unpushed-branch problem is gone. Five field-found defects this period show how much rests on one operator's machines. OpenClaw's issue backlog shrank about 20%, but update failures dominated its release notes and its self-rating is still Alpha (68%, not re-scored). Hermes has a better release pipeline and a healthier CI sample, against a bug backlog up 40%, breaking changes in consecutive releases and one release built from the wrong commit. |
| 13 | Docs/community/ecosystem | 4 | 9 | 9 | Tenant now has a public, user-facing docs site of 43 pages with reference sections and no internal ticket IDs. It has no public repo, users, forum or contributors, and the site describes and links to a product outsiders cannot open. OpenClaw and Hermes have 394K and 263K stars, thousands of contributors and docs sites of 435 pages or more. |
| 14 | Self-improvement loop | 5 | 8 | 8 | Tenant's `internal/improve` is byte-identical to last run: induction is heuristic and human-gated, SoulNudge is off and its fitness baseline is still not shipped. OpenClaw's Skill Workshop 2.0 saves a version before every learned change and adds undo, which closes the rollback gap I named. Hermes's background-fork review is unchanged and gained locks and an optional approval queue. |

## Changes from my 2026-10-02 scores

Nine cells moved, all up. No cell moved down.

| # | Dimension | Product | Was | Now | Why |
|---|---|---|---|---|---|
| 2 | Tool execution | Tenant | 5 | 7 | Background processes, paged reads, `workdir` and `timeout` are on main (spot check 3). No PTY and no sandboxed background jobs keep it below the rivals. |
| 3 | Browser & computer use | Tenant | 4 | 5 | The model now sees screenshots and images. Control is still locator-only and there is no computer use. |
| 8 | Model flexibility & local LLM | Tenant | 7 | 8 | Multimodal input, one of the two gaps I named, is closed. The provider-preset gap is not. |
| 9 | Security & safe defaults | Tenant | 6 | 7 | The unauthenticated SSE gateway I cited is closed. Dashboard, TUI and SQL hardening are on by default. An opt-in container sandbox now has live evidence. Host exec by default and no outside scrutiny cap it at 7. |
| 10 | Observability & ops | Tenant | 8 | 9 | "No self-update", my one named gap, is closed in code, with a service installer added. Its delivery problem is scored under #11. |
| 11 | Install/setup UX & cross-platform | Tenant | 4 | 5 | Install scripts and a service installer exist and are CI-tested. The score stays at 5 because nobody outside can download them (spot check 1). |
| 13 | Docs/community/ecosystem | Tenant | 2 | 4 | A public user docs site went from none to credible. Community is still zero, and the site is ahead of what is available. |
| 14 | Self-improvement loop | OpenClaw | 7 | 8 | Skill Workshop 2.0 versions every learned change and adds undo. Last run I marked it down because `auto` mode had no rollback. |
| 3 | Browser & computer use | Hermes | 8 | 9 | Password-manager sign-in, Bot Screen with human takeover, and sandboxed browser and computer use are new capability. The removal of the old `browser_*` tools is upgrade pain, which I count in the TCO step. |

Cells I looked at hard and held:
- **Hermes #12 (5).** The evidence moved both ways: a tested stable channel and a better CI sample, against a 40% larger bug backlog, breaking changes and a wrong-commit release. The CI sample is 15 runs in two hours. Not enough to move.
- **OpenClaw #10 and #12 (8, 6).** Update failures were already my main complaint last run. This run's evidence is more detailed, not different in kind.
- **Tenant #7 (6).** Event-driven `team_await` is a real gain. Limits off by default is a cost risk. I count the risk in the TCO step.
- **Tenant #5 and #12 (6, 7).** The two corrections to my old evidence roughly cancel against unchanged gaps.

Tenant moved in seven cells and each rival in one. That reflects where the work landed: Tenant shipped into gaps I had named, and the rivals were already near the top of the scale on most rows.

## Overall

Weights for this lens (sum 100), unchanged from 2026-10-02:
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

Weighted sums (score × weight, in the weight order above):
- **OpenClaw:** 96 + 72 + 40 + 90 + 80 + 72 + 72 + 56 + 48 + 54 + 45 + 36 + 16 + 16 = 793, so 7.93.
- **Hermes:** 108 + 60 + 40 + 90 + 72 + 72 + 72 + 56 + 36 + 54 + 40 + 36 + 16 + 16 = 768, so 7.68.
- **Tenant:** 60 + 84 + 70 + 40 + 32 + 56 + 64 + 63 + 48 + 36 + 30 + 20 + 12 + 10 = 625, so 6.25.

| | Weighted | TCO adjustment | **Overall** | 2026-10-02 overall |
|---|---|---|---|---|
| OpenClaw | 7.93 | −0.2 (dreaming, self-learning and swarm still on by default and spend tokens in the background; nine of fifteen stable releases fixed blocked or failed updates; schema changes block downgrade) | **7.7** | 7.7 |
| Hermes | 7.68 | −0.15 (about 12K fixed prompt tokens per call by one third-party measurement, down from about 14K; about 30K tokens per background review; breaking changes in consecutive releases, partly offset by the new stable channel) | **7.5** | 7.5 |
| Tenant | 6.25 | 0 (background LLM jobs still mostly off and the tool set is now cache-stable; the baseline was not re-measured; self-update removes the manual-upgrade cost in code but is unproven in public; team runs no longer have a default spend ceiling) | **6.3** | 5.5 |

Overall is rounded half up to one decimal. Tenant's 6.25 sits exactly on the boundary, so read it as "about 6.2 to 6.3".

Buyer's verdict: the order is unchanged. A newcomer should pick **OpenClaw** if reach and channels matter most, and should turn the sandbox and `exec.ask` on before first use and stay on the extended-stable channel to avoid update trouble. They should pick **Hermes** for the easiest first run and the most real self-improvement, and should set a sandboxed backend, because the default still runs on the host. **Tenant is still not a buyable option.** It closed about a third of the distance to the leader in six weeks, and it now has docs and installers, but an outsider following its own install page gets a 404.

## Where Tenant sits and the 3 highest-leverage gaps

Tenant is a careful, low-overhead, local-model-first appliance. It now leads both incumbents on security defaults, operator tooling and engineering hygiene, and it reached rough parity on basic execution. It still trails badly on reach, ecosystem and availability. For a buyer it has moved from "not yet a product" to "a product that has not been released". Weighted gap to the leader per row (leader's score minus Tenant's, times weight) points at the same three things:

1. **Release it.** Make the repository public, publish a signed non-pre-release so "Latest" carries the install scripts, and add issue templates and a place to ask questions. Docs/community (gap 50) and install (gap 48) are the two largest deficits in this lens. This single step should lift install from 5 to about 8 and docs/community from 4 to about 6, worth about 0.5 overall, and it is the only way the engineering and security scores can gain outside evidence. Until then, the public docs should stop saying the project is open source and stop printing one-liners that fail.
2. **Broaden channels.** Telegram and Slack first, then WhatsApp or Signal. Channels ties install as the second largest deficit (gap 48) and is the only large one with no movement in six weeks.
3. **Reuse the existing ecosystem instead of building one.** Run the scripts bundled in SKILL.md skills, import from the public skill hubs with a scan and the existing trust tiers, and add Gemini, OpenRouter and Bedrock presets. Extensibility (gap 18) and model presets (gap 8) are cheap to close this way, and a newcomer expects both.

Runners-up: a PTY and coordinate clicking for the browser (gaps 16 and 16), and making confinement the default on Linux and Windows once the container backend has been run live on those hosts. The last one would protect Tenant's one clear lead.
