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

### Secrets and credentials

- **API keys and tokens** are stored in
  `~/Library/Application Support/tenant/credentials.json` (macOS; equivalent
  OS config directories on Linux and Windows).
- Tenant centralizes credentials, the Atlassian, GitHub, X and MCP OAuth
  caches, the gsuite OAuth client, the dashboard token, peering tokens, and
  peer private keys behind one secret-storage boundary. On Unix, files are
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
  - By default every surface runs the `restricted` backend in **dry run**: a
    command it refuses is logged and re-run outside the sandbox, so it confines
    nothing yet. On Windows that is Linux `sh` inside WSL2, not PowerShell. On
    Linux, and on Windows without WSL, the default is the `host` backend, which
    confines nothing, and `tenant doctor`, the audit and the startup feed say
    why. `exec_profiles.default.enforce: true` turns enforcement on
    (`docs/changes/exec-defaults-dry-run.md`).
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
  `127.0.0.1:8770`. Optional listeners include the MCP HTTP+SSE gateway, peer
  server, and temporary OAuth callbacks. The dashboard requires its auth token
  on every listener, loopback included, and TLS as well off loopback; the
  unauthenticated MCP gateway requires an explicit insecure override for
  non-loopback binds.

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
| **Execution sandbox** | On by default in dry run on macOS and Windows, where it logs what it would refuse and re-runs it outside the sandbox, so it confines nothing until `exec_profiles.default.enforce` is `true`. On Linux the default is the unconfined `host` backend (reason below), until `exec_profiles` names `restricted`. **macOS** (Seatbelt): live-tested and red-teamed; filesystem roots, `network: none`, Mach/XPC services and signals to other processes are contained; there are no CPU, memory or process-count caps. **Linux** (bubblewrap): implemented and unit-tested but not yet validated on a live host, so run `tenant sandbox selftest` first. It is not the default because a dry run under it would fail reads of the deny roots (`~/.ssh` for `git`) instead of re-running them, and `/tmp` is private to each command. **Windows**: `restricted` runs the command inside a WSL2 distro under bubblewrap, giving the same filesystem and `network: none` confinement as Linux (the command runs as Linux `sh`, and so does its re-run outside the sandbox; Windows programs run only outside it, after approval; Windows paths are translated to their WSL mount). It is the Windows default since 2026-09-29; without WSL, the default falls back to PowerShell on the host and says why. At startup it selects a distro whose bubblewrap actually works and **fails closed** if none can, so a `restricted` surface is never run unconfined; validate with the Linux build inside WSL. A separate Job Object primitive provides resource limits. The native token/ACE/WFP path is retired: WSL2 + bubblewrap replaces it. |
| **Sandbox gaps** | There is no host-filtered network allowlist: `exec_profiles` rejects it and the backends fail closed, so the choice is `none` or `full`. A profile that allows network can reach Tenant's loopback listeners: the dashboard still needs its token, which confined commands cannot read, but the MCP gateway, when enabled, has no auth. A command that needs a credential must be given it raw or run outside the sandbox after approval; there is no credential-injection proxy yet. The file tools (`os_read_file` and the rest) run in Tenant's own process and are not sandboxed. Apart from Tenant's secrets and the credential stores, they can read the rest of the operator's files. See `docs/changes/sandbox-seam.md`. |
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

- 2026-07-17 (TEN-317): corrected the inbound-listener description; added Jina
  Reader, arbitrary web destinations, search/model/embedding/MCP/peer outbound
  boundaries; and aligned storage and approval claims with the code inventory in
  `docs/DATAFLOW.md`.
