# Change: the execution sandbox runs by default, in dry run (TEN-345 / TEN-347)

**Branch:** `audit/exec-defaults` (on top of `audit/linux-sandbox`) · **Status:** implemented,
tested live on macOS, cross-built and vetted for Linux and Windows. The Windows WSL path is
unit-tested only: it still has to be verified on a Windows box (see
[Windows (the R340)](#windows-the-r340)). Not yet deployed.

## What changed

**1. Commands run under the sandbox by default, in dry run.** With no `exec_profiles` entry,
every surface (`local`, `discord`, `imessage`, `cron-exec`, `cron-shell`, and sub-agents, which
take their parent's) now uses the `restricted` backend in **dry run**. In a dry run the sandbox
runs the command first. If it refuses something (a write outside the allowed folders, a read of
a credential store, a network call where the network is off, a Windows program), the command runs
again **outside** the sandbox, and the refusal is logged. No approval is asked for, and nothing it
recognises is blocked; [Known limits](#known-limits) lists the refusals it can't recognise, which
still fail. `cron-shell` also defaults to **no network**. In a dry run that is only logged too.

**2. Where the sandbox can't dry-run, commands stay on the host, and every report says why.**

| OS | Built-in default | Why |
|---|---|---|
| macOS | `restricted` (Seatbelt), dry run | Its refusals are recognised. Tenant checks once at start that `sandbox-exec` works, and falls back to the host if it doesn't. |
| Windows | `restricted` (bubblewrap in a WSL2 distro), dry run | Owner decision 2026-09-29. Commands run as **Linux `sh`**, not PowerShell. See [Windows (the R340)](#windows-the-r340). Without WSL, a distro or bubblewrap in it, Tenant falls back to PowerShell on the host and says why. |
| Linux | `host` | Under bubblewrap a read of a hidden folder (`~/.ssh` for `git`) fails with nothing to recognise, so it is never re-run, and `/tmp` is private to each command. Both need fixing first. Without `bwrap` installed, the reason says that instead. |

On the host nothing is confined, as before. The reason shows at startup (a `sandbox:` line in the
feed), in `tenant doctor` (a WARN), in `tenant security audit` (the `execution.host` finding) and
in `tenant sandbox probe`. You can still name `restricted` yourself on Linux (below).

**3. Commands get a clean environment.** A command no longer inherits all of Tenant's
environment. The old default, which dropped only variables named like `KEY`, `TOKEN` or `SECRET`,
is gone. A command now gets a safe base set:
- everywhere: `PATH`, `HOME`, `USER`, `LOGNAME`, `USERNAME`, `SHELL`, `LANG`, `LANGUAGE`, `LC_*`,
  `TZ`, `TERM`, `TMPDIR`, `TEMP`, `TMP`;
- on Windows also: `SystemRoot`, `SystemDrive`, `windir`, `ComSpec`, `PATHEXT`, `PSModulePath`,
  `USERPROFILE`, `HOMEDRIVE`, `HOMEPATH`, `APPDATA`, `LOCALAPPDATA`, `ProgramData`,
  `ALLUSERSPROFILE`, `PUBLIC`, `ProgramFiles`, `ProgramFiles(x86)`, `ProgramW6432`, the
  `CommonProgramFiles` trio, `USERDOMAIN`, `COMPUTERNAME`, `NUMBER_OF_PROCESSORS`,
  `PROCESSOR_ARCHITECTURE` and `OS` (names match in any case).

It also gets whatever its profile lists in `env_pass`, plus `env_set`. This applies to the host
backend too, so `os_exec` on the host no longer passes API keys, tokens, database URLs or proxy
passwords to commands. Inside WSL a command gets WSL's own environment, which does not carry
Tenant's variables. Local MCP servers (`mcp_servers`) keep their own environment rules.

**4. One switch turns enforcement on.** Set `exec_profiles.default.enforce` to `true`. It applies
to every surface unless that surface's own entry sets `enforce` itself or names a `backend`. An
entry that names a backend keeps the rules it had before this change: enforce on, network
inherited. So a profile you already wrote does not quietly drop to a dry run.

**5. "Always" on a sandbox prompt now saves only the folder that was refused.** An enforcing
profile asks before re-running a refused command outside the sandbox (`os_exec_unsandboxed`).
Before, "always" on that prompt turned the whole `sandbox` category to allow, so every later
refusal ran outside the sandbox without asking. Now "always" approves this run and saves the
refused folder as an exception for that one surface, under `exec_exceptions`. It never switches
the backend, never opens the category, and never saves a tool rule. The prompt says in advance
what "always" will save, or why it will save nothing:
- A network refusal saves nothing: no backend can allow a single host yet.
- A Windows program saves nothing: it only ever runs outside the sandbox.
- A refusal inside Tenant's own folders or a credential store saves nothing: no exception opens
  those.
- A refusal of your home folder itself, or anything above it, saves nothing: that would be too
  broad.
- A refusal whose output names no absolute path saves nothing, and neither does a folder that
  does not exist yet.

On Windows, a folder the WSL sandbox names (`/mnt/c/...`) is saved as its Windows path
(`C:\...`).

No agent tool can write exceptions. The file tools refuse the settings file, and only your
decision on a prompt saves one.

**6. Presets.** `safe` denies exec, `balanced` asks, and `full` allows it, all inside the sandbox.
`full` now leaves `sandbox` on **ask**. It used to allow it too, so every refused command re-ran
outside the sandbox without asking. The audit still reports `permissions.host-exec-allow` as
critical when exec is allowed on a surface whose commands really run on the host. A restricted
profile, dry run included, no longer triggers it.

**7. The reports show what each surface really runs under.** `tenant security audit` (the
`execution` block, now per surface), `tenant permissions effective` (`execution.profile`) and
`tenant doctor` (the new `exec sandbox` check) resolve each surface's profile the way the spawn
site does. They then ask its backend what it enforces on this machine, instead of printing
`host` / `none`. New audit warnings:
- `execution.dry-run`: the sandbox confines nothing yet;
- `execution.host`: which surfaces, and why;
- `execution.unavailable`: an enforcing profile whose backend is missing, so its commands don't
  run.

**8. Unattended violations.** Cron surfaces still never escalate. With enforcement on, a refusal
fails the job. In a dry run it is logged. The built-in `sandbox-unattended` alert rule already
puts a banner in the feed. Now every cron prompt job's summary, including the daily brief, also
starts with a line when there were violations since that job last ran:
`🛡 SANDBOX: N sandbox violation(s) since the last run …`. The count covers refusals,
would-be refusals and escalations: sandbox events 2001-2003.

**9. A dry run never blocks a command because its backend is missing.** If a dry-run profile names
a backend this machine lacks, the command runs on the host and `sandbox/2005` is logged. An
enforcing profile still fails closed, as before (`sandbox/3001`).

**10. macOS backend fixes the dry run needed.**
- With the network cut, most tools report a blocked lookup or connect as their own failure
  ("Could not resolve host"), not as "Operation not permitted". Those failures now count as
  network refusals, so they re-run (dry run) or escalate (enforcing) instead of just failing.
- In a dry run, `HOME` and `TMPDIR` are no longer pointed into the work dir, so a cron job's
  `cd ~/x` works and is not silently broken.

## Windows (the R340)

**What changes on deploy.** On a Windows box with WSL2, a distro and bubblewrap in it:
- **The agent's shell becomes Linux `sh`, in WSL.** The `os_exec` tool description now tells the
  model this. Commands run in `sh` (bash-compatible), not PowerShell or `cmd`. Windows paths are
  under `/mnt/<letter>` (`C:\Tenant` is `/mnt/c/Tenant`).
- **Every path runs the same `sh` command line.** That covers the sandboxed run, the dry-run re-run
  after a refusal, and an approved `os_exec_unsandboxed`. Outside the sandbox the command runs as
  `sh` in the same distro, without bubblewrap. It never switches to PowerShell.
- **Windows programs run only outside the sandbox.** An `.exe` (`powershell.exe`, `cmd.exe`,
  anything else through WSL interop) cannot start inside it: the sandbox hides `/run`, where WSL
  interop listens. The refusal reads "Windows programs run only outside the sandbox — ask to run
  it outside".
  - In the dry run such a command re-runs outside automatically.
  - Once you enforce, it asks for approval (`os_exec_unsandboxed`), so Windows-specific tasks work
    after an approval.
- **A surface that runs elsewhere says so.** If an operator pins a surface to the host, its commands
  run in PowerShell there. Each such result starts with one line, `[ran in PowerShell, on this
  Windows host]`, and the reverse case says `[ran in Linux sh, in the WSL sandbox]`.
- **Cron shell jobs run as `sh` too.** Existing jobs are `cmd` syntax, and nothing translates them.
  Startup (a `sandbox:` feed line) and `tenant doctor` (a WARN) list every cron shell job that will
  now run under `sh`. For each one, either rewrite it for `sh`, or pin `cron-shell` to the host,
  which keeps `cmd /C` exactly as before:
  ```json
  "exec_profiles": { "cron-shell": { "backend": "host" } }
  ```
  Pinning works per surface: `local`, `discord` and the others can be pinned the same way. Once
  every job is rewritten, remove the pin.
- **Scheduled prompt jobs that need Windows programs** (`cron-exec`) work in the dry run, because
  the re-run goes outside. Once you enforce, nobody is there to approve, so an unattended
  escalation is always denied. Pin `cron-exec` to the host if such jobs must keep working.
- **In the dry run the Windows drives stay visible**, read-only, so reading a Windows file works.
  A write outside the allowed folders is refused and re-run outside. Once you enforce, the drives
  are hidden except the work dir and the write roots and exceptions.

**Prerequisites.**
- WSL2, a Linux distro, and the `bubblewrap` package installed in that distro
  (`sudo apt install bubblewrap`). Unprivileged user namespaces must work, which is the WSL2
  default.
- **Tenant must run as the Windows user who owns the distro.** WSL distros are per user: a Tenant
  service running as SYSTEM sees none, and falls back to PowerShell on the host.
- `TENANT_SANDBOX_WSL_DISTRO` picks a distro when the default one has no bubblewrap.
- Check with `tenant sandbox probe`: the surfaces should read `restricted` with no
  "UNAVAILABLE". Also run `tenant sandbox selftest` from the Linux build inside WSL
  (`wsl -e /path/to/tenant-linux sandbox selftest`).
- Tenant picks the default once, at start. After installing WSL or bubblewrap, restart it.

**If WSL is missing,** the default falls back to PowerShell on the host, and the model is told
PowerShell. Startup, `tenant doctor`, `tenant security audit` and `tenant sandbox probe` all say
why, for example "WSL is not installed" or "no WSL distro has a working bubblewrap sandbox".

**The dry run runs a refused command twice.** The first run, inside the sandbox, can already have
done part of its work before the refusal. Examples: an append in the work dir, or a network call
where the network is allowed. That part happens again when the command re-runs outside. Vet
non-idempotent cron jobs with `tenant sandbox run --surface cron-shell -- '<command>'` first.

## How to read the sandbox log
The dashboard's **sandbox** tab, or `tenant logs query "log:sandbox"`:
- `sandbox/1` and `sandbox/1001`: every command, with its backend, surface and exit.
- `sandbox/2002` *Dry run: … would have refused*: the entry to read during the dry-run weeks. It
  names the surface and the refusal (`filesystem: …`, `network: …` or `program: …`). The
  command's own output names the file.
- `sandbox/2001` refused (enforcing), `2003` approved to run outside, `2004` an exception saved,
  `2005` backend unavailable so a dry run ran it on the host.
- `sandbox/3001` backend unavailable, and the profile enforces, so nothing ran.

`tenant logs query "log:sandbox id:2001-2003 since:7d"` lists every violation of the last week.
`tenant sandbox probe` shows each surface's profile, roots and exceptions.

## Operator how-to
All of this goes in `<config-dir>/settings.<agent>.json`. The running hub re-reads the file within
about 3 s, with no restart.

**Give commands an environment variable** (here to every surface; use a surface key for one):
```json
"exec_profiles": { "default": { "env_pass": ["JAVA_HOME", "HTTPS_PROXY"] } }
```

**Add an exception by hand** (a folder a surface's confined commands may write to), or remove one
by deleting its line:
```json
"exec_exceptions": { "local": ["~/projects"], "cron-shell": ["C:\\Tenant\\reports"] }
```

**Turn enforcement on** once the log shows only refusals you want:
```json
"exec_profiles": { "default": { "enforce": true } }
```

**Keep one surface on the host** (on Windows: PowerShell, and `cmd /C` for cron), or opt out
everywhere with `"default"`:
```json
"exec_profiles": { "cron-shell": { "backend": "host" } }
```

**Use `restricted` on Linux anyway**:
```json
"exec_profiles": { "default": { "backend": "restricted", "enforce": false } }
```

## Known limits
- **A dry run can still repeat side effects.** A refused command runs twice. Whatever it did
  before the refusal (a network call, an append) happens again outside the sandbox.
- **A dry run misses refusals it can't recognise, and those commands fail instead of re-running:**
  - on macOS, a command that hides its errors (`2>/dev/null`);
  - on macOS, services the sandbox blocks, such as the clipboard (`pbcopy`/`pbpaste`), `open`,
    `defaults` and `osascript`, whose tools report the block as their own failure;
  - on Windows (bubblewrap in WSL), a read of one of Tenant's own folders or a credential store,
    which the sandbox hides as an empty folder. Also, `/tmp` inside WSL is private to each
    command.

  Put a surface that needs them on the host, or accept the failure.
- **Enforcing on Windows hides the Windows drives** except the work dir, the write roots and the
  exceptions. A command that reads a hidden file sees it missing, and does not ask.
- **Linux stays on the host** until its backend can dry-run without breaking commands.
