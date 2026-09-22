# Change: the execution sandbox — per-surface exec backends, `exec_profiles`, refusal escalation (SEC-009/010/011)

**Branch:** `feat/sandbox-seam` · **Status:** implemented and tested; the macOS backend is
live-tested and red-teamed, the Linux and Windows backends are not yet validated live; not yet
deployed. **Nothing changes until an operator sets `exec_profiles`.**
**Tickets:** SEC-009 / TEN-345 (seam, host backend, profiles), SEC-010 / TEN-346 (restricted
backends), SEC-011 / TEN-347 (escalation, dry-run, unattended deny).

## What changed
Every command a model turn can run on the host now goes through one seam, `internal/sandbox`,
instead of calling `os/exec` directly: `os_exec` (the live tool mux in `tenant tui` /
`tenant serve`, and `tenant os`) and scheduled shell jobs. Approval still decides *whether* a
command runs; the backend decides *what it can reach* when it does. The `osys` danger classifier
stays in front as a fast fail-early check. It is not the boundary.

- **Backends.** A `sandbox.Backend` runs a command under a `Policy` and reports, through `Probe`,
  what it enforces on this host, per dimension (filesystem, network, limits, env), as
  `enforced` / `partial` / `none`, with a detail line that names the gaps.
  - `host`: the default on every surface. No filesystem or network confinement, and named as
    such. It honours the work dir, an env allowlist, the wall-clock timeout and the output cap.
    On the default profile the `os_exec` path matches the old runner exactly (output, exit text
    and timeout text, compared side by side in tests), and cron shell jobs keep `cmd /C` on
    Windows, the scrubbed environment, the 4000-byte output cap and their error text.
  - `restricted`: OS primitives, no external runtime. Seatbelt (`sandbox-exec`) on macOS,
    bubblewrap on Linux, a Job Object on Windows. How much each one enforces differs a lot; see
    [Enforcement by OS](#enforcement-by-os).
  - `container`: reserved for TEN-318. A profile that names it is rejected at load.
- **Policy.** Work dir, read / write / deny roots, network (`inherit` | `none` | `allowlist` |
  `full`), env allowlist, limits, and `enforce`. Precedence, strongest last: the grants (an empty
  read-root list means the whole host is readable), then deny roots, which revoke read and write,
  then the work dir, which stays usable even inside a deny root (cron's work dir lives under the
  data dir). The work-dir carve-out never reopens a deny root at or inside the work dir, and a
  work dir equal to a deny root stays denied (fail closed).
- **Surfaces.** A turn's surface is stamped where the turn starts: `discord` (the relay),
  `imessage` (the responder), `cron-exec` (scheduled prompt jobs), `cron-shell` (scheduled shell
  jobs). Unstamped is `local`: the TUI, the dashboard, `serve` chat and `tenant os`. Sub-agents
  inherit their parent turn's surface, including on team runtimes that start them on a
  long-lived base context.
- **`exec_profiles`** in `<config-dir>/settings.<agent>.json`, keyed by surface or `default`.
  Operator-only: no agent tool reads or writes a profile. Fields: `backend` (`host` |
  `restricted`), `enforce` (default `true`; `false` = dry-run), `network`, `allow`,
  `read_roots`, `write_roots`, `deny_roots`, `env_pass`, `env_set`, `cpu_seconds`, `memory_mb`,
  `max_pids`. Roots must be absolute or start with `~`. `tenant tui` and `tenant serve` load
  the profiles at startup and re-read the file within about 3 s of a hand edit (a feed line
  reports it). `tenant os` reads it once. The profile is resolved again for every command, so an
  edit applies to the next one with no restart. An invalid entry (unknown surface, backend or
  network, a relative root, a negative limit, `container`) is skipped and named; the rest load.
- **Built-in baseline.** Every profile's deny roots include the config dir, the data dir and the
  Tenant binary (the file, not its directory, which may be shared). `deny_roots` adds to this
  set; nothing removes from it. A confining profile also:
  - denies well-known credential stores and browser profiles: `~/.ssh`, `~/.aws`, `~/.gnupg`,
    `~/.netrc`, `~/.git-credentials`, `~/.config/gh`, `~/.config/gcloud`, `~/.azure`,
    `~/.kube`, `~/.docker`, `~/.npmrc`, `~/.pypirc`, the browser profile dirs (Chrome,
    Chromium, Brave, Edge, Firefox), on macOS `~/Library/Keychains`, `~/Library/Cookies` and
    `~/Library/Safari`, and on Windows the `Microsoft\Credentials` stores, `gcloud` and the
    Chrome / Edge / Brave / Firefox profiles;
  - runs with cron's scrubbed environment (variables whose names contain KEY, TOKEN, SECRET,
    PASSWORD or PASSWD are dropped) instead of all of Tenant's, unless the profile sets
    `env_pass` / `env_set`.
