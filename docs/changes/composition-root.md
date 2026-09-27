# Change: the composition root, split and held to its dependency rules (ARC-002)

**Branch:** `refactor/ten-314-composition-root` (slices 0–2, on top of
`feat/ten-297-provenance-inspector`) and `refactor/ten-314-control-plane`
(slices 3–4, on top of `feat/ten-321-plugin-registry`) · **Status:** slices
0–4 are implemented and tested on Windows; not yet deployed. What is left of
the ticket is under "Still open".
**Ticket:** ARC-002 / TEN-314.

## What changed

### Slice 0: the dependency directions, documented and enforced
- `internal/archtest` loads the module's import graph from the non-test source
  on every `go test ./...` and checks it against `rules.go`. Each package's
  rule lists the module packages it may import, and the first matching rule
  governs.
- The test also fails when:
  - a package matches no rule, so a new package needs a decision;
  - a rule matches no package, which means it is left over from a package that
    moved;
  - any package other than `internal/tui` and `cmd/tenant` imports Bubble Tea.
- `docs/ARCHITECTURE.md` §2 replaces its stale ASCII graph with the rules table,
  which is generated from `rules.go` and checked by the same test run
  (`go test ./internal/archtest -update` rewrites it).

### Slice 1: configuration moves to `internal/config`
- The types, defaults and validation of `config.json`, `credentials.json` and
  `settings.<agent>.json` move out of `package main`, together with the only
  safe ways to read and write those files:
  - `Launch` and its blocks, the provider catalog, and route resolution;
  - `Credentials` and `ResolveSecret`;
  - `Settings` and `ExecProfile`;
  - `Load`, `Launch.Save`, `Persist` and `AtomicWrite`.
- `internal/config` imports `fsperm`, `secrets`, `model`, `route` and
  `orchestra`, and no runtime package. It keeps a copy of the claudecode
  discovery sentinel rather than importing a model backend, and a test holds
  the two copies together.
- `cmd/tenant/config_compat.go` keeps package main's names as aliases and
  one-line wrappers (`launchConfig`, `loadLaunchConfig`, `persistLaunchConfig`,
  …), so call sites read as before.
- The unexported methods main called became exported (`Save`, `Active`, `Get`,
  `Set`, `ModelConfig`, `Problems`; `dashboardEnabled` became `IsEnabled`,
  since `Dashboard` already has an `Enabled` field). The compiler located every
  call site: 101 in main and 142 in its tests. A text rename would have caught
  `githubTUIControl.save` as well.
- Model probing (`fetchModels` and the embed setup hint) stays in main, as
  `cmd/tenant/modelprobe.go`.
- The pure configuration tests move with the code: 17 tests, among them
  round-trip, v1 migration, route resolution and validation, concurrent
  persist, credentials, settings and budgets.
- **The file formats are pinned.** `TestTheFileFormatsAreStable` lists every
  JSON key path of the three files (175) against a golden file. A renamed or
  dropped key fails the test, because operators' files still use it. The key
  paths from the pre-move types, dumped from a worktree of the previous commit,
  are identical.

### Slice 2: the tool mux moves to `internal/toolmux`
- `toolmux.Mux`, the agent's tool registry and dispatcher, leaves
  `package main` with its federation and dispatch-count code. Its
  responsibilities:
  - enable/disable with persisted curation;
  - lazy activation of stubbed plugins;
  - relevance ranking;
  - federated peer search;
  - remote-MCP adoption and trust.

  It builds no plugin: what to register stays in
  `cmd/tenant/toolmux_build.go`, which is split out of the old `toolmux.go`.
- A new leaf, `internal/control`, holds the plain data types the runtime, the
  terminal UI and the dashboard share. It starts with `ToolInfo`, which
  `internal/tui` now aliases, so the mux can list tools without importing the
  UI.
- **The API main uses was exported first, in place, before anything moved.**
  The compiler then located every call site: 193 method calls and 7 struct
  keys.
  - `Add`, `Restore`, `SetOnChange`, `RegisterActivator` and
    `RegisterMCPRemote` are now exported, among others. So are the `Stub` and
    `MCPDeps` fields.
  - Setting the MCP deps and reading their approval gate go through
    `SetMCPDeps` and `MCPConfirm`, instead of main writing into the struct.
  - `Owner(name)` reports which dispatcher serves a tool.
- `cmd/tenant/toolmux_compat.go` keeps main's names (`toolMux`, `plugin`,
  `stubPlugin`, `newToolMux`, …).
- The tests that exercise the mux's internals move with it: 69 tests, among
  them ranking, federation, peer boost, restore and curation, activation,
  auth state, labels and dispatch counts. `TestSearchPolicyPrompt_Content`
  stays in main, beside the prompt it tests.
- `archtest` gains both rules: `toolmux` may import `agent`, `model`,
  `memory/cosine`, `plugins/mcpremote`, `peering` and `control`.
- `docs/PLUGINS.md` and its drift guard point at `internal/toolmux/mux.go`.
  The DATAFLOW rows that cited `cmd/tenant/toolmux.go` by line numbers,
  which had already drifted, cite the builder by function.

### Slice 3: the approval broker moves to `internal/approval`
- `approval.Broker`, the decision point for every dangerous action, leaves
  `package main` with its pending-approval registry and the per-tool rule set.
  `Confirm` resolves an action's category, then applies, in order:
  - the surface's lock (the Discord relay's exec mode);
  - the operator's per-tool rules;
  - the category's mode (ask, allow or deny) and session grants.

  For "ask" it raises a request that any surface can resolve by id, or hands it
  to a channel's own backend (Discord buttons, iMessage replies). It fails
  closed when no surface can reach the operator.
