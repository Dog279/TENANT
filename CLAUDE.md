# Tenant: notes for Claude Code sessions

Tenant is a Go-native, single-binary AI agent (see README.md and docs/ARCHITECTURE.md).
`go build ./...`, `go test ./...`, `go vet ./...` are the checks; CI runs them plus the
security workflow, and a CI run costs real money, so run the equivalent locally and batch
fixes before pushing.

## The public documentation lives on the website

The reference users read is https://findtime.net/tenant/docs/, built from Markdown in the
findtime-net repository (https://github.com/Dog279/findtime-net, `tenant/docs/_src/`). The
docs there describe Tenant's `main` branch (**Latest**); each release gets a frozen copy the
site's version menu can switch to. `docs/` in this repository is for contributors:
architecture, design, runbooks, change notes.

## Rule: a change to behavior ships with its documentation change

When a pull request changes anything a user can see or type, the matching page on the site
changes in the same piece of work:

1. Check out the site next to this repository if it is not there:
   `git clone https://github.com/Dog279/findtime-net ../findtime-net`.
2. Edit the page(s) under `../findtime-net/tenant/docs/_src/` (the table below says which),
   then `sh tenant/docs/_src/build.sh` there (pandoc: `brew install pandoc`) and commit the
   Markdown and the generated HTML together.
3. Open a findtime-net pull request that references the TENANT pull request. Merge it when
   the TENANT one merges, never before: Latest documents `main`.
4. In this repository, update README.md if the feature is listed there, CHANGELOG.md as
   usual, and any `docs/*.md` runbook that describes the changed behavior.

Keep the docs tied to the code, not to memory: the CLI registry (`cmd/tenant/cliregistry.go`,
the command's own flags), the slash-command registry (`internal/tui/cmd_*.go`), the
configuration types (`internal/config/*.go`), the plugin descriptors
(`cmd/tenant/plugin_*.go`) and the dashboard routes (`internal/dashboard`) are the sources
of truth. A syntax example in the docs must match the registry row it documents.

### Which code maps to which page

Paths are under `tenant/docs/_src/` in findtime-net.

| Changed in TENANT | Update |
|---|---|
| `cmd/tenant/cliregistry.go`; a command's flags in `cmd/tenant/*ctl.go`, `commands.go`, `serve.go` | `reference/cli.md`, plus the area page that explains the command |
| `internal/tui/cmd_*.go` (a slash command or its help row) | `reference/slash-commands.md`, plus the area page |
| `internal/config/config.go` (a `config.json` key) | `reference/config.md`, plus the page that explains the feature |
| `internal/config/providers.go` (provider kinds) | `models/providers.md`, `reference/environment.md` |
| `internal/config/route.go`, `internal/route` | `models/routing.md`, `reference/config.md` |
| `internal/config/mcp.go` (`mcp_servers`) | `mcp/local.md`, `reference/config.md` |
| `internal/config/settings.go`, `cmd/tenant/execprofiles.go`, `internal/sandbox` | `reference/settings.md`, `first-launch/sandbox.md` |
| `internal/approval`, tool rules, command rules, presets (`permissionsctl.go`) | `first-launch/permissions.md`, `mcp/trust.md`, `dashboard/approvals.md` |
| `internal/dashboard` (a page, a route, a form, the REST API) | `dashboard/pages.md`, `dashboard/api.md`, and the page's own topic |
| `cmd/tenant/plugin_*.go` (a plugin's tools, flags, keys, configure fields, doctor check) | `mcp/built-in-tools.md`, `mcp/integrations.md`, `reference/environment.md` |
| `cmd/tenant/mcp*.go`, `internal/plugins/mcpremote` | `mcp/remote.md`, `mcp/trust.md`, `mcp/memory-server.md` |
| `cmd/tenant/setup.go`, `setupctl.go`, `internal/tui/setupmenu.go` | `first-launch/setup-wizard.md` |
| `cmd/tenant/doctor*.go` | `first-launch/doctor.md` |
| `cmd/tenant/service*.go`, `update.go`, `scripts/install.*`, `.goreleaser.yaml` | `setup/service.md`, `setup/update.md`, `setup/install.md`, `setup/requirements.md` |
| `cmd/tenant/backupctl.go`, `internal/backup` | `setup/backup.md` |
| `cmd/tenant/privacyctl.go`, `internal/privacy` | `setup/privacy.md` |
| the data or config dir layout (a new file or folder) | `setup/directories.md` |
| `internal/memory/*`, `cmd/tenant/memory_*.go`, the soul | `first-launch/memory.md` |
| skills (`internal/skillpkg`, `internal/memory/skills`, `cmd/tenant/skill*.go`) | `first-launch/skills.md` |
| `internal/model` (router, fallbacks, health gating, profiles, tool formats, reasoning) | `models/switching.md`, `models/fallbacks.md`, `models/tuning.md`, `models/local.md` |
| helpers (`agentctl.go`, `builtinagents.go`, `builtinsouls/`), `internal/orchestra`, research, goal, review | `models/helpers.md`, `models/teams.md` |
| `cmd/tenant/eval*.go`, `judgectl.go`, `internal/eval` | `models/eval.md` |
| `cmd/tenant/cronmgr.go`, `internal/cron` | `dashboard/cron.md` |
| `internal/logs`, `cmd/tenant/logs*.go`, `logctl.go`, alerts, `attach.go` | `dashboard/logs.md`, `dashboard/api.md` |
| the Discord relay, the iMessage responder | `dashboard/access.md` |
| `internal/peering`, `cmd/tenant/peer*.go` | `models/teams.md` (Peers), `reference/cli.md` |
| a new `os.Getenv` | `reference/environment.md` |
| the terminal UI's keys, timer or session commands | `first-launch/terminal-ui.md` |

A quick completeness check before opening the PR: `git diff --name-only origin/main`, map each
file through this table, and `grep -rn "<the command, flag or key>" ../findtime-net/tenant/docs/_src`.

### Versions

When a release is tagged, freeze the docs for it in findtime-net:

```sh
sh tenant/docs/_src/build.sh && sh tenant/docs/_src/snapshot.sh vX.Y.Z
```

That writes `tenant/docs/vX.Y.Z/` and lists the version in `tenant/docs/versions.js`, which
the site's version menu reads. Commit both. A frozen version is never edited, except to correct
a statement about what that version actually did.

## Other repository conventions

- `docs/ARCHITECTURE.md` carries generated repository facts (`scripts/docfacts`). Regenerate
  them from a clean, detached worktree of the commit, never from a checkout that has
  `.claude/worktrees/` inside it, or the counts are inflated and CI fails.
- Secrets never go in the repository: `config.json`, `credentials.json` and the data dir live
  in the operator's home directory. Tests use fixtures and the `echo` backend.
