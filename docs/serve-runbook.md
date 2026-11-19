# Running Tenant 24/7 — `tenant serve` (TEN-194)

`tenant serve` runs the **full long-term brain headless**: the same agent, tools,
memory, self-improvement, cron jobs, federation peer listener, and comms relays the
interactive TUI drives — but with no terminal UI. You "hop in" through the **web
dashboard**, which is always on in serve mode and is the live control + chat +
approval surface.

```
tenant serve                      # loopback dashboard on 127.0.0.1:9770
tenant serve --dashboard-addr 0.0.0.0:9770   # LAN/tailnet — REQUIRES TLS + auth (see below)
tenant serve --self-improve=false # disable background distill/consolidate/profile
```

## What runs in serve mode

| Subsystem | Default | Notes |
|---|---|---|
| Agent + full tool surface | on | memory, skills, recall, sub-agent orchestration |
| Web dashboard | **on** | the only control surface — chat, tools, approvals, models, memory |
| Headless approval drain | on | gated "ask" actions wait on the dashboard's Approvals & safety page and at `/api/approvals` (no TUI to prompt) |
| Self-improvement scheduler | on | distill / skill-induction / consolidate / profile; `--self-improve=false` to disable |
| Cron jobs | on | recurring agent prompts; honors `cron.allow_exec` + timezone |
| Federation peer listener | if `peer.listen` set | paired peers reach this instance |
| Discord relay | if a token is configured **and** `relay.enabled` | drive the hub from your phone |
| Nightly eval / soul-nudge | config-gated, off by default | `improve.eval_every` / `improve.soul_nudge_every` |

While the model is **degraded to echo** (endpoint unreachable / misconfigured), all
autonomous work (cron, self-improve, relay turns) is suspended automatically — exactly
as in the TUI. Recover by fixing the endpoint/keys; the reconnect monitor restores it
without a restart.

## Concurrency

The shared brain is driven through a single **turn gate**: only one turn runs at a
time across all sources (the dashboard now, peer-delegated runs later), so unattended
traffic can never race the working set. `GET /api/status` reports `turn_active`,
`turn_age_secs`, and `pending_approvals` for liveness.

## Approvals (important for unattended runs)

There is no terminal to approve a dangerous action. A gated **ask**-mode call waits in
a queue: answer it on the dashboard's **Approvals & safety** page (it appears within a
second, with what the agent wants to do and why) or via REST. Requests raised in a
Discord conversation are answered with the buttons on the Discord card; the page only
says they are there. See [dashboard-runbook.md](dashboard-runbook.md) §4.

```
GET  /api/approvals                      # list pending: [{id, category, action, detail, age_secs}]
POST /api/approvals/<id>  {"decision":"approve"}          # or approve_session | approve_always | deny
```

The page offers Allow once and Deny, and "for the rest of this session" behind a
confirm step where that grant is safe to show. `approve_always` is REST-only: a
permanent change is a category's mode.

To run hands-off, set the categories you trust to **Allow** on the Approvals & safety
page, or at launch with the plugins' allow flags (`--os-allow-exec`, `--os-allow-write`,
`--gsuite-allow-send`, `--imessage-allow-send`, `--discord-allow-send`,
`--web-allow-interact`, …); a mode saved on the page wins over a flag. Anything left in
**ask** waits for a decision (fail-closed). Waiting never approves: a request is refused
when the job that asked ends (a scheduled job's run is capped at 15 minutes,
`cron.per_run_minutes`), and every pending one is denied on shutdown.

## Network exposure

The dashboard binds **loopback by default**. A non-loopback `--dashboard-addr` is
**refused at startup unless BOTH TLS and an auth token are set** (`dashboard.tls_cert`,
`dashboard.tls_key`, `dashboard.auth`). To reach it from elsewhere without managing
certs, prefer publishing the loopback dashboard onto your tailnet (`/tailscale serve`
from a TUI session, persisted) rather than binding a public interface.

## The dashboard token

