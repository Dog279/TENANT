# Exec parity: Claude Code–level shell and file tools

Status: **PR 1 merged (#39); PRs 2–4 implemented as one PR** (2026-10-02, branch `feat/exec-parity-2`) · Jira TEN-488 (TEN-489..492) · Owner: Dylan

## Goal

With permissions set to allow it, Tenant's agent can do what a Claude Code
session does on the host: run long builds, install programs, start and watch
servers, and read and edit big files, without hitting the walls listed below.
It stays gated by the existing approval brokers on every surface.

## Why commands fail today (verified in code)

| Wall | Cause | Where |
|---|---|---|
| Builds, `brew` and installers die | os_exec timeout fixed at 60s, with no param and no config | `internal/plugins/osys/osys.go:24` |
| Errors are cut off | `clip` keeps the **head** of the output; errors are usually at the tail | `osys.go:361` |
| Installers and other side effects **run twice** | dry run (the default on macOS) runs under Seatbelt, sees a refusal, then **re-runs on the host** | `internal/sandbox/sandbox.go:404-416` |
| Commands run in odd places | os_exec has no cwd param, so it uses Tenant's own cwd. Under serve that is the data dir, which is a sandbox **deny root** | `cmd/tenant/execprofiles.go:542`, `deploy/launchd/*.plist` |
| `brew`, `node` etc. "not found" under serve | launchd's PATH lacks `/opt/homebrew/bin` and `/usr/local/bin` | plist `EnvironmentVariables` |
| Dev servers die or hang | no background process support: Seatbelt reaps children, and the host backend waits on held stdout | `restricted_darwin.go:147`, `host.go:68` |
| `sudo` and interactive installers hang to timeout | no stdin/TTY; sudo waits for a password | — |
| `git log --format=…` asks as "destructive" | classifier regex `\bformat\b.*-` matches it | `osys/classify.go:36` |
| Every `ls`/`grep`/`git status` prompts | no read-only command set; all exec is category `exec` | `osys/dispatch.go:54-79` |
| Big files can't be read | `os_read_file` caps at 64 KB with no offset/limit | `osys.go:27` |

## What the industry converged on (research, 2026-10-02)

Hermes Agent, OpenClaw and Claude Code all land on the same small set:

1. **One shell tool.** It takes `command`, `workdir`, `timeout` (default 2–3 min,
   cap 10–30 min) and `background`. At the timeout the command is
   **auto-backgrounded, not killed**. Output is capped at 30–100K chars, keeping
   the head *and* tail or spilling to a file.
2. **A background registry addressed by id.** It supports list, poll/log (paged)
   and kill, plus a completion notice. Hermes and OpenClaw add stdin write; only
   they add a PTY.
3. **Files.** Ranged read (offset/limit), full write, and exact string-replace
   edit with `replace_all`. Content search is a tool in Hermes/OpenClaw, but
   Claude Code now defaults to `rg`/`find` through the shell.
4. **Two-layer safety.** A command-text policy (a read-only set auto-allowed,
   allow/ask/deny rules matched per segment of a compound command, Allow
   Once/Always) plus an OS/container boundary, with a separately gated way out.
5. **No special install or sudo tool.** Installs go through the shell under
   approval. Only Hermes plumbs a sudo password, from an env file.

Tenant already has 3 (`os_read_file`, `os_write_file`, `os_edit_file`), the
broker for 4 (once/session/always, tool rules, per-surface brokers), and the
seam for 2 (`sandbox.Start` already runs long-lived MCP servers). The gaps are
narrow.

## Plan: four PRs, smallest first

### PR 1: fix the walls (no new tools)

- **os_exec params:**
  - `workdir`: absolute; default `$HOME`, never Tenant's cwd.
  - `timeout`: seconds; default 120, model may ask up to 600.
  - Operator ceiling `exec.max_timeout` (default 1800).
- **Output:** keep head + tail (e.g. 6K + 10K) with `…[N bytes omitted]…` between.
  Raise the cap to 30K.
- **PATH:** on darwin, build PATH once at startup from `/usr/libexec/path_helper -s`
  plus `/opt/homebrew/bin` if present. This is the OS's own PATH builder, the
  same one login shells use. It fixes serve/launchd.
- **sudo fails fast instead of hanging:** start every os_exec child in its own
  session (`SysProcAttr{Setsid: true}`, stdlib). With no controlling terminal,
  sudo can't open `/dev/tty`, which is how today it can grab the TUI's terminal
  or hang to the timeout. Stdin is already `/dev/null`. The model gets sudo's
  "a terminal is required…" and the description tells it to ask the operator,
  or to rely on a sudoers `NOPASSWD` rule the operator chose. Any other
  `/dev/tty` reader fails fast the same way.
- **Classifier:** fix the `format` false positive. Add a test row each for
  `git log --format=%H -n1` and `bash -c "$(curl …)"` (the second is currently
  missed).
- **Dry-run double execution:** **Decision D1** below.
- **Tool descriptions (Hermes's trick, free):** tell the model to use
  `os_read_file`/`os_edit_file` instead of `cat`/`sed`/heredocs, and to prefer
  non-interactive flags (`-y`, `--non-interactive`, `CI=1`).

Check: table tests for the timeout clamp, head+tail clip and classifier rows.

### PR 2: background processes (one new tool)

- `os_exec` gains `background: true`, which returns `{id, pid, log}` at once.
  A foreground command that hits its timeout is **moved to the background**
  instead of killed (Claude Code/OpenClaw behaviour), so long installs survive.
- New tool **`os_process`** with `action: list | log | wait | kill | write`:
  - `log` takes `offset`/`limit` (default: last 200 lines).
  - `wait` blocks up to 30s for exit.
  - `write` sends a line to stdin.
  - Output goes to `<data>/proc/<id>.log`. That covers simple `y/n` prompts
    without a PTY.
- Built on the existing `sandbox.Start`. Processes die with Tenant
  (`lifecycle` Drain), and finished entries expire after 1h.
- Gating: starting goes through os_exec's normal approval. `os_process`
  `log`/`list`/`wait` are ungated (read), and `kill`/`write` are category exec.

Skipped: PTY / send-keys. It needs a new dependency (creack/pty), and stdin
write covers most prompts. Add it when a real installer needs a TTY.

### PR 3: command-level permissions (the Claude Code feel)

- **Read-only set, auto-allowed** (category `read`, ungated like `os_read_file`):
  `ls cat head tail wc file stat find rg grep which pwd echo date env uname`,
  `git status|log|diff|show|branch`, `brew list|info`, and similar. It applies
  only when **every** segment of a compound command (`&& || ; | $( )`) is in the
  set, and no segment has an output redirect.
  As built it was fooled (TEN-534: `env <command>`, PowerShell method calls,
  `$(…)` as the command, options hidden in quotes, `sort -o` and the like, a
  network path, and a command that wrote the dispatcher's own `[DANGER: ` note
  so that the broker judged only the text before it). The rule since
  (`ReadOnly` in `internal/plugins/osys/classify.go`, whose comment lists what
  still runs unasked): each command is a bare read-only word, as written (no
  path, no `VAR=value` before it); a word that acts with some arguments has an
  argument rule (`readOnlyWords`) and takes only literal arguments, judged as
  sh and as PowerShell 5.1 each hand them to the program; no argument is a
  network path, and what one command prints is no other's path (`$( )` only
  after `echo` and `Write-Output`, no pipe into the cmdlets that read a piped
  string as a path, no `-OutVariable`); and a line the two shells would read
  differently is not read-only. The broker reads the command out of an
  approval's detail whole: only an `os_exec_dangerous` detail carries the
  note, after the command, cut at its last marker.
- **Command rules:** tool rules learn `os_exec(<prefix> *)` patterns, for example
  `os_exec(brew install *)`, `os_exec(npm run *)` and `os_exec(rm *)` set to deny.
  They are matched per segment like Claude Code, with deny > ask > allow. "Allow
  always" on an exec prompt offers to save the command's prefix as a rule. Rules
  stay operator-only: there is still no agent tool to edit them.
- The non-overridable floor stays as it is: the destructive classifier,
  settings-file protection, and credential deny roots.

Check: segment-splitter table tests (quotes, `$( )`, pipes, redirects).

### PR 4: ranged reads + search (cheap, optional)

- `os_read_file` gains `offset`/`limit` (lines) and returns `next_offset`. The
  budget rises to about 100K chars.
- **Skipped:** `os_search` / glob tools. Claude Code itself now defaults to
  `rg`/`find` via the shell, and PR 3 makes those prompt-free. Add a tool only
  if a model keeps misusing the shell for search.
- **Skipped:** `apply_patch` (V4A). It's an OpenAI-model format, and local
  models do better with string-replace. Add it if a GPT-family backend becomes
  primary.

## Decisions (Dylan, 2026-10-02)

- **D1: host + approvals** on the interactive surfaces (TUI/dashboard, Discord, iMessage)
  when they have no `exec_profiles` entry. Cron keeps its sandboxed default, and any entry
  opts a surface back into the sandbox.
- **D2: never store a sudo password.** sudo fails fast (Setsid), and passwordless sudo rules
  are the operator's choice.
- **D3: 120s default timeout**, with 600s as the most the model may ask for. The operator
  ceiling (1800s) waits for PR 2's move-to-background.
- **D4: read-only commands run without a prompt** (PR 3). iMessage stays deny-by-default.
- **Also: re-classify destructive (done in PR 1).**
  - `rm -r` is destructive only on `/`, top-level folders, home folders or their direct
    children, `.`, `..` and `*`.
  - Command words count only at command start.
  - `diskutil erase…` and `bash -c "$(curl …)"` are added.
  - `wiki_save` and `crm_*` are re-categorized as writes.
  - Still destructive, pending a decision: `jira_write` and `github_write`. Moving them to
    `send` would let `--discord-allow-send` approve them too.

## Out of scope

- The Docker container backend (its own plan, `docker-sandbox-plan.md`).
- Network allowlists.
- A persistent shell session: per-call `workdir` covers cwd, and env persistence
  is a footgun that even Claude Code doesn't do.

## Sources

- Hermes Agent: hermes-agent.nousresearch.com/docs/user-guide/features/tools;
  github.com/NousResearch/hermes-agent (`tools/terminal_tool.py`, `approval.py`,
  `file_tools.py`)
- OpenClaw: docs.openclaw.ai/tools/exec, /tools/exec-approvals,
  /gateway/background-process, /tools/elevated, /gateway/sandboxing
- Claude Code: code.claude.com/docs/en/tools-reference, /permissions,
  /sandboxing, /permission-modes
