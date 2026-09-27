# Change: the composition root, split and held to its dependency rules (ARC-002)

**Branch:** `refactor/ten-314-composition-root` (on top of
`feat/ten-297-provenance-inspector`) · **Status:** in progress. Slices 0 and 1
are done and tested on Windows; later slices extend this record.
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

## Why
All configuration, runtime lifecycle, control plane and plugin registry code
lived in `package main`, with dependency directions nobody wrote down or
checked. ARC-002 asks for these to move into internal packages behind
enforced rules, so `cmd/tenant` becomes thin command adapters.

## Evidence
- `go vet ./...` is clean, and `go test ./...` passes (65 packages).
- The smoke eval passes 5/5 against its baseline.
- Mutation testing:
  - archtest: 7 mutations of the rules and checker, all caught;
  - config: a renamed `config.json` tag and a renamed `settings.json` tag, both
    caught by the format test.

## Operator notes
- No behavior change. `config.json`, `credentials.json` and
  `settings.<agent>.json` read and write exactly as before.