- **Refusals.** When a confining backend refuses a filesystem or network access:
  - with `enforce: true`, the turn's approver is asked for **`os_exec_unsandboxed`**, with the
    refusal named ("SANDBOX REFUSED: … approve to re-run it OUTSIDE the sandbox"). The action is
    categorized `destructive`, so it prompts wherever `destructive` is on `ask` (the default on
    the host broker and on the Discord relay). The iMessage broker denies it by default, and the
    cron exec approver always denies it. If approved, the command re-runs on the host and its
    output says so. If denied, the model gets the confined result with the refusal appended to
    the error so it can adjust. Cron shell jobs and `tenant os` have no approver: the job or
    command fails with the refusal named.
  - with `enforce: false` (dry-run), the command re-runs on the host without asking, and its
    output carries "sandbox dry-run: this would have been refused (…)".
  - a `limit` violation (on macOS: the wall-clock timeout, or a background process reaped after
    the run) is reported but never escalates.
- **The `sandbox` log source** records every confined run, refusal, approved escalation, dry-run
  would-be refusal and backend start failure (`docs/LOGGING.md`).
- **The cron exec approver's self-modification guard** now resolves a write target (relative
  paths, `..`, symlinks) against the config, data and binary dirs. The old substring check could
  be walked around, and the file tools' write roots deliberately include those dirs.
- **The file tools cannot touch Tenant's controls or secrets, on any surface.** Two built-in
  lists apply whatever the `write` mode, and in `tenant os` too. *Protected* files can be read
  but never written: `settings.<agent>.json`, the soul, the peer cert, the claude spend ledger and
  the Tenant binary. *Secrets* can be neither read nor written: `config.json` and
  `dashboard-auth-token` (they hold the dashboard token), `credentials.json`, `peers.json`, the peer key, the OAuth token files, a
  configured service-account or TLS key, the claude CLI's login, and the credential stores the
  exec sandbox denies. A write grant can no longer widen an exec profile or a permission mode, and
  a read can no longer hand a turn a credential to send. See [Operator notes](#operator-notes).
- **The file tools' default write roots stop at Tenant's own dirs.** They used to include the
  parent of the config and data dirs. On a default install that is the OS-wide config root, and
  on a `C:\Tenant` server it holds the binary and the claude CLI's config.
- **The file tools open exactly the path they checked.** A path is resolved first. Dangling
  symlinks are followed too: they used to be checked as the link itself while a write through one
  created the target. On macOS and Linux the resolved path is then opened with no symlink allowed
  anywhere on it. A symlink swapped in after the check fails the open instead of redirecting it,
  and that holds for every file-tool boundary.
- **Operator CLI.** `probe` and `run` take `--config` / `--data` / `--agent` to select the
  instance.
  - `tenant sandbox probe [--json]` shows each surface's resolved profile (backend, network,
    work / read / write / deny roots) as loaded from the settings file, what that backend
    enforces on this host, and whether this OS has a `restricted` backend at all. It exits 1 if
    a surface's backend is unavailable, because that surface's commands would fail closed.
  - `tenant sandbox selftest [--json]` runs the escape suite against `host` (the unconfined
    baseline) and this OS's `restricted` backend. It exits non-zero if:
    - `restricted` fails to contain a case in a dimension its Probe claims to enforce;
    - a containment in such a dimension is unproven, because the adversary also failed on the
      unconfined host;
    - any legitimate twin fails;
    - the OS has no restricted backend, or no case could be evaluated.
  - `tenant sandbox run --surface <s> [--backend host|restricted] [--timeout 60s] [--workdir DIR] -- <cmd>`
    runs one command under a surface's profile the way its spawn site would. It prints the
    output, exit code, timeout and violations, and exits 1 unless the command ran cleanly.
    It calls the backend directly, so it never escalates or re-runs.

