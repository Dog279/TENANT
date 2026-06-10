# Tenant

A **Go-native, single-binary agent framework** built on the Model Context
Protocol (MCP). A planner↔tool loop with parallel fan-out, a layered memory
system that learns from interaction traces, a modular plugin model, and both a
terminal UI and a web dashboard — all in one statically-linked, CGO-free binary.

Bring your own LLM: it runs against a **local** model (Ollama, llama.cpp, vLLM)
or a **cloud** API (OpenAI, Anthropic, Z.ai, Grok) through one OpenAI-compatible
path. No Python runtime, no service mesh — just the binary and a model endpoint.

> **Status — early but functional.** Single-operator. Works today: local TUI
> chat, the web dashboard, the plugin toolset, the layered memory + compaction
> pipeline, multi-agent orchestration, and an offsite Discord relay. APIs and
> formats may still change. This is a **sanitized public snapshot** — it ships
> **zero** keys, memory, or personal data; you supply your own (stored locally,
> never committed).

## Features

- **One static binary.** CGO-free; the only external dependency is a model
  endpoint you point it at.
- **Pluggable model backends.** Local (Ollama / llama.cpp / vLLM) or cloud
  (OpenAI / Anthropic / Z.ai / Grok) — switch with one command, mix per role.
- **Layered memory.** Working set → episodic memory → distilled semantic facts →
  an append-only archive → an editable "soul" (identity/persona). Context
  compaction is **archive-sourced** (summaries are reversible and auditable, not
  lossy folds) with a persistent goal header to resist drift.
- **Tools / plugins** (read-by-default; mutating actions are gated):
  OS (shell + files), web (browse / search / read), SQL (SQLite + Postgres),
  Google Workspace (Gmail + Calendar), X / Twitter, iMessage (via BlueBubbles),
  Discord, and a plain-markdown wiki with a transparent indexer.
- **Built-in expert sub-agents.** Five specialists — **Programmer, Researcher,
  Writer, QA, Strategist** — ship in the binary with a workflow-tuned persona
  (plan → debate → implement → test → show evidence). They inherit your primary
  model, so the orchestrator can spawn them with **zero extra config**: point
  tenant at a model and you have a team.
- **Multi-agent orchestration + deep research.** An orchestrator spawns
  concurrent sub-agents that coordinate over a live bus; a research mode plans →
  fans out web researchers → returns a cited report.
- **Safety.** Dangerous actions (exec / write / destructive / outbound send)
  require explicit approval, with per-category permissions you control.
- **Surfaces.** Full-screen TUI (streaming chat + live activity), a
  server-rendered web dashboard (no JS build step), an MCP memory server over
  stdio, and an **offsite Discord relay** — text your agent from your phone and
  approve each privileged action with a tap.

## Quickstart

New here? **[docs/QUICKSTART.md](docs/QUICKSTART.md)** is a step-by-step first-time
setup (Ollama models, the memory DB, model + loop-ceiling tuning, and the soul).
The short version:

Requires **Go 1.26+** and an LLM you can reach (a local Ollama, or a cloud API key).

```sh
git clone https://github.com/<you>/tenant.git
cd tenant
go build -o tenant ./cmd/tenant          # produces ./tenant (tenant.exe on Windows)

./tenant setup                            # interactive wizard: pick a provider, paste a key
./tenant doctor                           # verify config, endpoints, and keys
./tenant tui                              # full-screen chat + activity feed
```

Kick the tires with **no model and no keys** (deterministic offline backend):

```sh
./tenant --backend echo tui
```

A couple of one-shot examples:

```sh
./tenant web "summarize the top story on Hacker News"
./tenant os "what's using the most disk in my home dir?"      # read-only; --allow-exec to act
printf 'remember I prefer Go\nwhat do I prefer?\n' | ./tenant chat
```

## Configuration & your data