Every dashboard request needs the bearer token in `<config>/dashboard-auth-token`
(created on first start), or `dashboard.auth` in config.json when you set one. Only
`GET /healthz` is open, for monitors; `HEAD /healthz` needs the token too.
`tenant serve` prints the token at start-up only when stdout is a terminal. Under
launchd or systemd, stdout goes to a file, so the start-up banner and the `system`
log line say where the token lives instead: `cat` that file to see it.
`tenant attach` and `tenant logs` read it themselves.
A browser presents the token once, on the sign-in form, and gets a random session id
in its cookie, never the token: the session lasts 7 days, ends on Log out, and every
session ends when serve restarts. Only the `Authorization: Bearer` header takes the
token itself; no path takes it from the URL (`/ws` included).

Builds before TEN-360 printed the token to stdout (launchd keeps it in `serve.log`,
which nothing rotates) and wrote it into `<data>/logs/system/*.log`. If you ran one,
rotate the token: stop serve, delete `<config>/dashboard-auth-token` (or change
`dashboard.auth`), and start serve again. Browsers then sign in with the new token.

**Windows service.** Under a service account `<config>` is that account's
`%AppData%\tenant`: `C:\Windows\System32\config\systemprofile\AppData\Roaming\tenant`
for SYSTEM, `C:\Windows\ServiceProfiles\LocalService\AppData\Roaming\tenant` for LOCAL
SERVICE, unless the service passes `--config`. An administrator reads the token from an
elevated PowerShell with `Get-Content -LiteralPath "<config>\dashboard-auth-token"` on
builds that share a service's secrets with Administrators (September 2026); older
builds lock administrators out of the token and config.json alike. The steps, and
checking the folder with `icacls`, are in [dashboard-runbook.md](dashboard-runbook.md)
§2. A service has no screen, so a remote service's or an integration's browser sign-in
is finished in a browser on the server itself (dashboard-runbook.md §7).

## What happens on an OS restart or shutdown

A stop (`SIGTERM` / `SIGINT`, the Windows service's Stop or pre-shutdown control, a logoff
for the Windows task) has two speeds (TEN-499):

1. **The first seconds make the data safe, on every OS.** The turn gate closes, cron and
   self-improvement start nothing new, pending approvals are denied, and every turn in
   flight is **cancelled at once**: the model call ends, a running tool's child process dies
   with the process group (Unix) or the job object (Windows), and there is no 30-second
   "salvage" answer. Nothing already produced is lost: every message is in the archive as it
   is written, the stores are WAL SQLite, and `os_write_file` / `os_edit_file` replace a file
   by rename, so a kill at any instant leaves the old file or the new one, never a torn one.
2. **The stop budget bounds the teardown.** `--stop-budget` (the unit passes the OS's grace:
   90 s on macOS and Linux, 4 s for the Windows task, 120 s for the Windows service, 5 s hand-run)
   is a deadline on the staged shutdown: stop intake (peer listener → Discord relay →
   dashboard), drain the cut turn and any cron/improve job, close the stores last. A step
   that outlives the budget is abandoned and the rest are skipped, both in the `system` log;
   serve then exits non-zero and the boot record stays unclean, which the next start reports
   as it does for a crash. The data is safe either way: the budget only decides how tidy the end is.

What the operator sees:

- **The interrupted notice.** A Discord or iMessage turn the stop cut gets, within 2 seconds,
  "I was stopped mid-task by a system shutdown or restart. Say `continue` when I'm back.";
  the cut is recorded in `<data>/interrupted.json` and as a `turn_interrupted` event in the
  archive. After boot, once the relays are up, each recorded channel gets one more line:
  "I was stopped at <time> while <excerpt> (<n> tool calls had run). Tell me to continue."
  Then the record is cleared. The turn is **never re-run by itself**: the tools that ran before
  the cut may have had side effects (an email sent, a file edited). Say `continue`: the archive
  holds the partial trace, so the agent picks up with the context. A cron run the stop cut is
  marked `interrupted: Tenant stopped` in its history; its next run is the next scheduled one.
- **Messages sent while Tenant was down.** iMessage catches up through its chat.db cursor.
  Discord keeps the last DM seen per channel in `<data>/discord-cursor.json` and, on every new
  gateway session (`READY`), reads each channel back and routes the DMs after the cursor,
  oldest first, as if they had just arrived (the `discord` log says how many). A DM's text is
  caught up; a file attached to it is not fetched.

