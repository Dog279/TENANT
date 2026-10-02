# Tenant — Quickstart

First-time setup for Tenant: a single-binary, Go-native AI agent with persistent
memory, tools, and a terminal UI. This guide takes you from nothing to a working
agent on a **local Ollama model**. Hand it to Claude or follow it yourself.

> TL;DR: install Ollama + pull two models → install Tenant → `tenant setup` → `tenant tui`.

---

## 0. Prerequisites
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

## 2. Install Tenant

macOS or Linux:

```sh
curl -fsSL https://github.com/Dog279/TENANT/releases/latest/download/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://github.com/Dog279/TENANT/releases/latest/download/install.ps1 | iex
```

The script downloads the release for your OS and CPU, checks its SHA-256 against
the release's `checksums.txt`, and installs `tenant` to `~/.local/bin`
(`%LOCALAPPDATA%\Programs\tenant` on Windows). No sudo or administrator rights,
no Go toolchain, and no Gatekeeper or SmartScreen prompt: a `curl` or PowerShell
download carries no quarantine mark (a browser download does). If the directory
is not on your `PATH`, the script says what to add. `TENANT_VERSION` pins a
version and `TENANT_INSTALL_DIR` picks the directory.

Or download the archive from the
[latest release](https://github.com/Dog279/TENANT/releases/latest) and put
`tenant` (`tenant.exe` on Windows) on your `PATH`. Confirm with `tenant version`.

---

## 3. First-time setup

Run the interactive wizard — it writes a machine-wide config so you configure
endpoints/models **once** instead of passing flags every launch:

```bash
tenant setup
```

It walks you through (arrow keys ↑/↓ + Enter):
- **Provider** → choose **Ollama (local)**. Endpoint defaults to `http://localhost:11434`.
- **Model** → e.g. `qwen2.5:7b` (must be a model you `ollama pull`ed).
- **Tool format** → Ollama defaults to `openai` (function-calling); keep it unless you know otherwise.
- **Embeddings** → endpoint `http://localhost:11434`, model `nomic-embed-text` (768-dim). Keep the defaults.

Non-interactive equivalent:
```bash
tenant setup --non-interactive --provider ollama --vllm-model qwen2.5:7b \
  --embed-endpoint http://localhost:11434 --embed-model nomic-embed-text
```

At the end the wizard offers to run Tenant in the background; [§8](#8-run-it-in-the-background)
says what that installs.

> Offline/dev with no model? `--backend echo` runs a deterministic stub (no real
> inference, no real memory) — fine for poking at Tenant, not for real use.

---

## 4. Database / memory setup

There is **no manual DB step** — Tenant uses embedded SQLite and creates every
store on first run under your OS data dir:

- **macOS:** `~/Library/Application Support/tenant/`
- override with `--data <dir>` (data) and `--config <dir>` (config/secrets)

Stores created automatically: `episodes.db` (conversation history), the fact
store, `skills.db`, `usage.db`, `tenant_meta.db`. Secrets live separately in the
**config** dir (`credentials.json`) — never in the data dir, never in git. Tenant
verifies `0600` files/`0700` parent directories on Unix and a protected
current-user-only DACL on Windows.

Verify everything is wired (this is the important step):
```bash
tenant doctor          # diagnoses provider, embeddings, DB, dimension consistency
tenant doctor --fix    # applies safe fixes where it can
```

Two things `doctor` commonly flags:
- **Embeddings endpoint down** → start Ollama / `ollama pull nomic-embed-text`.
- **Embedding dimension mismatch** → you switched embedders after storing data.
  Re-embed existing memory with the current model:
  ```bash
  tenant memory reembed
  ```

---

## 5. Launch + basic tuning

```bash
tenant tui
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

Prefer a hosted model? Add a keyed provider in one shot (the key is stored with
platform-specific owner-only protection, never printed, never committed):

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

## 8. Run it in the background

`tenant setup` offers this at the end; otherwise:

```sh
tenant service install     # --print shows the unit without installing it
tenant service status
```

Tenant then starts when you log in and restarts after a crash:

- **macOS:** a LaunchAgent; logs in `~/Library/Logs/tenant/`. The install refuses
  paths under `~/Desktop`, `~/Documents` and `~/Downloads` (TCC would block them).
- **Linux:** a systemd user unit; `journalctl --user -u tenant`. Run
  `loginctl enable-linger $USER` so it survives logout.
- **Windows:** a scheduled task at logon, running as you. On a headless box,
  `tenant service install --system` from an elevated terminal installs a real
  Windows service as SYSTEM instead (it shows in the Services app).

`start`, `stop`, `restart` and `uninstall` do what they say; `restart` waits for
the running turn to finish unless you pass `--now`. `tenant doctor` reports the
service. An OS restart or shutdown mid-task is safe: the turn is cut and
recorded, your channel gets "I was stopped mid-task… say `continue`", and after
boot Tenant reminds you and catches up the Discord DMs sent while it was down.
Nothing is re-run on its own.

---

## 9. Update

```sh
tenant update --check      # current vs latest, nothing changed
tenant update              # download, verify, back up, swap, restart the service
```

`update` verifies the checksum (and the cosign signature when `cosign` is
installed), smoke-runs the new binary, writes an unencrypted backup to
`<data>/backups/pre-update-<version>.tenant-backup`, swaps the binary keeping the
old one as `tenant.prev`, restarts the service and confirms the new version
answers; if it does not, the old binary goes back by itself. `--version vX.Y.Z`
picks a release, `--yes` skips the prompt, `--now` restarts without waiting for
the running turn. Tenant checks for a newer release once a day and writes one
log line; `"update_check": false` in `config.json` turns that off. Nothing is
applied automatically, and a dev build refuses to update.

Rollback: `tenant update --rollback` puts the old binary back. If the new
version migrated data, restore the backup too:
`tenant service stop && tenant backup restore <file> --yes && tenant service start`.

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
