# Changelog

Notable changes to Tenant. Releases are tagged `vX.Y.Z`; their binaries are on the
Releases page.

## Unreleased

- **Fixed: Tenant could hang at start-up, with no dashboard, while its embedder was stuck
  (TEN-555).** Start-up re-indexed the wiki before anything else came up, and an embedder that
  took the request and never answered (an Ollama runner stuck at 100% CPU) held it there for as
  long as it hung. The first re-index now runs beside start-up, start-up waits at most 10
  seconds for the embedder to say its dimension, and every wiki embedding request has a limit
  (3 minutes for a note, 30 seconds for a search), after which the tool says the embedder did
  not answer.
- **Fixed (Windows): a save could fail while another program had the file open.** Tenant
  replaces a file by renaming a finished copy over it, and Windows refuses that rename while
  any other handle is open on the file (a virus scanner, the search indexer, a reader of
  Tenant's own). The rename is now tried again for up to about 300 ms before it is reported.
- **A new fleet machine starts with this Tenant's approval settings as they are.** Until now
  a machine's copy held Run commands, Change files, Irreversible actions, Send messages and
  Leave the sandbox at Ask first even where this Tenant allows them, so a master set to allow
  commands still asked for every `fleet_exec`. The copy is now exact: what this Tenant allows
  without asking, the machine does too. To start every machine tighter than the master, set a
  fleet default (`tenant fleet permissions --defaults set exec ask`, or Fleet > Defaults).
  Machines already confirmed keep the settings they have; to give them the new copy run
  `tenant fleet permissions --all --from master` (or **Copy again** on a machine's page). A
  turn that has read from one machine still asks before it acts on another.
- **Fixed: `$variables` lost in a PowerShell command wrapped in `powershell -Command "..."`.**
  `os_exec` (and so `fleet_exec` on a Windows machine) already runs in PowerShell; when the
  agent wrapped its script in a second one, the outer shell expanded every `$name` inside the
  double quotes to nothing and the script failed. A plain wrapper is now taken off and the
  script runs as written (the approval shows the script that runs); a wrapper that cannot be
  taken off safely runs as given and the result says why it probably failed.
- **Fixed: a scheduled exec job could not post what it read.** The exec runner never passed the
  sensitive-egress check to the job's approver, so every Discord message, mail or other outbound
  call that quoted a wiki, memory or mail read was refused. A report built from the wiki now
  goes out, and each such flow is still a Security record; a vault secret still never leaves in a
  message.
- **A scheduled exec job keeps the helper tools** (`spawn_agent`, `team_*`): a helper inherits the
  job's approver and exec surface, so the same unattended rules apply to it, and the `cron_*`
  tools stay refused. Jobs without exec still have no helper tools.
- **The dashboard's chat follows the conversation.** New messages and the activity rail keep
  the view at the newest line. Scrolling up holds your place; scrolling back to the bottom, or
  sending a message, follows again.
- **Fixed: a scheduled exec job could not write its own notes** when Tenant runs from a folder
  that also holds the wiki (an install at `C:\Tenant` with the wiki in `C:\Tenant\twiki`): the
  guard that keeps an unattended job out of Tenant's own files took the whole install folder,
  so every `wiki_save`, and every write into the wiki or `<install>/work`, was refused. The wiki
  (wherever it is configured) and the install folder's `work` directory are the job's to write;
  config, data and the rest of the install folder stay protected.
- **The dashboard has a new look, in light and dark (TEN-547).** It is drawn with Web Awesome's
  free stylesheets (3.15.0, MIT), kept in the binary: nothing loads from another site. A sun or
  moon button at the top right switches between light and dark; until you choose, the page
  follows the computer's setting. Pages use the window's whole width, and secondary text is
  brighter in both schemes.
  - **The alerts bell.** The line across the top of every page is gone. A bell at the top right
    counts what is waiting for you (approvals and notices, "99+" past 99) and opens them; it
    turns amber while your agent is busy.
  - **Two columns.** On a wide screen Research, Sub-agents, Tools, Scheduled jobs, Fleet, a
    machine's page, Approvals & safety and Access put the forms and settings on one side and
    the list on the other, each scrolling on its own. A narrow screen stacks them as before.
  - **Chat's activity list scrolls on its own.** Opened beside the chat, a long run's lines
    ran down past the message box and made the whole page scroll. The list now keeps to the
    height of the chat, scrolls inside it and follows the newest line, as the terminal app's
    feed does.
  - **A confirm step no longer moves the page.** It opens as a box over the page: under its
    button on a wide screen, at the bottom of a phone's.
  - **Helpers are called Sub-agents on the dashboard:** the page, its words, and the run
    limits' names ("Sub-agents per run", "Sub-agents at once", in the terminal app's list
    too). Commands and settings keep their names.
  - **Scheduled jobs.** The time zone is a list. When has Custom: Daily, Weekly, Monthly or
    Yearly, the minutes past the hour, and a week of hours to tick or drag across. A weekly job
    runs at the same hours on each day ticked. A typed schedule is still there, behind "Type a
    schedule instead".
  - **Integrations** lists the services you can add first, then the connected ones, each with
    its settings behind Settings.
  - The page tells browsers and Dark Reader not to darken it a second time (`color-scheme:
    only light`, `darkreader-lock`): darkened again, the light scheme came out dark grey.

## 0.2.0 (2026-11-24)

- **Fleet groups, default categories and bulk changes (TEN-546).** Machines can be put in named
  groups (`tenant fleet group`, `/fleet group`), each with rules laid over its members when the
  group is applied (`tenant fleet group apply`), never live: a call is still decided by the
  machine's own scheme alone, and changing a group moves no machine. The defaults for new
  machines now take categories as well as rules (`tenant fleet permissions --defaults set|unset
  <category>`, settings key `fleet_permissions`; groups are `fleet_groups`). A new machine's
  copy is the master's scheme clamped, then the default categories, then the fleet's rules, then
  its groups' rules in name order, each winning over the one before; the confirm screen says
  `fleet default` and `group <name>` where they did. `tenant fleet permissions --group <g> |
  --all` makes one edit to many machines, one Security record per machine, exit 1 if any was not
  changed; `tenant fleet update --group <g>` updates a group's members in turn. Both keys are
  strict and bounded: unreadable, no machine can be confirmed or copied again until fixed. New
  events `security/4325` and `fleet/5003`. See docs/fleet-runbook.md, section 3a.
  - **On the dashboard's Fleet page:** the fleet's defaults are edited there (each category as
    Ask, Allow, Deny or From this Tenant; the fleet's rules), groups are made, filled, applied
    and deleted, the list can be narrowed to a group, and ticked machines (or everyone shown)
    take one change at once: a category, a rule, Apply fleet rules, Copy again, or an update.
    A change that lets the agent do more, replaces settings or updates first shows a step that
    says how many machines and which; the answer says "Done on 5 of 7." and why for each
    machine not changed. A machine's page uses the same controls and words as this Tenant's own
    Approvals & safety and Tools pages. These changes take a signed-in browser; the API key
    gets no more than it had.
  - **A rollout** updates many machines a few at a time (1 to 8 at once) from the Fleet page:
    a refused machine is skipped, and the first that rolls back or fails stops it. One at a
    time, kept in the running Tenant's memory. `tenant fleet update --all` is unchanged.
  - A change to the fleet's rules for new machines now leaves a Security record on every
    surface (`security/4325`), as a change to a default category or a group does.
  - **What is done is what was shown.** A group's rule takes no step, since it moves no
    machine; so the steps that do move machines hold to what they showed. A confirmation whose
    machine would no longer start with the settings on its screen (a default, a fleet rule or
    one of its groups was edited meanwhile) confirms nothing, on the command line and on the
    Fleet page; Apply and Copy again on the page are refused and drawn again the same way, and
    a group's Apply reaches only the members its step named. The record of a confirmation names
    the defaults and groups it laid, and Apply's record says which of a machine's own rules it
    replaced.
- **A fleet machine can be confirmed from another computer, if you turn that on.** A pending
  machine was confirmed only at the master: its terminal, or a browser signed in at the
  dashboard's 127.0.0.1 address. On a master with no screen that meant a console for every
  machine. `tenant fleet remote-confirm on` (or `/fleet remote-confirm on`), run by a person
  at the master's terminal, lets a signed-in dashboard browser elsewhere confirm one: its page
  then offers the form with one more field, the sign-in key again. It is off by default, has no
  switch on the dashboard, and the command refuses to turn it on when its input is not a
  terminal; a request that carries only the API key is still refused; wrong keys are slowed
  per address as wrong sign-ins are; and the confirmation and every refusal are recorded with
  the surface `dashboard-remote`, the session and address that asked, and what a proxy said of
  who it passed on. The key again stops a session that leaked, not the key's holder: with this
  on, whoever holds the sign-in key (the API key is the same key) can confirm a machine from
  anywhere the dashboard is reached. The fleet runbook's sentence that there is "no dashboard
  button" was wrong since the Fleet page shipped, and is corrected.

- **An install script installs its own release.** The scripts asked GitHub for the latest
  release, which skips prereleases, so `install.ps1` or `install.sh` taken from a prerelease
  installed 0.1.0, and the fleet's paste line then failed with `unknown command: child`. The
  release workflow now stamps each release's version into its scripts (`TENANT_VERSION` still
  overrides; the tracked copy still asks for the latest), the release notes give the line for
  that tag, and the fleet's paste lines fetch the master's own release, so a new machine runs
  the master's version. The release smoke test installs without `TENANT_VERSION`, as a user does.

- **The fleet has its own log.** A master's fleet wrote four audit records to the Security log
  and a few free-form lines; setting up a fleet, its port opening and a machine connecting left
  nothing where an operator looks. The fleet is now a catalogued log (`fleet`, 50 events; `tenant
  logs query log:fleet`, `/logs log:fleet`, the dashboard's Logs page):
  - **Setup:** this Tenant becoming a master, the addresses machines dial, a code issued, a
    machine enrolled, confirmed, renamed or revoked, the relay switch. A change made by a
    one-shot `tenant fleet` command is written before the command returns, so it is in the log
    as a running Tenant's is. The Security log keeps the audit record of each, as before.
  - **The port:** open and closed as one activity with its uptime, held by another process,
    not started and why (once per cause), a refused handshake.
  - **Machines:** connected, disconnected and why, a machine that sends what the link does not
    allow.
  - **Work:** each call on a machine with its outcome and the hash of the machine's own audit
    record of it (`fleet/1201`, where the free-form `fleet call` line was), each step of an
    update sent over the link, and one that did not go through.
  - **Memory:** the machines' memory opened or unavailable and why, copies taken and restored,
    forgets applied, and every warning that was a free-form line.
  - No line holds an enrollment code or the recovery identity. A child machine's own process
    has no log book: it keeps its audit log and writes to its own log and the system's.
- **The dashboard can be served to the local network, for a site with no VPN (TEN-544).** It is
  off by default, and loopback with `/tailscale serve` stays the way in unless you turn this on.
  `/dashboard serve lan [HOST:PORT]` in the terminal app (typed at the keyboard), `--dashboard-lan`
  on `tenant serve` and `tenant tui`, or `dashboard.lan` in `config.json` starts a second
  listener, by default on every interface at port 9771, with no restart; `/dashboard serve off`
  closes it. The loopback listener is untouched.
  - **HTTPS always.** Tenant makes a self-signed certificate once and keeps it, owner-only, in
    the config dir (`dashboard-lan-cert.pem`, `dashboard-lan-key.pem`); your own
    `dashboard.tls_cert` and `tls_key` are used instead when set. A browser warns about a
    self-signed certificate the first time: Tenant prints its SHA-256 fingerprint when the
    listener starts and in `/dashboard`, to compare with what the browser shows.
  - **Who may connect.** This computer, private and link-local addresses, and the networks in
    `dashboard.lan.allow`. Anything else gets 403 before sign-in is looked at, and a Security
    record. Sign-in is the dashboard's own key; the session cookie is marked Secure.
  - **What it answers to.** The address a request arrived at, or this computer's own name, on
    the listener's port. A name another site points at this computer is refused, as before.
  - **What stays at this computer.** Confirming a fleet machine, and turning this listener on or
    off: there is no page or API for that, and a line the app submits for you does not do it.
  - `tenant doctor` and `tenant security audit` name the listener and the certificate's
    fingerprint; turning it on or off is Security event 4304.
  - Run on macOS on loopback addresses only: sign-in over HTTPS, the certificate, the refusals
    and the on and off. Not yet tried from a second computer, or on Linux or Windows.
- **`/tools` and `/skill list` fill the width of the pane.** The plugins go side by side, each
  whole with its tools under it, in as many straight columns as the pane takes, so a long list
  takes a fraction of the scrolling (the built-in tools fit one screen in a wide window). A
  narrow pane takes fewer columns, down to the one list as before.
- **A fleet can be set up from the terminal app and from the dashboard.** Until now only
  `tenant fleet init` made a Tenant a fleet master, and `/fleet` and the Fleet page told you to
  run it. `/fleet init [--listen ADDR] [--address HOST:PORT]...` now does the same in the
  terminal app: the app steps aside while the recovery identity is shown on the terminal and
  typed back, so the identity is never in the app's chat, its transcript or a log. The Fleet
  page of a Tenant with no fleet has **Set up the fleet**, with the fleet port's address and
  the addresses machines dial filled in as `init` would choose them. It takes a signed-in
  browser, never the API key, and does not make the recovery key: the page says no machine's
  memory is kept until `tenant fleet init` or `/fleet init` at a terminal makes it. The running
  Tenant takes the new fleet within a few seconds, with no restart. On all three, `init` now
  refuses an address that is not a `host:port` before it makes anything.
- **Process protection: other programs running as you are kept out of a running Tenant.** Until
  now a program running as the same user (malware, a tool, a command the agent was talked into
  running) could read Tenant's memory, where its keys are while it runs. Tenant now asks the
  operating system to refuse that, first thing at every start, for every command (TEN-535). What
  that amounts to on each system, no wider than it is:
  - **Linux:** `ptrace` (a debugger), `/proc/<pid>/mem`, `/proc/<pid>/environ`,
    `process_vm_readv`, `pidfd_getfd` and `perf_event_open` are refused, and a crash leaves no
    memory dump, in a file or with a crash collector. The command line and `/proc/<pid>/status`
    stay readable. Run on WSL2, kernel 6.18.
  - **Windows:** reading the memory, writing a dump (Task Manager's "Create dump file" now says
    access is denied), attaching a debugger, and reading, suspending or redirecting any of its
    threads are refused, and so is rewriting the access list that says so. A program running as
    you can still end Tenant, and can end, wait for and resume each of its threads: ending
    threads stops work and reads nothing. Task Manager still lists Tenant (its End task
    was not tested), and `taskkill /F`, PowerShell's `Stop-Process -Force` and Node's
    `process.kill` end it, which need the right to query the process's information; plain
    `Stop-Process` and Python's `os.kill` ask for more and are refused. A program running as
    you could always read Tenant's exit code, times, priority,
    counters, module paths and where its memory lies and how it is protected, and that right
    adds which of its pages are resident and shared (the working set), its handles by number,
    kind and rights, its mitigation policy, and a walk of its threads that gives no more than
    opening each by id: nothing of its memory's content, no handle out of it, no thread
    redirected. A program running as you can also read each thread's access list, which Tenant
    reads back itself at start: it opens every thread the system lists, by its id, compares its
    list with the one it set, and a thread that is not as set, or does not let Tenant open it,
    turns the control off (ending other threads does not hide it); so do more than 512 threads
    at start. `tenant doctor` reads every thread's list again when it asks a running Tenant. The
    command line, the program's path and the account stay readable. Tenant also puts its file
    name on your Windows Error Reporting exclusion list. Run on Windows 11, not elevated; an
    actual crash dump by Windows Error Reporting was not attempted.

    Two other ways in are closed. The console host (`conhost.exe`) opens every console program
    with full access when the program starts, and any program running as you can copy that
    handle; so Tenant leaves its console and joins it again once its access list is set, and the
    handle the host then holds carries the rights above and no more. When Tenant is alone
    on its console, a helper (`tenant __console-anchor`) holds the console open for that
    instant. The helper is born with Tenant's closed access list, so no other program can
    suspend it; if it is ended, or the console cannot be joined again, Tenant leaves its console
    for good and asks nothing. And a window lets any program running as you load code into the process that owns
    it; the window through which Windows tells `tenant serve` about a logoff or a shutdown is
    now owned by a helper process (`tenant __session-watch`, started by serve with none of
    serve's environment), so Task Manager shows a second, small `tenant.exe` under serve.
    Neither opens a window: a command Tenant starts runs on Tenant's own console, hidden or
    not, and a Tenant that has no console (the service, or one another program started
    detached) starts its commands and its helper with no console window at all, where Windows
    would otherwise open one on your desktop for each.

    Two ways in are not closed in this release. Whoever started Tenant holds a handle to it
    from before these controls, with full access, for as long as it keeps it, and a program
    running as you can copy that handle out of the starter (a shell that waits for `tenant
    serve` is one such starter). And a program that is already running can open Tenant in the
    instant between its start and these controls.

    Put plainly: the Windows controls refuse a program that starts after Tenant has finished
    starting. A program already running when Tenant starts (and so one that ends Tenant and
    waits for it to be started again) can still get in, through the instant before the controls
    or through the full-access handle the console host holds until Tenant has joined its console
    again, and what it takes then keeps working; closing that takes Tenant being started already
    closed, which this version does not do. A program running as you can also make the Windows
    Error Reporting exclusion fail at start (delete the value and deny writes to the key), so that
    Tenant asks at a terminal whether to go on without it or, as a hidden serve, refuses to start.
  - **macOS:** Tenant denies a later debugger attach and turns core files off. This was compiled
    and not yet run on a Mac, and what the call stops on its own is not measured, so doctor
    reports it as "attach denied, not verified". The stronger control is the hardened-runtime
    flag on the installed binary, which makes macOS refuse a debugger outright. The install
    script, `tenant service install` and `tenant update` now put it there: they sign a copy of
    the binary on your machine (`codesign --force --sign - --options runtime`, no entitlements),
    check the flag on the copy, and only then move it into place. If signing fails, what was
    installed stays as it was and the command says so. Doctor reports the flag and the kind of
    signature, and the audit raises a release binary that lacks it. The signing step has not
    yet run on a Mac either: it is tested against a stand-in for `codesign`. The command line
    and the environment Tenant started with stay readable to your other programs, so a key set
    in an environment variable is reported: a warning in the system log at start, and a finding
    in `tenant security audit`. Save keys with `/configure <plugin>` or the dashboard's keys
    page.

  This is one layer. It protects the running process, not the files on disk, which a program
  running as you can still read; and it does not stop an administrator or root, a program that
  starts Tenant itself, or one that gets in during the instant between Tenant's start and these
  controls. `tenant doctor` has a new "process protection" check that shows each control as on
  or off, what the system says about itself (`kernel.yama.ptrace_scope`, whether the process is
  elevated, System Integrity Protection and FileVault), what stays readable, and what a program
  running as you can still do; `tenant security audit` raises a finding when a control is off.

  When a Tenant is running, doctor reports that process and not its own: it asks it over the
  dashboard's `/api/status`, and the first line names the process. The answer is read at that
  moment where the system lets a process read its own state again, because some controls can be
  undone from outside while Tenant runs: the Windows Error Reporting exclusion is a registry
  value of yours, and on Linux `prlimit` raises the core limit of a process of the same user.
  Such a control shows as OFF, with what was found, and a restart applies it again. The
  exclusion is kept by file name and doctor applies the controls to itself at its start like
  every command, so on Windows it puts a removed exclusion back before it asks; it then says
  "OFF until this doctor started" rather than report it on. What cannot
  be read again is marked "as it was when Tenant started": the access lists of the threads that
  exist and the console host's handle on Windows, the attach denial on macOS. With no Tenant
  running, doctor says that what it shows is what a start would get.

  There is no switch to turn it off: a program running as you could restart Tenant with it set.
  To attach a debugger, build with `-tags debugattach`; `tenant version`, doctor and the audit
  say when a build was made that way. With `GOTRACEBACK=crash` (or `wer` on Windows) the Go
  runtime asks for a memory dump at a crash, which Tenant cannot lower from inside; doctor points
  it out.

  **When a protection cannot be applied, a person has to say so.** `tenant tui` and `tenant
  serve` read what the system refused before they load anything else. At a terminal they print
  which protection is missing and what that leaves open, and ask `Continue without it? [y/N]`;
  anything but yes exits. With no terminal (the service, a scheduled start) Tenant does not
  start, and the reason is in its log (system 3030) and in the system's own log (the
  Application event log on Windows, the journal or syslog elsewhere), unless that control
  failing for that reason in that version was acknowledged before. A yes is recorded in
  `settings.<agent>.json` under `protection_acknowledged`: the control, the reason, the version
  and the time. A different control, a different reason or a new version asks again. Every start
  that goes ahead on an acknowledgement says so (system 2030, and a line in the status feed).
  Every other command that opens Tenant's data is not asked, but leaves the same record at
  every start it makes without a control: system 2030 in the log book, with the command, the
  control and the reason, and a line on stderr at a terminal; every command that loads a secret
  or runs an agent turn is among them. `help`, `version`, `completion`, a plain `doctor` or
  `security audit`, and `eval --subset smoke` or `ui` (no key, no model) write nothing. When the
  log book cannot take the record (its folder made unwritable, `logbook.db` not opening), the
  command says so on stderr, terminal or not, and the record goes to the system's own log. The
  log book is your file, so a program running as you can still delete a record from it
  afterwards without doctor noticing; only the failure path's copy in the Windows Application
  event log is out of its reach.

  `tenant service install`, `start` and `restart`, and `tenant setup` when it installs the
  service, ask first, at the terminal you typed them in, because the service has nobody to ask.
  `tenant update` asks about the new version before it changes anything: it runs the new binary,
  reads what that binary could not apply here (`tenant version --protection` prints it), and
  with nobody at a terminal it stops there and the running version stays. `--yes` does not
  answer this question. Doctor and the audit show what is off and when it was acknowledged. No
  other command asks, and a `-tags debugattach` build is never asked about.

  For people working on the code: on Windows the protection changes a default that every object
  the process creates inherits, so a command must be started through `internal/procprotect`
  (`Start`, `Run`, `Output`, `CombinedOutput`, or `Child` with the call inside `Ordinary`). A
  command started any other way fails with "pipe: Access is denied" for a user who is not an
  administrator. A test in `internal/archtest` holds the source to it. The same default would
  land on a file or a folder made where nothing is inherited from the folder above (one set up
  with `icacls /inheritance:r`, say), and its owner could then not open it. So a command and a
  tool call run inside `procprotect.Ordinary`, `internal/fsperm` makes its files and folders
  inside it, and a directory `fsperm.MkdirAll` makes itself is made to pass its permissions on
  to what is made in it. A directory that was there already is never changed: making its
  entries pass on would hand them to every file already in it. What is left: a file made with
  the plain `os` calls on another goroutine, in a folder that Tenant did not make and that
  passes nothing on.
  Make such a file through `fsperm`, or inside `Ordinary`. The acceptance tests of
  the acknowledgement run a `tenant` built with `-tags procprotecttest`, in which the environment
  can say that a control cannot be applied; `tenant version` announces such a build, and no
  release is built with the tag.
- **A machine joins a fleet in one paste** (TEN-541). `tenant fleet enroll <name>` now prints,
  beside the lines for a machine that has Tenant, the whole line for a machine with nothing on
  it: `install.sh` (Linux, macOS) or `install.ps1` (PowerShell) from the latest release, which
  checks the download and goes straight on to `tenant child install`, as the user's own child or
  as a service from boot. The code travels in `TENANT_ENROLL_CODE` or on standard input, never
  as an argument; the boot-time line for Linux and macOS keeps it off sudo's command line
  (`sudo --preserve-env`), and the install script's own example now does too. The same output
  is `/fleet enroll <name>` in the terminal app, and the dashboard's Fleet page has **Add
  machine**: a name and whether it runs as you or from boot, then the line with a Copy button,
  shown once, in that answer only. Making a code there takes a signed-in browser (not the API
  key); confirming the machine stays on this computer, with its six digits. `tenant fleet
  address list | add | remove` changes the addresses a code carries one at a time, and every
  place a code is made warns when each IP address on it is private, link-local or loopback.
  `docs/fleet-runbook.md` has the three routes: a LAN, an overlay network, a public address.
  Joining stays by code only: no discovery, no request to join.
- **The main releases are signed with a project key as well** (TEN-542). Beside cosign's
  keyless bundle, a release of the main repository carries `checksums.txt.sig`, an Ed25519
  signature over `checksums.txt` made by the release workflow with a key held as a repository
  secret; the public key is built into the release binary. A binary that carries the key takes,
  with `tenant update`, only a release whose signature verifies under it, with nothing
  installed; a missing, malformed or wrong signature refuses the update, and no flag skips it.
  cosign is still checked when it is installed, and then both must pass. A fleet machine on the
  default rule takes a release when cosign verifies it or, when its Tenant carries the key, when
  the project signature its master sent verifies; one that is sent and wrong refuses at once.
  `tenant fleet update` says which check each machine passed. A fork's binaries and development
  builds carry no key and update exactly as before. The maintainer makes and replaces the key
  with `go run ./scripts/releasekey keygen` (SECURITY.md, "Release signatures"); a repository
  without the secret and the variable releases as before.
- **A fleet: one Tenant that owns your other machines** (TEN-531, the first of three parts).
  Install Tenant on a machine you own and run `tenant child install` with a one-time code from
  the master (`tenant fleet enroll <name>`): the machine becomes a **child**. It has no model
  and no agent. It dials the master, holds one link open, and serves that machine's tools to
  the master's agent. The agent works on it with ten new tools that take a `host`: `fleet_list`,
  `fleet_exec`, `fleet_process`, `fleet_read`, `fleet_list_dir`, `fleet_write`, `fleet_edit`,
  `fleet_copy`, `fleet_tools` and `fleet_call`. They are there only while a confirmed machine
  exists. Peering is unchanged: it is for a machine with a different operator.
  - *Enrollment.* The machine mints its own key and shows six digits; the master sends it
    nothing until you type them at a terminal (`tenant fleet confirm <name>`, or `/fleet confirm`).
    No tool, relay or approval card can confirm a machine. `tenant fleet init | enroll | list |
    show | confirm | rename | revoke | permissions | relay`, and `/fleet` in the terminal.
  - *The link.* The child dials the master's fleet port (`fleet.listen`, default `0.0.0.0:9777`,
    off until `tenant fleet init`) over TLS 1.3 with both certificates pinned by fingerprint: no
    certificate authority and no token. A certificate that is not pinned is dropped before
    anything is answered. Idle, a link carries one ping and its answer every 30 seconds, about
    100 bytes a minute; a dead link is noticed within a minute by both ends, and the child
    reconnects by itself. One process holds the links (`tenant serve` should); another on the
    same data dir says which.
  - *Approvals stay on the master.* A tool on the child asks the master over the link. The card
    names the machine, and on the dashboard its headline says "on your fleet machine <name>"
    with no step for the rest of the session. Each machine has its own approval scheme, copied
    from the master's when it is confirmed, with commands, file writes, destructive actions,
    sends and leaving the sandbox no looser than ask. Every shell command on a machine asks until
    you answer "always", which saves a rule for that machine alone. A relay or a cron job reaches
    no machine, reads included, until you open it (`tenant fleet relay discord on`, `/cron
    fleet`). `tenant fleet …` and `tenant child …` join `tenant service …` and `tenant update …`
    among the commands the agent does not run unasked.
  - *On the machine.* `tenant child status | pause | resume | audit | uninstall`. `child.json`
    holds a ceiling the master cannot lift (`no-exec`, `read-only`, a list of the only tools the
    machine serves), read from the disk on every call. Every call is written to a hash-chained
    audit log on the machine before it runs, and the master keeps that record's hash with its own
    record of the call.
  - *What a machine returns is untrusted.* Results are framed and labelled, the egress check
    covers them, and after a turn has read from one machine anything but a read on another asks;
    that card says which machine the turn read from.
  - The run trace, the dashboard's activity rail (`fleet_exec@web1`) and the turns log's tool
    records name the machine a fleet call worked on.
  - Not in this part: a machine's memory (TEN-532); `tenant fleet update`, `tenant child rehome`,
    the dashboard's Fleet page and doctor checks (TEN-533). A picture a tool makes on a machine
    stays there (the result says so). `docs/fleet-runbook.md` has the walk-through.
- **A fleet machine is updated over its link** (TEN-533). `tenant fleet update <name>` (or
  `--all`, or Update on the machine's page of the dashboard) fetches the release for that
  machine's system and processor, verifies it as `tenant update` verifies its own, and sends it
  over the link with its SHA-256: the machine needs no way to the internet. The machine checks
  the hash before it touches anything, runs the new version beside the old one until it reaches
  the master, then swaps the binaries and restarts into it. A new version that exits, or does not
  reach the master within a minute, is rolled back and the machine reconnects on the old one; a
  machine stopped in the middle finishes or undoes the update at its next start. Background
  commands on the machine end at the restart. An older version is sent only with
  `--allow-older`, and an offline machine is refused (nothing is queued). The command prints each
  step and how it ended; `tenant child status` on the machine shows the last update. The update
  is allowed whatever link version the machine speaks.
  - *Doctor* on a master checks each machine through the running Tenant: reachable, its version
    against the master's, whether the master still serves its link version, and how its last
    update went.
  - The machine's hello now also says which processor it has (an older machine leaves it out;
    `--arch` names it then).
- **A fleet machine's memory is kept, encrypted, on both ends** (TEN-532, the second part).
  - *Keys.* `tenant fleet init` makes the fleet key (sealed by Windows' data protection, an
    owner-only file elsewhere) and a recovery identity, which it shows you once at a terminal
    and records only after you have typed it back. Keep it away from this computer: with it, and
    only with it, a master rebuilt from nothing can read its machines' memory again. Without a
    terminal init makes everything else, shows no secret and exits 1; run it again at a terminal.
    A fleet made before this gets its keys at its next `tenant fleet init`.
  - *Copies.* A machine's memory lives in the master's RAM, and on its disk only as an
    encrypted snapshot. Every five minutes a changed memory becomes a new generation, encrypted
    to the fleet key and the recovery key, and goes to the machine, which keeps the last three
    and cannot read them. Each time a machine connects the two sides compare: the newer wins,
    the machine's copy rebuilds a master that lost or damaged its own, and a copy made under
    another fleet key marks the machine **needs recovery key** and nothing is overwritten.
    A replaced memory is kept aside on the master for thirty days. Each snapshot ends with an
    authenticator only the fleet key's secret makes: a copy that names this master's key and
    does not carry it is refused unread, so knowing the fleet key's or the recovery key's public
    half is not enough to put a memory on the master. A copy read with the recovery identity
    (`tenant fleet recover`) cannot be checked that way: whatever it says the master recorded
    (the inventory, notes) is recalled from then on as the machine's own words, inside its frame,
    and the master takes the machine's inventory again.
  - `/fleet` and `tenant fleet show` say each machine's memory: in step, behind, needs recovery
    key, or why it is not known. `tenant doctor` checks the fleet key (and that it is held in
    memory kept out of the swap file) and that the recovery key was proven. `tenant backup` warns
    when a fleet master's backup is not encrypted. `tenant fleet revoke --delete-scheme` deletes
    the machine's memory too.
  - *What goes into it.* A turn's episode stays in the master's memory, tagged with the machines
    it worked on, and each of those machines gets the request and its own calls with what came
    back: never another machine's output, the agent's answer or the master's own memory. At
    confirmation the master records the machine's OS, account, version, sandbox and ceiling as
    facts, each from a field it checked. A new tool, `fleet_note`, records a fact about a machine
    in the agent's words; nothing goes to the machine. Distillation runs over each machine's own
    turns, into that machine's facts. A machine's memory needs an embedder, as the master's does.
  - *How it comes back.* A machine's memory comes into a conversation with the first answer from
    that machine there, and `memory_search` takes a `host` once the conversation has had one. The
    master's own words (the inventory, notes) are quoted lines; everything else is the machine's
    output and comes inside that machine's frame, escaped and labelled untrusted, so the egress
    check covers it, and a note that quotes a machine's output asks first. A relay or job that may
    not reach a machine gets none of its memory, nor does a helper, team, research or goal run.
  - *Forget.* `tenant privacy forget` reaches every machine's memory: the holder deletes it there
    and from the memory kept aside, sends the machine a new copy and tells it to delete its older
    ones, also once a machine that was offline is back, and applies it again to a memory rebuilt
    from a copy made before it. Copies a machine keeps under an earlier fleet key are not deleted;
    you are told. Retention pruning does not reach the machines' memory. The request, which
    names what is forgotten, is kept sealed until every machine still in the fleet has had it,
    has dropped its older copies and keeps none under another key; then it is deleted. A copy
    older than the forget that comes back after that, from a backup of a machine's own disk, is
    not purged, and a memory rebuilt from it holds what was forgotten again.
- **A fleet machine verifies an update's release itself** (TEN-533). `tenant fleet
  update` now sends the release's checksums and signature bundle with the archive, before any of
  it; a machine checks, offline, that the signature is the release workflow's of the repository
  its own binary was built from, that the checksums list the archive with the hash it was sent,
  and that the archive is its own system's and processor's for the version named, and refuses
  anything else before the archive is on the wire. A master that was taken over, or whose copy
  of an archive was changed, can no longer install it on its machines. This needs `cosign` on
  the machine and a signed release; a development build refuses every update on this rule and
  says so. `tenant child install --updates master` (or later, on the machine,
  `tenant child updates master`) makes a machine take its master's hash instead, to run a build
  of your own; it is set only on the machine, kept by rehome, shown by `tenant child status`, and
  recorded in the audit log. `tenant fleet update` says which rule each machine applied.
- **A fleet survives losing its master** (TEN-533). `tenant child rehome`, run on a machine with
  its child stopped, gives it another master: a master restored from a plain backup or rebuilt
  from nothing. It takes a code from the new master's `tenant fleet enroll` as `child install`
  does (standard input or `$TENANT_ENROLL_CODE`, never an argument) and keeps the machine's key,
  ceiling, tools, sandbox, pause, audit log and memory copies; only which master it trusts
  changes, in one step, and a rehome that fails leaves it its old master's. The new master holds
  it as pending until you type its six digits; the old master's certificate is refused from then
  on. Nothing on the link can start one. The audit log records who did it and both masters'
  fingerprints.
  - `tenant fleet recover <name> [--was ID]` brings the recovery identity (at the terminal, or on
    standard input) to the running master, which reads the machine's memory from its copy made
    under the old fleet key, makes it the machine's memory and sends it back under the new key.
    Until then a re-homed machine's memory says **needs recovery key** and none of it is read.
  - The dashboard's Fleet page shows each machine's memory (in step, behind, needs recovery key,
    or unknown with the reason). Doctor's line for each machine says its memory and, when it
    needs the recovery key, the command; a recovery key that was never proven names
    `tenant fleet init`, which now asks at a terminal for the identity you kept and records the
    proof.
- **Fixed: on Windows, a Tenant with no console window opened one for every command it ran.** A
  Tenant started by the service manager has no console to share, so each command got a new
  console window. Such a Tenant now starts its commands with a console that has no window; one
  in a terminal, or with the logon task's hidden console, shares it as before.
- **Fixed: an MCP request cancelled the moment it was sent could run to the end.** A request is
  now registered as it is read; one cancelled before its handler starts is not run.
- **Fixed: a local MCP server could be left running after its tools were closed.** A server whose
  start was still under way at that moment went on to start afterwards and was never stopped.
  Closing now cancels a start that is under way and waits for it.
- **Security: a command that only looked read-only ran with no approval prompt (TEN-534).** With
  the exec permission on ask, a shell command that only looks at the machine (`ls`, `git status`)
  runs without a prompt, on the host and on Discord. The check behind that was fooled, in three
  ways, and all of these ran unasked.
  - **A note Tenant itself adds to a destructive command.** A command that contained the text
    `  [DANGER: ` was judged only up to that text and then run whole:
    `echo hi  [DANGER: x]; <any command>` passed as `echo hi`. An allow rule for the first part
    covered the rest, and a deny rule for the rest never saw it. The whole command is judged now.
    A command rule also reads the statement after a lone carriage return, where PowerShell starts
    a new one: an allow rule for the first statement no longer covers it.
  - **Commands that act.** `env <any command>`; on Windows a PowerShell method call or assignment
    such as `(Get-Item C:\x).Delete()`, a script block, `& (…)`, a second statement behind a lone
    carriage return, and a quote inside a quoted argument, which PowerShell passes on unescaped
    so that the program's own parser drops it (`git log '--out"put=FILE'` writes FILE);
    `-p`, which is `-PipelineVariable` for `Get-Date` and `echo`; `$(…)` or backticks in place of
    the command
    (`$(echo rm -rf x)`); an option that a substitution or a variable supplies or that quotes hide
    (`find . $(echo -delete)`, `find . '-delete'`); a `VAR=value` before the command
    (`PATH=… ls`); a path to a file with a read-only name (`./ls`); a word in other letters than
    its own (`HEAD` is not `head`); and the words that write, change a setting or start another
    program with some arguments: `sort -o` and `-T`, `uniq IN OUT`, `tree -o`, `find -fprint0`,
    `rg --pre` and `-z`, `file -C`, `-p` and `-z`, `git log|diff|show --output=FILE`,
    `git <command> --help` (man, or a browser on Windows), `git log --show-signature` and `%G`
    formats (gpg), `%(describe)` formats and `git describe --broken` (more git processes),
    `git branch --sort=signature` and `--sort=describe` (gpg, git describe), `git branch NAME`
    and `-f`, `git remote -v add`, `brew info --github`, `brew info` of a path, URL or
    `user/tap/formula` (brew runs the formula's Ruby, or fetches it), `brew deps --brewfile`,
    `date -s`, `hostname NAME`.
  - **Reads that go to another machine.** A network path (`\\host\share\file`, `//host/share`)
    sends your Windows sign-in to that host. It asks now wherever it is written, and so does a line
    in which one command prints a path for another to read: `$(…)` as the argument of anything but
    `echo` and `Write-Output`, a pipe into `Get-ChildItem`, `dir`, `ls` or `Get-Item`, and
    `-OutVariable` and its kin. So do the spellings that hide one from a plain reading on
    Windows: a quoted start with text after it (`cat 'x'//host/share/f` is `x` and
    `//host/share/f` to PowerShell), a quote inside a quoted argument of a program
    (`head '/"/host/share/f'`), a doubled quote inside a quoted part (`q"a"" //host/share/f"" b"`),
    `@FILE`, a file of arguments to Git for Windows' Unix tools, an empty variable that joins the
    text around it (`Get-Content \$null\host\share\f` reads `\\host\share\f`), a digit before a
    path glued to an option (`grep -1f//host/share/f`), the file after `grep -f` or
    `rg --ignore-file`, and a parameter name with a character PowerShell ignores in it
    (`-Out<U+0640>Variable`). `git remote show NAME` (it connects; `-n` does not),
    `Get-Process -ComputerName`, PowerShell's `WSMan:` drive, `/net/HOST` (an automount on macOS),
    bash's `/dev/tcp/…` also behind quotes, a backslash, a variable or a `~+` that the working
    directory spells (`/dev/t'c'p/…`, `~+/tcp/…` with workdir `/dev`), and `os_exec` with a
    network share or `/net/HOST` as its working directory (refused: `cd` there inside the command)
    are covered too.

  The check reads a line as sh and as PowerShell at once, and whatever it cannot tell asks. The
  plain forms run unasked as before: `env`, `env VAR=value`, `printenv`, `date`, `date +FORMAT`,
  `date -u`, `hostname`, `hostname -f`, `Get-Item`, `Get-ChildItem`, `git status`,
  `ls -la | grep x`, `echo $(whoami)`. Some harmless forms ask now: a read-only word given by its
  path (`/usr/bin/grep`), after `VAR=value` or in capitals (`LS`; PowerShell's own names such as
  `Get-ChildItem` are taken in any case); `date -d`; `$(…)` after any word but `echo`
  (`wc -l $(git ls-files)`); a variable among the arguments of any word but `echo` and
  `Write-Output` (`cat $HOME/.profile`, `-Stream :$DATA`; single-quoted `'$x'` is text and stays);
  `find`, `sort`, `git` and the other words above with an
  unquoted glob among their arguments (`find . -name *.go`: quote the pattern); a quote inside a
  quoted argument of any word but a PowerShell cmdlet (`grep "it's" f`); a quoted start with text
  after it (`cat 'a'b`); an argument that begins with `@`; `~user` in what `<` reads;
  `git branch --sort=contents:signature`; an option with a character outside ASCII; a subshell
  `(ls)`, backticks, `${HOME}` and anything else in braces, `::` anywhere, and characters outside
  ASCII that are not letters. A command rule
  (`/permissions add os_exec(date -d *) allow`) lets a form you use run unasked again.
- **Security: `tenant service` and `tenant update` asked for your yes only at the start of a line
  (TEN-534).** A command that stops, removes or replaces the Tenant an agent runs in is marked
  dangerous, so it waits for your yes even when exec is allowed (TEN-498). The mark was put only
  on a line that began with it: `cd /tmp; tenant update`, `sudo tenant service uninstall` and
  `sh -c 'tenant update'` ran as ordinary commands. It is found now in any command of the line,
  read as sh and as PowerShell, and behind `sudo`, `doas`, `env`, `nohup`, `exec`, `command`,
  `time`, `nice`, `timeout`, `xargs`, `eval`, `iex`, `start`, `Start-Process`, and a shell's `-c`,
  `/c` or `-Command`. Text stays text: `grep "tenant service" x.log` and `echo tenant update` are
  not marked. This catches the plain spellings, not every one: a variable, an alias or a script
  file still hides the command, and what holds then is the exec permission itself.
- **Fixed: a power cut could leave more files than `config.json` unreadable.** The fix that
  flushes a saved file to the disk before it replaces the old one now covers every file Tenant
  saves that way: the user profile, a research run's manifest, the Claude Code session ledger, the
  tool statistics, the route decision log, a log-book spool file, a downloaded Discord attachment,
  and a file rewritten by a privacy erase. Saving the user profile on Windows no longer deletes the
  old profile before the new one is in place. A temporary file that a power cut left behind in the
  middle of a save is removed by the next save of the same file.
- **Fixed: on Windows, a standard user's `tenant service` commands acted on someone else's
  service.** On a machine where an administrator had installed Tenant with `--system`, another
  account's `tenant service status` described that service, and its `tenant service uninstall`
  tried to stop it, failed with "Access is denied" and left the account's own task in place.
  Without `--system` the commands now act on your own logon task when you have one, and on the
  `--system` service only when that is all there is; `--system` always names the service.
  `tenant service status` says when both are installed. `tenant doctor` and `tenant update`
  follow the same rule.
- **Fixed: a scheduled shell job that a stop cut short was recorded as a failed command.** Its
  history said "command failed: exit status 1"; it now says "interrupted: Tenant stopped", as a
  scheduled prompt already did.
- **Tests no longer touch a service unit in your home directory (Linux, macOS).** On a machine
  with Tenant installed as a background service, the tests that run `tenant doctor --fix` rewrote
  the real systemd unit or LaunchAgent to start the test binary. Reproduced on Linux; the same
  code path exists on macOS. The Windows half was fixed earlier.
- **Fixed: on Windows, `tenant update`, `tenant service restart` and `tenant service stop` ended
  the background task without a clean stop.** They ended the scheduled task, which Windows does by
  killing the process, so every update and every restart left "The previous run ended
  unexpectedly" in the log as a critical error. They now ask Tenant to stop first and wait for it
  (up to 6 seconds), and end the task only if it is still there. The first update from 0.2.0-rc.3
  or earlier still ends the old version the hard way: it does not know how to be asked.
  `tenant service uninstall` asks first too. `--system` was not affected.
- **Tests no longer touch the service installed on the machine they run on.** On a machine where
  Tenant was installed as a background service, the tests that run `tenant doctor --fix` found the
  real service, saw that it did not match the test binary, and rewrote it to start the test binary.
  The tests now have no service manager to talk to unless one asks for it (`TENANT_LIVE_SERVICE=1`).
- **Fixed: on Windows, a restart, shutdown or logoff ended the background task without a clean
  stop.** The Tenant that `tenant service install` starts at logon was simply ended: no shutdown
  step ran, a turn in flight got no "I was stopped" notice, and the next start logged "The previous
  run ended unexpectedly". Windows was never telling it: a program that hides its console window is
  told about the end of a session through a window, not through its console, and Tenant listened
  only on the console. It now listens on a window of its own (never shown) and stops the way it does
  for any other stop request. Windows allows about five seconds; Tenant takes well under one when
  idle. `--system` was not affected: the service manager tells a service directly.
- **Fixed: a power cut soon after a settings change could leave `config.json` unreadable.** A save
  wrote the new file and renamed it into place without first flushing it to the disk. When the
  power went within seconds, the file could come back with the right name and length and nothing
  but zero bytes in it, and Tenant then refused to start (`config.json corrupt at line 1, column
  1`). Seen on Windows Server 2025 with the `--system` service. Every save of `config.json`, an
  agent's settings file, the cron jobs, the Discord cursor and the interrupted-turn record is now
  flushed before it replaces the old file; a flush that fails leaves the old file in place.
- **Fixed: `tenant service uninstall` and `tenant service stop` refused to run when `config.json`
  did not load.** They stopped at the same "config.json corrupt" error as everything else, so a
  service that could not start on a broken config could not be stopped or removed either. Both
  now print the error as a warning and carry on: neither needs anything from the config. `install`,
  `start`, `restart` and `status` still stop at the error.
- **Fixed: `tenant service install` on Windows needed an administrator.** From an ordinary terminal
  it failed with "Access is denied": the scheduled task was registered to start at any user's
  logon, which Windows allows only an administrator to set up. It is now registered for your
  account alone and needs no elevation. The same change removes two limits the old registration
  carried: Windows ended the task after 72 hours, and neither started it on battery power nor kept
  it running when the machine went onto it. A command line longer than 261 characters is no longer
  refused. `--system` is unchanged and still needs an elevated terminal.
- **Fixed: `tenant service restart` on Windows left Tenant running without its dashboard.** The new
  process started a fraction of a second after the old one was ended, found the dashboard's
  address still taken, and gave up on it, so `tenant service status` said "not reachable" and a
  `tenant update` would not have seen the new version come up. A `tenant serve` that starts now
  waits up to 10 seconds for an address that is in use.
- **The install scripts in a release no longer have their own header comment rewritten.** The
  release replaced the repository placeholder everywhere, including the comment that names it.
- **Log book housekeeping: a process whose lease has run out starts no further task.** The
  process holding the housekeeper lease learned it had lost it only at its next 30-second
  check. When that check was held up until the lease's minute had passed (a long write to the
  log book, the machine asleep), another process took the lease over while the first went on
  to the rest of its tasks. A run of the tasks now stops when its lease runs out, and what is
  left is done under the next lease. A task already under way is not interrupted.
- **Container sandbox: `full` networking no longer shares Docker's default bridge.** A command
  given `"network": "full"` ran on the default bridge, where every container without a network
  of its own lives and where containers reach each other: on a real engine a sandboxed command
  fetched a page from another container there. It now runs on a bridge Tenant makes for itself
  (`tenant-sbx`, created on first use) with traffic between its containers off. The command
  still reaches the internet, but not your other containers and not another sandboxed command.
  If a network of that name already exists and lets its containers reach each other, the
  command is refused with the fix (`docker network rm tenant-sbx`). Not changed: a `full`
  command can still reach services listening on the host itself.
- **Fixed: approving a write into a folder granted read-only never ran on a real engine.** When
  the container sandbox refused a write to a folder the profile grants under `read_roots`, an
  approved re-run added the folder as writable and kept the read-only grant, so the same folder
  was mounted twice and the engine refused to start the container ("Duplicate mount point").
  The refusal stood, so nothing ran unconfined, but the approval did nothing. A folder named by
  more than one grant is now mounted once, by its widest grant. Found by the first run of the
  live tests on a working engine.
- **Container sandbox: HOME is a folder of the container's own.** `HOME` was the work dir, so
  the dotfiles, caches and history a command wrote stayed in your folder on the host, where the
  next "fresh" container and your own tools read them. It is now `/home/sandbox`, a 64 MB
  in-memory folder only the sandbox user can enter, gone when the command ends. A command that
  needs a file to last writes it in the work dir.
- **Container sandbox: `cpu_seconds` is enforced, per process.** A profile's `cpu_seconds` is
  now a kernel CPU-time limit on each process in the container (`--ulimit cpu`). It stops one
  runaway process; a command that spreads its work over many is bounded by its time limit and
  by 2 CPUs, which is why `tenant sandbox probe` reports limits on this backend as partial.
- **Container sandbox: the image is built from `debian:trixie-slim`,** pinned by digest.
  bookworm, the base until now, left regular security support in July 2026. **Run
  `tenant sandbox image build` again after updating**: the backend never pulls or rebuilds on
  its own, and an image built before this keeps the old base.
- **SECURITY.md says who is trusted:** the local user account and every process running as it.
  The execution sandbox protects the host from a command the agent runs, not Tenant or the
  container engine from the user's other processes.
- **`tenant sandbox probe` and `tenant doctor` point out credential-looking names in `env_pass`.**
  A sandboxed command can read every variable it is passed, and with the network on can send
  it anywhere. When a surface's `env_pass` forwards a name that looks like a credential
  (`…_TOKEN`, `…_API_KEY`, `…PASSWORD`, `SSH_AUTH_SOCK`), both now say so, per surface
  (`env_pass_secret_looking` in `probe --json`). A hint only: the name is still passed, and a
  secret under a plain name is not noticed.
- **The container sandbox refuses a writable folder that is itself a git directory.** A bare
  repository (or a checkout's `.git` handed over directly) has its `hooks` folder and its
  `config` at the top of what the command could write, and git runs both on the host the next
  time it is used there. Such a folder is refused as the work dir and as a write root, with the
  reason; granted read-only it is allowed, and a folder that holds a checkout is unaffected.
- **The terminal app no longer sends a model's or a tool's text to your terminal as it came.**
  A carriage return or a terminal escape sequence in a command, in its output or in a model's
  answer could put words over the line before them, recolor or clear the screen, or retitle
  the window. An approval prompt drew the command the same way, so part of a command could be
  out of sight of the person approving it. An approval prompt now shows every character that
  draws nothing or moves other text as a marker (`⟨U+000D⟩`, `⟨U+202E⟩`), as the dashboard's
  approval card does. Everywhere else in the app, whole escape sequences (a tool's colors) are
  removed, a carriage return becomes a line break, and any other control character shows as a
  marker. What a slash command prints from stored text (a memory fact, a skill, a job's name)
  is treated the same way; where the app has styled such text its colors stay, but nothing in
  it can conceal text, move the cursor, clear or retitle. The dashboard and the log files
  already did this.
- **The container sandbox says when the engine cannot run anything, before the first command.**
  `docker version` and `docker info` answering does not mean the engine can start a container:
  a Docker Desktop stuck starting (seen after a WSL update) answers both, and every command
  then waited out its whole time limit and returned nothing. Before a surface first uses the
  `container` backend, Tenant now starts one check container through the same hardened
  arguments, with a 20-second limit. If it does not finish, the surface's commands are refused
  with that reason, and the check is retried with back-off. The check also reads what only a
  container can show: whether a user namespace is in effect (from its uid map) and the kernel it
  runs on. `tenant sandbox run` and `tenant sandbox selftest --backend container` apply the same
  check, and `tenant sandbox probe` reports its result.
- **The Security log says what boundary a sandboxed command had.** On the container backend,
  the record of a command (`4201`) now holds the image and the id its tag resolved to, every
  folder mounted and whether it was writable, the network, the limits, the names (never the
  values) of the environment variables passed in, how much the command
  wrote and whether that hit the output cap, and how long it took. **Corrected:** the record
  of an approved re-run (`4203`) said the command "ran outside" the sandbox even when the
  re-run stayed in a container with one more grant; it now says where it ran.
- **`tenant sandbox stop-all`** removes every Tenant sandbox container on the engine, running
  or not: the stop for a sandboxed command that must end now. It needs no running hub. A
  command whose container is removed ends as killed.
- **An approval to re-run a refused command covers that exact run.** When the container
  sandbox refuses a command and you approve the re-run, Tenant now takes a digest of what
  will run (the command, every mount, the network and the image the tag points at) before it
  asks and again just before the run. If they differ, because an image was rebuilt or a folder
  re-pointed while the prompt waited, nothing runs and the refusal says to ask again. The
  re-run is then started from the description that was hashed, with the image named by its
  id, not its tag, so nothing is looked up again between the last check and the run.
- **`config.json` can name the container sandbox's engine** (`container.endpoint`,
  `container.tls_cert_dir`): a local pipe or socket, `ssh://`, or `tcp://` with mutual TLS.
  **Fixed with it:** a `tcp://` endpoint that passed the TLS check was still launched with no
  TLS flags, because the environment that turns TLS on is stripped from the docker CLI, so
  the connection was plain HTTP. A `tcp://` endpoint is now always launched with
  `--tlsverify` and the CA, certificate and key from the certificate folder, and is refused
  when any of the three is missing.
- **The WSL kernel floor is checked against the kernel that is running.** `wsl --version` prints
  the installed kernel, and a WSL VM started before an update keeps running the old one. The
  floor (6.18.40) now also applies to the kernel the engine reports: below it refuses, and a
  running kernel that only trails the installed one is a warning that says to `wsl --shutdown`.
- **A container profile that would mount a protected folder is refused when the settings load.**
  A `container` entry in `exec_profiles` whose `read_roots` or `write_roots` contain, equal or
  sit inside Tenant's config or data dir, its binary, a credential store or the entry's own
  `deny_roots` was accepted, and every command on that surface was then refused at run time.
  The entry is now skipped at load with a message naming the grant and the folder.
- **Fixed: the start-up sweep never found a previous run's containers.** Commands were labelled
  with a per-process id, and the sweep looked for the install's id, so a container left by a
  crash was never removed. Every surface's container backend now uses the install's id.
- **`tenant doctor` has a `container sandbox` row.** When a surface uses the `container`
  backend it says whether the backend can run a command right now and, when not, the reason
  and the fix for that reason (install Docker, start it, update it, build the image, restart a
  stuck Docker Desktop). It starts one short-lived container. It is skipped when no surface
  uses the backend.
- **The container sandbox closes its wave-1 hardening** (TEN-455, TEN-456, TEN-457,
  TEN-458, TEN-460; TEN-438). The `docker` CLI is launched from its absolute path with
  an environment Tenant builds (no inherited `DOCKER_*` or `NVIDIA_*`) and an explicit
  `-H` endpoint; `tcp://` without TLS is refused, and so is running while a daemon answers
  on `127.0.0.1:2375` in the clear. The version floors now refuse rather than warn:
  Engine 29.4.3, runc and containerd by release line, Docker Desktop 4.44.3, and on
  Windows WSL 2.7.10 with a kernel at or past 6.18.40; seccomp must be on. The probe
  records the versions, user-namespace, LSM, seccomp profile and image store, caches only
  success and retries a failure with back-off. `"enforce": false` is rejected at load for a
  `container` profile. Every container runs under an in-container `timeout -s KILL`, so one
  orphaned by a crash still dies; the sweep at `serve` start removes a previous run's
  containers by a stable per-install label. Output is bounded as it arrives on every
  backend, and over the cap the container is killed at once. On an approved escalation the
  command re-runs INSIDE a fresh container with one grant (the refused folder, or the
  network for that command); only a Windows program a Linux container cannot run goes to the
  host, and an `sh` line is never handed to PowerShell. The approval prompt shows the exact
  command with control characters escaped and a risk line Tenant writes. Mount sources:
  a trailing dot or space anywhere is refused, 8.3 short names are expanded, and a socket,
  pipe or device is never a source. Refusal attribution reads a streaming scan of the whole
  output rather than the capped display string, so a refusal line that lands past the cap
  is still attributed and the same command is labelled the same way every run.
- **The MCP SSE gateway now requires a token. Existing SSE clients need one more setting.**
  `tenant mcp-memory --sse-addr` had no authentication, and a loopback bind kept other
  machines out but not the other programs on the same one, or a browser on it: the gateway
  took a request under any Host, from any Origin, with any content type. Now every request
  must carry `Authorization: Bearer <token>`, the token in `<config>/gateway-auth-token`,
  created on first start (the gateway prints where it is, never the token). It is a token of
  its own, not the dashboard's. **To update a client:** add that header to its configuration
  for the gateway; a client that cannot send a header should launch `tenant mcp-memory` over
  stdio. The gateway also refuses a request that carries another site's Origin; on a loopback
  bind it answers only to `localhost` or a loopback address on its own port; a message must be
  `application/json`; and each stream's endpoint carries a random `sessionId` that every
  message must send back. `tenant security audit` now reports the listener as authenticated,
  and `tenant doctor` audits the token file with the other secrets.
- **The SQL tools stay inside their database, and the engine enforces it.** `sql_query` was
  read-only by how a statement read: the text was classified and anything but a read refused.
  The classifier tracked `'…'` and `"…"` quoting but not SQLite's `` `…` `` and `[…]`, so a
  comment marker inside such a name could hide a second statement from it. `sql_query` now runs
  on a connection the database engine holds to read-only (`query_only`), so a change fails
  there whatever the text looked like, and the classifier reads all four quoting forms.
  `ATTACH`, `DETACH` and `VACUUM INTO`, which reach a file other than the configured database
  (and create one that is not there), are refused outright with no request for approval, and the
  connection itself allows no attached database. A plain `VACUUM` still runs once confirmed.
- **Fixed: `tenant serve` could stay down forever when the docker CLI stalled.** At start-up
  serve removes containers a previous run left behind, and it waited on that `docker` call
  with no time limit: on one Docker Desktop `docker ps --filter label=…` never returned, so
  the dashboard never came up and nothing was logged. The sweep now gives up after 20 seconds
  with a warning that says the CLI did not answer, and start-up carries on. It lists
  containers with their label and matches this install's itself, which answers where the
  engine's label filter did not. Every other `docker` call that manages the engine (version,
  info, image check, removal) has a 15-second limit too, and a stalled image check is no
  longer reported as "the sandbox image is not built".
- **The dashboard never fetches an address an answer names.** An image in the agent's Markdown
  was drawn as a picture, which a browser requests the moment the page shows it: a request no
  approval saw, to an address the model wrote. An image is now a link showing its description,
  and the dashboard's Content-Security-Policy loads images and media from the dashboard only
  (`img-src 'self' data:; media-src 'self'`).
- **Fixed: the Memory page's Knowledge time view** showed every fact ever learned, replaced
  and removed ones too, and on a narrow window its table squeezed the column names into
  one letter per line. It now shows the live facts, with **Show history** for the rest; the
  table's names and dates stay on one line and the table scrolls sideways instead; a long
  timeline scrolls inside its card.
- **Fixed: a judge send-back is said in the chat.** When the judge in the loop sent an answer
  back, the terminal app's answer just stopped, with the reason only in the activity feed. The
  chat pane now says "judge: sent back (1/3): … — your agent is trying again".
- **The agent answers with components** (TEN-517). A new built-in tool, `ui_render`, lets the
  model say "show this as a table" or "as steps" and hand over the data; Tenant draws it from
  a fixed template in the terminal app (a boxed table, ✓ ◐ ○ lines) and on the dashboard's
  Chat page (a real table, a checklist), in place of the tool card, and again when the page
  reloads. The model never writes markup: every value is text, and a bad shape gets a one-line
  error it can act on ("table: 14 columns, the limit is 12"). Caps: 12 columns, 200 rows, 200
  characters a cell, 50 steps, 32 KB a call. The `ui` tool is on by default, like the memory
  tools (`/disable ui` turns it off), and needs nothing configured. A new eval subset, `ui`, checks the model reaches
  for a component when the answer is one and stays in prose otherwise.
- **Choices and forms** (TEN-518). Two more component kinds: `choices` draws the likely next
  messages as buttons (numbered options in the terminal), and `form` asks you for values
  (text, a number, a pick list, a slider) with a Send button (one prompt per field in the
  terminal). A tap or a filled form starts your next turn, and that message is marked
  "via choices" or "via form" on both surfaces, so a model-written line is never mistaken
  for something you typed. The server fills the form from the saved call, checks every value
  (the option exists, the number is in range, the pick is one of the options) and refuses a
  message that would be a slash command; a form whose call is gone (a restart, a compaction)
  says so and sends nothing.
- **Activity beside the chat** (TEN-520). The dashboard's Chat page has a **Show activity**
  toggle that opens a rail of what the agent is doing as it happens, the same lines as the
  terminal app's feed: each tool call and its result, the judge's checks, a compaction, an
  error. The rail keeps the last 200 lines and sits below the chat on a phone.
- **The dashboard listens on port 9770 by default** (was 8770). On macOS, Apple's `sharingd`
  owns 8770, so a fresh install's `/dashboard on` failed with "address already in use". A
  saved `dashboard.addr` is unchanged; a config without one now serves at
  http://127.0.0.1:9770.
### Added
- **1Password.** A plugin over the `op` command-line tool with a service account token
  (`/configure onepassword`, the Integrations page, or `OP_SERVICE_ACCOUNT_TOKEN`). The agent
  lists the vaults the account can see (`op_vaults`) and the items in one (`op_items`), reads
  an item's shape with every concealed value replaced by its `op://` reference (`op_item`),
  and resolves one reference at a time with `op_read`, which is gated under `egress` and
  logged by reference, never by value; the value is scrubbed from the log book the moment it
  is read. `op_create` and `op_edit` (the `write` category; `allow_write` or
  `--onepassword-allow-write` skips the per-action prompt) keep credentials, with
  `generate_password` so a new password never passes through the model; `op_delete` archives
  and always asks. A `vaults` setting fences the agent to named vaults on top of the account's
  own scope. `tenant onepassword whoami|vaults|items <vault>` is the read-only one-shot;
  doctor checks the CLI and the token.
  A 1Password secret carries its own provenance, `vault`, and an unattended run may type it
  into a web form or a command line but never quote it into a message, a post, a mail, a
  search, a URL, a remote service or a helper's task: the unattended approver refuses that.
  It signs in one of three ways, the `auth` setting. A service account token is the default.
  `connect` calls a self-hosted Connect server's REST API (`connect_host` and a stored
  `connect_token`, or `OP_CONNECT_HOST` with `OP_CONNECT_TOKEN`): no `op` binary and no rate
  limits, and since a Connect server cannot archive, `op_delete` is refused there. `desktop`
  goes through the 1Password app on the machine (an optional `account`), which asks the
  person there to approve with Touch ID, Windows Hello or the system password. Desktop
  sign-in is for a person at the machine: a scheduled, Discord or iMessage turn is refused
  before `op` is started, the `vaults` allowlist is required because the approval covers the
  whole account, and doctor only lists the app's accounts, so it never raises a prompt.
- **`request_timeout` on a provider.** `providers.<name>.request_timeout` (a Go duration such as
  `"90s"`) bounds one request to that endpoint, chat, tokenize or embed. A request that hits it
  fails as endpoint-down, so fallbacks and health gating move on instead of the turn hanging on a
  stalled server; the caller's own cancel is still a cancel. Empty means no limit beyond the turn's.
  OpenAI-compatible kinds only; it reaches named helpers and fallback bindings too.
- **The Judge** (TEN-510, TEN-516): a decision model checks the agent's work. Cloudflare's
  Clef-flash (or any System One server) triages each prompt while the model works; for a
  task, it checks the answer is fully done, covers every part, claims nothing no tool did,
  and didn't hand the work back. A failing check sends the work back with a note (up to 3
  rounds by default, `/judge rounds` 1 to 5, not counted against the step limit); an answer
  it still can't confirm goes out marked "judge: not confirmed". A denied or blocked action
  is never retried for it. Off by default: `/judge use clef` prompts for the server's
  address, then its key (hidden; blank for none), asks the server once and switches on;
  `/judge on|off`; the Model page's Judge card. Saved as `judge` in config.json (kind,
  server, model, rounds), the key in credentials.json. Not applied to `/goal`, helper, peer
  or research turns; fails open with a one-minute pause when the judge is down.
