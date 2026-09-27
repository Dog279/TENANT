# Tenant plugin contract

A **plugin** is a self-contained capability bundle (web browsing, SQL,
wiki search, Gmail, etc.) that registers tools with Tenant's tool
multiplexer and dispatches calls when the model invokes one.

This document is the authoritative reference for what a plugin must
provide, how it gets wired in, how the lifecycle works, and the
load-bearing invariants you must not break. Every claim cites the
file:line in the codebase so it can be verified.

> Drift guard: `cmd/tenant/plugins_doc_test.go` asserts the key
> identifiers below still exist in the codebase. If you rename
> `plugin` / `ToolDispatcher` / `Tools()` / `Dispatch()`, the test
> breaks before this doc rots.

---

## 1. The contract

```go
// internal/toolmux/mux.go
type plugin interface {
    agent.ToolDispatcher
    Tools() []model.ToolSpec
}
```

Every Tenant plugin satisfies this. Two responsibilities:

1. **Advertise its tools** via `Tools()` — returns the JSON-schema
   specs the LLM sees in its function-calling prompt.
2. **Dispatch invocations** via `agent.ToolDispatcher.Dispatch` —
   runs the tool when the model calls it.

### `Tools()` return shape

```go
// internal/model/llm.go:97
type ToolSpec struct {
    Name        string          `json:"name"`
    Description string          `json:"description"`
    Parameters  json.RawMessage `json:"parameters"` // JSON schema
}
```

- **Name** — globally unique across the entire mux. Conventionally
  `<plugin>_<verb>` (e.g. `web_navigate`, `sql_query`). The mux
  errors at registration if two plugins claim the same name.
- **Description** — one short sentence the model reads to decide
  whether to call this tool. Lead with the verb. Avoid hedging.
- **Parameters** — a JSON Schema object. Must include `type`,
  `properties`, and `required`. `internal/agent/tools.go:181`
  type-checks each arg against the schema; missing required args
  fail fast with a useful error.

### `Dispatch()` return semantics — read carefully

```go
// internal/agent/tools.go:102
type ToolDispatcher interface {
    Dispatch(ctx context.Context, call model.ToolCall) (
        result string, isError bool, err error,
    )
}
```

Three return values; each means something distinct:

| return       | meaning                                            | model sees it? | agent loop  |
| ------------ | -------------------------------------------------- | -------------- | ----------- |
| `result, false, nil` | normal success                             | yes            | continues   |
| `result, true,  nil` | LOGICAL error ("file not found")           | yes            | continues   |
| `_, _, err`          | TRANSPORT failure (network, context cancel) | no (the err is summarized) | may abort the batch |

The `isError=true / err=nil` path is the right choice for "the tool
ran but the operation failed in a way the model can act on" — e.g.
"row not found", "Chrome timed out loading the page". The model
sees it, re-plans, retries with different args.

`err != nil` is for failures the model has no useful judgment about
— network down, OS-level error, context cancellation. The agent
loop treats this as a hard failure for that call.

---

## 2. The activator pattern (lazy init)

Heavy plugins (web spawns Chrome, sql opens a file handle, gsuite
authenticates) shouldn't run their setup until someone actually
enables them.

```go
// internal/toolmux/mux.go
activators map[string]func() (plugin, func(), error)
activated  map[string]bool
```

An activator is registered via `mux.RegisterActivator(label, fn)`
(internal/toolmux/mux.go). It runs at most once, on first `/enable <label>`:

```go
// internal/toolmux/mux.go — maybeActivate
//   1. Read m.activated[label] under the lock; if true, return.
//   2. Mark activated, snapshot the activator fn, drop the lock.
//   3. Run fn() — possibly slow (Chrome spawn, OAuth).
//   4. Re-take the lock; if someone else's activation won the race,
//      discard ours via `go cleanup()` (internal/toolmux/mux.go).
//   5. Otherwise install the built plugin + register its cleanup.
```

The contract for an activator function:

```go
func() (plugin, func(), error)
//      ^        ^         ^
//      |        |         error if build failed (Chrome missing, etc.)
//      |        cleanup function called at mux shutdown
//      the live plugin, ready to Dispatch
```

The cleanup func goes into `m.cleanups[]` and runs in reverse-order
at shutdown (LIFO).

### Activator vs. immediate registration

- **Activator** — plugin has expensive setup (network, subprocess,
  auth). Register the stub immediately, defer the real build to
  `/enable`. Web and SQL use this.