### Reboot drill

Run it once per configuration before a release: macOS, Linux, Windows task, Windows `--system`.

1. `tenant service install`, then `tenant service status` says running.
2. From Discord (or iMessage), start a long turn: "run `sleep 120` and tell me when it's done"
   (`relay.allow_exec` on, or any turn that runs long enough).
3. While it runs, reboot the machine.
4. While it is down, send the bot one more DM.
5. After boot, check:
   - the channel shows the interrupted notice, then the boot-time "I was stopped at …" line;
   - the DM sent during the reboot was answered;
   - `tenant logs` (the `system` log) shows the shutdown's steps within the budget and a clean
     stop before it, and the start-up has no "ended without a clean shutdown" line;
   - `tenant doctor` is clean (the databases, the service, serve liveness).
6. Say `continue` and check the agent picks the task up with its context.

## Run as a service

`tenant service install` writes the unit for this OS, enables it and starts it (TEN-498);
`tenant setup` offers it at the end. The unit pins `--config` and `--data` to the dirs the
command ran with and passes `--stop-budget`, the grace the OS grants a stop.

```
tenant service install [--print]   # --print shows the unit and changes nothing
tenant service status              # installed? running (version, turn_active)? where the logs are
tenant service restart [--now]     # waits for the running turn to finish (15 min ceiling) unless --now
tenant service start | stop
tenant service uninstall
```

`tenant doctor` warns when no service is installed or when the unit no longer matches the
dirs or binary this command uses; `--fix` rewrites it.

`install`, `start` and `restart` first check process protection, at the terminal they are typed
in: when this system cannot apply one of the controls they say which and what that leaves open,
and ask `Continue without it? [y/N]`. The service has nobody to ask, so it starts only once
someone has said yes here (see "When a protection cannot be applied" below). Run from a script,
with no terminal, they stop with the reason instead. On macOS `install` also signs the binary
with the hardened runtime before it writes the unit (see "macOS: the hardened runtime").

Without `--system`, the verbs other than `install` act on your own logon task when you have one, and
on a `--system` service only when that is all this account has; `--system` names the service. So a
standard user on a machine where an administrator installed the service sees and removes their own
task, and `status` adds a line when both are installed.

`uninstall` and `stop` work when `config.json` does not load (they print the error as a warning):
a serve that refuses a broken config can still be stopped and removed. The other verbs stop at
the error, like every other command.

| | macOS | Linux | Windows | Windows `--system` |
|---|---|---|---|---|
| Unit | LaunchAgent `~/Library/LaunchAgents/com.tenant.serve.plist` | systemd user unit `~/.config/systemd/user/tenant.service` | Scheduled task `\Tenant\Serve` at logon, as you | Windows service `Tenant` as SYSTEM (elevated terminal; headless boxes) |
| Logs | `~/Library/Logs/tenant/serve.log`, `serve.err.log` | `journalctl --user -u tenant -f` | none for stdout | none for stdout |
| Stop budget | 90s | 90s | 4s | 120s |