Making `restricted` the default on any surface is deliberately left for later (TEN-347).

## Why
Before this, all that stood between a model-issued command and the host was the approval gate
and the `osys` classifier. The classifier is a denylist of catastrophic patterns. An obfuscated
command slips past it, and it has nothing to say about *reading* a secret or sending one
somewhere. On the remote and unattended surfaces (Discord, iMessage, cron), a prompt-injected
command ran with all of Tenant's reach: its config and data dirs, `~/.ssh`, and every token in
its environment. `os_exec` inherited all of Tenant's environment, and cron's scrub dropped only
variables whose *names* looked secret.

The seam puts a boundary under the gate that an operator can tighten per surface without
changing any spawn site. It reports what each backend enforces on the host it runs on, and turns
a refusal into an explicit decision instead of a silent failure.

## Evidence
All green on macOS (Apple silicon). `internal/sandbox` also vets and builds for Linux and
Windows.
- **Seam core (any OS):** `Execute` against a fake backend: pass-through, a denied or nil
  escalator keeps the confined result, an approved one re-runs on the host and keeps the
  refusal, dry-run re-runs without asking, a limit violation never escalates or re-runs. Also:
  env inherit vs allowlist, host exit codes / work dir / timeout / output cap, backend
  resolution, and the host `Probe` claiming nothing.
- **Pure builders (any OS):** the bubblewrap argv (core flags; `--unshare-net` only for `none`;
  dir, file and absent deny masks; write binds; carve-out ordering) and the Job Object limit
  mapping, plus a compile-time check against `x/sys/windows`.
- **Escape suite** (`escapecore`, driven by `go test` through `escapetest` and on a live box by
  `tenant sandbox selftest`). Four adversarial cases, each with a legitimate twin that must
  succeed: read a secret outside the roots, write outside the roots, a `..` path escape, and
  outbound network. Against `host`, **nothing is contained**, which is the gap the seam exists
  to close. Against Seatbelt, **all four are contained** and every twin succeeds (`selftest`:
  PASS, host 0/4, Seatbelt 4/4).
- **Red-team suite, live against Seatbelt** (`redteam_darwin_test.go`). One gap is recorded and
  skipped: macOS has no PID or memory cap. One behaviour is pinned by design: with `read_roots`
  empty, the whole host outside the deny roots is readable. Contained:
  - a hardlink of a denied file into the work dir;
  - symlink read-through and write-through;
  - `rename` into and out of the boundary;
  - network under `none`: raw TCP, `curl`, DNS, a unix-domain socket, and a loopback listener
    standing in for the dashboard API (its full-network twin connects, so the probe is real);
  - Mach/XPC services: `launchctl`, the pasteboard, `cfprefsd`, the Keychain via `securityd`,
    and LaunchServices `open`;
  - `kill` of a process outside the sandbox;
  - a nested `sandbox-exec` with an `(allow default)` profile;
  - persistence writes to a `$HOME` dotfile, `~/Library/LaunchAgents` and `/tmp`;
  - a backgrounded child outliving the run (the process group is reaped);
  - `stat` and `ls` inside a deny root (metadata is denied too);
  - a parent secret, hidden by an isolated env;
  - the wall-clock timeout.