- **Immediate** — plugin is cheap to construct (no I/O, no auth).
  Just call `mux.add("name", plugin)`. Wiki uses this.

---

## 3. The stub / "needs setup" pattern

Unconfigured plugins MUST register as disabled stubs, not be silently
absent. Operators discover what's available via `/tools`; absent
plugins are invisible.

```go
// internal/toolmux/mux.go
type Stub struct { // toolmux.Stub; stubPlugin in package main
    specs []model.ToolSpec
    hint  string
}

func (s Stub) Dispatch(context.Context, model.ToolCall) (string, bool, error) {
    return "this plugin is not configured — " + s.hint, true, nil
    // ^ isError=true: the model sees the message and won't loop
}
```

Stubs are registered disabled (`mux.SetEnabled(label, false)`,
internal/toolmux/mux.go) so:

- `/tools` shows them in the catalog with a disabled marker
- Calling them returns the `hint` text so the model + operator both
  know what's missing
- `/enable` will fail until the underlying config is supplied
  (e.g. `--sql-db <file>` for the SQL plugin)

The `hint` is the contract between the stub and the operator. Make
it actionable: name the flag, file, or command to fix it.

---

## 4. Runtime enable/disable + tools/list_changed

`/enable foo` and `/disable foo` (TUI commands) call
`mux.SetEnabled(label, on)`. The mux:

1. Updates internal state under its lock (internal/toolmux/mux.go).
2. **Snapshots the enabled map** while still under the lock
   (internal/toolmux/mux.go).
3. **Releases the lock** (internal/toolmux/mux.go) — the next step does I/O.
4. Calls the registered `onChange` hook with the snapshot
   (internal/toolmux/mux.go).

The `onChange` hook (registered via `mux.SetOnChange(fn)`) is set in
two places at startup:

- `cmd/tenant/commands.go:343 / :1646` — persists the new state to
  `settings.json` so it survives restart.
- `cmd/tenant/mcpserver_wire.go` (when MCP server is running) —
  calls `srv.NotifyToolsChanged(ctx)` which emits
  `notifications/tools/list_changed` to every connected MCP client.

External MCP clients (Claude Desktop, IDE plugins) re-fetch the tool
list when they get that notification. The mux is the single source of
truth; clients never cache.

---

## 5. Worked example — adding a plugin

A first-party plugin is two things: its package under `internal/plugins/<id>/`,
and one **descriptor** in `cmd/tenant/plugin_<id>.go` that declares everything
the host needs to know about it (TEN-321). The registry
(`internal/pluginreg`, built in `cmd/tenant/plugindesc.go`) checks the
descriptors against each other and indexes them. Every surface reads it:
- the launch path and the stub catalog with its activators;
- the approval categories;
- `/configure`, the dashboard's Integrations page and `tenant setup`;
- doctor and the CLI;
- `permissions effective` and the security audit;
- privacy's connectors and the keys page;
- the file-tool guards and backup.

Take `wiki` as the example.

### Step 1 — the dispatcher

```go
// internal/plugins/wiki/dispatch.go
type Dispatcher struct { ix *Index }

func NewDispatcher(ix *Index) *Dispatcher { return &Dispatcher{ix: ix} }

func (d *Dispatcher) Tools() []model.ToolSpec {
    return []model.ToolSpec{
        {Name: "wiki_search", Description: "...", Parameters: searchSchema},
        {Name: "wiki_read",   Description: "...", Parameters: readSchema},
    }
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
    switch call.Name {
    case "wiki_search": return d.search(ctx, call)
    case "wiki_read":   return d.read(ctx, call)
    default:            return "", true, fmt.Errorf("unknown tool: %s", call.Name)
    }
}
```

`Tools()` must work on a nil service: the stub and the registry list the
specs without configuring anything.

### Step 2 — the descriptor