Everything the hub does is in the log book either way (`tenant logs`, the dashboard's Logs page).

- **macOS (TCC):** `install` refuses a binary, config dir or data dir under `~/Desktop`, `~/Documents` or
  `~/Downloads`: macOS silently denies those to a background process. Good homes: `~/.local/bin`,
  `~/.config/tenant`, `~/Library/Application Support/tenant`.
- **Linux (linger):** the user manager stops at logout unless the account lingers; `install` prints
  `loginctl enable-linger <user>` when it is off. Run it once.
- **Windows:** installing the task needs no administrator: it is registered for your account alone,
  with no time limit and no battery conditions. Its console window is hidden. A logoff or a shutdown
  reaches serve through a window of its own that is never shown (Windows sends them as window
  messages, not as console events, to a process that has hidden its console window), and serve has
  about 4.5 seconds to finish before Windows ends it. `restart`, `stop` and `uninstall` (and so
  `tenant update`) ask serve to stop through the same window and wait for it, up to the 4-second
  stop budget and a 2-second margin; only then is the task ended, which is a hard kill and all that
  a serve from 0.2.0-rc.3 or earlier understands,
  and a serve that starts while the old one still holds the dashboard's address waits up to 10
  seconds for it. `--system` is a real service with
  a Stop control and `sc failure` restarts; `sc`/the Services app work on it too.

## Upgrade and roll back

`tenant update` moves an install to the latest release with a way back (TEN-500). It downloads the
release archive for this OS and architecture, verifies its SHA-256 (and its signature when `cosign`
is on PATH), on macOS signs the new binary with the hardened runtime, runs the new binary's
`version --protection` before touching anything, asks, then:

1. writes an unencrypted backup of the config and data dirs to `<data dir>-backups/pre-update-<old version>.tenant-backup`
   (credentials are left out; the previous pre-update backup is replaced);
2. swaps the binary in with no gap, keeping the old one as `tenant.prev`;
3. restarts the service (`tenant service restart`, which waits for the running turn unless `--now`);
4. polls `/api/status` for up to 90 s until it reports the new version; if it never does, the old binary
   goes back and the service is restarted again.

```
tenant update --check                 # one line, running vs latest; exit 10 when an update exists
tenant update [--version vX.Y.Z] [--yes] [--now] [--force]
tenant update --rollback              # put tenant.prev back; prints the data-restore command
```

**A new version asks again about a protection it cannot apply**, and the service it runs as has
nobody to ask. So the update asks, before it changes anything: that run of the new binary prints
what the new binary could not apply on this system, and each control nobody has acknowledged for
the new version is put to you with `Continue without it? [y/N]`. A yes is recorded for the new
version, and the restarted service starts on it. `--yes` answers the update's own question, not
this one. **An unattended update** (no terminal: cron, a script) that meets one changes nothing,
exits 2 and says why: `not updated; <running version> keeps running. On this system <new version>
cannot apply a process protection, …`. Run `tenant update` at a terminal once to acknowledge it.
`--rollback` does not ask: the previous version starts on the acknowledgement recorded when it
was the running one (the last 16 are kept).

**Schemas move forward only.** The log book refuses a database written by a newer version, so once
the new version has migrated a store, the old binary alone is not enough. Rollback is the old binary
plus the pre-update backup, restored by hand, never automatically:

```
tenant service stop && tenant backup restore <data dir>-backups/pre-update-<old version>.tenant-backup --yes && tenant service start
```

An OS stop in the middle leaves `<data>/update-state.json`: serve logs it at boot, `tenant doctor` warns,
and the next `tenant update` clears it when the new version is what runs, or offers `--rollback`.
serve also checks for a newer release once a day and files one log-book event per new tag
(`"update_check": false` in `config.json` turns that off); nothing is ever applied unasked.

## Restart-storm guard (24/7)

If you rely on cron, leave **`cron.catchup` OFF** (the default). With catchup on, a
restart after downtime fires every missed job at once. Federation resumes from its
persisted peer cursor, so a restart does not re-pull the world.

## Process protection

At every start, before anything else, Tenant asks the operating system to keep other programs
that run as the same user out of its process (TEN-535, `internal/procprotect`). It does this
for every command, the service included. `tenant doctor` shows each control under "process
protection"; `tenant security audit` raises a finding when one is off.

| | Refused to the user's other programs | Still readable |
|---|---|---|
| Linux | a debugger (`ptrace`), `/proc/<pid>/mem`, `/proc/<pid>/environ`, `process_vm_readv`, `pidfd_getfd`, `perf_event_open`; no crash dump, to a file or to a crash collector | the command line, `/proc/<pid>/status` |
| Windows | reading memory, a dump (Task Manager, procdump), a debugger, reading, suspending or redirecting a thread, rewriting the access lists, changing the process's token, the full-access handle the console host would hold, code loaded through a window (serve owns none) | the command line, the program's path, the account |
| macOS | a later debugger attach (reported as "attach denied, not verified"), core files; with the hardened runtime on the installed binary, a debugger outright (see below) | the command line, and the environment Tenant started with |

It is one layer. The files on disk are as readable as before, an administrator or root is not
stopped, and neither is a program that started Tenant itself, or one that gets in during the
instant between Tenant's start and these controls. The Linux and Windows rows were run on real
machines (WSL2 kernel 6.18, Windows 11); the macOS row has not been run on a Mac.

Not stopped on Windows, and open in this release:

- **Ending threads.** A program running as the same user can end Tenant, and can end, wait for
  and resume each of its threads. Ending threads stops work and reads nothing.
- **What the owner's rights show.** On the process the owner keeps `0x101401`: end it, wait for
  it, its basic details, and its information (`PROCESS_QUERY_INFORMATION`), which `taskkill /F`,
  `Stop-Process -Force` and Node's `process.kill` need to end it (Python's `os.kill` asks for
  every right and is still refused). Any program of the user could always read the exit code,
  the times, the priority, the counters, the paths of the modules and where the process's memory
  lies and how it is protected; that right adds which of its pages are resident and shared (the
  working set), its handles by number, kind and rights, its mitigation policy, and a walk of its
  threads that gives no more than opening each by id: nothing of its memory's content, no handle
  out of it, no thread redirected. On each thread the owner keeps `0x121001`: end,
  wait for, resume, and read its access list, which is how Tenant checks every thread's list at
  start; it says nothing of the thread's memory or registers.
