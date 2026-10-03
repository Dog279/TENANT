# Changelog

Notable changes to Tenant. Releases are tagged `vX.Y.Z`; their binaries are on the
Releases page.

## Unreleased

### Added
- **Background processes** (TEN-490): `os_exec background: true` starts a command and returns
  its id at once, and a command still running at its timeout is moved to the background instead
  of being killed. `os_process` lists, reads (`log`, paged like a file), waits for, feeds (`write`
  one line to stdin) and kills them; logs live under `<data>/proc/`, processes die with Tenant,
  and a finished one is forgotten after an hour. Host surfaces only: a sandboxed surface keeps
  the kill-at-timeout runner.
- **Read-only commands run without a prompt** (TEN-491): `ls`, `cat`, `rg`, `git status`, `brew
  list`, `find` without `-delete`/`-exec` and the like skip the exec prompt on the host and
  Discord when every segment of the command only looks and nothing redirects; iMessage stays
  deny-by-default. A category deny and the operator's rules still bind.
- **Command rules** (TEN-491): `/permissions add os_exec(brew install *) allow` (or `deny`,
  `ask`) decides a command by its text, per segment of a compound command: any deny denies,
  else any ask asks, else allow when every segment matched. `os_exec(git push)` is that command
  alone. "Always" on an exec prompt now saves the command's family as such a rule instead of
  opening all of exec.
- **Paged file reads** (TEN-492): `os_read_file` takes `offset` and `limit` (lines) and a cut
  read ends with `next_offset`; a page is ~100K characters (was a 64 KB cap with nothing past it).
- **`tenant service`** (TEN-498): `install`, `uninstall`, `status`, `start`, `stop` and
  `restart` run Tenant in the background through the OS: a LaunchAgent on macOS (logs in
  `~/Library/Logs/tenant/`), a systemd user unit on Linux, a scheduled task at logon on
  Windows, or with `--system` a Windows service running as SYSTEM. `install --print` shows
  the unit without installing it; `restart` waits for the running turn unless `--now`.
  `tenant setup` offers the install at the end and `tenant doctor` reports the service.
- **An OS restart or shutdown mid-task loses nothing** (TEN-499). The first seconds of a
  stop cut the turn, record it in the archive and tell the operator's channel; after boot
  Tenant posts a reminder for the cut turn and catches up the Discord DMs sent while it was
  down. Nothing is re-run on its own. `os_write_file` and `os_edit_file` write through a
  temp file and a rename, so a kill mid-write leaves the original intact.
- **`tenant update`** (TEN-500) checks the release, verifies the checksum (and the cosign
  signature when cosign is installed), smoke-runs the new binary, backs the data up to
  `<data dir>-backups/pre-update-<version>.tenant-backup`, swaps the binary keeping
  `tenant.prev`, restarts the service and confirms the new version answers; on failure it
  puts the old binary back. `--check`, `--version vX.Y.Z`, `--yes`, `--now`, `--force` and
  `--rollback`. Serve checks for a newer release once a day and writes one log-book line;
  `"update_check": false` turns that off. Nothing is applied automatically, and a dev build
  refuses to update.
- **One-line install** (TEN-501): `install.sh` and `install.ps1` ship with every release,
  verify the SHA-256 against `checksums.txt` and install to `~/.local/bin` or
  `%LOCALAPPDATA%\Programs\tenant` without sudo or admin rights. A packaging CI job installs
  a snapshot through both scripts on all three OSes.
- **Install, service and update guides, and a release smoke job** (TEN-502): QUICKSTART and
  the README carry the one-liners; after each public release a job installs it through the
  scripts on all three OSes, checks the version and the absent quarantine mark, renders the
  service unit and reinstalls through `tenant update`.

### Changed
- **Dashboard cleanup** (TEN-402): http://localhost:8770 (and [::1]) reaches a loopback dashboard
  without being added to `allowed_hosts` (TEN-414); the cron form offers preset schedules, with the
  cron line still typable (TEN-420) and a failed job's row shows its error (TEN-427); the research busy refusal is in
  plain words (TEN-413); a connector URL's key stays out of the system log and of connect
  errors (TEN-409).