- **Seatbelt specifics:**
  - a work dir inside a deny root is usable while its siblings stay denied;
  - a deny root inside the work dir stays denied;
  - a work dir equal to a deny root fails closed;
  - stragglers are reaped and reported without turning success into failure;
  - exit-status text matches `os/exec`.
- **`cmd/tenant`:**
  - profile validation and resolution: overrides, `~` expansion, a surface entry beating
    `default`, the built-in host profile with enforce on, the credential baseline and scrubbed
    env only on confining profiles;
  - host parity with the old runner through the `osys` API;
  - cron `cmd /C` argv and exit text;
  - `os_exec_unsandboxed` categorized `destructive`;
  - settings hot reload: invalid entries are kept in the file, an unchanged file is a no-op;
  - the resolved self-modification guard, tested with details built to evade the old
    substring check;
  - live on macOS: a refusal raises the prompt, a denial stays confined, an approval re-runs
    and says so, dry-run runs on the host without asking, an unattended refusal fails the cron
    job, and one shared `os` service resolves a different profile per surface.
- **File-tool guards:**
  - Through the live tool mux, with `write` on `allow`: `os_write` / `os_edit` / `os_append` /
    `os_mkdir` on the settings file are refused as a hard boundary on the `local`, `discord`,
    `imessage` and `cron-exec` surfaces. The paths tried: absolute, relative, `..`, relative
    `..`, and through a symlinked dir. `config.json`, `credentials.json` and `peers.json` are
    refused the same way. The broker is never asked, the files keep their contents, ordinary
    writes still pass, and Tenant's own settings save still works.
  - Also through the live mux, `os_read_file` is refused, with nothing leaked, for every secret
    kind:
    - `config.json`, `credentials.json`, `peers.json` and the peer key;
    - an MCP token cache and the Atlassian, GitHub and gsuite OAuth files;
    - a service-account key whose name contains `[`, and a dashboard TLS key;
    - the claude CLI's login and an `~/.ssh` key.

    Ordinary files still read.
  - The default write roots include the config and data dirs but not their parent. In a
    `C:\Tenant`-shaped layout the binary and `claude-config` are refused, while the wiki in `data`
    stays writable.
  - On Windows, PowerShell profiles and modules under Documents are refused. That test runs only
    on Windows.
  - The guards don't depend on the write roots: operator `--os-write-roots` and `tenant os` keep
    them all, the soul, peer cert, spend ledger and binary included.
  - At the `osys` layer, these are refused:
    - a dangling symlink and a symlink loop;
    - case and long-s spellings, and another agent's file;
    - a directory in a file's place;
    - a hard link to a secret file, and one into a secret directory;
    - a macOS firmlink path (`/System/Volumes/Data/...`);
    - on Windows only, `settings.json.`, `settings.json ` and `settings.json::$DATA`.

    Operator paths are literal: only `*` is a wildcard.
  - Races: a directory on the checked path is swapped for a symlink after the check.
    - A write through it is refused, and the protected file is untouched, not even truncated.
    - A read through it is refused, and no byte of the secret comes back.
    - On macOS and Linux, the swap cannot carry an ordinary write outside the write roots.
    - The identity re-check, which is what covers hosts that cannot refuse a symlink at open, is
      tested on its own.
  - Each guard piece was confirmed to fail a test when removed.

## Operator notes
- **Nothing changes until you set `exec_profiles`.** With no entry, every surface runs on
  `host`, the pre-seam behaviour, and `tenant sandbox probe` shows exactly that.

**Example** (in `<config-dir>/settings.<agent>.json`):

```json
"exec_profiles": {
  "default":    {"backend": "restricted", "enforce": false},
  "local":      {"backend": "host"},
  "discord":    {"backend": "restricted", "network": "none", "write_roots": ["~/tenant-work"]},
  "cron-shell": {"backend": "restricted", "network": "none"}
}
```

