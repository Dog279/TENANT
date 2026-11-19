# Security Policy

## Supported Versions

| Version | Supported |
| ------- | --------- |
| 0.x.x   | Best-effort (pre-release) |

Tenant has not reached 1.0. We treat security reports seriously and will
investigate and fix them, but there are no SLAs, no back-patches, and no
guaranteed response windows until the first stable release.

## Reporting a Vulnerability

**Email:** dtaylor@findtime.net  

**Do not** file a public GitHub issue for a security vulnerability.

What to include:

- A description of the issue and its impact.
- Steps to reproduce, or a proof-of-concept, if you have one.
- The Tenant version (`tenant --version` or git commit).
- Your operating system and architecture.

What to expect:

- Acknowledgment within **48 hours**.
- An initial assessment within **5 business days**.
- Coordinated disclosure: we will work with you on a timeline and credit you
  in the advisory unless you ask otherwise.
- If we cannot reproduce the issue, we will say so and ask for more detail
  rather than closing it silently.

## Security Architecture

### Threat model

Tenant is a **single-user desktop application**. It runs with local user
privileges, stores data on the local machine, and connects to external APIs
(Anthropic Claude, Atlassian, Google Workspace, X, Discord, and configured
integration endpoints) only when the user configures them. When web access is
enabled, browser navigation can contact arbitrary HTTP(S) destinations and
keyless search can contact DuckDuckGo. The optional Jina Reader fallback is
configuration-gated and off by default because using it crosses a separate
privacy boundary. See `docs/DATAFLOW.md` for the code-level inventory.

**Who is trusted.** The local user account Tenant runs as, and every process
running as that account, is trusted. Tenant's files and tokens are that
account's to read. Where the container sandbox is used, so is the container
engine's API, and access to that API is root in the engine's VM and read/write
on every drive it shares. The execution sandbox protects the host from a
command the agent runs. It does not protect Tenant, or the engine, from the
user's other processes.

### Process protection

Tenant's files are the account's to read, but its running process no longer
is. At every start Tenant asks the operating system to refuse other programs
of the same user access to its memory (TEN-535): on Linux the dumpable flag is
turned off, which refuses `ptrace`, `/proc/<pid>/mem`, `/proc/<pid>/environ`,
`process_vm_readv`, `pidfd_getfd` and `perf_event_open` and stops crash dumps;
on Windows the access lists of the process and of every thread give the owner
only the rights to end it, wait for it, read its basic details and query its
information (for a thread: end it, wait for it, resume it, read its access
list; every thread's list is read back and checked at start), the process's
token can be read and
copied by its owner but not changed, the console host is made to open the
process again so that the handle it keeps carries those rights and no more,
the window `tenant serve` needs is owned by a helper process that holds none
of serve's secrets, and the program is excluded from Windows Error Reporting;
on macOS a later debugger attach is denied and core files are off.

This is one layer, and it is narrow. The command line stays readable on every
system, and on macOS so does the environment the process started with. It does
not stop an administrator or root, a kernel exploit, or a program that started
Tenant itself and so held access to it from the first instant. On Windows two
routes of that kind are open and known: the starter's handle can be copied out
of the starter by another program of the same user for as long as the starter
keeps it, and a program that is already running can open Tenant in the instant
between its start and the controls. Put plainly, the Windows controls refuse a
program that starts after Tenant has finished starting; a program already
running when Tenant starts (and so one that ends Tenant and waits for it to be
started again) can still get in, through the instant before the controls or
through the full-access handle the console host holds until Tenant has joined
its console again, and what it takes then keeps working; closing that takes
Tenant being started already closed, which this version does not do. A program
of the same user can also make the Windows Error Reporting exclusion fail at
start (delete the value and deny writes to the key), so that Tenant asks at a
terminal whether to go on without it or, as a hidden serve, refuses to start.
A program of the same user can also end single threads of Tenant, which stops
work and reads nothing, and read each thread's access list. Such a program
could always read a protected Tenant's exit code, times, priority, counters,
the paths of its modules and where its memory lies and how it is protected,
and the right `taskkill /F` and `Stop-Process` need to end it
(`PROCESS_QUERY_INFORMATION`) adds which of its pages are resident and shared
(the working set), its handles by number, kind and rights, its mitigation
policy, and a walk of its threads that gives no more than opening each by id:
nothing of its memory's content, no handle out of it, no thread redirected.
When a control is not in force, every command that opens Tenant's data (every
command that loads a secret or runs an agent turn among them; eval's smoke
and ui subsets load neither) writes system event 2030 to the log book (`tenant logs`) at each
start, with the command, the control and the reason; serve and the TUI also
ask first. When the log book cannot take the event (its folder made
unwritable, `logbook.db` not opening), the command says so on stderr and the
record goes to the operating system's log instead. The log book itself is the
user's file: a program of the same user can delete the events from it
afterwards, and doctor's log-book checks do not notice. It does not
protect the data: the dashboard token, the credentials file, memory, logs and
run traces are plain files that a program of the same user can read, and such
a program can also stop Tenant and run its own copy against them. The Linux
and Windows controls were shown to refuse each access on a real machine; the
macOS ones have not been run on a Mac. There is no run-time switch; a build
made with `-tags debugattach` has no protection and says so in
`tenant version`, `tenant doctor` and `tenant security audit`.

