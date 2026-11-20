# Changelog

Notable changes to Tenant. Releases are tagged `vX.Y.Z`; their binaries are on the
Releases page.

## Unreleased

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
