# Changelog

Notable changes to Tenant. Releases are tagged `vX.Y.Z`; their binaries are on the
Releases page.

## Unreleased

### Added
- **iMessage passive and respond modes.** `/imessage passive` reads every text and answers
  none; `/imessage respond` answers the allowlist and `imessage.operator`, and only in a
  1:1 iMessage chat. Group chats, SMS and RCS never drive the agent, and can no longer
  approve an action either. A text the responder doesn't answer is logged as an
  `imessage` Observed event (log alerts can act on it) and shown in the activity feed.
  The dashboard's Access page has the same switch. The allowlist now applies per message,
  not when the responder starts.
- **The iMessage responder runs over BlueBubbles.** Set a BlueBubbles URL and it answers
  texts from any OS, not only a Mac. Messages on a Mac stays the default.

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

## 0.1.0 (2026-09-29)

The first tagged release. It has Windows, macOS and Linux binaries for amd64 and arm64,
with checksums and SPDX SBOMs.