### Secrets and credentials

- **API keys and tokens** are stored in
  `~/Library/Application Support/tenant/credentials.json` (macOS; equivalent
  OS config directories on Linux and Windows).
- Tenant centralizes credentials, the Atlassian, GitHub, X and MCP OAuth
  caches, the gsuite OAuth client, the dashboard token, the MCP gateway token,
  peering tokens, and peer private keys behind one secret-storage boundary. On Unix, files are
  verified as **0600** and their direct parent directories as **0700**. On
  Windows, inherited access is disabled and an explicit DACL grants full
  control only to the current user. Reading a cache that an older build left
  loose tightens it. One list of these files drives both `tenant doctor`,
  which fails on any that is unprotected, and the file tools, which refuse to
  read them (TEN-337).
- **No secrets are committed to the repository.** The repo is a clean template;
  `credentials.json` is in `.gitignore`.
- MCP remote connectors use **OAuth 2.1 with Dynamic Client Registration
  (DCR)**. OAuth tokens receive the same platform-specific owner-only
  protection. Run `tenant doctor` to audit the effective protection of every
  managed secret file.
- Managed secret files are currently plaintext inside that OS access-control
  boundary. Use full-disk encryption for defense against offline disk access.

### Local storage

- **SQLite** databases store memory, episodes, learned skills, usage, and
  self-improvement metadata. Wiki indexes, transcripts, archives, logs,
  screenshots, and research artifacts use files alongside those databases.
  They live in the OS data/config directories or an operator-selected path, not
  in the repo.
- Tenant creates its config/data roots with owner-only directory permissions and
  writes archives, databases, research artifacts, proposals, OAuth caches, and
  peer/identity state as owner-only files on POSIX systems, eval reports
  included. Plain `tenant doctor` only reports loose permissions and changes
  nothing; `tenant doctor --fix` removes legacy group/world permission bits. It
  opens every path without following symlinks and changes the mode through that
  descriptor, so a symlink swapped in during the repair is skipped (TEN-333).
- **Tenant does not encrypt local data at rest.** File modes are access control,
  not encryption; database, transcript, report, and credential contents remain
  plaintext to the owning account and to any process or administrator that can
  read that account. Use OS full-disk encryption for at-rest protection.
- **Backups** (`tenant backup`, TEN-305) are one file holding the config and
  data dirs. An encrypted backup (`--encrypt` passphrase via age's scrypt, or
  `--recipient` X25519 public keys) carries the credential and key files; an
  unencrypted one leaves every managed secret out, but still holds your memory,
  archive and `config.json` in plaintext. The file is written owner-only.
  Restore verifies checksums, database integrity and schema before it replaces
  anything, keeps the replaced directories, and refuses while a Tenant process
  holds the data-dir lock (`<data>/.tenant.lock`) or a dashboard answers.
  A restore also brings back the instance identity (peer key, paired peers,
  dashboard token, relay setup).