- `tenant setup` writes **`config.json`** to your OS config dir and secrets to
  **`credentials.json`** (perms `0600`) — both in your **home directory**, never
  in the repo.
- Add or switch providers any time: `tenant model add ...` / `tenant model use ...`
  (or `/model` inside the TUI).
- Memory, the archive, transcripts, and the wiki live in your OS **data dir**
  (gitignored). Nothing personal is ever committed.
- **This repository contains no API keys, no learned memory, and no wiki
  content** — it is a clean template. Everything you generate stays on your machine.

## Built-in expert sub-agents

Five specialists ship in the binary and are spawnable out of the box — the
orchestrator delegates to them by name, and each runs on **your** configured
model (no per-agent setup):

| Role | What it does |
|---|---|
| **Programmer** | implements features and fixes: smallest diff, root-cause only, regression test, clean build before done |
| **Researcher** | deep multi-source research: pulls the primary source, adversarially verifies every claim, cites everything |
| **Writer** | docs, PR/commit prose, summaries: direct voice, accuracy over polish, never overclaims |
| **QA** | adversarial verifier: tries to break the work, verifies against reality with file:line evidence |
| **Strategist** | founder-mode scoping + neutral judge: challenges the premise, finds the narrowest high-value wedge |

```sh
./tenant orchestrate "add rate limiting to the API and prove it works"
```

The orchestrator picks the right specialist per sub-task. To inspect, override,
or add your own, use `/agents` in the TUI (or `tenant agents`); a profile you add
with the same name overrides the built-in (e.g. to pin a specialist to a bigger
model). The matching identity/soul/rules files live under
`cmd/tenant/builtinsouls/agents/` if you want to edit the personas, and a 6th —
**Main**, the conductor persona — ships there too for use as your primary agent's
soul (`/memory soul import cmd/tenant/builtinsouls/agents/Main`).

## Commands

| Command | What it does |
|---|---|
| `setup` | one-time config wizard → `config.json` |
| `model <list\|use\|add>` | manage model backends; set the primary |
| `tui` | full-screen terminal UI (streaming chat + activity feed) |
| `chat` | headless agent loop (one stdin line = one turn) |
| `serve` | background self-improvement (distillation) on a cadence |
| `doctor [--fix]` | diagnose (and repair) the setup |
| `web` / `sql` / `wiki` / `gsuite` / `x` / `imessage` / `os` | one-shot agent turn scoped to one plugin (read-by-default; `--allow-*` to act) |
| `orchestrate "<task>"` | multi-agent team over a live bus |
| `research "<question>"` | deep research → cited report (`--out FILE`) |
| `memory search "<q>"` | search episodes + facts from the CLI |
| `distill` | run one episodic→semantic distillation pass |
| `mcp-memory` | run the MCP memory server over stdio |
| `eval` | run the eval harness |

Run `tenant` with no arguments for the full list and flags.

## Repository layout

```
cmd/tenant/            # the binary: CLI commands, TUI, wiring
internal/agent/        # agent runtime — planner loop, tool dispatch, compaction trigger
internal/memory/       # working / episodic / semantic / archive / soul / compress (compaction)
internal/model/        # model router + backends (vllm-compatible, anthropic, echo) + tool formats
internal/plugins/      # first-party tools: os, web, sql, gsuite, x, imessage, discord, wiki
internal/dashboard/    # server-rendered web control panel
internal/tui/          # terminal UI
internal/orchestra/    # multi-agent bus + spawn/await
internal/eval/         # evaluation harness + task fixtures
docs/                  # architecture, design, plugin, and memory docs
```

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the as-built reference and
[`docs/DESIGN.md`](docs/DESIGN.md) for design intent.

## Building & testing

```sh
go build ./...
go test ./...
go vet ./...
```

## License

Not yet licensed. **Add a `LICENSE` file** (e.g. Apache-2.0 or MIT) before relying
on this in your own project — until then, all rights are reserved by default.
