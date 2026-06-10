# Tenant — Quickstart

First-time setup for Tenant: a single-binary, Go-native AI agent with persistent
memory, tools, and a terminal UI. This guide takes you from nothing to a working
agent on a **local Ollama model**. Hand it to Claude or follow it yourself.

> TL;DR: install Ollama + pull two models → `go build` → `tenant setup` → `tenant tui`.

---

## 0. Prerequisites
- **Go 1.26+** (`go version`)
- **Ollama** for local inference + embeddings — https://ollama.com/download
- ~6–10 GB disk for a small model

You can also use a cloud provider (OpenAI / Anthropic / Z.ai / Grok) instead of
Ollama — see [§6](#6-cloud-providers-optional). Ollama is the simplest fully-local start.

---

## 1. Install + run Ollama

Tenant needs **two** Ollama models: one for chat/reasoning, one for embeddings.
The embedding model is **not optional** — without it, memory falls back to a hash
stand-in and the agent is effectively amnesiac (it can't recall past episodes or
stored facts).

```bash
# 1. install Ollama (see link above), then:

# 2. pull a tool-capable chat model (qwen2.5 and llama3.1 both do function-calling well)
ollama pull qwen2.5:7b          # bigger = better for agentic/tool work (qwen2.5:14b, etc.)

# 3. pull the embedding model (REQUIRED for memory)
ollama pull nomic-embed-text

# 4. make sure the server is running (listens on :11434)
ollama serve                    # leave running; or it auto-starts on macOS
```

Sanity check it's up:
```bash
curl -s http://localhost:11434/v1/models | head -c 200
```

---

## 2. Build Tenant

```bash
git clone https://github.com/Dog279/TENANT.git
cd TENANT
go build -o tenant ./cmd/tenant
./tenant --help
```

---

## 3. First-time setup

Run the interactive wizard — it writes a machine-wide config so you configure
endpoints/models **once** instead of passing flags every launch:

```bash
./tenant setup
```

It walks you through (arrow keys ↑/↓ + Enter):
- **Provider** → choose **Ollama (local)**. Endpoint defaults to `http://localhost:11434`.
- **Model** → e.g. `qwen2.5:7b` (must be a model you `ollama pull`ed).
- **Tool format** → Ollama defaults to `openai` (function-calling); keep it unless you know otherwise.
- **Embeddings** → endpoint `http://localhost:11434`, model `nomic-embed-text` (768-dim). Keep the defaults.

Non-interactive equivalent:
```bash
./tenant setup --backend vllm --vllm-endpoint http://localhost:11434 \
  --embed-endpoint http://localhost:11434 --embed-model nomic-embed-text
```

> Offline/dev with no model? `--backend echo` runs a deterministic stub (no real
> inference, no real memory) — fine for poking at the framework, not for real use.

---

## 4. Database / memory setup

There is **no manual DB step** — Tenant uses embedded SQLite and creates every
store on first run under your OS data dir:

- **macOS:** `~/Library/Application Support/tenant/`
- override with `--data <dir>` (data) and `--config <dir>` (config/secrets)

Stores created automatically: `episodes.db` (conversation history), the fact
store, `skills.db`, `usage.db`, `tenant_meta.db`. Secrets live separately in the
**config** dir (`credentials.json`, `0600`) — never in the data dir, never in git.

Verify everything is wired (this is the important step):
```bash
./tenant doctor          # diagnoses provider, embeddings, DB, dimension consistency
./tenant doctor --fix    # applies safe fixes where it can
```

Two things `doctor` commonly flags:
- **Embeddings endpoint down** → start Ollama / `ollama pull nomic-embed-text`.
- **Embedding dimension mismatch** → you switched embedders after storing data.
  Re-embed existing memory with the current model:
  ```bash
  ./tenant memory reembed
  ```

---

## 5. Launch + basic tuning

```bash
./tenant tui
```

Inside the TUI, type `/help` for the full command tree. The essentials for an
initial tune:

### Models
- `/model` — list configured backends + the active one
- **`/model pick`** — arrow-key picker: choose a provider → its **live** model list (fetched from the provider) → swap, all without leaving the TUI
- `/model add <name> <endpoint> [toolfmt]` — add a backend mid-session, e.g.
  `/model add ollama http://localhost:11434 openai`
- `/model use <name> [<model>]` — switch primary (and optionally pin a variant)

### Loop ceiling (initial tune)
The agent runs a planner↔tool loop each turn. The **ceiling** caps how many tool
calls it makes before it's forced to synthesize an answer:
```
/ceiling          # show the current ceiling
/ceiling 30       # raise it (long, multi-step tasks); lower it to keep turns short
```
Start around `20–30` for agentic work; lower if turns feel runaway, raise if the
agent stops before finishing long tasks.

---

## 6. Cloud providers (optional)

Prefer a hosted model? Add a keyed provider in one shot (the key is stored
`0600` in the config dir, never printed, never committed):

```bash
# in the TUI:
/model add-cloud anthropic sk-ant-...      # or: openai / grok / zai
/model pick                                 # then pick the model variant live
```
or set keys via the arrow-key menu: `/configure`.

---

## 7. Soul (identity) — already pre-wired

**You do not need to set this up to start.** Tenant ships with a **pre-wired soul
from the creator**: a default "Main" persona plus a set of built-in expert
sub-agents (specialists). It contains **no personal data** and works out of the
box — the agent already has a sensible identity and operating instructions.

The *soul* is the agent's identity and operating instructions — applied on every
turn, globally. It's **operator-only**: the agent cannot rewrite who it is.

When you *do* want to customize it:
```
/memory soul              # view the current soul
/memory soul import <path-to-markdown>   # replace it with your own (operator-only)
```
Write the markdown as first/second-person operating instructions (who the agent
is, how it should work, what it must/must not do). The built-in specialists are
merged underneath, so a custom soul won't drop them.

---

## Troubleshooting
- **Agent "forgets" everything** → embeddings aren't working. `tenant doctor`, then
  `ollama pull nomic-embed-text` + `ollama serve`.
- **`401` adding a cloud key** → make sure you used the provider's own key; Tenant
  sends the correct auth per provider (Anthropic `x-api-key`, others Bearer).
- **Model not responding / wrong tool calls** → check the tool format (`tenant setup`
  → Tool format) matches your model family; `openai` is right for most Ollama models.
- **Timer / long runs** → the in-TUI turn timer counts in `h:mm:ss`, so long
  autonomous runs keep tracking past an hour.

You're set. `tenant tui`, say hello, and `/help` for everything else.
