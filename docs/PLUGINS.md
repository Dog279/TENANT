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
// cmd/tenant/toolmux.go:29
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
// cmd/tenant/toolmux.go:57
activators map[string]func() (plugin, func(), error)
activated  map[string]bool
```

An activator is registered via `mux.registerActivator(label, fn)`
(toolmux.go:151). It runs at most once, on first `/enable <label>`:

```go
// cmd/tenant/toolmux.go:314 — maybeActivate
//   1. Read m.activated[label] under the lock; if true, return.
//   2. Mark activated, snapshot the activator fn, drop the lock.
//   3. Run fn() — possibly slow (Chrome spawn, OAuth).
//   4. Re-take the lock; if someone else's activation won the race,
//      discard ours via `go cleanup()` (toolmux.go:334).
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
// cmd/tenant/toolmux.go:254
type stubPlugin struct {
    specs []model.ToolSpec
    hint  string
}

func (s stubPlugin) Dispatch(context.Context, model.ToolCall) (string, bool, error) {
    return "this plugin is not configured — " + s.hint, true, nil
    // ^ isError=true: the model sees the message and won't loop
}
```

Stubs are registered disabled (`mux.SetEnabled(label, false)`,
toolmux.go:558) so:

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

1. Updates internal state under its lock (toolmux.go:274).
2. **Snapshots the enabled map** while still under the lock
   (toolmux.go:300).
3. **Releases the lock** (toolmux.go:303) — the next step does I/O.
4. Calls the registered `onChange` hook with the snapshot
   (toolmux.go:304).

The `onChange` hook (registered via `mux.setOnChange(fn)`) is set in
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

## 5. Worked example — wiring a new plugin

Take `wiki` as the simplest example. Three steps:

### Step 1 — define the dispatcher

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

### Step 2 — wire into `buildToolMux`

```go
// cmd/tenant/toolmux.go:444
if pf.wikiDir != "" {
    ix, err := buildWikiIndex(ctx, c, router, pf.wikiDir, log)
    if err != nil { return fail(err) }
    mux.add("wiki", wiki.NewDispatcher(ix))
}
```

For a heavy plugin you'd use `registerActivator` instead, with the
build inside the closure.

### Step 3 — register the stub for unconfigured runs

```go
// cmd/tenant/toolmux.go:534 — the stub catalog
{label: "wiki", specs: wiki.NewDispatcher(nil).Tools(),
 hint:  "relaunch with --wiki-dir <dir>"},
```

The stub is built from `NewDispatcher(nil)` — the dispatcher must be
nil-tolerant for `Tools()` (just return the static spec list). It
won't be called for `Dispatch()` since the stub overrides that.

That's it. No central registry to update, no init() side effects, no
hidden coupling.

---

## 6. Hard rules learned the hard way

These are non-negotiable. Comments in `toolmux.go` flag each one.

### 6.1 — `restore()` must not trigger `onChange`

The mux loads its persisted enabled-state at startup. If that load
called `onChange`, the just-loaded state would be re-saved
immediately, creating noise and a race with the file-writer.

Implementation: `restore()` directly mutates `m.enabled` without
going through `SetEnabled`, bypassing the hook (toolmux.go:62
comment).

### 6.2 — Snapshot under the lock, then unlock BEFORE I/O

The `onChange` callback persists to `settings.json` or sends MCP
notifications. Both are I/O. Holding the mux lock during I/O blocks
every concurrent tool call.

Pattern: take lock → mutate `m.enabled` → make a defensive copy of
the map → unlock → call hook with the copy (toolmux.go:297).

### 6.3 — Activator race: cleanup the loser

Two concurrent `/enable web` calls both see `activated["web"] = false`
and both start the build. One wins the install; the other MUST
discard its build with `go cleanup()` (non-blocking) so its Chrome
process is killed.

Implementation: `maybeActivate` re-checks under the lock after the
build (toolmux.go:334).

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

| Plugin    | File                                  | Heavy? | Activator? |
| --------- | ------------------------------------- | ------ | ---------- |
| `web`     | `internal/plugins/web/`               | Yes (Chrome) | Yes |
| `sql`     | `internal/plugins/sql/`               | Yes (DB handle) | Yes |
| `wiki`    | `internal/plugins/wiki/`              | No     | No (immediate) |
| `gsuite`  | `internal/plugins/gsuite/`            | Yes (OAuth) | Yes |
| `imessage`| `internal/plugins/imessage/`          | Yes (BlueBubbles WS) | Yes |
| `discord` | `internal/plugins/discord/`           | No (REST-only Surface A) | No (immediate when --discord) |
| `x`       | `internal/plugins/x/`                 | Yes (login) | Yes |
| `atlassian`| `internal/plugins/atlassian/`        | Yes (API token or OAuth 3LO) | Yes |
| `mcpremote`| `internal/plugins/mcpremote/`        | Yes (remote OAuth2.1 + DCR, browser) | Yes (tools mirror a remote MCP server's gating) |
| `osys`    | `internal/plugins/osys/`              | No (sandboxed exec) | No (immediate, always on) |
| `crm`     | `internal/plugins/crm/`               | No (wraps the external `crm-tool` binary) | No (registered when `--crm-tool-path`/`$CRM_TOOL_PATH` is set; stub otherwise) |
| `cron`    | `internal/plugins/cron/`              | No     | No (wired live in the TUI, not the stub catalog) |

`osys` is the "always-on" outlier — it's zero-config and registers
immediately every launch. Use it as the template for any future
zero-config plugin.

---

## 8. Future work (NOT in scope today)

- **Out-of-process plugins** — running each plugin as a separate
  process over the existing MCP transport so a crashing web plugin
  doesn't take the binary down. Promising but fights the
  single-binary CGO-free thesis. Defer until in-process crash
  reliability is the bottleneck.
- **A `plugin/v1` package** — extracting the interface into its own
  package to forbid deep imports from out-of-tree plugins. Worth
  doing the moment Tenant gets its first external contributor.
- **Per-plugin health checks** — `mux.Health()` returning per-plugin
  liveness so `tenant doctor` surfaces a degraded plugin without
  needing a tool call to discover it.