- **The starter's handle.** Whoever started Tenant holds a handle to it from before the
  controls, with full access, for as long as it keeps it. A program of the same user can copy
  that handle out of the starter. A shell that waits for `tenant serve` is such a starter.
- **The first instant.** A program that is already running can open Tenant between its start
  and the controls, and keeps what it opened.
- **A program already running when Tenant starts.** The Windows controls refuse a program that
  starts after Tenant has finished starting. A program already running when Tenant starts (and
  so one that ends Tenant and waits for it to be started again) can still get in, through the
  instant before the controls or through the full-access handle the console host holds until
  Tenant has joined its console again, and what it takes then keeps working; closing that takes
  Tenant being started already closed, which this version does not do.
- **The error-reporting exclusion.** A program of the same user can make the Windows Error
  Reporting exclusion fail at start (delete the value and deny writes to the key), so that
  Tenant asks at a terminal whether to go on without it or, as a hidden serve, refuses to start.
- **A thread changed in the first instant.** Tenant takes the system's list of its threads and
  opens each by its id, sets its access list, reads it back and compares it entry by entry. A
  thread whose list cannot be read, is anything but the one Tenant set, or cannot be opened at
  all turns `windows.thread-dacl` off ("its access list, read back, is not the restricted one
  …", "its access list cannot be read back", "a thread does not let this process open it at all
  …"), and serve and the TUI ask, as for any missing control. Only a thread that has ended since
  the list was taken is passed over, and Windows tells that apart from one that refuses to be
  opened. Ending other threads does not hide one. More than 512 threads at start (a starting
  Tenant has about a dozen; only a program adding threads to it from outside gets there) turns
  the control off too, with "the process has more threads than a starting Tenant has", rather
  than making the start as long as that program likes.

What an operator meets:

- **Doctor reports the Tenant that is running.** With a serve (or a TUI with the dashboard) up,
  `tenant doctor` asks it over `/api/status` and reports that process, named in the first line
  ("The Tenant running at … (process N, version V), asked just now"). What can be read again is
  read at that moment: both controls on Linux, the core limit on macOS, and on Windows the
  process's access list, the one new threads get, every thread's own access list, and the
  error-reporting exclusion. A control that was undone while Tenant ran shows as `OFF: … it was
  in force when Tenant started and is not any more: …`, and a restart applies it again; a
  thread's list changed after the start through a handle taken before the controls (which the
  start cannot stop) shows that way, as `… a thread: its access list, read again, is not the
  restricted one`. What cannot be read again says "as it was when Tenant started": the console
  host's handle on Windows, the attach denial on macOS. With nothing running the first line says "No
  running Tenant answered … This is what a start would get". `tenant security audit` reports
  its own process, as before.