Here `imessage` and `cron-exec` take `default` (restricted, dry-run), `local` stays unconfined,
and `discord` and `cron-shell` are enforced with no network. A surface's own entry **replaces**
`default`; it is not merged with it. Fields it leaves out fall back to the built-in values
(`host`, enforce on, network `inherit`), not to `default`. Repeat `"backend": "restricted"` in
every entry that should be confined.

- **Rollout:**
  1. `tenant sandbox probe`: check what each surface resolves to and what this host's
     `restricted` backend really enforces. Fix any surface reported UNAVAILABLE; its commands
     would not run at all.
  2. `tenant sandbox selftest`: it must PASS on this machine before you rely on `restricted`.
     A PASS that says "NOT a full boundary" means some dimensions are not claimed on this OS.
  3. `tenant sandbox run --surface cron-shell -- '<command>'`: try real commands, especially
     your cron jobs, and read the violations.
  4. Dry-run: set `"enforce": false` on the surface and let it run for a representative
     stretch. Read the `sandbox` log (`tenant attach --follow --source sandbox`) for "would have
     refused" lines, and widen `write_roots` / `env_pass` / `network` where a refusal is
     legitimate. Dry-run needs refusal detection, which only the macOS backend has today (see
     [Enforcement by OS](#enforcement-by-os)).
  5. Enforce: remove `"enforce": false`.
- **Pin the escalation prompt.** `os_exec_unsandboxed` is `destructive`, so any broker whose
  `destructive` mode is `allow` approves it silently. On the Discord relay,
  `/relay permissions set destructive allow` would auto-approve every re-run outside the
  sandbox. Pin it with a tool rule, which every broker checks before the category:
  `/permissions add os_exec_unsandboxed ask` (or `"tool_rules": {"os_exec_unsandboxed": "ask"}`
  in the settings file). An exact rule also outranks a glob such as `os_exec*`. Answer these
  prompts with a plain approve (once):
  - with the rule in place, `always` flips the rule to `allow`;
  - without it, `session` or `always` at the TUI or dashboard grants the whole `destructive`
    category, for the session or for good.
- **The file tools cannot touch Tenant's controls or secrets.** On every surface and in
  `tenant os`, whatever the `write` mode, two built-in lists apply.
  - **Protected:** readable, but never written by `os_write`, `os_edit`, `os_append` or
    `os_mkdir`:
    - `settings.<agent>.json` for every agent sharing the config dir, and its temp file;
    - the soul (`soul/`);
    - `peer-cert.pem`;
    - the claude spend ledger (`claudecode-usage.json`, the weekly budget rail);
    - the Tenant binary;
    - on Windows, the PowerShell profile and module folders under Documents.
  - **Secret:** never read or written:
    - `config.json` and `dashboard-auth-token`, because they hold the dashboard's auth token;
    - `credentials.json`;
    - `peers.json` and `peer-key.pem`;
    - `mcp/`, `atlassian-token.json`, `github-token.json` and `oauth_client.json`;
    - the gsuite service-account key and the dashboard TLS key, when set;
    - the claude CLI's config: `$CLAUDE_CONFIG_DIR`, else `~/.claude` and `~/.claude.json`;
    - the credential stores and browser profiles listed above, the same set the exec sandbox
      denies to confined commands.

  These are hard boundaries: no approval opens them, and the refusal tells the model so. Every
  way of reaching a file is refused:
  - the target is resolved first: relative to Tenant's working directory, through `..`, and
    through symlinks, dangling ones included;
  - names match regardless of case, and an entry naming a directory covers everything in it;
  - an existing file is also matched by identity, so a hard link to it is refused too;
  - the tools then open exactly the path they checked, with no symlink allowed anywhere on it
    (macOS 11+, Linux 5.6+), and check the open file's identity again, so a symlink swapped in
    after the check cannot redirect the operation.

  So `write: allow` no longer lets a turn edit its own `exec_profiles`, category modes or tool
  rules, and no read hands a turn a credential to send. Tenant's own saves don't go through the
  file tools and are unaffected: change these files with `/permissions`, `/approve always`,
  `/model`, `tenant keys`, `tenant peer`, the dashboard, or by hand.
- **The default write roots are Tenant's own dirs and your working folders:** the config and data
  dirs, Desktop, Documents, Downloads and the temp dir. They are never a parent of the config or
  data dir. They used to include that parent:
  - on a default install it is the OS-wide config root (`~/.config`, `%AppData%`,
    `~/Library/Application Support`), which holds autostart entries;
  - on a `C:\Tenant` server it holds the binary and the claude CLI's config.

  Nothing Tenant does needs the parent, and on a server the wiki lives in the data dir.
  `--os-write-roots` still replaces the whole list.
- **`write: allow` on a confined surface** no longer hands a turn its own controls. None of the
  known autostart locations are in the default roots any more. A turn can still change anything
  else in them without asking: your Desktop, Documents and Downloads, the temp dir, and Tenant's
  data dir, the log book included. Keep `write` on `ask` where that matters, or narrow
  `--os-write-roots`. The cron exec approver still refuses any write that resolves into the
  config, data or binary dirs.
- **`os_exec` has no work dir.** Under a confining profile it runs in Tenant's own working
  directory and can write only to `write_roots` (plus a per-run scratch dir it is not told about
  on macOS, and a private `/tmp` on Linux). Give the profile a `write_roots` entry for anything
  the agent should produce. `tenant sandbox run` uses a fresh temp dir as the working directory
  when the profile has none, so a relative write that passes there can still be refused in a
  real `os_exec` call. Cron shell jobs run in `<data-dir>/cron-work`, which the carve-out keeps
  usable.
- **The file tools are not sandboxed.** `os_read_file`, `os_list_dir`, `os_write` and the rest
  run inside Tenant's own process. They are bounded by `--os-deny-roots` / `--os-write-roots`,
  the write approval and the two lists above, not by an exec profile. Reads outside those stay
  broad: a turn can read the rest of your home directory. Add paths to `--os-deny-roots` if a
  surface must not read them.

### Enforcement by OS

| | macOS: Seatbelt | Linux: bubblewrap | Windows: Job Object |
|---|---|---|---|
| **State** | Implemented, live-tested, red-teamed | Written; argv builder unit-tested; cross-compiles. **Not yet run live**: validate it with `tenant sandbox selftest` on a Linux host or under WSL2 | Job Object live. The dedicated low-privilege account, NTFS ACEs on the roots and the WFP egress filter are scaffolded, **TODO(TEN-346)** |
| **Filesystem** | Read, write and deny roots enforced (deny also hides metadata) | Write and deny roots enforced. **`read_roots` is ignored**: the whole host stays visible read-only, minus the deny roots. `/tmp` is a private tmpfs | **Not enforced.** `probe` reports `partial` because the Job Object contains the process tree; no root is enforced |
| **`network: none`** | Enforced: TCP, UDP/DNS, unix sockets, loopback | `--unshare-net` (fresh network namespace) | **Not enforced** |
| **Limits** | Wall clock only. The command's process group is reaped after the run. No CPU, memory or PID caps | Wall clock only. Fresh PID namespace, `--die-with-parent`. No CPU, memory or PID caps | Wall clock, plus Job Object caps from `memory_mb`, `max_pids` and `cpu_seconds` (per process); kill-on-close |
| **Env** | Allowlist. `HOME` and `TMPDIR` point into the work dir when there is one | Allowlist | Allowlist |
| **Refusal detection** (escalation, dry-run) | Yes, heuristic: it keys on "Operation not permitted" | **No.** A denied access is an ordinary failed command: no escalation prompt, and dry-run does not re-run it on the host | **No** (nothing is refused yet) |

- **macOS:** usable today, within the gaps below.
- **Linux:** needs `bwrap` on `PATH`. Without it, a `restricted` profile fails closed and the
  command is not run. If bubblewrap cannot create its namespaces on the host, every command
  fails, and `selftest` shows it as failed legitimate twins. Until `selftest` passes on your
  host, treat it as unverified.
- **Windows:** filesystem and network are not enforced until the account, ACE and WFP work
  lands. **Do not rely on the `restricted` backend on Windows yet.**

### Known gaps
- **No secret proxying.** A confined command that needs a credential either gets it raw
  (`env_pass` / `env_set`, or a readable path) or runs outside the sandbox after approval. A
  credential-injection proxy, which would add secrets to outbound requests outside the sandbox
  so the command never sees them, is a follow-up. The default env scrub is name-based: a secret
  in a variable whose name lacks KEY / TOKEN / SECRET / PASSWORD / PASSWD survives it. Set
  `env_pass` for a strict allowlist, and include `PATH`.
- **Re-running can repeat side effects.** Dry-run and an approved escalation both run the
  command a second time, on the host. Whatever it did before the boundary stopped it happens
  twice, for example a network call on a profile that allows network, or an append in a
  writable root. Vet non-idempotent jobs with `tenant sandbox run` before a dry-run.
- **`network: allowlist` is not enforced anywhere yet.** The `allow` list is accepted, but no
  backend consumes it. Seatbelt and bubblewrap allow all outbound traffic (the same as `full`),
  and Windows enforces no network policy. Use `none` where it matters.
- **The file-tool guards have residuals.**
  - **No symlink refusal at open on some hosts.** Windows can't refuse a symlink on the whole path
    at open. Neither can Linux before 5.6, or a host whose seccomp filter blocks openat2. There,
    the open file's identity is checked again instead:
    - it catches a redirect into a guarded file, or into a file directly inside a guarded
      directory;
    - it misses a redirect into a file deeper inside a guarded directory, and one out of the deny
      or write roots;
    - a redirected create can leave an empty file at a guarded name that did not exist yet.

    On Windows the restricted backend doesn't confine the filesystem yet anyway, so a process
    able to swap a symlink could reach those files directly.
  - **Directories are still created by path.** This covers `os_mkdir`, and a write creating its
    parent folders. A swap can make a directory appear at a guarded name that did not exist, and
    Tenant's own save of that file then fails until you remove it. Nothing is ever written into a
    guarded file.
  - **`--os-deny-roots` matches more loosely.** Its entries still match by resolved path, and
    case-sensitively outside Windows. Only the built-in lists match by folded name and by
    identity.
- **A profile that allows network can reach Tenant's own loopback listeners.** The dashboard
  needs its auth token on every listener, loopback included (TEN-323). The token is
  `dashboard.auth`, else the generated `dashboard-auth-token`, both in the config dir, which
  confined commands and the file tools cannot read. The legacy MCP HTTP+SSE gateway, when
  enabled, has no auth: keep confined surfaces on `network: none` while it runs.
- **macOS:**
  - No CPU, memory or PID caps: a fork or memory bomb runs until the run's deadline stops it
    (the `os_exec` timeout, or the cron job's time limit).
  - A descendant that calls `setsid()` leaves the reaped process group and can outlive the run
    (still inside the Seatbelt profile).
  - File metadata (existence, size, times) is readable everywhere outside the deny roots; file
    contents are not.
  - Refusal detection keys on the "Operation not permitted" text. A command that discards its
    stderr fails with no refusal recorded (no prompt, no dry-run re-run), and one that prints
    that text for another reason is treated as refused.
- **Linux:** no Landlock layer under bubblewrap, no rlimits or cgroups, and `read_roots` is
  ignored (TODO(TEN-346)).
- **Windows:** see [Enforcement by OS](#enforcement-by-os). The restricted backend enforces
  resource limits only.
- **Container backend:** an ephemeral container per run, with real memory and PID caps and
  stdio MCP servers inside it, is TEN-318. `"backend": "container"` is rejected at load until
  then.
