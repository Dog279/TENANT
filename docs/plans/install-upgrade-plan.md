# Install & upgrade: implementation and test plan (TEN-495, GAP-3)

Status: implementation plan, 2026-10-02 (rev 3: adds OS-event safety, PR E, and the Windows service).
Workstream TEN-495. Source: the 2026-10-02 competitive review (`Tracking/competitive-reviews/2026-10-02/`), where
install and cross-platform scored **4.3** against 8 and 8.3.

**Goal:** a newcomer on macOS, Linux or Windows goes from nothing to a supervised background service with one
command plus `tenant setup`, later upgrades with `tenant update` with a way back, and an OS restart or shutdown at
any moment, mid-turn or mid-upgrade, loses nothing that was already produced and tells the operator what was cut.
Re-score target **≥7**.

## Decisions (operator, 2026-10-02)

| # | Decision | Effect on this plan |
|---|---|---|
| D1 | Windows runs as whoever ran `tenant service install`; `--system` runs `serve` as SYSTEM for headless boxes | User mode: a scheduled task at logon, no stored password. `--system`: a real Windows service (SCM), which is what gets a proper shutdown notice (see PR E) |
| D2 | Update **check** on by default with a config opt-out; a manual `tenant update` command | `update_check` config field; a daily check that only writes a log line; nothing is applied automatically |
| D3 | `install.sh` instead of Homebrew for now | No Homebrew cask and no .deb/.rpm: the script plus `tenant service install` cover Linux |
| D4 | Skip notarization (certificates exist, so it can be added later) | Script installs carry no quarantine flag; later: GoReleaser's `notarize.macos` block plus two secrets |
| D5 | Skip Windows code signing; buy a certificate if Tenant takes off | `install.ps1` downloads carry no Zone.Identifier mark |
| D6 | Defer the Docker image | Out of scope |
| D7 | Bring-your-own Google OAuth client | Out of scope; `tenant setup` points at `docs/SETUP-GSUITE-OAUTH.md` |
| D8 | OS restart/shutdown (not power loss) must be safe for Tenant and for an upgrade in flight; graceful stops preferred | PR E, and the `--stop-budget` each unit passes to `serve` |

Reading of D2: the check is automatic, the apply is not. An opt-in automatic apply would be ~15 lines on top of
this and is not planned.

## Ground rules

- **No new Go modules.** Stdlib (`net/http`, `archive/tar`, `archive/zip`, `compress/gzip`, `crypto/sha256`,
  `text/template`), GoReleaser 2.18 (already the release tool), each OS's own service manager, and
  `golang.org/x/sys/windows/svc` from the `x/sys v0.47.0` module the build already requires.
- **Generalized for any host.** No host-specific paths or names. Defaults come from the same directory resolution
  every command uses (`commonFlags.resolveDirs`, `cmd/tenant/wiring.go:116`).
- **One seam for external commands.** `var runCmd = func(ctx context.Context, name string, args ...string) (string, error)`
  in `cmd/tenant/service.go`, swapped by tests for a recorder, the way `holdDataDir` and `promptPassphrase` already
  work. Every `launchctl`, `systemctl`, `loginctl`, `schtasks`, `sc` and `cosign` call goes through it.