- **`os_exec` works like a Claude Code shell (TEN-489).**
  - It takes `workdir` (default: your home folder, never Tenant's own directory) and `timeout`
    (default 120s, up to 600s; it was a fixed 60s).
  - Long output keeps its start **and** its end, where errors are, up to 30 KB.
  - With no `exec_profiles` entry, the TUI, dashboard, Discord and iMessage run commands on the
    host behind their approvals. The old dry run ran a command the sandbox refused a second time
    on the host, so installers ran twice. Cron keeps its sandboxed default, and any
    `exec_profiles` entry opts a surface back in.
  - On the host a command starts in its own session: `sudo` and other prompts fail at once
    instead of hanging or grabbing the TUI's terminal. A timeout kills everything the command
    started, and a backgrounded child no longer hangs the call. `SSH_AUTH_SOCK` passes through,
    so git over ssh works.
  - Under launchd, Tenant fills in the login-shell PATH (`path_helper`, plus Homebrew), so `brew`,
    `node` and friends are found.
- **Fewer dead ends for the agent (TEN-489).**
  - An object or array argument sent as JSON text (Qwen via vLLM does this) is decoded instead
    of rejected, so `editJiraIssue`'s `fields` works.
  - A sandbox refusal is noticed even when the command exits 0 (`… || echo DENIED`, `| head`), so
    a dry run re-runs it on the host as it should.
  - A file-tool write outside the writable roots asks (`os_write_outside`, destructive) instead of
    being refused (so `/tmp` works with an approval). Protected and secret files stay hard
    refusals.
- **The destructive check is narrower (TEN-489).**
  - `rm -r` counts as destructive only for `/`, a top-level folder, a home folder or its direct
    child, `.`, `..` or `*`. `rm -rf node_modules` is an ordinary command again.
  - `shutdown`, `reboot` and `init` count only as commands, so `git log --grep=reboot` is fine,
    and `git log --format=…` is no longer a "disk format".
  - Newly caught: `diskutil erase…` and `bash -c "$(curl …)"`.
  - `wiki_save` and the gated CRM tools are writes, not destructive.

### Fixed
- **A corrupt `config.json` is no longer overwritten.** Tenant refuses to start on it and
  names the line and column where it broke. Before, the first setting saved from the
  defaults it fell back to (a dashboard toggle, a permission preset) wrote an empty config
  over the file.
- **The README quickstart runs:** `tenant tui --backend echo`. Flags go after the command
  name. The help text now lists every backend: `echo`, `vllm`, `anthropic`, `claudecode`.
- **`tenant agents …` honors `--config` and `--data`.** Before, it edited the default
  config, and could save a flag as the agent's model.
- **`tenant <command> --help` shows the command's help for every command.** Commands that
  read a subcommand or a task first used to fail with a usage error.
- **`budgets.max_wall_time` accepts a duration such as `"30m"`.** The `budgets` keys are now
  documented in docs/orchestration.md.
- **The eval baseline diff and the soul-nudge gate find their baseline under the data dir**,
  wherever Tenant starts, not only when it runs from a checkout.
- **`/setup` text prompts in the TUI accept typing.** The Model, Endpoint, Embeddings,
  gateway-address and API-key prompts never focused the input box, so keys were dropped
  and Enter saved a blank value.
- **Switching to the `claudecode` provider no longer reports it UNREACHABLE** and no longer
  starts an endless auto-reconnect loop. Its `claudecode://` endpoint is a local CLI, not
  an HTTP server.
- **`/disable` then `/enable` rebuilds a plugin from its current config.** A settings change
  to GitHub, Atlassian, Google, X or SimpleFIN, or to an MCP connector, now applies without a
  restart. Before, re-enabling reused the copy built from the old settings.
- **Claude Code's built-in tool calls show in the dashboard, TUI and turns log** (TEN-481),
  named `claude:Read`, `claude:Bash` and so on. Under the `claudecode` provider they used to
  run unseen inside `claude -p`.
- **The startup check no longer reports the `claudecode` provider UNREACHABLE** (TEN-482). It
  looks for the `claude` program instead of probing `claudecode://auto` over HTTP.
- **SQL, wiki, CRM, iMessage and Discord pick up config changes on `/disable` then `/enable`**
  (TEN-483), with no restart. Research and peer sharing follow a wiki moved to a new folder.
  Flags passed at launch still win, and a skill turned off in config.json stays off. Setup
  rejects a folder as the SQL database path.
- **`tenant setup` → Modify opens a list of sections** (TEN-477). Pick one, change it, and come
  back; Skills asks which skill to edit instead of walking every one.
- **The wiki tools name the wiki folder** (TEN-478), so the model doesn't have to search the
  disk for it.
- **Deep research no longer saves the model's planning notes as the report** (TEN-480). A
  report cut off by the token limit, or made mostly of planning, is retried once and otherwise
  fails the run.
- **A mode change on the Approvals page is no longer undone** when the settings watcher ticks
  before the change is saved (TEN-428).
- **The main agent sees helpers added, renamed or removed while it runs** (TEN-405).
- **Windows: hard-killing tenant.exe no longer leaves its headless Chrome running** (TEN-401).
  Tenant runs its children in a job that Windows closes with it, and removes profile folders
  left by earlier runs. Sign-in browsers still stay open after Tenant exits.

### Security
- **Dashboard sign-in** (TEN-403, TEN-415): after five wrong keys from one address, each
  further wrong key doubles a wait (up to a minute) during which that address gets 429;
  `tenant doctor` warns when the sign-in key is under 32 characters. The sign-in cookie no
  longer signs a request another site or another port on this computer started (an embedded
  image, a script, a fetch): the browser's Sec-Fetch-Site says so, and such a request gets 401
  with credential "cross-site cookie" in event 2103; page navigations are unaffected.
- **X sign-in:**
  - a callback without the sign-in's state is ignored rather than aborting the sign-in;
  - a busy callback port fails at once;
  - a second callback can't block the flow.

### Removed
- **The Linux and Windows `restricted` sandbox backends**: bubblewrap, and bubblewrap inside
  WSL2. `restricted` is now macOS-only (Seatbelt).
  - Linux and Windows run commands on the host until the container backend ships
    (docs/plans/docker-sandbox-plan.md). On Windows that means PowerShell, and `cmd /C` for
    cron.
  - An `exec_profiles` entry that names `restricted` there fails closed when it enforces.
  - A local MCP server with the default sandbox doesn't start there until its entry sets
    `"sandbox": "host"`.
  - `TENANT_SANDBOX_WSL_DISTRO` is gone.
- **`gateway.mode: "both"` and `tenant setup --both`.** One `mcp-memory` process serves one
  transport, so "both" never served stdio as well. A saved `"both"` loads as `"sse"`, which
  is what it already did.
- **The `deploy/launchd/com.tenant.serve.plist` template.** `tenant service install` writes
  the unit.

## 0.1.0 (2026-09-29)

The first tagged release. It has Windows, macOS and Linux binaries for amd64 and arm64,
with checksums and SPDX SBOMs.