- **Retention and deletion** (`tenant privacy`, TEN-309). Every data class can
  have a retention period (`privacy.retention_days`); nothing but the relay
  inbox (30 days) is pruned until one is set. `tenant privacy forget` deletes
  rows for real, on a `secure_delete` connection, optimizes the full-text
  indexes, truncates the WAL, and rewrites archive files without the removed
  events. It also removes what was derived only from them. Facts that lose one
  of several sources record the removal in `fact_source_tombstones`. The log
  book loses the events of forgotten sessions and runs (in `security` they are
  tombstoned, keeping the chain), from `logbook.db`, its text files and its
  archives (TEN-374). It cannot reach backups already made (`privacy status`
  counts those that predate the last forget), the log book's other events
  before their own retention, or a soul you wrote; its preview says so.
- Outbound reads also disclose data: model calls send prompt/tool context;
  search sends queries; browser/Jina reads send URLs; integrations send resource
  identifiers and credentials. The complete flows are in `docs/DATAFLOW.md`.
- No data leaves the machine unless a plugin explicitly sends it (e.g., posting
  to Discord, calling an LLM API).

### Permission model

Plugins are **read-by-default**. Mutating and destructive operations are gated:

- Shell execution, file writes, destructive SQL, web interaction, and outbound
  sends use plugin-specific gates. Depending on the path, authorization is a
  per-action approval, a persisted permission mode, an `--allow-*` flag, or
  feature enablement. Ordinary SQL writes and MCP memory writes are flag-only;
  cron mutation tools are marked gated but have no dispatcher-side confirmation
  hook. See the approval matrix in `docs/DATAFLOW.md`.
- Interactive destructive paths ask unless the operator has already selected an
  applicable allow mode; scheduled exec runs only after separate job/global
  opt-ins and applies additional destructive-command checks.
- **Offsite surfaces.** A turn from the Discord relay or the iMessage responder
  is approved by that surface's own broker, never the local one. iMessage modes
  default to `deny`; Discord's default to `ask`, a button in the channel. Tool
  rules apply on every surface.
  - The relay's exec mode (`relay.allow_exec`, `/relay exec`) is off by
    default. While it is off, the Discord broker refuses exec, write,
    destructive, sandbox and send, whatever the relay permissions or a tool
    rule say. Discord's own posts and reactions still follow `send`.
  - `tenant permissions effective --surface local|discord|imessage` shows what
    each broker grants, and which setting grants it.
  - `tenant security audit` checks every live surface and exits non-zero on a
    critical finding, such as a surface that runs commands or writes files with
    no approval.
  - Both commands read the config without changing any file. Pass them the
    launch flags Tenant runs with (`--os-allow-exec` and the like): they cannot
    see a running process's flags.
- **Execution sandbox.** Approval decides *whether* a shell command runs; an
  execution backend decides *what it can reach*.
  - Commands the agent runs (`os_exec` and scheduled shell jobs) go through a
    backend chosen per surface (`local`, `discord`, `imessage`, `cron-exec`,
    `cron-shell`) in the operator's `exec_profiles` in `settings.<agent>.json`.
    No agent tool can change them, and the file tools refuse that file.
  - On macOS every surface runs the `restricted` backend (Seatbelt) by default,
    in **dry run**: a command it refuses is logged and re-run outside the
    sandbox, so it confines nothing yet; `exec_profiles.default.enforce: true`
    turns enforcement on (`docs/changes/exec-defaults-dry-run.md`). Linux and
    Windows have no restricted backend: the default is the `host` backend, which
    confines nothing, and `tenant doctor`, the audit and the startup feed say
    so. A hardened `container` backend for every OS, driven through the `docker`
    CLI, is now available and opt-in per surface (`"backend": "container"`). Its
    wave-1 hardening is covered by unit tests against a fake CLI; the live cases
    on a real engine, and what wave 1 still owes, are listed in
    `docs/plans/docker-sandbox-hardening.md` §9. It never mounts the Docker socket,
    runs each command in a fresh read-only, non-root, all-capabilities-dropped
    container with `no-new-privileges` and no network by default, with HOME on
    a tmpfs of the container's own (nothing a command leaves in HOME reaches
    the host or the next container), and fails
    closed when the engine or image is not ready, or when the engine answers but
    cannot start a container (a check container runs before first use).
  - Commands get an environment allowlist on every backend: a safe base set plus
    the profile's `env_pass`, never Tenant's API keys or tokens.
  - When the `restricted` backend refuses a file or network access, the
    command can re-run outside the sandbox only if `os_exec_unsandboxed` is
    approved. That action has its own `sandbox` category (ask by default, deny
    on iMessage), so a grant for it covers only further escalations.
    "Always" on it saves only the refused folder, for that surface
    (`exec_exceptions`). Unattended jobs never escalate.
  - `tenant sandbox probe` shows what each surface gets on this machine, and
    `tenant sandbox selftest` tests it.
  - Per-OS status is in the table below; details are in
    `docs/changes/sandbox-seam.md`.