- **Crash-safe first, graceful second.** Every durable write must survive a kill at any instant (that is mostly true
  today, see PR E's audit). The graceful stop path then adds "tell the operator what was cut" and a clean marker.
  Budgets are never trusted: the first 4 seconds of a stop make the data safe on every OS; whatever time the OS
  grants beyond that is a bonus.
- **CI costs money.** Each PR runs the local CI mirror first (the Docker `golang:1.26` Linux run in CI's order, the
  macOS suite, `go vet` for all three OSes, docfacts from a clean worktree), then one push. Live tests that touch a
  real service manager or reboot a machine run on the operator's machines, not in CI.
- **Five PRs, in order:** A `tenant service`, E stop and resume safety, B `tenant update`, C install scripts and
  release plumbing, D docs and the release smoke job. A, E and B are useful on the private repo today.

## What exists and what changes

| Piece | Today | After |
|---|---|---|
| Release build | GoReleaser: 3 OS × 2 arch, tar.gz/zip, SBOMs, `checksums.txt`, cosign skipped while private | Same, plus `install.sh`/`install.ps1` as release assets, a `Repo` ldflag, a post-release smoke job |
| Version | `-X tenant/internal/version.{Version,Commit,BuildTime,Target,Dirty}`; dev builds say `0.0.1` | Plus `Repo` (owner/name) from `GITHUB_REPOSITORY` |
| macOS service | A plist template with `{{PLACEHOLDERS}}` edited by hand (`deploy/launchd/com.tenant.serve.plist`) | `tenant service install` writes it; the template is deleted |
| Linux service | None | A systemd user unit written by `tenant service install` |
| Windows | `schtasks /SC ONSTART /RU SYSTEM` in `scripts/migrate-to-server.ps1`; `runAsService()` and the dashboard already speak of a "Tenant service" that does not exist | User mode: a scheduled task at logon. `--system`: a Windows service through the SCM, which makes the dashboard's "open Services, find Tenant" note true |
| Stop path | SIGTERM → staged teardown (TEN-194: deny approvals, stop intake, drain, close stores) → boot record marked clean (TEN-373). No per-step deadline; an in-flight turn is cancelled silently; the agent then spends up to 30 s on a "salvage" synthesis (`internal/agent/agent.go:850`) | A stop budget per supervisor, a deadline on the teardown, no salvage while stopping, the turn recorded as interrupted, the operator told, and a boot-time resume (PR E) |
| Upgrade | None | `tenant update`, `--check`, `--rollback`, interruption-safe (PR B) |
| Status API | `/api/status` has `turn_active` but no version | Plus `version` |
| Schema direction | Forward-only: the log book refuses a newer database (`internal/logs/store/store.go:313`) | Unchanged. **Rollback = old binary + the pre-update backup** |

---

## PR A: `tenant service`

```
tenant service install [--print] [--system]   # write the unit, enable it, start it; --print only shows it
tenant service uninstall                      # stop, disable, remove
tenant service status                         # installed? running (version, turn_active)? unit path, log path
tenant service start | stop                   # through the service manager
tenant service restart [--now]                # waits for turn_active=false unless --now
```

### Files

| File | Change |
|---|---|
| `cmd/tenant/service.go` (new) | `cmdService` dispatch; `serviceSpec`; `newServiceSpec(c *commonFlags, system bool)`; `runCmd` seam; `waitDrained(ctx, base, token, timeout)`; `--print` |
| `cmd/tenant/service_darwin.go`, `service_linux.go`, `service_windows.go` (new) | Per OS: `unitText(spec) string` (a `text/template` constant), `install`, `uninstall`, `start`, `stop`, `restart`, `installed() (bool, string)` |
| `cmd/tenant/svc_windows.go` (new) | The SCM entry point: `svc.IsWindowsService()` at the top of `main`, `svc.Run("Tenant", handler)`; the handler runs `cmdServe` and maps Stop/Shutdown/PreShutdown to the same cancel SIGTERM uses (details in PR E) |
| `cmd/tenant/cliregistry.go` | Register `service` in section `run`, `Sub: install, uninstall, status, start, stop, restart`, `Flags: --print, --system, --now` |
| `cmd/tenant/serve.go` | Two flags the units pass: `--stop-budget <duration>` (default 5s) and, Windows only, `--hidden` (hide the console window; PR E says why it must be hidden, not freed) |
| `cmd/tenant/setup.go` | After setup succeeds and stdin is a terminal: `Run Tenant in the background when you log in? [Y/n]` → `install` |
| `cmd/tenant/doctor.go` | `checkService`: installed? running? do the unit's `--config`/`--data` match the current dirs? `--fix` rewrites the unit when they drift |
| `cmd/tenant/agents_dashctl.go` | `RestartNote` → "Run `tenant service restart`" (or the Services app for a `--system` install) |
| `internal/dashboard/rest.go` | `Version string \`json:"version"\`` in the status struct, from `version.Version` |
| `internal/plugins/osys/classify.go` | One destructive pattern: `^tenant\s+(service|update)\b`, so an agent-run `os_exec` of these needs approval |
| `deploy/launchd/com.tenant.serve.plist` | Deleted |
| `scripts/migrate-to-server.ps1` | The `schtasks` block becomes `tenant service install --system` |
| `docs/serve-runbook.md` | "Run as a service" per OS |

### Spec

```go
type serviceSpec struct {
    Exe, CfgDir, DataDir, LogDir string // absolute; Exe = os.Executable() with symlinks resolved
    System bool                         // Windows only: SCM service as SYSTEM. Elsewhere: "not supported"
    StopBudget time.Duration            // written into the unit as --stop-budget (table in PR E)
    Label  string                       // "com.tenant.serve" | "tenant" | `\Tenant\Serve` or "Tenant"; tests set another
}
```

`ponytail:` one fixed label per user. Add `--label` when someone runs two Tenants under one account.

### Per OS

| | macOS | Linux | Windows, user mode | Windows `--system` |
|---|---|---|---|---|
| Unit | LaunchAgent plist (same keys as the old template plus `ExitTimeOut 100`) | systemd user unit: `Restart=on-failure`, `RestartSec=10`, `TimeoutStopSec=100`, `WantedBy=default.target` | Scheduled task `\Tenant\Serve`, interactive token, `--hidden` | SCM service `Tenant`, `start= auto`, `sc failure … restart/10000/restart/60000/restart/300000` |
| Path | `~/Library/LaunchAgents/<label>.plist` | `~/.config/systemd/user/tenant.service` | Task Scheduler | Services |
| install | write → `launchctl bootstrap gui/<uid> <plist>` → `launchctl kickstart -k gui/<uid>/<label>` | write → `systemctl --user daemon-reload` → `enable --now tenant` → linger check | write the task's XML (a logon trigger for the installing account's SID, least privilege, no time limit, no battery conditions; action `<exe> serve --config … --data … --stop-budget 4s --hidden`) → `schtasks /Create /F /TN \Tenant\Serve /XML <file>` → `/Run` | `sc create Tenant binPath= "…serve … --stop-budget 120s" start= auto` → `sc description` → `sc failure` → `sc start` (needs an elevated terminal; detected and said) |
| uninstall | `launchctl bootout` → remove plist | `disable --now` → remove unit → `daemon-reload` | `schtasks /End` → `/Delete /F` | `sc stop` (wait) → `sc delete` |
| start / stop | `kickstart` / `bootout` + `bootstrap` | `systemctl --user start|stop` | `/Run` / `/End` | `sc start` / `sc stop` |
| restart | `kickstart -k` (SIGTERM) | `systemctl --user restart` (SIGTERM) | `/End` then `/Run` (hard kill) | `sc stop` + wait STOPPED + `sc start` (SCM Stop control, graceful) |
| installed? | `launchctl print gui/<uid>/<label>` exit 0 | unit file exists | `schtasks /Query` exit 0 | `sc query Tenant` exit 0 |
| Logs | `~/Library/Logs/tenant/serve.log`, `serve.err.log` | `journalctl --user -u tenant` | none for stdout (the log book holds everything; panics land in the data dir's crash files) | same |

Details that matter:
- **Running** is decided the same way everywhere: `GET /api/status` through `attachFetchStatus`
  (`cmd/tenant/attach.go:217`; token from `--token`, else `dashboard.auth`, else the generated config-dir token).
- **Restart drains first:** `waitDrained` polls `/api/status` until `turn_active` is false, default 15 minutes (a cron
  run's ceiling), then exits non-zero with "still busy; use --now". An unreachable hub skips the wait.
- **Windows user mode keeps its console.** Logoff and shutdown reach a Windows process as console control events,
  which Go's runtime turns into SIGTERM (`runtime/os_windows.go:1020`). A process without a console gets nothing
  and is simply killed. So `--hidden` hides the console window (`ShowWindow(GetConsoleWindow(), SW_HIDE)` through
  `syscall.NewLazyDLL`, ~8 lines) instead of freeing it. The window shows for a moment at logon; the live test checks
  that this is tolerable. If not, the alternative is a no-password S4U task (`schtasks /NP`), which runs in session 0
  with no window but does not work for Microsoft-account logons; that is a later decision, not a blocker.
  **Corrected 2026-11-17:** the console was not enough. Calling `ShowWindow` loads `user32.dll`, and Windows
  sends a process that has it loaded no `CTRL_LOGOFF_EVENT` or `CTRL_SHUTDOWN_EVENT`: it sends
  `WM_QUERYENDSESSION` and `WM_ENDSESSION` to the process's top-level windows instead. On Windows Server 2025
  every restart and logoff ended the task's serve with no shutdown step. serve now owns a window that is never
  shown and starts the stop on `WM_ENDSESSION` (`cmd/tenant/sessionend_windows.go`).
- **The Windows task is registered from XML, not with `/SC ONLOGON`** (changed 2026-11-13, after the first run on
  an account that is not an administrator). `/SC ONLOGON` makes a trigger for any user's logon, which only an
  administrator may register: everyone else got "Access is denied". It also left schtasks' defaults in place: the
  task ended after 72 hours, did not start on battery power and ended when the machine went onto it. The XML names
  one account, by SID, in the trigger and the principal ("run only when user is logged on", no password), sets no
  time limit and no battery conditions, and has no `/TR`, so the 261-character limit on the command line is gone. A
  `.cmd` wrapper was never an option: `/End` would kill `cmd.exe` and orphan `tenant.exe`.
- **A restarted serve waits for the dashboard's address** (same date). `restart` on the task is `/End` then `/Run`,
  and the new process started about 200 ms later, while the old one still held the port. It then ran without a
  dashboard, and `tenant update` could not have seen it come up. At start-up serve now waits up to 10 seconds for
  an address that is in use (`dashboardManager.EnableAtBoot`); any other bind error is reported at once.
  That is D1. The `--system` service keeps `runas.go`'s consequences (browsing off under a service account).
- **macOS TCC:** refuse `install` when `Exe`, `CfgDir` or `DataDir` sits under `~/Desktop`, `~/Documents` or
  `~/Downloads`, naming the path and a good home.
- **Linux linger:** after `enable --now`, read `loginctl show-user <user> -p Linger --value`; when `no`, print
  `loginctl enable-linger <user>`. Without linger the service stops at logout.
- **`--system` on macOS/Linux** errors with "not supported yet". A LaunchDaemon or a system unit is a later ticket.

### Tests (PR A)

| Test | Where | What fails if the logic breaks |
|---|---|---|
| `TestServiceSpec` | `service_test.go` | Paths absolute, `~` expanded, symlinked `Exe` resolved, label and stop budget per OS/mode |
| `TestServiceRefusesTCCDirs` | `service_test.go` | The pure check with an injected home: Desktop/Documents/Downloads refused, Library allowed |
| `TestWaitDrained` | `service_test.go` | `httptest` `/api/status` returns `turn_active: true` twice then `false`; restart's `runCmd` fires only after; `--now` skips; timeout returns "still busy" |
| `TestUnitText` | per-OS `_test.go` | Rendered unit equals a golden string for a fixture with spaces in paths (plist: byte-equal to the old template for the same values, plus `ExitTimeOut`) |
| `TestInstallArgv` / `TestUninstallArgv` / `TestRestartArgv` | per-OS `_test.go` | The recorder's argv list, in order, equals the table above, for user mode and `--system` on Windows |
| `TestInstallLongAndReservedPaths` / `TestWriteTaskXML` | `service_windows_test.go` | A command line past the old 261-character limit installs; `&` and `<` in a path are escaped and read back as written; the definition is written as UTF-16 with a byte-order mark and removed |
| `TestSvcHandler` | `svc_windows_test.go` | Fake control channel: `Stop`, `Shutdown` and `PreShutdown` each cancel serve's context once; status goes Running → StopPending → Stopped; the accepted set includes `AcceptPreShutdown` |
| `TestClassifyTenantService` | `osys/classify_test.go` | `tenant service uninstall` and `tenant update --yes` classify as destructive |
| `TestServiceLive` | per-OS `_test.go`, gated `TENANT_LIVE_SERVICE=1` | Real install with a test label pointing at `tenant version`, then `installed()` true, `uninstall`, `installed()` false. On Windows both modes (`--system` from an elevated terminal). By hand on a Mac and a Windows box |

**Done when:** on a Mac, a Linux desktop and a Windows machine, `tenant setup` ends with a running service that
survives a reboot (Linux: after `enable-linger`); `doctor` reports it; `restart` waits for a turn to finish; on the
Windows box, `--system` shows up in the Services app and `sc stop` ends with a clean boot record.

---

## PR E: stop and resume safety (D8)

What an OS stop looks like to Tenant, and how long it gets:

| Supervisor | Signal | OS grace before a kill | `--stop-budget` the unit passes |
|---|---|---|---|
| launchd (macOS) | SIGTERM | `ExitTimeOut`, default system-defined (man launchd.plist); the unit sets 100 s, and the OS may still cut a shutdown short | 90s |
| systemd user unit (Linux) | SIGTERM | `TimeoutStopSec` 100 s inside the user manager, which the system itself stops with its own ~90–120 s limit | 90s |
| Scheduled task (Windows, user) | console CTRL_LOGOFF/SHUTDOWN → SIGTERM via the Go runtime | a few seconds (Windows' hung-app timeouts); the Task Scheduler service may end the task sooner | 4s |
| SCM service (Windows, `--system`) | `SERVICE_CONTROL_PRESHUTDOWN` (accepted) then `STOP` | pre-shutdown default 180 s per service; a plain `sc stop` has no limit | 120s |
| Hand-run `tenant serve` / Ctrl-C | SIGINT/SIGTERM | none (the human is waiting) | 5s default |

The design has two speeds: **the first 4 seconds make the data safe on every OS**; a longer budget only buys a
tidier end to the turn in flight.

### The stop path (serve, on SIGTERM / SCM stop)

1. **Stop intake, now.** Already there (TEN-194): the turn gate closes, cron and the improve scheduler start nothing
   new, pending approvals are denied.
2. **Cut the in-flight turn, don't finish it.** The model call is cancelled at once (a half answer has no value). An
   in-flight *tool call* gets until `budget − 3s` to return, then its context is cancelled, which already kills the
   child process group on Unix (`internal/sandbox/host_unix.go:16`) and the job object on Windows
   (`cmd/tenant/childprocs_windows.go`). A `stopping` value on the context makes the agent **skip the 30-second
   salvage synthesis** (`agent.go:850`): that path exists for research wave timeouts, not for shutdowns.
   *As built (TEN-499): the tool-call grace is dropped and the whole turn is cancelled the moment the stop lands;
   a tool's own atomicity is the guarantee (the crash-safe `os_write_file` below, children dying with the process
   group or job object), and `stopping` is a process flag the agent and relays read, not a context value.*
3. **Record what was cut, durably.** The turn's messages are already in the T5 archive (working memory appends to the
   archive on every message, `internal/memory/working`). Add one archive event `turn_interrupted` with the reason
   (`os stop`), and write `<data>/interrupted.json` atomically (temp + rename, the `config.Persist` pattern):
   `[{surface, channel, inbound_id, excerpt, tools_run, at}]`. Cron marks its cut run `interrupted: Tenant stopped`
   instead of `context canceled` (`internal/cron/engine.go:535`, one line).
4. **Tell the user, best effort, 2 s.** The relay sends on a short background context (its `deliver` currently
   returns silently once the context is cancelled, `cmd/tenant/discordrelay.go:336`): "I was stopped mid-task by a
   system shutdown or restart. Say `continue` when I'm back." Same for iMessage.
5. **Close in order, under a deadline.** `life.Shutdown()` runs under `context.WithTimeout(budget)`; today it has no
   deadline (`internal/lifecycle/lifecycle.go:106`), so one hung stop step could eat the whole grace period. Stores
   close last-opened-first as now; the log book flushes; the boot record is marked clean (TEN-373). If the budget
   runs out first, the boot record stays unclean, which is the truth, and the next boot says so as it does today.
6. Exit 0.

### Boot-time resume

- **Unclean boot record:** already reported ("An earlier Tenant ended without a clean shutdown…",
  `cmd/tenant/system_events.go:142`). Unchanged.
- **`interrupted.json` present:** post to each listed channel once: "I was stopped at <time> while <excerpt>
  (<n> tool calls had run). Tell me to continue." Then remove the file. **No automatic re-run:** the tools that ran
  before the cut may have had side effects (an email sent, a file edited), and running the turn again would repeat
  them. The archive holds the partial trace, so `continue` has the context.
- **Messages that arrived while Tenant was down.** iMessage already catches up through its persisted ROWID cursor
  (`internal/plugins/imessage/watch.go:39`). Discord does not: a session that re-identifies after a reboot has
  missed every DM sent meanwhile (`internal/plugins/discord/events.go:20`). Add a per-channel last-seen message id
  (`<data>/discord-cursor.json`, written atomically after each processed message) and, on READY, `ReadChannel(ch, 20)`
  (exists, newest first) for each known channel, feeding the messages newer than the cursor through the normal
  inbound path in order. Snowflake ids are time-ordered, so "newer" is an integer compare.
- **`/goal` loops and cron runs** are marked interrupted; neither resumes by itself, for the same side-effect reason.
  The next cron run is the next scheduled one.

### Durability audit (what a kill at any instant does today)

| Write | Today | Verdict |
|---|---|---|
| SQLite stores (episodic, semantic, usage, runtrace, log book) | WAL, `synchronous(NORMAL)`, busy timeout | Committed transactions survive a process kill; an OS shutdown flushes the page cache, so NORMAL is enough for D8 (power loss is out of scope) |
| T5 archive (JSONL) | fsync per event by default (`archive.go:119`) | Safe |
| Working memory (T1) | In memory, mirrored to T5 on every append | Safe; rebuilt from T5 |
| `config.json`, settings | temp + flush + rename (`internal/config/persist.go`) | Safe. The flush was added on 2026-11-17: without it a power cut on Windows Server 2025 left `config.json` as zero bytes, and serve would not start. Power loss stays out of scope for D8, but a config that stops the next start is worth the flush |
| Backups | temp `.partial` + rename (`internal/backup/create.go:159`) | Safe |
| Boot record / crash files | written at start, marked at clean stop | Safe by design: an unclean stop is detected, not hidden |
| **`os_write_file` / `os_edit_file`** | `Truncate(0)` then `WriteAt` in place (`internal/plugins/osys/osys.go:182`) | **Not safe:** a kill between the truncate and the write leaves the operator's file empty or partial. Fix: write to a temp file in the same directory, preserve the original mode, `os.Rename` over the target, with the same `vet` applied to the destination path so `Roots.Protect` and the symlink guards keep their meaning. `os_append_file` stays as is (`O_APPEND` writes are atomic per call) |
| Tool child processes | Killed with Tenant on both Unix (process group) and Windows (job object) | Safe; no orphans continue after a stop |

### Files (PR E)

| File | Change |
|---|---|
| `cmd/tenant/serve.go` | `--stop-budget`; the teardown under a deadline; the `stopping` context value; `interrupted.json` writer and boot-time reader |
| `internal/lifecycle/lifecycle.go` | `Shutdown` takes a context; each step logs "timed out" when the deadline passes |
| `internal/agent/agent.go` | Skip the salvage synthesis when the context carries `stopping`; emit `turn_interrupted` |
| `cmd/tenant/discordrelay.go`, `imessagerelay.go` | The best-effort notice on cancel; the boot-time notice; Discord cursor + catch-up |
| `internal/cron/engine.go` | The `interrupted` label |
| `internal/plugins/osys/osys.go` | Atomic `WriteFile`/`EditFile` |
| `cmd/tenant/svc_windows.go` (from PR A) | The handler reports `StopPending` with a wait hint of the budget, cancels, waits for serve to return, reports `Stopped` |
| `docs/serve-runbook.md` | "What happens on an OS restart" and the reboot drill |

### Tests (PR E)

| Test | Where | What fails if the logic breaks |
|---|---|---|
| `TestShutdownDeadline` | `internal/lifecycle` | A step that blocks forever under a 200 ms budget: `Shutdown` returns within ~250 ms, that step is logged as timed out, later steps still run |
| `TestStoppingSkipsSalvage` | `internal/agent` | Cancel a turn with tool trace and the `stopping` value: the fake planner records zero salvage calls, `TurnResult.Error` is the cancel, the archive has `turn_interrupted` |
| `TestToolCallGetsBudget` | `internal/agent` | A fake tool that returns after 1 s under a 4 s budget completes; under a 500 ms budget it is cancelled |
| `TestInterruptedNotice` | `cmd/tenant` | Stop mid-turn: `interrupted.json` written atomically with channel and inbound id, and the relay's fake client received one notice; next start posts the boot notice once and removes the file; a second start posts nothing |
| `TestDiscordCatchUp` | `cmd/tenant` | Cursor persisted after each message; a fake `ReadChannel` returns three messages, one older than the cursor: two are processed, in order, and a second READY processes none |
| `TestCronInterruptedLabel` | `internal/cron` | Stop mid-run: the history entry reads `interrupted: Tenant stopped` |
| `TestWriteFileAtomic` | `internal/plugins/osys` | A failure injected between the temp write and the rename leaves the original intact; success replaces the content and keeps the mode; a protected path is still refused |
| `TestSvcHandlerStopBudget` | `svc_windows_test.go` | `PreShutdown` → `StopPending` carries the budget as its wait hint; serve returning late still ends in `Stopped` |
| **Reboot drill** (by hand, per OS, in the release checklist) | `docs/serve-runbook.md` | Install the service; start a long turn (`os_exec sleep 120` from Discord); reboot the machine. After boot: the boot record is clean, the channel shows the interrupted notice, a DM sent during the reboot was answered after boot, `tenant doctor` is clean. Run on macOS, Linux, Windows user mode and Windows `--system` |

**Done when:** the reboot drill passes on all four configurations, and `go test ./...` stays green on the 3-OS matrix.

---

## PR B: `tenant update`

```
tenant update --check                       # one line: current vs latest; exit 0 up to date, 10 update available
tenant update [--version vX.Y.Z] [--yes] [--now] [--force]
tenant update --rollback                    # put tenant.prev back; print the data-restore command
```

### Files

| File | Change |
|---|---|
| `internal/version/version.go`, `.goreleaser.yaml` | `Repo` var; `-X tenant/internal/version.Repo={{ envOrDefault "GITHUB_REPOSITORY" "" }}` |
| `internal/selfupdate/release.go` (new) | `Latest(ctx, client, repo, token)`, `ByTag(...)` → `Release{Tag; Assets map[name]url}`; `AssetName(version, goos, goarch)` mirrors the GoReleaser `name_template` |
| `internal/selfupdate/verify.go` (new) | `ParseChecksums`, `SHA256`, `CosignVerify(ctx, run, …)` |
| `internal/selfupdate/extract.go` (new) | `ExtractBinary(archive, dest, member)` for tar.gz and zip: only `tenant`, `..` and absolute names refused, mode 0755 |
| `internal/selfupdate/swap.go` (new) | `Swap(exe, newFile)` and `Rollback(exe)`, interruption-safe (below) |
| `internal/selfupdate/state.go` (new) | `update-state.json` marker: `{from, to, step, started}` |
| `cmd/tenant/update.go` (new) | `cmdUpdate`: the flow below; `--check`; the daily check job; config `update_check` |
| `internal/config/config.go` | `UpdateCheck *bool \`json:"update_check,omitempty"\`` (nil = on), same shape as `NeverCloud` |
| `internal/logs/catalog/…` | Events in the Tenant group: `UpdateAvailable`, `UpdateInterrupted` |
| `cmd/tenant/commands.go` (serve) | `life.StartFunc` goroutine: first check 5 min after boot, then every 24 h with jitter; one log-book event when a newer release appears; at boot, report a leftover `update-state.json` |
| `cmd/tenant/doctor.go` | `checkUpdate`: INFO "update available"; WARN "update to Y was interrupted at <step>, running Z" |
| `cmd/tenant/cliregistry.go` | Register `update` in section `start`, `Flags: --check, --version, --yes, --now, --force, --rollback` |

### Flow

1. **Refuse what isn't ours.** `Version == "0.0.1"` or `Dirty == "true"` or `Repo == ""` → exit 2 "development
   build; use a release build or `--force`". A leftover `update-state.json` is finished first (step 11).
2. **Resolve the release.** `GET https://api.github.com/repos/<Repo>/releases/latest` (or `/releases/tags/<v>`).
   `GITHUB_TOKEN` in the environment is sent as a bearer when present (private-repo dogfooding). `tag_name` minus
   its `v` is compared with `version.Version`; equal → "up to date" (exit 0; `--force` reinstalls). GitHub's `latest`
   excludes prereleases, so an rc is reached only by `--version`.
3. **Download** the archive for `runtime.GOOS/GOARCH` and `checksums.txt` into `<dir of exe>/.tenant-update/`
   (same filesystem, so renames are atomic; the directory is wiped at the start of every run, which cleans up after
   an interrupted one). Assets are fetched by API URL with `Accept: application/octet-stream`, which works for
   public and private repos.
4. **Verify.** SHA-256 against `checksums.txt`; mismatch → exit 1, nothing touched. If `cosign` is on PATH and the
   release carries `checksums.txt.sigstore.json`: `cosign verify-blob --bundle … --certificate-identity-regexp
   '^https://github.com/<Repo>/\.github/workflows/release\.yml@' --certificate-oidc-issuer
   https://token.actions.githubusercontent.com checksums.txt`, failing closed. Without cosign, say so.
   `ponytail:` checksum-only trusts GitHub's TLS for authenticity; in-process bundle verification needs sigstore-go, a new
   module. Revisit if the operator base outgrows "cosign on PATH when it matters".
5. **Smoke-test before touching anything:** run `<new> version`, require exit 0 and the tag's version in the output.
6. **Confirm** unless `--yes`: "Update 0.2.0 → 0.3.0? A backup goes to <path>. [y/N]".
7. **Back up data** with `backup.Create` (`internal/backup/create.go:78`), unencrypted (no prompt; credentials are
   left out, and migrations never touch them), to `<data>/backups/pre-update-<oldversion>.tenant-backup`, deleting
   older `pre-update-*` files. The write is temp + rename, so an OS stop here leaves either no backup or a complete
   one, never a torn one. `ponytail:` one rollback point; keep N when someone needs to go back two versions.
8. **Write the marker** `update-state.json` `{from, to, step: "swap"}` (atomic), then **swap**:
   - Unix: `os.Link(exe, exe.prev)` (after removing an old `.prev`), then `os.Rename(staged, exe)`. There is **no
     instant at which `tenant` is missing**: the hard link keeps the old binary under both names until the rename
     replaces the entry atomically. On a filesystem without hard links, fall back to the two renames.
   - Windows: `rename(tenant.exe → tenant.prev.exe)`, `rename(tenant.new.exe → tenant.exe)`. A running image may
     be renamed but not replaced, so the two-step is unavoidable; the gap is two consecutive syscalls. If an OS stop
     lands exactly there, the service fails to start on boot and the runbook's one line fixes it
     (`ren tenant.new.exe tenant.exe`, or re-run `install.ps1`). An undeletable old `.prev` (the previous version is
     still running because Tenant was never restarted) → exit 2 "restart Tenant, then retry".
9. **Restart:** if a service is installed, `service restart` (drain-aware; `--now` passes through). Otherwise print
   "restart Tenant to finish the update". Interactive `tenant tui` sessions keep running the old binary until they exit.
10. **Confirm:** when a hub was reachable before the swap, poll `/api/status` for up to 90 s until `version` equals
    the new one. On timeout or a non-matching version: `Rollback(exe)`, `service restart`, exit 1 with
    "rolled the binary back; if the new version migrated data, restore with:
    `tenant service stop && tenant backup restore <path> --yes && tenant service start`". Data is never restored
    automatically. Marker removed on success.
11. **An OS stop at any step:** steps 1–7 leave only staging files and a complete-or-absent backup; the next run wipes
    staging. A stop after the swap leaves the marker: serve logs `UpdateInterrupted` at boot, `doctor` warns, and the
    next `tenant update` finishes the job: running version equals `to` → clear the marker and say so; otherwise offer
    `--rollback`. `backup restore` is untouched by all this: it already takes the exclusive data lock and swaps
    directories.

`--rollback` runs steps 8 to 10 in reverse from `tenant.prev` and prints the same restore command.

**Update awareness, not auto-update (D2):** serve's daily job and `doctor` share `selfupdate.Latest`; one request a
day stays inside GitHub's 60/hour limit. The log-book event is the only output; the existing alert rules can turn it
into a Discord message. `"update_check": false` is the only new config field. Nothing is applied from cron, from
serve, or by an agent: PR A's classifier row makes an agent-run `tenant update` an approval, and `update` without a
terminal needs `--yes`.

### Tests (PR B)

| Test | Where | What fails if the logic breaks |
|---|---|---|
| `TestAssetName` | `selfupdate` | Six GOOS/GOARCH pairs produce the exact GoReleaser names (`x86_64`, `aarch64`, `.zip` on Windows) |
| `TestParseChecksums` | `selfupdate` | GoReleaser's `<hash>  <name>` lines, names with spaces, blank lines |
| `TestExtractBinary` | `selfupdate` | tar.gz and zip built in the test: only `tenant` extracted, mode 0755 on Unix, `../x` and absolute members refused |
| `TestSwapNoGap` | `selfupdate` (unix) | After `Link`, both names resolve to the old binary; a failure injected before the rename leaves `tenant` runnable and old; after the rename `tenant` is new and `.prev` old; rollback restores |
| `TestSwapWindows` | `selfupdate` (windows) | Two-rename path; an old `.prev` held open gives the "restart first" error; the recovery message names `tenant.new.exe` |
| `TestUpdateFlow` | `cmd/tenant/update_test.go` | One `httptest` server plays GitHub and the hub; the archive holds a fake `tenant` built once per test binary with `go build` from `testdata/fakebin/main.go`; `runCmd` records the restart. Subtests: happy path (swapped, `.prev`, backup written, marker gone, restart recorded); checksum mismatch; smoke failure; health mismatch (`.prev` restored, restore command printed); dev-build refusal; `--check` exit 10; `--version` pins a tag; `GITHUB_TOKEN` becomes a bearer |
| `TestUpdateInterruptedMarker` | `cmd/tenant` | A marker with `to` equal to the running version: the next run clears it and says so; with another `to`: it offers rollback; serve boot with a marker logs `UpdateInterrupted`; doctor warns |
| `TestCosignVerifyArgv` | `selfupdate` | Exact `cosign verify-blob` argv; non-zero exit fails closed; no `cosign` → the checksum-only note |
| `TestServeUpdateCheck` | `cmd/tenant` | A newer tag writes one `UpdateAvailable` event; the same tag none; `update_check: false` makes no request; a connection error makes no event |
| `TestUpdateLive` | gated `TENANT_LIVE_UPDATE=1` + `GITHUB_TOKEN` | Against the real private releases: resolve, download this host's asset, verify, extract, run `version`. No swap |

**Done when:** on the private repo, `tenant update` moves a Mac and a Windows install from `v0.1.0` to the next tag
with a drain, a backup and a health check, `--rollback` returns them, a reboot forced between the backup and the
health check ends with the marker reported and then cleared, and `TestUpdateFlow` is green on all three CI OSes.

---

## PR C: install scripts and release plumbing

### Files

| File | Change |
|---|---|
| `scripts/install.sh` (new, POSIX sh, ~80 lines) | OS/arch from `uname` (`arm64` → `aarch64`); version from `TENANT_VERSION` or the releases API (`sed`, no jq); download from `${TENANT_BASE_URL:-https://github.com/__REPO__/releases/download/v$VERSION}`; verify with `sha256sum` or `shasum -a 256`; extract `tenant` to `${TENANT_INSTALL_DIR:-$HOME/.local/bin}`, no sudo; PATH hint; "Next: tenant setup" |
| `scripts/install.ps1` (new, ~70 lines) | Same steps with `Invoke-RestMethod`, `Invoke-WebRequest`, `Get-FileHash`, `Expand-Archive`; install to `$env:LOCALAPPDATA\Programs\tenant`; add it to the **user** PATH when missing |
| `.github/workflows/release.yml` | Before GoReleaser: copy both scripts to `dist-scripts/` with `__REPO__` replaced by `$GITHUB_REPOSITORY` (one `sed`), so the tracked scripts never hard-code the repo |
| `.goreleaser.yaml` | `release.extra_files` for the stamped scripts; `release.header` with the one-liners; the `Repo` ldflag |
| `.github/workflows/packaging.yml` (new) | Only when `.goreleaser.yaml`, `scripts/install.*`, `release.yml` or itself change, on the 3-OS matrix: `goreleaser check`; `goreleaser release --snapshot --clean --skip=publish,sign,sbom`; `shellcheck -s sh` (Linux); a PowerShell parse check (Windows); each script run against the snapshot served by `python3 -m http.server` with `TENANT_BASE_URL`/`TENANT_VERSION`, then `tenant version` |
| `README.md`, `docs/QUICKSTART.md` §2 | The one-liners |

```bash
curl -fsSL https://github.com/<owner>/<repo>/releases/latest/download/install.sh | sh
```

```powershell
irm https://github.com/<owner>/<repo>/releases/latest/download/install.ps1 | iex
```

No Gatekeeper or SmartScreen prompt without signing (D4/D5): files fetched by `curl`, PowerShell or Go's `net/http`
carry no `com.apple.quarantine` attribute and no `Zone.Identifier` stream; only browser downloads do. Go's linker
ad-hoc signs darwin/arm64 binaries, which Apple silicon requires. PR D's smoke job asserts both.

### Tests (PR C)

- The packaging workflow is the test; it runs only when packaging files change.
- Locally before the push: `goreleaser check`, a snapshot build, `shellcheck -s sh scripts/install.sh`, and the
  script against `python3 -m http.server --directory dist`. `install.sh` must also pass `sh -n` under `dash` in the
  Docker Linux mirror, since macOS `sh` is bash.

**Done when:** the packaging job is green on all three OSes and a snapshot install through each script yields a
working `tenant version`.

---

## PR D: docs and the release smoke job

### Files

| File | Change |
|---|---|
| `docs/QUICKSTART.md` | §2 becomes the one-liners plus `tenant setup`; short "Run it in the background" and "Update" sections |
| `docs/serve-runbook.md` | "Run as a service" per OS, "Upgrade and roll back" (forward-only schema, the three-command restore, the Windows swap-gap fix), "What happens on an OS restart", the reboot drill |
| `README.md` | Install block |
| `CHANGELOG.md` | Entries for A–E |
| `docs/migration/deploy-and-rollback.md` | Leave for the TEN-448 doc cleanup; link the new runbook section from it |
| `.github/workflows/release.yml` | `smoke` job, `needs: release`, 3-OS matrix |

### The smoke job (once per tag, not per PR)

1. Run the one-liner against the release just published (`TENANT_VERSION=${GITHUB_REF_NAME#v}`).
2. `tenant version` must contain the tag's version.
3. No quarantine: on macOS `xattr -p com.apple.quarantine <bin>` must fail; on Windows
   `Get-Item <bin> -Stream Zone.Identifier` must fail.
4. `tenant service install --print` renders on every OS. Then a real `install` → `status` → `uninstall` on Windows
   (user mode; `sc` needs elevation the runner has, so `--system` too) and macOS (`launchctl bootstrap gui/…`
   should work in the runner's logged-in session; **verify on the rc tag**, keep it only if green). On Linux the
   runner has no user session bus, so run `systemd-analyze --user verify <unit>` on the printed unit (**verify on
   the rc tag** that it works without a running manager; otherwise `--print` only).
5. Install cosign and run `tenant update --version <tag> --force --yes`: a reinstall of the same version, which
   exercises download, checksum, the cosign path, the smoke test, the marker and the swap on a real release.

**Done when:** the smoke job is green on all three OSes for the TEN-449 rc tag, and QUICKSTART's install section
fits on one screen.

---

## Order, dependencies, tickets

| PR | Depends on | Lands while private? | Size |
|---|---|---|---|
| A `tenant service` (+ Windows SCM, hidden console) | — | Yes | ~550 lines Go + ~350 tests, 3 deletions |
| E stop and resume safety | A (budgets, SCM handler) | Yes | ~400 lines Go + ~350 tests |
| B `tenant update` | A (restart path), E (marker reporting at boot) | Yes; dogfood with `GITHUB_TOKEN` | ~550 lines Go + ~450 tests |
| C scripts + plumbing | B (`Repo` ldflag) | Config and the packaging job, yes; assets arrive with the first tag after merge | ~150 lines shell/PowerShell + ~60 YAML |
| D docs + smoke | C | Docs yes; the smoke job first runs on the TEN-449 rc tag | docs + ~50 YAML |

Each PR: branch off main, local CI mirror, one push, merge commit, branch kept. Docfacts regenerate if
`docs/ARCHITECTURE.md` changes (the service command, the stop path and the update job are architecture facts).

Jira, when the operator says go: five Tasks under TEN-495 (one per PR), and TEN-313 (Windows supervision)
re-parented under TEN-495 and closed by PR A.

## Risks

| Risk | Mitigation |
|---|---|
| A bad release bricks a 24/7 install | `<new> version` before the swap; health check after; automatic `.prev` swap-back; pre-update backup |
| Rollback fails because the new version migrated a database | The log book refuses a newer schema loudly; the backup plus the printed three-command restore |
| An OS stop lands mid-turn | The 4-second safe path: cut, record, notify, flush; archive already holds every message; boot-time notice; no silent loss and no duplicate side effects |
| An OS stop lands mid-upgrade | Staging wiped on the next run; atomic backup; no-gap swap on Unix; two-syscall gap on Windows with a one-line fix; the marker makes an unfinished update visible at boot and finishes it next run |
| A hung component eats the grace period | The teardown runs under the budget's deadline; the boot record stays honest |
| The operator's files torn by a kill | `os_write_file`/`os_edit_file` become temp + rename |
| Windows gives seconds, not minutes | The 4 s budget for the user task; the SCM service with pre-shutdown for `--system` |
| Discord DMs sent during a reboot vanish | Per-channel cursor and catch-up on READY (iMessage already has this) |
| Supply chain through the update path | HTTPS + SHA-256 always; cosign when on PATH; never automatic; agent-run `tenant update` needs approval |
| Windows task needs an administrator, or is ended by schtasks' defaults | Registered from XML for one account; `TestServiceLive` passes from a terminal that is not elevated |
| Linux service dies at logout | Linger checked; the command printed |
| Repo rename at the public cut | `Repo` is an ldflag from `GITHUB_REPOSITORY`; scripts are stamped at release time |
| Runner support for a real service install | Tried on the rc tag; falls back to `--print` + unit verification |
| CI spend | Packaging job is paths-filtered; smoke job runs per tag; live tests and the reboot drill run by hand |
