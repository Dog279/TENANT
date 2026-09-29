# Change: the execution sandbox runs by default, in dry run (TEN-345 / TEN-347)

**Branch:** `audit/exec-defaults` · **Status:** implemented and tested on macOS (live), cross-built
for Linux and Windows; not yet deployed.

## What changed

**1. Commands run under the sandbox by default, in dry run.** With no `exec_profiles` entry,
every surface (`local`, `discord`, `imessage`, `cron-exec`, `cron-shell`, and sub-agents, which
take their parent's) now uses the `restricted` backend in **dry run**. In a dry run the sandbox
runs the command first. If it refuses something (a write outside the allowed folders, a read of
a credential store, a network call where the network is off), the command runs again on the
host, unconfined, and the refusal is logged. No approval is asked for, and nothing it recognises
is blocked; [Known limits](#known-limits) lists the refusals it can't recognise, which still
fail. `cron-shell` also defaults to **no network**. In a dry run that is only logged too.

**2. Where the sandbox can't dry-run, commands stay on the host, and every report says why.**
A dry run only re-runs what the backend can recognise as a refusal. The restricted backend is
the default only where it can do that without breaking commands:

| OS | Built-in default | Why |
|---|---|---|
| macOS | `restricted` (Seatbelt), dry run | Its refusals are recognised. Tenant checks once at start that `sandbox-exec` works, and falls back to the host if it doesn't. |
| Windows | `host` | The Windows `restricted` backend runs commands as **Linux `sh` inside WSL2**, not PowerShell or `cmd`. As a default it would change what every command and every cron job does. |
| Linux | `host` | Under bubblewrap a read of a hidden folder (`~/.ssh` for `git`) fails with nothing to recognise, so it is never re-run, and `/tmp` is private to each command. Both need fixing in the Linux backend first (TEN-346). Without `bwrap` installed, the reason says that instead. |

On the host nothing is confined, as before. The reason shows at startup (a `sandbox:` line in the
feed), in `tenant doctor` (a WARN), in `tenant security audit` (the `execution.host` finding) and
in `tenant sandbox probe`. You can still name `restricted` yourself on Linux or Windows (below).

**3. Commands get a clean environment on every backend.** A command no longer inherits all of
Tenant's environment. The old default, which dropped only variables named like `KEY`, `TOKEN` or
`SECRET`, is gone. A command now gets a safe base set:
- everywhere: `PATH`, `HOME`, `USER`, `LOGNAME`, `USERNAME`, `SHELL`, `LANG`, `LANGUAGE`, `LC_*`,
  `TZ`, `TERM`, `TMPDIR`, `TEMP`, `TMP`;
- on Windows also: `SystemRoot`, `SystemDrive`, `windir`, `ComSpec`, `PATHEXT`, `PSModulePath`,
  `USERPROFILE`, `HOMEDRIVE`, `HOMEPATH`, `APPDATA`, `LOCALAPPDATA`, `ProgramData`,
  `ALLUSERSPROFILE`, `PUBLIC`, `ProgramFiles`, `ProgramFiles(x86)`, `ProgramW6432`, the
  `CommonProgramFiles` trio, `USERDOMAIN`, `COMPUTERNAME`, `NUMBER_OF_PROCESSORS`,
  `PROCESSOR_ARCHITECTURE` and `OS` (names match in any case).

It also gets whatever its profile lists in `env_pass`, plus `env_set`. This applies to the host
backend too, so `os_exec` on the host no longer passes API keys, tokens, database URLs or proxy
passwords to commands. Local MCP servers (`mcp_servers`) keep their own environment rules.

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
- A refusal inside Tenant's own folders or a credential store saves nothing: no exception opens
  those.
- A refusal of your home folder itself, or anything above it, saves nothing: that would be too
  broad.
- A refusal whose output names no absolute path saves nothing.

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

## How to read the sandbox log
The dashboard's **sandbox** tab, or `tenant logs query "log:sandbox"`:
- `sandbox/1` and `sandbox/1001`: every command, with its backend, surface and exit.
- `sandbox/2002` *Dry run: … would have refused*: the entry to read during the dry-run weeks. It
  names the surface and the refusal (`filesystem: …` or `network: …`). The command's own output
  names the file.
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
"exec_exceptions": { "local": ["~/projects"], "cron-shell": ["/srv/reports"] }
```

**Turn enforcement on** once the log shows only refusals you want:
```json
"exec_profiles": { "default": { "enforce": true } }
```

**Keep one surface on the host**, or opt out everywhere with `"default"`:
```json
"exec_profiles": { "discord": { "backend": "host" } }
```

**Use `restricted` on Linux or Windows anyway**:
```json
"exec_profiles": { "default": { "backend": "restricted", "enforce": false } }
```
On Windows your commands and cron jobs then run as Linux `sh` inside WSL2.

## Known limits
- **A dry run can still repeat side effects.** A refused command runs twice. Whatever it did
  before the refusal (a network call, an append) happens again on the host.
- **A dry run misses refusals it can't recognise, and those commands fail instead of re-running:**
  - on macOS, a command that hides its errors (`2>/dev/null`);
  - macOS services the sandbox blocks, such as the clipboard (`pbcopy`/`pbpaste`), `open`,
    `defaults` and `osascript`, whose tools report the block as their own failure.

  Put a surface that needs them on the host, or accept the failure.
- **Windows and Linux stay on the host** until their backends can dry-run without breaking
  commands (native Windows confinement, and Linux read attribution, TEN-346).
- **The environment allowlist is enforced on the host, macOS and Linux.** Through WSL (`restricted`
  on Windows), a command gets WSL's own environment.
