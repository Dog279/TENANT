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
*(Replace with a real address before launch. A dedicated security@ alias or
GitHub Security Advisories is recommended.)*

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
(Anthropic Claude, Atlassian, Google Workspace, X, Discord) only when the user
configures them.

### Secrets and credentials

- **API keys and tokens** are stored in
  `~/Library/Application Support/tenant/credentials.json` (macOS; equivalent
  OS config directories on Linux and Windows).
- The credentials file is written with **0600 permissions** (owner read/write
  only).
- **No secrets are committed to the repository.** The repo is a clean template;
  `credentials.json` is in `.gitignore`.
- MCP remote connectors use **OAuth 2.1 with Dynamic Client Registration
  (DCR)**. OAuth tokens are persisted locally with 0600 permissions.

### Local storage

- **SQLite** databases store memory, episodes, the wiki index, and transcripts.
  They live in the OS data directory, not in the repo.
- No data leaves the machine unless a plugin explicitly sends it (e.g., posting
  to Discord, calling an LLM API).

### Permission model

Plugins are **read-by-default**. Mutating and destructive operations are gated:

- Shell execution, file writes, SQL writes, and outbound sends require
  **explicit user approval** per action (or an `--allow-*` flag for one-shot
  commands).
- The agent proposes actions; it does not auto-execute destructive operations.
- Eleven plugin channels are available: OS (shell + files), web, SQL, Google
  Workspace, X, iMessage, Discord, wiki, cron, Atlassian, and MCP remote. Each
  respects the same read-by-default, gate-to-act pattern.

### Network

- Outbound connections: LLM API endpoints, Atlassian APIs, Google APIs, X API,
  Discord API, BlueBubbles (iMessage bridge). No others.
- **No telemetry, no analytics, no phone-home.** Tenant never calls a server
  the user did not configure.
- No inbound listening ports in normal operation (the web dashboard renders
  server-side and does not open a public port).

## Known Security Considerations

| Area | Status |
| ---- | ------ |
| **Single-user model** | Tenant assumes one operator. It does not authenticate users or separate sessions. Anyone with access to the running process can issue commands as the operator. |
| **Local secrets** | Credentials are on disk, protected by file permissions. On a shared or compromised machine, this is insufficient. Use a dedicated user account and full-disk encryption. |
| **LLM output** | The agent processes and acts on LLM-generated text. Prompt injection is a recognized risk. The read-by-default permission model limits blast radius, but a sufficiently crafted input could trick the agent into requesting a destructive action. The user still must approve it. |
| **Supply chain** | Go dependencies are pinned in `go.sum`. Run `go vet ./...` and review `go.mod` before building from source. |
| **Eval harness** | Tenant ships an evaluation harness with adversarial test fixtures. These are designed to probe the agent's safety boundaries. They do not contain real exploits. |
| **Memory and transcripts** | Agent memory, episode transcripts, and wiki content are stored in plaintext SQLite. Sensitive conversation content may persist indefinitely. The `tenant memory` commands can prune and manage this data. |
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

## Pre-launch checklist

Before opening the repository to the public, ensure:

- [ ] Replace the placeholder contact email above with a real address.
- [ ] Consider enabling GitHub Private Vulnerability Reporting on the repo.
- [ ] Add a `SECURITY.md` link to `README.md`.
- [ ] Verify `.gitignore` covers `credentials.json`, `*.db`, and all data
      directories.
- [ ] Run `go vet ./...` and address any findings.
- [ ] Confirm no secrets or personal data exist in git history
      (`git log --all -p | grep -i 'key\|token\|secret\|password'`).