- **The Quality model** (TEN-516): the model that grades Quality evals has its own name
  everywhere. `/eval model [set <kind> <model> [endpoint]|clear]` (was `/judge set`),
  `tenant eval --quality-model` (was `--judge-model`), the Quality page's "Quality model"
  card, and `improve.quality_model*` in config.json; the old `judge_loop` and
  `improve.judge*` keys migrate on load and are never written again.
- **The dashboard's Chat streams and reads like a document** (TEN-515). The answer appears as it is
  written and settles rendered from Markdown (headings, lists, code, tables, task boxes). A tool call
  is a card you can open to what was asked and what came back, with a link into Logs; a failed call
  is open already. Opening or reloading the page shows the last 20 exchanges of the conversation.
- **A readable terminal app** (TEN-513, TEN-514). The conversation is turn blocks: a header with
  who, the time and the model, the text rendered from Markdown (headings, lists, code panels,
  tables, task boxes) and wrapped at the pane width, never mid-word. A tool call is one row inside
  the block where it happened; Ctrl+O opens every row to its arguments and result, and a failed call
  is open already. The line under a running answer says what the agent is doing and for how long.
  The activity feed groups a turn's lines under a header with the time and the first words of the
  message, and the context line is one line. `/feed off` and Alt+F hide the feed for a wider
  column; the choice is saved (`tui.feed` in `config.json`). The Markdown engine is an in-house
  copy of goldmark under `internal/md` (MIT; see its README), so `go.mod` gains nothing.