- **File tools.** The in-process file tools follow the same limits on every
  surface, whatever the approval mode. Each is a hard boundary no approval
  opens.
  - They never read or write Tenant's secrets: `config.json` and
    `dashboard-auth-token`, which hold the dashboard token, `credentials.json`,
    token and key files, and the claude CLI's login.
  - They never read or write the well-known credential stores.
  - They never write Tenant's controls: the settings file, the soul and the
    binary.
  - By default they write only to Tenant's own dirs and the operator's
    Desktop, Documents, Downloads and temp dir.
- Eleven plugin channels are available: OS (shell + files), web, SQL, Google
  Workspace, X, iMessage, Discord, wiki, cron, Atlassian, and MCP remote. Each
  has the gate behavior documented in the data-flow matrix.

### Network

- Outbound connections include model and embedding endpoints; arbitrary browser
  destinations; DuckDuckGo, Brave, Tavily, and Jina web services; Atlassian,
  Google, X, and Discord APIs; BlueBubbles; remote MCP servers and discovered
  OAuth endpoints; Tenant peers; and explicitly configured control endpoints.
  Jina Reader (`r.jina.ai`) is an explicit opt-in fallback: after local Chrome
  extraction fails, Tenant may send the eligible target URL to Jina for remote
  extraction. Tenant blocks loopback/private targets and URLs with credentials,
  signatures, or token-bearing query parameters before relay, and records every
  request with the target host in the log book (the dashboard's Logs page).
- **No telemetry or analytics path is implemented.** Tenant never calls a server
  the user did not configure, but this does not mean every destination is
  individually named: DuckDuckGo and Jina are keyless web fallbacks after the
  web feature is enabled.
- The dashboard is a normal inbound listener: TUI/serve enables it by default on
  `127.0.0.1:9770`. Optional listeners include the MCP HTTP+SSE gateway, peer
  server, and temporary OAuth callbacks. The dashboard requires its auth token
  on every listener, loopback included, and TLS as well off loopback. The MCP
  gateway requires a bearer token of its own (`gateway-auth-token`) on every
  request, refuses a request that carries another site's `Origin`, and on a
  loopback bind answers only to loopback host names; it serves plain HTTP, so
  a non-loopback bind requires an explicit insecure override.

### Release signatures

A release publishes `checksums.txt` (the SHA-256 of every archive) and, beside
it, up to two signatures over that file. `tenant update`, the install scripts
and a fleet master check the archive against `checksums.txt`; the signatures
say who made `checksums.txt`.

- **cosign** (`checksums.txt.sigstore.json`). Made keylessly by the release
  workflow through GitHub's OIDC token and recorded in Sigstore's public
  transparency log. It ties the release to a run of *this repository's*
  `release.yml`: there is no long-lived secret to steal, and any fork can make
  the same kind of signature from its own workflow. Checking it needs the
  `cosign` program (and its trust root) on the machine. `tenant update` checks
  it when cosign is on PATH; a fleet machine checks it offline.
- **The project key** (`checksums.txt.sig`, TEN-542). An Ed25519 signature made
  by the main repository's release workflow with a private key held as the
  repository secret `TENANT_RELEASE_SIGNING_KEY`; the public half is built into
  the release binary (`TENANT_RELEASE_PUBKEYS`, up to three keys). It ties the
  release to whoever holds that key. Checking it needs nothing installed: the
  binary verifies it with Go's standard library. It is only as good as the
  key's custody, and the key is guarded and replaced by hand.

A binary that carries the project key takes, with `tenant update`, only a
release whose `checksums.txt.sig` verifies under one of its keys: a missing,
malformed or wrong signature refuses the update and changes nothing, and no
flag skips it. cosign is still checked when it is installed, and then both must
pass. A fleet machine on the default update rule takes a release when cosign
verifies it or, when its own binary carries the key and its master sent the
project signature, when that verifies; a project signature that is sent and
does not verify refuses the update without trying cosign. A fork's binaries,
and every development build, carry no key: for them nothing about updating
changes, and `checksums.txt.sig` is not looked at.

**Making the first key (the maintainer, once).** Nothing in the repository
creates a key, a secret or a variable.

1. On a trusted machine, from the repository root:
   `go run ./scripts/releasekey keygen -out <path outside the repository>`.
   It writes the private key to that file (owner-only, never over an existing
   file) and prints the public key.
2. In the GitHub repository's settings, *Secrets and variables > Actions*: add
   the file's contents as the **secret** `TENANT_RELEASE_SIGNING_KEY`, and the
   printed public key as the **variable** `TENANT_RELEASE_PUBKEYS`. Then keep
   the file offline or delete it.
3. Tag a release. The workflow's "Check the project key" step fails the
   release before anything is built if only one of the two is set, the variable
   does not parse, or the secret's public half is not in the variable. After
   GoReleaser publishes, "Sign checksums.txt with the project key" signs,
   verifies its own signature, and uploads `checksums.txt.sig`.
4. Check it: install the new release and run
   `tenant update --version <tag> --force --yes` (a reinstall of the same
   version through the full path). It prints `verified checksum and the
   project key's signature (...)`, or `verified checksum, the project key's
   signature and the release workflow's signature` with cosign installed. The
   release workflow's smoke job runs the same command on three systems for a
   public repository. Binaries of releases from before the key carry none, so
   updating *from* them shows nothing new.

**Replacing the key.** Make the new key with `keygen`. In release N, set
`TENANT_RELEASE_PUBKEYS` to `<old>,<new>` and keep signing with the old secret:
binaries of release N trust both. From release N+1, put the new private key in
`TENANT_RELEASE_SIGNING_KEY`. Once no machine you care about runs a release
older than N, drop the old public key from the variable. A binary from before
release N trusts only the old key and cannot take a release signed with the new
one; it is moved on by installing release N first (`tenant update --version
vN`). If the key is lost or stolen, remove it from the variable at once: the
releases made after that carry only the new key, but binaries already out there
still trust the stolen one until they update.

The workflow signs after GoReleaser has published the release, so for the few
seconds between the two a binary that carries the key refuses the new release
as unsigned; running `tenant update` again then works.

### Chrome process sandbox

Tenant launches Chrome with its process sandbox enabled by default and does
not pass `--no-sandbox`. If Chrome runs inside a separately isolated Linux
container or user namespace that cannot support its own sandbox, an operator
may set `"browser_no_sandbox": true` in `config.json`. Tenant rejects that
escape hatch unless it detects the outer isolation and emits a security warning
whenever the insecure mode is activated. Do not use it on a normal desktop or
as a workaround for Chrome installation or permission problems. Run
`tenant security audit` to report the effective state.

## Known Security Considerations

| Area | Status |
| ---- | ------ |
| **Single-user model** | Tenant assumes one operator. It does not authenticate users or separate sessions. Anyone with access to the running process can issue commands as the operator. |
| **Local secrets** | Credentials are on disk, protected by verified owner-only POSIX modes or a protected current-user-only Windows DACL. On a compromised account, this is insufficient. Use a dedicated user account and full-disk encryption. |
| **LLM output** | The agent processes and acts on LLM-generated text. Prompt injection is a recognized risk. The read-by-default permission model limits blast radius, but a sufficiently crafted input could trick the agent into requesting an action. Gated actions ask unless the operator already granted an allow mode/flag; network reads do not prompt per request. |
| **Execution sandbox** | **macOS** (Seatbelt): on by default in dry run, where it logs what it would refuse and re-runs it outside the sandbox, so it confines nothing until `exec_profiles.default.enforce` is `true`. Live-tested and red-teamed: filesystem roots, `network: none`, Mach/XPC services and signals to other processes are contained; there are no CPU, memory or process-count caps on commands. **Linux and Windows**: no confining backend yet. Commands run on the unconfined `host` backend, with the environment allowlist; on Windows a Job Object primitive (`jobobject`) adds resource limits only. A hardened `container` backend for every OS is available and opt-in per surface (`docs/plans/docker-sandbox-hardening.md`); it is not the default anywhere yet. A surface an operator sets to `restricted` where there is none fails closed instead of running unconfined (an entry that names a backend enforces unless it sets `"enforce": false`). |
| **Sandbox gaps** | There is no host-filtered network allowlist: `exec_profiles` rejects it and the backends fail closed, so the choice is `none` or `full`. A profile that allows network can reach Tenant's loopback listeners: the dashboard and the MCP gateway, when enabled, each still need their token, which confined commands cannot read. A command that needs a credential must be given it raw or run outside the sandbox after approval; there is no credential-injection proxy yet. The file tools (`os_read_file` and the rest) run in Tenant's own process and are not sandboxed. Apart from Tenant's secrets and the credential stores, they can read the rest of the operator's files. See `docs/changes/sandbox-seam.md`. |
| **Supply chain** | Go dependencies are pinned in `go.sum`. Run `go vet ./...` and review `go.mod` before building from source. |
| **Eval harness** | Tenant ships an evaluation harness with adversarial test fixtures. These are designed to probe the agent's safety boundaries. They do not contain real exploits. |
| **Memory and transcripts** | Agent memory is stored in plaintext SQLite; raw archives, saved transcripts, wiki content/indexes, logs, screenshots, and research artifacts are plaintext files. Sensitive conversation content may persist indefinitely. The `tenant memory` commands manage memory databases but do not remove every auxiliary artifact. |
| **Maturity** | This is pre-release software. Security hardening is ongoing. There has been no formal security audit. |

## Scope

### In scope

- Vulnerabilities in Tenant's handling of secrets, credentials, and OAuth
  tokens.
- Privilege escalation within the application (e.g., bypassing the permission
  gates on destructive actions).
- Remote code execution via plugin input or LLM output.
- Data exfiltration through unintended channels.
- Authentication or authorization flaws in MCP remote connector setup.

### Out of scope

- Vulnerabilities in third-party services Tenant connects to (Anthropic,
  Atlassian, Google, X, Discord, BlueBubbles, Jina). Report those to the
  respective vendor.
- Vulnerabilities in the user's Go toolchain, operating system, or network.
- Issues that require physical access to the machine *and* an already-compromised
  user account (the single-user model assumes the local account is trusted).
- Social engineering attacks against the operator.
- Denial-of-service against locally-running services.
- Theoretical prompt injection that does not demonstrate a concrete security
  impact beyond what the permission model already guards against.

## Security Document Changelog

- 2026-11-19 (TEN-542): added Release signatures: what cosign and the project
  key each prove, what a binary that carries the key refuses, what a fork
  gets, and how the maintainer makes and replaces the key.
- 2026-11-18 (TEN-535): Process protection on Windows now covers the console
  host's handle and code loaded through a window, and states what is still
  open: the starter's handle, the instant before the controls, and ending
  single threads.
- 2026-11-18 (TEN-535): added Process protection. The running process is no
  longer readable by the account's other programs on Linux and Windows; the
  files on disk still are, and the trust statement above stands for them.
- 2026-11-10: stated who is trusted (the local user account and its processes;
  the container sandbox protects the host from a command, not from them). The
  container sandbox's HOME is a tmpfs of the container's own, its image is
  built from a pinned `debian:trixie-slim`, and a `cpu_seconds` limit is a
  per-process cap there.
- 2026-11-10: the MCP HTTP+SSE gateway is no longer unauthenticated. It requires
  a bearer token (`<config>/gateway-auth-token`, a managed secret), checks Host
  and Origin, and accepts only JSON messages in the stream's own session.
- 2026-07-17 (TEN-317): corrected the inbound-listener description; added Jina
  Reader, arbitrary web destinations, search/model/embedding/MCP/peer outbound
  boundaries; and aligned storage and approval claims with the code inventory in
  `docs/DATAFLOW.md`.
