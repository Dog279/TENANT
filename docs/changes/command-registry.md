# Change: one command registry — generated help and completion, TUI commands in feature files (DOC-003, ARC-001)

**Branch:** `refactor/ten-320-303-command-registry` · **Status:** implemented and tested; not yet
deployed.
**Tickets:** DOC-003 / TEN-320, ARC-001 / TEN-303.

## What changed
- **`internal/cmdreg`** is the command registry both surfaces use. A command is declared once,
  and the registry refuses a duplicate name, a missing summary or handler, and an unknown
  section. It holds:
  - the command's names;
  - its help section;
  - a one-line summary;
  - its usage rows (a row can also list the command under a second section);
  - an optional beginner task;
  - subcommands and flags for completion;
  - the surface's handler.
- **The CLI dispatches through it.** `cmd/tenant/cliregistry.go` declares every subcommand, and
  `main` is a lookup. The help and completion are generated from the same declarations:
  - `tenant` and `tenant help` show the jobs a new operator does first, as tasks: connect a
    model, open the app, fix a problem, run the hub, find a memory, back up, see and forget
    data;
  - `tenant help all` is the full reference by section;
  - `tenant help <command>` shows one command;
  - `tenant completion bash|zsh|fish|powershell` prints a completion script.

  `tenant <command> --help` now exits 0 after printing the flags, instead of reporting "flag:
  help requested" as an error.
- **Nothing dispatched is missing from the help.** The old `usage()` left out `oauth-setup`,
  `consolidate`, `goal`, `skills`, `atlassian`, `ack` and `undo`; they are all there now. It
  also described `serve` as "background self-improvement". The help (and ARCHITECTURE §5.13)
  now describes the headless 24/7 hub:
  - the TUI's memory, tools and approvals, driven from the dashboard;
  - self-improvement and the nightly eval;
  - cron and retention;
  - MCP connectors;
  - the peer listener;
  - the Discord relay.
- **The TUI's 1,061-line `handleSlash` switch is gone.** Each case is now a function in its
  feature's file, next to the helpers it uses:
  - `cmd_models.go`: `/model`, `/route`, `/ceiling`, `/reasoning`, `/eval`, `/judge`;
  - `cmd_memory.go`: `/memory`, `/compress`, `/expand`, `/ack`, `/undo`;
  - `cmd_skills.go`: `/tools`, `/enable`, `/disable`, `/skills`, `/skill`;
  - `cmd_peers.go`: `/peer`, `/mcp`, `/github`;
  - `cmd_approvals.go`: `/permissions`, `/approve`, `/deny`, `/relay`, `/imessage`;
  - `cmd_setup.go`: `/setup`, `/configure`, `/cancel`;
  - `cmd_research.go`: `/research`, `/goal`, `/review`, `/cancel-clarify`;
  - `cmd_agents.go`: `/agents`;
  - `cmd_cron.go`: `/cron`;
  - `cmd_session.go`: `/help`, `/quit`, `/clear`, `/cls`, `/mouse`, `/whoami`, `/dashboard`,
    `/tailscale`.

  `tui.go` went from 7,149 lines to about 3,060. `handleSlash` is a registry lookup. A new
  command goes into its feature's `xxxCommands()` list, and a new feature is one line in
  `slashFeatures`.
- **The move was mechanical.** A Go AST tool cut each case body out verbatim, turned the
  `break`s that left the switch into `return nil`, and declared only the variables each case
  read. It moved each helper function whole, doc comment included. Every function the old file
  declared still exists, and the TUI's existing 3,500-line test suite passes with only its
  help tests pointed at the registry.
- **TUI help.** `/help`, `/help <category>` and `/help all` render from the registry.
  - Bare `/help` opens with a "Start here" list (`/model`, `/setup`, `/help`, `/quit`,
    `/clear`) before the categories.
  - `/expand` and `/relay` were dispatched but had no help. `/relay` gets its six forms.
  - `/memory forget` now says it hides rather than deletes, and points at
    `tenant privacy forget`.
  - Help columns count runes, so rows with `↑ / ↓` or `…` line up.

## Why
Every new command meant editing a central switch and, separately, a help table. The two
drifted: `/expand` and `/relay` had no help, seven CLI commands were missing from `tenant`'s
usage, and `serve`'s description was months stale. The TUI's command logic was one function the
size of a small program, where every feature's change collided with every other's.

## Evidence
- **`internal/cmdreg`:** lookup by alias and case, rows grouped by section (including a row
  listed under a second section, and section keys), and the refusal rules.
- **`internal/tui`:** every one of the 62 names the old switch handled still dispatches; every
  command is in `/help all` under its own name; "Start here" comes before the categories; an
  unknown command gets the old message; plus the existing suite.
- **`cmd/tenant`:**
  - every name `main` used to accept still dispatches;
  - every command is in `tenant help all`;
  - the beginner help is task-shaped and under a third of the reference;
  - `tenant help serve` names every part of the hub;
  - per-command help;
  - completion scripts for all four shells contain every command, subcommand and flag;
  - the bash script passes `bash -n`.
- **By hand:** the PowerShell script parses, and `TabExpansion2` completes `tenant bac` →
  `backup`, `tenant backup ` → `create verify restore`, and `tenant backup --pass` →
  `--passphrase-file`.
- **The appliance terminology test** (TEN-324) still holds against the new `main.go`.