- **Images** (TEN-493): paste a picture into the TUI with Ctrl+V (Alt+V on Windows), drag image
  files in, or `/image <path>`. Discord and iMessage photos reach the model too, and
  `os_read_image` and `web_screenshot` now show it the picture instead of describing the file.
  Images are saved under `<data>/attachments`, scaled to 1568 px, and kept for the last three
  exchanges (at most 8 per request). Tenant tests at launch whether a local model can see;
  `/model vision on|off|auto` (`vision` on the provider) overrides it, and a model that can't
  gets a note instead. `tenant eval --subset vision` checks it end to end.
- **GLM tool calling** (TEN-503): a `glm` tool-call format for Z.ai's GLM-4.5 family and later (the
  `<tool_call>name` + `<arg_key>`/`<arg_value>` shape its chat template writes), picked by name
  for models called glm/chatglm, offered in setup, the dashboard and `/model`, and tried by the
  server check. The parser keeps typed values, reads a call cut off by max_tokens, and hands the
  JSON and Hermes shapes some GLM servers write to the Qwen parser.
- **iMessage passive and respond modes.** `/imessage passive` reads every text and answers
  none; `/imessage respond` answers the allowlist and `imessage.operator`, and only in a
  1:1 iMessage chat. Group chats, SMS and RCS never drive the agent, and can no longer
  approve an action either. A text the responder doesn't answer is logged as an
  `imessage` Observed event (log alerts can act on it) and shown in the activity feed.
  The dashboard's Access page has the same switch. The allowlist now applies per message,
  not when the responder starts.
