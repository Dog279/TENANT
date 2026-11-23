# Tenant

[![CI](https://github.com/Dog279/TENANT/actions/workflows/ci.yml/badge.svg)](https://github.com/Dog279/TENANT/actions/workflows/ci.yml)
[![Security](https://github.com/Dog279/TENANT/actions/workflows/security.yml/badge.svg)](https://github.com/Dog279/TENANT/actions/workflows/security.yml)

**Your own AI agent, in one binary.** Tenant is a Go-native, single-binary agent
you run yourself, built on the Model Context Protocol (MCP). A planner↔tool loop
with parallel fan-out, a layered memory that learns from your conversations,
first-party tools built in, approvals before anything dangerous, and every
surface you need to drive it: a terminal UI, a web dashboard, Discord and
iMessage. One statically linked, CGO-free binary; no Python, no service mesh,
no account.

Bring your own LLM: a **local** model (Ollama, llama.cpp, vLLM, anything
OpenAI-compatible), a **cloud** API (OpenAI, Anthropic, Z.ai, xAI, Sakana), or
the Claude Code CLI on your subscription. Switch with one command, mix per
helper, fall back automatically.

Tenant is an **appliance, not a library**: you extend it with MCP servers (your
tools, run out of process) and Agent Skills (your know-how), never by importing
its Go packages. See [ADR 0001](docs/adr/0001-appliance-not-framework.md).

> **Status: early but functional.** Single-operator. Works today: the terminal
> UI, the web dashboard, the plugin toolset, the layered memory and compaction
> pipeline, multi-agent orchestration, the Discord relay and the iMessage
> responder. APIs and formats may still change. This is a **sanitized public
> snapshot**: it ships **zero** keys, memory or personal data; everything you
> generate stays on your machine.

## Documentation

The full reference lives at **[findtime.net/tenant/docs](https://findtime.net/tenant/docs/)**,
organized by area with every command, setting and slash command:

| Area | Covers |
|---|---|
| [Setup & install](https://findtime.net/tenant/docs/setup/install) | one-line install, where files live, the background service, updates, backups, retention |
| [First launch](https://findtime.net/tenant/docs/first-launch/setup-wizard) | the setup wizard, doctor, the terminal UI, permissions and approvals, the execution sandbox, memory and soul, skills |
| [Models & helpers](https://findtime.net/tenant/docs/models/providers) | providers, switching live, local models, fallbacks, prompt routing, the loop ceiling, helpers, teams and research |
| [MCPs & tools](https://findtime.net/tenant/docs/mcp/overview) | remote and local MCP servers, trust and tool rules, built-in tools, integrations, Tenant as an MCP server |
| [Dashboard](https://findtime.net/tenant/docs/dashboard/overview) | sign-in, every page, approvals, Discord and iMessage access, scheduled jobs, logs and alerts, the REST API |
| [Reference](https://findtime.net/tenant/docs/reference/config) | `config.json` and `settings.<agent>.json` key by key, the CLI, the slash commands, environment variables |

In this repository: [docs/QUICKSTART.md](docs/QUICKSTART.md) (a first-time
walkthrough on a local Ollama model), [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
(the as-built reference), [docs/DESIGN.md](docs/DESIGN.md) (design intent),
[docs/LOGGING.md](docs/LOGGING.md), [docs/PLUGINS.md](docs/PLUGINS.md),
[docs/serve-runbook.md](docs/serve-runbook.md),
[docs/dashboard-runbook.md](docs/dashboard-runbook.md) and
[SECURITY.md](SECURITY.md).

## Install

macOS or Linux:

```sh
curl -fsSL https://github.com/Dog279/TENANT/releases/latest/download/install.sh | sh
tenant setup
```

Windows (PowerShell):

```powershell
irm https://github.com/Dog279/TENANT/releases/latest/download/install.ps1 | iex
tenant setup
```

The script verifies the release's SHA-256 and installs for the current user, no
sudo or Go toolchain needed; the archives are also on the
[Releases page](https://github.com/Dog279/TENANT/releases/latest) with checksums,
a cosign signature and SBOMs, and, once the project key is set up, a
project-key signature (`checksums.txt.sig`) that `tenant update` checks with
nothing installed. `TENANT_VERSION` pins a version and
`TENANT_INSTALL_DIR` picks the directory. Or build from source with Go 1.26:
`go build ./cmd/tenant`.

## Quickstart

```sh
tenant setup            # interactive wizard: pick a provider, paste a key or point at Ollama
tenant doctor           # verify config, endpoints, keys, stores (--fix repairs what it safely can)
tenant tui              # full-screen chat + activity feed; /help lists the slash commands
tenant service install  # keep it running in the background (launchd, systemd, or a Windows task/service)
```

Memory needs an embedding model; the default is a local Ollama serving
`nomic-embed-text` (`ollama pull nomic-embed-text`). Without one the agent runs
but cannot recall. Kick the tires with no model and no keys:

```sh
tenant tui --backend echo
```

One-shot turns scoped to a single plugin (read-only unless you pass its
`--allow-*` flag):

```sh
tenant web "summarize the top story on Hacker News"
tenant os "what's using the most disk in my home dir?"
printf 'remember I prefer Go\nwhat do I prefer?\n' | tenant chat
```

## Features

- **One static binary.** CGO-free; the only external dependency is a model
  endpoint you point it at.
- **Pluggable model backends.** Local (Ollama, llama.cpp, vLLM, any
  OpenAI-compatible server) or cloud (OpenAI, Anthropic, Z.ai, xAI Grok,
  Sakana Fugu, the Claude Code CLI). Switch live with `/model use`, pick from a
  provider's live model list with `/model pick`, chain `fallbacks` for rate
  limits and outages, and route each prompt between a small local model and a
  big one with a local classifier (`/route`).
- **Images.** Paste a screenshot into the TUI (Ctrl+V), drag a file in, or
  send a photo over Discord or iMessage, and a model that can see gets the
  picture. Tenant tests which local models can see at launch.
- **Layered memory.** Working set → episodic memory → distilled semantic facts →
  a skill library → an append-only archive, plus an editable, operator-only
  soul (identity and rules). Context compaction is **archive-sourced**
  (summaries are reversible and auditable, not lossy folds) with a persistent
  goal header to resist drift. `/memory`, `tenant memory`, a fact inspector on
  the dashboard, `tenant privacy forget` for real deletion.
- **Tools, read by default.** OS (shell, files, processes), web (browse,
  search, read, screenshot, click), SQL (SQLite), a plain-Markdown wiki, Google
  Workspace (Gmail, Calendar, Drive), X, iMessage (Messages on a Mac, or
  BlueBubbles), Discord, GitHub (branches, PRs, issues, CI, a gated merge),
  Atlassian (Jira), SimpleFIN (bank balances), 1Password (vaults, items and
  one secret at a time, through the `op` CLI), cron. Anything that changes the
  world is gated.
- **MCP, two ways.** Connect a **remote** MCP server with a browser sign-in
  (`/mcp add <url>`), or declare a **local** one in `config.json` that Tenant
  starts and confines out of process. Each connector has live trust
  (`ask` / `allow` / `deny`), and the agent can adopt a tool server it built
  itself (`mcp_add`), gated by its own permission category.
- **Agent Skills.** Reusable recipes in the open `SKILL.md` format: import,
  export, version, diff, revert, and trust explicitly. Nothing untrusted reaches
  the agent, including what the agent saves for itself.
- **Safety.** Eight categories (`exec`, `write`, `destructive`, `web`, `send`,
  `egress`, `mcp`, `sandbox`), each `ask` / `allow` / `deny`, per surface. Per-tool
  rules (`/permissions add delete_* deny`) and command rules
  (`/permissions add "os_exec(brew install *)" allow`) for tools nobody
  pre-programmed. An execution sandbox (Seatbelt on macOS; dry run, enforcement,
  per-surface profiles). A Security log that cannot be cleared.
  `tenant security audit` reports the whole posture.
- **Helpers and teams.** Five specialists ship in the binary (Programmer,
  Researcher, Writer, QA, Strategist) and inherit your model, so
  `tenant orchestrate "<task>"` works with zero extra config. Add your own with
  a pinned model and identity. Deep research fans out web researchers and
  returns a cited report; `/goal` keeps working until a judge says a condition
  holds.
- **Surfaces.** Full-screen TUI (streaming chat + live activity feed), a
  server-rendered web dashboard (no JS build step; publish it to your tailnet
  with `/tailscale serve`), an **offsite Discord relay** (text your agent from
  your phone and approve each privileged action with a tap; DM it files), an
  **iMessage responder** (an allowlist, passive or respond modes, approvals by
  text from the operator handle), a JSON API, and `tenant mcp-memory` to serve
  Tenant's memory to other MCP clients.
- **Runs 24/7, loses nothing.** `tenant service` installs it under launchd,
  systemd, or a Windows task or service. An OS restart cuts the running turn
  safely, tells your channel, and catches up the messages you sent while it was
  down. Scheduled jobs (`/cron`) on crontab or `@every` schedules. Every
  subsystem logs to one log book with a filter language, alerts that message
  you, and a hash-chained Security log. `tenant update` with a verified
  download, a pre-update backup and a rollback; `tenant backup` as one
  verifiable file; `tenant privacy` for retention per data class, forgetting,
  and a full export.

## Built-in helpers

| Helper | What it does |
|---|---|
| **Programmer** | implements features and fixes: smallest diff, root cause only, regression test, clean build before done |
| **Researcher** | deep multi-source research: pulls the primary source, adversarially verifies every claim, cites everything |
| **Writer** | docs, PR and commit prose, summaries: direct voice, accuracy over polish, never overclaims |
| **QA** | adversarial verifier: tries to break the work, verifies against reality with file:line evidence |
| **Strategist** | founder-mode scoping and neutral judge: challenges the premise, finds the narrowest high-value wedge |

```sh
tenant orchestrate "add rate limiting to the API and prove it works"
```

The orchestrator picks the right specialist per sub-task. `/agents` in the TUI
(or `tenant agents`) inspects, overrides or adds profiles; a profile you add with
the same name overrides the built-in (to pin a specialist to a bigger model).
The identity files live under `cmd/tenant/builtinsouls/agents/`, with a sixth,
**Main**, the conductor persona for your primary agent's soul
(`/memory soul import cmd/tenant/builtinsouls/agents/Main`).

## Configuration and your data

- `tenant setup` writes **`config.json`** to your OS config dir and secrets to
  **`credentials.json`**, both in your home directory, never in the repo. Secret
  files are owner-only (`0600` / `0700` on Unix, a protected DACL on Windows).
  Per-agent runtime settings (enabled tools, permission modes, tool rules, exec
  profiles) live in **`settings.<agent>.json`**, which the running hub re-reads
  live.
- Memory, the archive, the log book, research and the wiki live in your OS
  **data dir** (gitignored). Where exactly, per OS, is in the
  [docs](https://findtime.net/tenant/docs/setup/directories).
- Most settings change three ways: a slash command (live, persisted), the
  dashboard (live, persisted), or the JSON file by hand.
- **This repository contains no API keys, no learned memory, and no wiki
  content.** It is a clean template; everything you generate stays on your
  machine.

## Commands

| Command | What it does |
|---|---|
| `setup` | the one-time wizard → `config.json` (`--non-interactive` for scripts) |
| `tui` / `chat` | the terminal UI; the same agent on stdin |
| `serve` / `service <install\|status\|start\|stop\|restart\|uninstall>` | the headless 24/7 hub, and running it under the OS |
| `doctor [--fix]` | diagnose (and repair) the setup |
| `update [--check\|--rollback]` | upgrade to the latest release with a verified download, a backup and a way back |
| `model <list\|use\|models\|add\|remove>` | manage model backends (also `/model` in the TUI) |
| `route <status\|on\|off\|shadow\|assisted\|everywhere\|stats>` | the prompt router (also `/route`) |
| `agents <list\|add\|model\|rename\|soul\|show\|remove>` | helper profiles (also `/agents`) |
| `orchestrate "<task>"` / `research "<question>"` / `goal "<condition>"` / `review <plan.md>` | a team, deep research, the autonomous loop, cascading plan review |
| `memory <search\|import\|reembed>` / `distill` / `consolidate` | memory from the shell |
| `skills <list\|show\|trust\|import\|export\|seed\|auto>` | the skill library and Agent Skills packages |
| `mcp connect <url>` / `mcp-memory` | connect a remote MCP server; serve Tenant's memory over MCP |
| `peer <invite\|join\|list\|share\|…>` | federate two Tenants with mutual consent |
| `fleet <init\|enroll\|confirm\|list\|permissions\|…>` / `child <install\|status\|pause\|resume\|…>` | own your other machines from this one: a child serves its tools to the master's agent, and approvals stay on the master |
| `permissions <effective\|preset>` / `security audit` / `sandbox <probe\|selftest\|run\|redteam>` | the safety posture |
| `logs <query\|show\|trace\|stats\|export\|verify\|config\|alerts\|…>` / `log <source> "<msg>"` / `attach` | the log book |
| `backup <create\|verify\|restore>` / `privacy <status\|retention\|prune\|forget\|export>` / `runs export` | your data |
| `web` / `os` / `sql` / `wiki` / `gsuite` / `x` / `imessage` / `simplefin` / `onepassword` | one-shot turns scoped to one plugin (read by default; `--allow-*` to act) |
| `eval` | the eval harness |

`tenant help` lists the jobs to start with, `tenant help all` every command,
`tenant help <command>` one command, and `tenant <command> --help` its flags.
Tab completion: `tenant completion bash|zsh|fish|powershell`. Inside the TUI,
`/help` does the same for the slash commands. The complete lists are in the
[CLI](https://findtime.net/tenant/docs/reference/cli) and
[slash command](https://findtime.net/tenant/docs/reference/slash-commands)
references.

## Repository layout

```
cmd/tenant/            # the binary: CLI commands, TUI wiring, plugins' command surfaces
internal/agent/        # agent runtime: planner loop, tool dispatch, compaction trigger
internal/memory/       # working / episodic / semantic / skills / archive / soul / compress
internal/model/        # model router + backends (OpenAI-compatible, anthropic, claudecode, echo) + tool formats
internal/plugins/      # first-party tools: osys, web, sql, wiki, gsuite, x, imessage, discord, github, atlassian, simplefin, onepassword, crm, cron, mcpremote
internal/mcp/          # the MCP client; internal/mcpserver is the memory server
internal/approval/     # the control plane's decision point for dangerous actions
internal/sandbox/      # the execution sandbox (host, restricted, jobobject backends)
internal/dashboard/    # server-rendered web control panel + JSON API
internal/logs/         # the unified log book: store, text folders, alerts, the API
internal/tui/          # terminal UI
internal/orchestra/    # multi-agent bus + spawn/await + budgets
internal/eval/         # evaluation harness + task fixtures
reference/             # copy-paste code + tutorials: log into the log book; a tool server the agent adopts live
docs/                  # architecture, design, plugin, logging and memory docs; runbooks; change notes
```

## Building and testing

```sh
go build ./...
go test ./...
go vet ./...
```

## License

[MIT](LICENSE) © 2026 Dylan.

Tenant carries a trimmed copy of [goldmark](https://github.com/yuin/goldmark) (MIT, Yusuke
Inuzuka) under `internal/md/goldmark`, which renders the agent's Markdown in the terminal app and
the dashboard; its README says what was kept.

## Security

Found a vulnerability? See [SECURITY.md](SECURITY.md) for reporting instructions.
