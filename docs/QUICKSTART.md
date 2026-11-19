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
download carries no quarantine mark (a browser download does). On macOS the
script signs the binary on your machine with the hardened runtime
(`/usr/bin/codesign`, ad hoc, no entitlements) before it moves it into place,
which makes macOS refuse a debugger that tries to attach to Tenant; if that
fails, nothing is installed or changed. If the directory
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

A separate **repeat guard** ends a turn early when the agent makes the same tool
calls and gets the same results back several rounds in a row (default 8). A poll
whose output changes never trips it. The feed says "stuck repeating", not
"loop ceiling hit", when it fires:
```
/ceiling repeat        # show it
/ceiling repeat 12     # allow longer identical wait loops
/ceiling repeat off    # only the loop ceiling ends a turn (0 works too)
/ceiling repeat default
```
Saved as `repeat_guard` in config.json (0 or missing = default, negative = off).

### The judge
A judge can check the agent's work before an answer reaches you: a decision
model (Cloudflare's Clef-flash, served with the System One API) decides whether
each prompt is a task and, for tasks, whether the answer really did it. If not,
the work goes back to the model, up to three times; an answer it still can't
confirm is marked `— judge: not confirmed`. If you deny an action, it never asks
again. Off by default:
```
/judge use clef          # prompts for your server's address, then its key (blank for none); asks it once, switches on
/judge                   # kind, model, server, key, on or off, rounds
/judge on | off
/judge rounds 2          # send-backs per answer, 1 to 5
```
Saved as `judge` in config.json, the key in credentials.json; the dashboard's
Model page has the same card. The model that grades the Quality evals is a
different setting: `/eval model`.

### Pasting images
Show the model a picture without pointing it at a file:
- **Ctrl+V** pastes the clipboard's image (a screenshot, a copied picture). On
  Windows it's **Alt+V**, since Windows Terminal keeps Ctrl+V for text. With no
  image on the clipboard, Ctrl+V pastes text as before. On a Mac use Ctrl+V too:
  terminals keep Cmd+V for their own text paste and send nothing for an image.
- **Drag** image files into the terminal (macOS and Linux terminals).
- **`/image <path>`** attaches a file by path (`/img` for short).

Each image shows in the input as `[image 1: name W×H]`, and the footer counts what's
attached. Backspace right after a tag removes it, and the image with it. Images are saved under `<data>/attachments` (PNG, JPEG, GIF, webp; HEIC on
macOS), scaled to at most 1568 px, and follow the archive's retention. Linux
needs `wl-paste` (Wayland) or `xclip` (X11) for Ctrl+V. Photos sent over Discord
and iMessage reach the model the same way, and `os_read_image` and
`web_screenshot` show it the picture too.

The model sees the last three exchanges' images, at most 8 per request. Tenant
checks whether a local model can see images when it starts, with a small test
picture: `/model vision` shows the answer. If a model gets it wrong, set it:
```
/model vision on      # this model sees images
/model vision off     # it doesn't: send a note instead of the picture
/model vision auto    # test it (the default)
```
Saved as `vision` on the provider in config.json. Hosted Anthropic models always
see images; a model that can't gets a short note naming the saved file instead.

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
service. If this system cannot apply one of Tenant's process protections,
`install`, `start` and `restart` say which and ask `Continue without it? [y/N]`
before they go on: the service has nobody to ask, and does not start until
someone has said yes at a terminal (`docs/serve-runbook.md`, "When a protection
cannot be applied"). On macOS `install` first signs the binary with the hardened
runtime. An OS restart or shutdown mid-task is safe: the turn is cut and
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
installed; a main-repository release binary also requires the release's
project-key signature, `checksums.txt.sig`, which needs nothing installed), smoke-runs the new binary, writes an unencrypted backup to
`<data dir>-backups/pre-update-<version>.tenant-backup`, swaps the binary keeping the
old one as `tenant.prev`, restarts the service and confirms the new version
answers; if it does not, the old binary goes back by itself. `--version vX.Y.Z`
picks a release, `--yes` skips the prompt, `--now` restarts without waiting for
the running turn. Tenant checks for a newer release once a day and writes one
log line; `"update_check": false` in `config.json` turns that off. Nothing is
applied automatically, and a dev build refuses to update.

A new version asks again about a process protection it cannot apply on this
system, so `update` asks you before it changes anything (`--yes` does not answer
that question). Run with no terminal, an update that meets one changes nothing,
exits 2 and says why; run it once at a terminal. On macOS `update` signs the new
binary with the hardened runtime before it runs it or moves it into place.

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