- **iMessage shows Contacts names.** On a Mac, chats, messages, the activity feed and
  Observed events name the sender from Contacts ("Pat Lee (+15550001111)"), read-only
  from the Contacts database under the same Full Disk Access. Allowlists still match
  the handle.
- **Turning iMessage on gives the agent its tools at once.** `/imessage passive` or
  `respond` (and `/enable imessage`) turn on the `imessage_*` tools live: no
  `--imessage` flag, setup or restart.
- **The iMessage responder runs over BlueBubbles.** Set a BlueBubbles URL and it answers
  texts from any OS, not only a Mac. Messages on a Mac stays the default.
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
- **`team_await` returns on news, not on a clock** (TEN-529). It used to block until every
  helper had finished or three minutes had passed, then report "timed out" with nothing new and
  get called again: on one machine 101 of 127 awaits returned at exactly 3m0s with helpers still
  working, a full-context planner call each. Now it returns as soon as a helper finishes (that
  result comes back) or a helper messages the orchestrator (the message comes back, drained so
  `team_check` doesn't repeat it), and says who is still running. The orchestrator awaits again
  for the rest. Only this conversation's helpers wake it, as `team_await` already reported only
  them. The ceiling with no news at all is 30 minutes; `tenant orchestrate --await-timeout` now
  means that ceiling. A stuck helper is still the run budgets' job, and its cancel wakes the await.
- **Team-run limits are off by default** (TEN-508). Tokens per run (was 500,000), estimated
  cost ($25), time (30m), tool calls per run (256) and per answer (64), and helpers per run
  (16) no longer stop a run unless you set them under `budgets` in config.json. A value you
  already set keeps working. The cost rates no longer default to guessed hosted prices
  ($15/$75 per million), so there's no cost estimate until you set them. The machine guards
  stay on: 4 helpers and 4 browsers at once, 1000 bus messages. `/agents limits` and the
  dashboard's Helpers page show every limit, its value and its key. Alert investigations
  keep their own ceilings; their $5 cost ceiling, like any cost limit, needs the rates.
- **The surfaced tools stay put for a whole session** (TEN-505). Tool ranking used to pick
  a new top-K for every turn, and since the tools array sits at the top of the prompt,
  any change threw away the model server's prefix cache for the whole conversation.
  Now ranking only adds tools: earlier picks keep their place, new ones are appended,
  and tools loaded with `load_tool` stay too, so the array changes only when it grows.
  A turn where ranking fell back to the whole catalog (a failed query embedding) is
  sent but doesn't stick. `/clear`, `/compress` and automatic compaction start a fresh
  set. `per_turn_tool_ranking: true` in config.json brings back per-turn ranking.
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
- **A helper on another backend can spawn again.** A named agent whose provider was not the
  main one (`agents.QA.provider: claudecode` with a vLLM main model) failed every spawn with
  `resolve embedder role: no factory registered for backend "vllm"`: its router registered the
  shared embedder's profile but not the embedder's backend, so only a helper on the same
  backend as the embedder could be built. The embedder's backend is registered on every
  helper's router now.
- **Cron no longer hangs Tenant on a daylight-saving fall-back day.** The scheduler's next-run
  search rebuilt each hour with `time.Date`; on a day where 01:00 happens twice (US: 2026-11-01)
  that picked the first 01:00, added an hour, landed on the second 01:00, and picked the first one
  again, forever. A `cron_add` whose first run fell on or after that day spun at 100% CPU with the
  scheduler locked, so the turn never ended and `cron_list`, the ticker and the cron pages all
  blocked; from Oct 31 every daily job would have done the same at its reschedule. Hours now
  step in absolute time, every step is guaranteed to move forward, and a regression test crosses
  both the fall-back and the spring-forward day.
- **The Judge no longer sits out long turns.** A check's state could reach 56,000 characters
  (100 tool calls), which a 9B judge on a pair of Tesla T4s takes 20 s to read, and the client
  gave up at 10 s: `judge unavailable; answers go out unchecked` on every long turn. The state is
  now budgeted at 12,000 characters (the request and the answer at 3,000 each, then the newest
  tool calls, outputs for the newest 8), it is sent request-first so a truncating server drops old
  tool calls rather than the ask, the client waits 30 s, and each `⚖ judge` line says what the
  check cost (`· 3114 tokens, 3.4s`).
- **A quick command's output is never lost.** `os_exec` on the host could run `printf ok` and
  get `(no output)`: the sandbox's process starter let Go's `Wait` close the stdout pipe the
  moment the child exited, before the reader had drained it (it showed as a flaky
  `TestLoggingEndToEnd` on CI). The process now owns its stdout pipe, `Wait` leaves it alone,
  EOF comes when the last writer is gone, and a kill closes it.
- **A pasted image shows at once, and Backspace removes it.** In a terminal narrower than
  the footer (about 140 columns once tokens are counted), the footer wrapped and the
  input stopped redrawing, so a pasted `[image N: …]` tag appeared only after Enter. The
  footer now fits any width, says how many images are attached, and Backspace right after
  a tag deletes the whole tag and its image. A pasted image path with spaces attaches as
  one file. On a Mac, use Ctrl+V for images: terminals keep Cmd+V for their own text
  paste (the footer says "Ctrl+V image"). `--vllm-tool-format`, `tenant model add
  --tool-format` and `/model add` now list `glm` in their help.
- **A named self-hosted model gets its real context window** (TEN-507). Tenant read a
  vLLM server's `max_model_len` only when the provider's model was left blank, so naming
  the model (e.g. a 500K `glm-5.3-flash`) held it to the 128K default. Launch,
  `/model use`, fallbacks, planner-big, agent profiles and the judge now look the named
  model up in `/v1/models` and use its length (and the served casing of its name); an
  unlisted model or an unreachable server keeps the default.
- **Waiting on a job no longer cuts the turn short** (TEN-506). The repeat guard now
  counts a round as a repeat only when the same tool calls return the same results (as
  the model sees them, after the context cap), so a poll whose output changes keeps
  going until the loop ceiling. It trips after 8
  identical rounds instead of 3, is set live with `/ceiling repeat <n|off|default>` (0 = off) or the dashboard's Model page (saved as
  `repeat_guard`), and is reported as "stuck repeating the same tool calls" with its own
  event (turns 2004), not as "loop ceiling hit".
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
