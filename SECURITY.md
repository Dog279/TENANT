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
(cloud model providers, Atlassian, Google Workspace, X, Discord, and configured
integration endpoints). When web access is enabled, browser navigation can
contact arbitrary HTTP(S) destinations, keyless search can contact DuckDuckGo,
and a failed local read can send the complete target URL to Jina Reader without
a Jina key. See `docs/DATAFLOW.md` for the code-level inventory.

### Secrets and credentials

- **API keys and tokens** are stored in
  `~/Library/Application Support/tenant/credentials.json` (macOS; equivalent
  OS config directories on Linux and Windows).
- The credentials file is requested with **0600 permissions**. That is
  owner-only on Unix; on Windows, Go's mode bits do not create a new owner-only
  ACL, so protection depends on the containing profile directory and existing
  Windows ACLs.
- **No secrets are committed to the repository.** The repo is a clean template;
  `credentials.json` is in `.gitignore`.
- MCP remote connectors use **OAuth 2.1 with Dynamic Client Registration
  (DCR)**. OAuth tokens are persisted locally with requested mode 0600, subject
  to the same Windows ACL caveat.

### Local storage

- **SQLite** databases store memory, episodes, learned skills, usage, and
  self-improvement metadata. Wiki indexes, transcripts, archives, logs,
  screenshots, and research artifacts use files alongside those databases.
  They live in the OS data/config directories or an operator-selected path, not
  in the repo.
- Outbound reads also disclose data: model calls send prompt/tool context;
  search sends queries; browser/Jina reads send URLs; integrations send resource
  identifiers and credentials. The complete flows are in `docs/DATAFLOW.md`.

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
- Eleven plugin channels are available: OS (shell + files), web, SQL, Google
  Workspace, X, iMessage, Discord, wiki, cron, Atlassian, and MCP remote. Each
  has the gate behavior documented in the data-flow matrix.

### Network

- Outbound connections include model and embedding endpoints; arbitrary browser
  destinations; DuckDuckGo, Brave, Tavily, and Jina web services; Atlassian,
  Google, X, and Discord APIs; BlueBubbles; remote MCP servers and discovered
  OAuth endpoints; Tenant peers; and explicitly configured control endpoints.
- **No telemetry or analytics path is implemented.** This does not mean every
  destination is individually configured: DuckDuckGo and Jina are keyless web
  fallbacks after the web feature is enabled.
- The dashboard is a normal inbound listener: TUI/serve enables it by default on
  `127.0.0.1:8770`. Optional listeners include the MCP HTTP+SSE gateway, peer
  server, and temporary OAuth callbacks. Non-loopback dashboard exposure
  requires TLS plus auth; the unauthenticated MCP gateway requires an explicit
  insecure override for non-loopback binds.

## Known Security Considerations

| Area | Status |
| ---- | ------ |
| **Single-user model** | Tenant assumes one operator. It does not authenticate users or separate sessions. Anyone with access to the running process can issue commands as the operator. |
| **Local secrets** | Credentials are on disk, protected by file permissions. On a shared or compromised machine, this is insufficient. Use a dedicated user account and full-disk encryption. |
| **LLM output** | The agent processes and acts on LLM-generated text. Prompt injection is a recognized risk. The read-by-default permission model limits blast radius, but a sufficiently crafted input could trick the agent into requesting an action. Gated actions ask unless the operator already granted an allow mode/flag; network reads do not prompt per request. |
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
  Atlassian, Google, X, Discord, BlueBubbles). Report those to the respective
  vendor.
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