- **It knows no plugin and no surface.** The command injects:
  - which category an action belongs to: `cmd/tenant/approval.go`'s
    `categorize` reads the host's table, then the plugin registry's;
  - how modes and rules persist (`SetPersist`);
  - the event stream (`SetEvents`);
  - a channel's ask backend (`SetAsk`) and the surface's lock (`SetLock`).

  Main used to assign the broker's unexported fields directly. Those
  assignments are now setters, and `Locked(action)` reports a surface's lock.
- **The fail-closed default moves with it.** `approval.Categorize` files an
  action no table knows under destructive, which always asks unless the
  operator opened it.
- **The approval types the surfaces share move to `internal/control`:**
  `ApprovalRequest`, `ApprovalDecision`, `PendingApproval`, `PermissionInfo`
  and `ToolRuleInfo`. `internal/tui` and `internal/dashboard` alias them, so
  their callers read as before, and the broker imports neither UI.
- **What stays in main** is what depends on the command: the host's action
  table, `categorize`, the launch flags' grants (`seedFromFlags`), and aliases
  for the broker's old names.
- **The tests move with the code:** 22 tests covering the registry fan-out,
  decisions, persistence, the Discord and iMessage brokers, and tool rules.
  They use a small stand-in for the command's action tables. `TestCategorize`
  and the flag seeding stay in main, beside the tables. A new test pins each
  broker's defaults: the host and Discord ask, and the offsite broker denies.
- `archtest` gains the rule: `approval` may import `agent`, `control`, `logs`
  and `runtrace`. `dashboard` may now import `control`.

### Slice 4: one staged shutdown for serve and the TUI
- **`internal/lifecycle` orders a long-running command's teardown.** A
  component registers how to stop it, and at which stage, where it is built.
  `Shutdown` then runs the stages in order:
  1. **Unblock:** deny pending approvals, so no gated turn stays blocked.
  2. **Intake:** stop the peer listener, the Discord relay and the dashboard,
     so nothing starts a turn or a job mid-teardown.
  3. **Drain:** let cron and self-improvement finish their running jobs.
  4. **Release:** close the tool stats, the team runtime and bus, the stores
     and the rest, last opened first, like the deferred calls they replace.

  A failing or panicking stop is logged and reported, and the rest still run.
  `Shutdown` runs once, so a command both defers it (for an early return) and
  calls it at the end. A stop registered after teardown began runs at once.
- **`serve` keeps its order (TEN-194).** It is now declared where each
  component is built, instead of in a block at the end plus a stack of
  defers. The only change is within the drain stage: self-improvement now
  drains before cron. Both still finish before anything is released.
- **The TUI now tears down like `serve`.** It used to drain cron and
  self-improvement and then close its stores. It never:
  - denied pending approvals;
  - stopped its peer listener, its relay or its dashboard.

  So a background turn could arrive, or sit blocked on an approval, while the
  stores closed. The relay and the dashboard stop with `Shutdown`, which
  persists nothing. Their enabled setting survives the exit, as it does in
  `serve`.
- **Only process-level resources stay deferred:** the log hub closes after
  the teardown, and the signal context is released with the process.
- **Not stopped at exit in either command:** the iMessage responder, whose
  `Stop` persists "off". It ends with the context, as before.
- `archtest` gains the rule: `lifecycle` is a leaf.

## Why
All configuration, runtime lifecycle, control plane and plugin registry code
lived in `package main`, with dependency directions nobody wrote down or
checked. ARC-002 asks for these to move into internal packages behind
enforced rules, so `cmd/tenant` becomes thin command adapters.

## Evidence
- `go vet ./...` is clean, and `go test ./...` passes (66 packages after
  slice 2, 68 after slice 3, 69 after slice 4).
- The smoke eval passes 5/5 against its baseline.
- Mutation testing:
  - archtest: 7 mutations of the rules and checker, all caught;
  - config: a renamed `config.json` tag and a renamed `settings.json` tag, both
    caught by the format test;
  - approval: 12 mutants of the broker and its wiring, all caught by failing
    tests. They cover the fall-through category, the shared registry, the lock,
    the ask backend, rules, persistence, flag seeding, events and the offsite
    default. Two first compiled only with dead code left in, and were rerun as
    mutants that build;
  - lifecycle and the teardown wiring: 9 mutants, all caught. They cover stage
    order, order within a stage, running once, a late stop, a panic, reported
    errors, a TUI step missing, a serve step in the wrong stage, and a bare
    defer.
- **Serve and the TUI** are held to the same teardown by
  `TestServeAndTUITearDownAlike`. It reads both commands' source and checks
  that each registers the shared hub's stops at the same stages. It also
  checks that neither closes anything through a bare `defer`.

## Still open
- **`cmd/tenant` is not yet a set of thin command adapters.** It still holds
  about 40,000 lines in 133 files. The hub's construction is written out
  twice: in `cmdServe`, and in `cmdTUI` with the TUI's controls interleaved.
  It covers the router, stores, approval broker, tool mux, team runtime,
  cron, relay, dashboard and peers. The next step is one `buildHub` that both
  commands call. Then the hub, and the control adapters (`*ctl.go`) that
  serve the TUI and the dashboard, can move behind internal packages.
- **The plugin descriptors stay in `package main`** (`plugin_<id>.go`). Their
  hooks are adapters over the command's flags and controls. The registry's
  rules and indexes are `internal/pluginreg` (TEN-321).

## Operator notes
- No change to files: `config.json`, `credentials.json` and
  `settings.<agent>.json` read and write exactly as before.
- On exit, the TUI now denies approvals still pending and stops its peer
  listener, relay and dashboard before closing its stores, as `serve` does.