- **Windows: two more `tenant.exe` processes.** serve starts a small helper, `tenant
  __session-watch`, which owns the window Windows sends a logoff or a shutdown to and hides the
  console for `--hidden`. It has none of serve's environment and reads no config. It ends with
  serve; if it is ended alone, serve logs it and runs on, but a logoff, a shutdown and `tenant
  service stop` then end serve without a clean stop, as they did before 0.2.0-rc.3 (the next
  start logs "The previous run ended unexpectedly"). Restart Tenant to get the helper back. For an instant at
  start a second helper, `tenant __console-anchor`, holds the console open while Tenant leaves
  it and joins it again; that is how the console host comes to hold a handle with the rights
  the owner keeps and no more. The helper is born with Tenant's own closed access list, so no
  other program can suspend it or get into it. If it is ended, or is not on the console within
  30 seconds, or the console cannot be joined again, Tenant leaves its console for good rather
  than ask anything: the console window closes, Tenant runs on with no console (serve keeps
  serving; `chat` and the TUI, which need the keyboard, end), and doctor shows the control on
  with "left its console and did not join it again". A Tenant with no console (the `--system`
  service) has nothing to leave, and doctor says so.
- **Windows: no window opens.** A command Tenant starts runs on Tenant's own console, which the
  logon task hides. A Tenant that has no console (the `--system` service, one another program
  started detached, or one that left its console and could not join it again) starts every
  command, and its helper, with `CREATE_NO_WINDOW`: Windows would otherwise give each a new
  console with a window on the desktop. The one place this is decided is `procprotect.Child`.
  With `--hidden` the console window now goes when the helper is up, a moment later than it
  did when serve hid it itself.
- **Windows: folders that pass no permissions on.** In a folder with no inheritable
  permissions (after `icacls /inheritance:r`, for one) a new file takes the default of the
  thread that makes it. A data, config or log directory Tenant makes itself passes its own
  permissions on to what is made in it: the entries it was made with, marked to be inherited.
  A directory that was there before Tenant used it is never changed, nor is anything in it.
  Files a command or a tool writes are made with your ordinary permissions.
- **Stopping a stuck Tenant still works.** `kill`, `systemctl stop`, Task Manager's "End task"
  and `tenant service stop` are not affected.
- **Dumps and debuggers are refused, to you too.** `gdb -p`, `dlv attach`, `strace -p`,
  `gcore`, Task Manager's "Create dump file" and `procdump` fail with a permission error unless
  run as root or from an elevated prompt. To debug Tenant, build it with `-tags debugattach`:
  that build has no protection, and `tenant version`, doctor and the audit say so. On macOS a
  protected Tenant started under a debugger is ended by the kernel at start.
- **`GOTRACEBACK=crash`** (or `wer` on Windows) in Tenant's environment asks the Go runtime for
  a memory dump at a crash, and Tenant cannot lower it from inside. On Linux no dump is written
  anyway; doctor says which control stops it, and warns on Windows.
- **Windows keeps one value in the registry.** Tenant puts its file name on the user's Windows
  Error Reporting exclusion list:
  `HKCU\Software\Microsoft\Windows\Windows Error Reporting\ExcludedApplications`, a value
  named `tenant.exe`. It is by file name and it stays after an uninstall; delete the value to
  undo it.
- **macOS: keep keys out of environment variables.** macOS shows a user's programs each
  other's starting environment. A key that arrives that way is reported in the system log at
  start (system 5020) and by the audit. Save it with `/configure <plugin>` or the dashboard's
  keys page instead.
- **Commands the agent runs are ordinary processes.** They are not protected, they see none of
  Tenant's secret variables unless an exec profile passes one by name, and on Linux they inherit
  a core-file limit of 0, which they may raise (`ulimit -c`).

### When a protection cannot be applied

A control the operating system refuses has to be acknowledged by a person; Tenant does not carry
on silently (`cmd/tenant/protectiongate.go`). Only `tenant tui` and `tenant serve` are held to
it, and they check before they read the config, a secret or a store. No other command asks.