```go
// cmd/tenant/plugin_wiki.go
func wikiPlugin() *pluginDescriptor {
    return &pluginDescriptor{
        ID: "wiki", Title: "Wiki (markdown knowledge base)", Version: 1,
        Flags:       bindWikiFlags,   // --wiki-dir, into the plugin's own wikiFlags
        On:          func(pf *pluginFlags) bool { return wikiFlagsOf(pf).dir != "" },
        ApplyConfig: applyWikiConfig, // skills.wiki.settings → flags the operator did not pass
        ExpandPaths: expandWikiPaths,
        Launch:      launchWiki,      // builds the plugin when its flags configure it
        Prepare:     prepareWiki,     // first run: the vault and a starter note
        Tools:       func() []model.ToolSpec { return ... },
        Configure:   func() skillKind { return ... }, // settings and secret fields, probe
        SetupOnly:   true, SetupOrder: 1,
        Actions:     map[string]string{"wiki_save": catDestructive}, // permission classes
        StubHint:    "relaunch with --wiki-dir <dir>",
        External:    ...,             // the vault, for backup's "not covered" list
        Doctor:      []doctorCheck{{"wiki dir (if configured)", checkWikiDir}, ...},
        CLI:         []*cliCommand{{Names: []string{"wiki"}, Section: "plugins", Run: cmdWiki, ...}},
    }
}
```

The descriptor's fields, by concern:
- **Identity:** `ID` (the mux label, the key under `skills` in `config.json`,
  and the `<id>` of `skill:<id>:<field>` in `credentials.json`), `Title`, and
  `Version` of the saved settings shape.
- **Tools:** `Tools`, plus `PerAgent` for a plugin every agent gets its own
  copy of (web).
- **Config schema:** `Configure` (settings and secret fields, validators, the
  liveness probe, the setup hint), `SetupOnly`, `SetupOrder`,
  `AdaptConfigure`, `SelfEnables` and `OnConfigured`.
- **Permission classes:** `Actions` (every approval action id the plugin
  raises, with its category) and `Grants` (the categories its allow-flags
  pre-approve).
- **Lifecycle:** `Flags`, `On`, `ApplyConfig`, `ExpandPaths`, `Launch`,
  `Activate` (built on first `/enable` from saved config), `StubHint`,
  `Prepare` and `Eval`.
- **Health:** `Doctor` checks, and the probe in `Configure`.
- **Files and keys:** `ConfigSecrets` and `DataSecrets` (doctor audits them,
  the file tools refuse them), `PointedSecrets` (files the operator points it
  at), `Connector` and `Forget` (what `tenant privacy forget --connector`
  removes), `External` (for backup), and `Keys` (keys-page rows).
- **Surfaces:** `CLI`.

A plugin fills in only the fields it needs.

### Step 3 — name it in the registry

```go
// cmd/tenant/plugindesc.go — plugins()
pluginreg.MustNew(rules, sqlPlugin(), wikiPlugin(), ...)
```

The position is the plugin's place in the launch order, the stub catalog,
doctor and the CLI. That's it: there is no switchboard to edit, and tests
keep it that way.
- `TestEveryPluginDeclaresItself` checks that a plugin declares identity,
  version, tools, the permission class of every gated tool, and a stub hint.
- The registry refuses a tool or an action two plugins declare, an unknown
  category, and a reserved host label.
- `TestPluginsAreNamedOnlyInTheirOwnFiles` fails when a plugin's settings or
  secrets are named outside its own files.
- `TestTheSwitchboardsImportNoPlugin` fails when a surface imports a plugin
  package instead of reading the registry.

## 6. Hard rules learned the hard way

These are non-negotiable. Comments in `internal/toolmux/mux.go` flag each one.

### 6.1 — `Restore()` must not trigger `onChange`

The mux loads its persisted enabled-state at startup. If that load
called `onChange`, the just-loaded state would be re-saved
immediately, creating noise and a race with the file-writer.

Implementation: `Restore()` directly mutates `m.enabled` without
going through `SetEnabled`, bypassing the hook (internal/toolmux/mux.go
comment).

### 6.2 — Snapshot under the lock, then unlock BEFORE I/O

The `onChange` callback persists to `settings.json` or sends MCP
notifications. Both are I/O. Holding the mux lock during I/O blocks
every concurrent tool call.

Pattern: take lock → mutate `m.enabled` → make a defensive copy of
the map → unlock → call hook with the copy (internal/toolmux/mux.go).

### 6.3 — Activator race: cleanup the loser

Two concurrent `/enable web` calls both see `activated["web"] = false`
and both start the build. One wins the install; the other MUST
discard its build with `go cleanup()` (non-blocking) so its Chrome
process is killed.

Implementation: `maybeActivate` re-checks under the lock after the
build (internal/toolmux/mux.go).

### 6.4 — `Dispatch` `isError` vs. `err` is a real distinction