| How Tenant is started | What happens |
|---|---|
| `tenant tui` or `tenant serve`, at a terminal | It says which control is off, the system's reason and what that leaves open, and asks `Continue without it? [y/N]`. `y` or `yes` goes on and is recorded. Anything else, the end of the input included, exits 1. |
| the service, a scheduled start, `serve --hidden`, or input or output that is not a terminal | It does not start and exits 1. The reason is in the log book (system 3030) and in the system's own log. |
| either, once that control failing for that reason in that version was acknowledged | It starts, and says so at every start: system 2030, and a `process protection: running without …` line in the status feed (serve's stdout). |
| any other command that opens Tenant's data (`chat`, `os`, the plugin commands, `eval`, `log` and the rest) | It is not asked and runs, and leaves a record at every start: system 2030 in the log book (`tenant logs query`), with the command, the control, the system's reason and what that leaves open, and one line on stderr when stderr is a terminal: `process protection: running without <control>: <reason>.` followed by a pointer to `tenant doctor`. Every command that loads a secret or runs an agent turn is among them. `help`, `version`, `completion`, `doctor` and `security audit` without `--fix`, and `eval --subset smoke` or `ui` and `eval --list` (no key, no model) open nothing and write nothing. With every control in force nothing is written. |
| any of these, when the log book cannot take the record (its folder made unwritable, `logbook.db` not opening, a failed write) | The same line on stderr, terminal or not, ending `the record could not be written to the log book (<why>) and is in the system's log`, and the record in the system's own log (the Application event log on Windows, syslog or the journal elsewhere). The log book is the user's file: a program running as the user can also delete a record from it afterwards (from `logbook.db` and the day's text file), and doctor's log-book checks still say OK; on Windows the Application event log is the one copy such a program cannot delete. |

The record is in `settings.<agent>.json`, the settings of the agent that serve or the TUI runs
as (`main` for the service):

```json
"protection_acknowledged": [
  {"control": "linux.dumpable", "reason": "operation not permitted", "version": "0.3.0", "at": "2026-11-19T03:02:11Z"}
]
```

All three have to match: a different control, a different reason or a new version asks again.
The newest 16 entries are kept, so a rollback finds the one made for the version it goes back
to. Deleting an entry makes Tenant ask again. The agent's file tools refuse this file
(`TestFileToolsCannotRewriteControlFiles`); a program running as you could still write it, which
is tampering and outside what process protection covers.

`tenant service install`, `start` and `restart`, `tenant setup` when it installs the service,
and `tenant update` (for the new version) ask at the terminal they are typed in, so the
question comes up while you are there. A build made with `-tags debugattach` has no controls and
is never asked about.

Where to read why a service did not start:

- **Tenant's log:** `tenant logs query "provider:system id:3030"`; the same line is in
  `<data>/logs/system/<day>.log`.
- **Windows:** Event Viewer, Windows Logs, Application, source `Tenant`, level Error:
  `Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='Tenant'} -MaxEvents 5 | Format-List TimeCreated, Message`.
  Event Viewer prints "the description for Event ID 1 from source Tenant cannot be found" above
  the text, because Tenant registers no message file (that would take an administrator).
- **Linux:** `journalctl -t tenant` (the syslog line), and `journalctl --user -u tenant` for the
  `error:` line the unit's stderr carries.
- **macOS:** `~/Library/Logs/tenant/serve.err.log` has the `error:` line; the syslog line is
  `log show --last 10m --predicate 'eventMessage CONTAINS "did not start"'`.

This refusal is the one thing Tenant writes to the system's own log. The writes themselves are
not exercised by the test suite, which cannot take a line back out of that log: the tests check
that the log can be opened, and `TENANT_LIVE_SYSTEM_LOG=1 go test ./cmd/tenant -run
TestSystemLogLive` writes one line under a test name (`TenantTest` on Windows, `tenant-test`
elsewhere) to read back by hand.

A control's reason is the same at every start for the same cause, because the acknowledgement
is matched on it. What differs from start to start (which threads, for one) is in the control's
`detail`, which doctor prints in brackets after the reason and nothing matches on
(`TestFailureReasonsAreTheSameAtEveryStart` in `internal/procprotect` holds every control's
source to it).

### macOS: the hardened runtime

The hardened-runtime flag on the binary is what makes macOS refuse a debugger outright ("Not
allowed to attach to process"), with no password prompt. It works with a local, ad hoc
signature, so three things put it on the binary they install, each the same way
(`cmd/tenant/codesign.go`, `scripts/install.sh`): sign a **copy** with
`/usr/bin/codesign --force --sign - --options runtime` (no entitlements), read the flag back from
the copy (`codesign -dv`), and only then move the copy into place.

| | The copy | When signing fails |
|---|---|---|
| `scripts/install.sh` | `<install dir>/.tenant.<pid>` | the script stops; a Tenant installed before is untouched |
| `tenant update` | the staged binary in `<bin dir>/.tenant-update/`, signed before its smoke test | the update stops; the running version stays |
| `tenant service install` | `<binary>.signing`, when the binary does not carry the flag yet | a warning; the service is installed with the binary as it was |

A binary that already carries the flag is left alone, whoever signed it. `tenant doctor` shows
the flag and the kind of signature ("ad hoc, signed on this machine", "ad hoc, by the Go linker",
or the certificate's name); `tenant security audit` raises `process.darwin.hardened-runtime` for
a release binary without it. A source build without it is not a finding: it is "attach denied,
not verified", and `tenant service install` signs it. A release signed and notarized with a
Developer ID certificate is TEN-539.

**None of this has run on a Mac.** The Go side is tested against a stand-in for `codesign`, the
script under `sh` against one on `PATH`, and the Packaging workflow's macOS job checks the flag
on what the script installs. On a Mac, check:

```sh
# 1. The install script signs what it installs.
TENANT_INSTALL_DIR="$HOME/.local/bin" sh scripts/install.sh          # or the release one-liner
codesign -dv "$HOME/.local/bin/tenant" 2>&1 | grep '^CodeDirectory'   # want flags=0x10002(adhoc,runtime)
codesign --verify --strict "$HOME/.local/bin/tenant" && echo verified

# 2. The signed binary runs, and doctor sees the flag.
tenant version
tenant doctor | grep -A12 'process protection'   # want "on:  the hardened runtime on this binary … (ad hoc, signed on this machine)"

# 3. A debugger and vmmap are refused outright, with no password prompt.
tenant serve --backend echo --dashboard=false --data /tmp/t-data --config /tmp/t-cfg &
lldb -p "$!" -o detach -o quit                   # want "attach failed … Not allowed to attach to process"
vmmap "$!" | head -3                             # want a refusal, and no password prompt
kill "$!"

# 4. A source build: nothing warns, and `service install` signs it where it is.
go build -o /tmp/tenant-src ./cmd/tenant
codesign -dv /tmp/tenant-src 2>&1 | grep '^CodeDirectory'   # Apple silicon: flags=0x20002(adhoc,linker-signed)
/tmp/tenant-src doctor | grep -A12 'process protection'     # want "a source build without the hardened runtime … attach denied, not verified"
lldb -p "$(/tmp/tenant-src serve --backend echo --dashboard=false --data /tmp/t-data --config /tmp/t-cfg >/dev/null 2>&1 & echo $!)" -o detach -o quit   # record what PT_DENY_ATTACH alone does
/tmp/tenant-src service install --data /tmp/t-data --config /tmp/t-cfg   # a test Mac: this installs com.tenant.serve
codesign -dv /tmp/tenant-src 2>&1 | grep '^CodeDirectory'   # want flags=…(adhoc,runtime), and no /tmp/tenant-src.signing left
/tmp/tenant-src service status --data /tmp/t-data --config /tmp/t-cfg   # running, on the signed binary
/tmp/tenant-src service uninstall --data /tmp/t-data --config /tmp/t-cfg

# 5. `tenant update` leaves a signed binary that the service starts.
tenant update && codesign -dv "$(command -v tenant)" 2>&1 | grep '^CodeDirectory' && tenant service status

# 6. The tests, on the Mac, and the system-log line.
go test ./cmd/tenant ./internal/procprotect
TENANT_LIVE_SYSTEM_LOG=1 go test ./cmd/tenant -run TestSystemLogLive
log show --last 5m --predicate 'eventMessage CONTAINS "TEN-535"'
```

## Health

`tenant doctor` includes a **serve liveness** check: when a hub is reachable it probes
`/api/status` and WARNs on a stuck turn or a waiting approval queue; it SKIPs silently
when no hub is running (so a TUI-only setup isn't nagged). `tenant doctor --json` stays
CI-usable.