`isError=true, err=nil` → model sees the message and can act.
`err != nil` → the agent loop treats it as a hard failure.

Confusing these breaks the agent loop. "File not found" must be
`isError=true, err=nil` — the model can retry with a different path.
"OS write error" must be `err=non-nil` — the model can't fix it.

### 6.5 — Stubs always return `isError=true`

A stub call means the tool is unavailable. The model needs to know
that and stop calling it. Returning `false` would let the model
think the stub's "this plugin is not configured" text is a valid
result and keep using it.

---

## 7. Quick reference — every plugin in the repo

| Plugin    | File                                  | Heavy? | Built |
| --------- | ------------------------------------- | ------ | ----- |
| `web`     | `internal/plugins/web/`               | Yes (Chrome) | Per agent, each agent's own browser on first use; a shared session only with `--web` on commands without agents |
| `sql`     | `internal/plugins/sql/`               | Yes (DB handle) | At launch with `--sql-db`; stub otherwise |
| `wiki`    | `internal/plugins/wiki/`              | No     | At launch with `--wiki-dir`; stub otherwise |
| `gsuite`  | `internal/plugins/gsuite/`            | Yes (OAuth) | At launch with `--gsuite`, or by its activator from `/configure` |
| `imessage`| `internal/plugins/imessage/`          | Yes (BlueBubbles WS) | At launch with `--imessage`; stub otherwise |
| `discord` | `internal/plugins/discord/`           | No (REST-only Surface A) | At launch with `--discord`; stub otherwise |
| `x`       | `internal/plugins/x/`                 | Yes (login) | At launch with `--x`, or by its activator |
| `atlassian`| `internal/plugins/atlassian/`        | Yes (API token or OAuth 3LO) | At launch with `--atlassian`, or by its activator |
| `mcpremote`| `internal/plugins/mcpremote/`        | Only if the server asks (OAuth2.1 + DCR, browser; a local no-auth server connects instantly) | The MCP connector family, not a descriptor: one stub and activator per connector, remote (`mcp_remotes`) or local (`mcp_servers`: out of process under the execution sandbox, TEN-318); per-connector trust `ask` / `allow` / `deny`, live (`/mcp trust`, dashboard, `mcp_trust`); under `ask` every unannotated tool prompts |
| `osys`    | `internal/plugins/osys/`              | No. `os_exec` runs through the execution-sandbox seam (`internal/sandbox`): the default `host` backend confines nothing, and an operator `exec_profiles` entry moves a surface to the OS `restricted` backend (`docs/changes/sandbox-seam.md`). The file tools run in-process, outside it | Always at launch, enabled only with `--os` |
| `github`  | `internal/plugins/github/`            | No (REST control plane; code I/O stays on the local os_* tools) | At launch with `--github`, or by its activator |
| `crm`     | `internal/plugins/crm/`               | No (wraps the external `crm-tool` binary) | At launch when `--crm-tool-path`/`$CRM_TOOL_PATH` is set; stub otherwise |
| `simplefin`| `internal/plugins/simplefin/`        | No (one HTTPS GET; read-only by protocol) | By its activator from `/configure` |
| `cron`    | `internal/plugins/cron/`              | No     | A host tool, wired live in the TUI and serve, not a descriptor |

Every first-party plugin above except `mcpremote` and `cron` is declared in
`cmd/tenant/plugin_<id>.go` (`osys` as `os`).

`osys` is the "always-on" outlier: zero-config, built every launch and
registered disabled until `--os` (its `Launch` always builds; its `On`
decides). Use it as the template for any future zero-config plugin.

---

## 8. Who writes plugins

In-process plugins are **first-party only** ([ADR 0001](adr/0001-appliance-not-framework.md)):
they are compiled into the binary and reviewed with it, and this contract is an internal one
that may change with any refactor. A tool from anyone else is an **MCP server**, which Tenant
reaches over the protocol, gates with connector trust and tool rules, and runs out of process
when it is local (EXT-003 / TEN-318). Know-how, as opposed to a tool, is an **Agent Skill**
(EXT-001 / TEN-298). There is deliberately no `plugin/v1` package: an importable plugin API
would put untrusted code inside the approval, egress and sandbox boundaries.

Still open:
- **Runtime health**: each plugin declares its doctor checks and its liveness probe in its
  descriptor (ARC-003 / TEN-321), but there is no `mux.Health()` yet that reports a running
  plugin's liveness without a tool call.
