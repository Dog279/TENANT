# ADR 0001: Tenant is an appliance; MCP and Agent Skills are how you extend it

**Status:** Accepted, 2026-09-26 · **Ticket:** ARC-004 / TEN-324 · **Decided by:** the owner

## Context
Tenant has called itself an "agent framework" since the first design doc, but nothing a
framework needs exists:

- The module path is `tenant`, which nothing outside this repository can import.
- Every package that does real work is under `internal/`.
- There is no versioned API, no compatibility promise, and no out-of-tree plugin.

Meanwhile the extension work that users actually asked for has gone through the Model
Context Protocol:

- remote MCP servers with OAuth sign-in;
- live adoption with `mcp_add`;
- per-connector trust (`ask` / `allow` / `deny`) and per-tool rules.

The July 2026 architecture review (finding "Tenant is called a framework but is not
embeddable") asked for one position, recorded, with the README to match.

Tenant's security model settles the question as much as its code does. Approval
categories, the sensitive-egress gate, the execution sandbox and the file-tool guards all
assume that code running inside the process is Tenant's own. An in-process plugin loaded
from elsewhere would sit inside every one of those boundaries.

## Decision
Tenant is an **appliance**: one binary you install, configure and run, not a library you
build on.

1. **Extension boundaries.** There are two stable ones:
   - **MCP servers.** A tool you want Tenant to have runs as an MCP server. Tenant talks
     to it over the protocol, gates it with connector trust and tool rules, and, for a
     local server, runs it out of process under the execution sandbox where the OS has one
     (EXT-003 / TEN-318).
   - **Agent Skills.** Know-how you want Tenant to have is a skill: a `SKILL.md` package in
     the open Agent Skills format that Tenant imports, exports, versions and trusts
     explicitly (EXT-001 / TEN-298).
2. **In-process plugins are first-party only.** They are compiled into the binary and
   reviewed like the rest of it. The plugin descriptor registry (ARC-003 / TEN-321) is how
   Tenant organizes its own plugins. It is an internal mechanism with no compatibility
   promise, not a plugin API.
3. **Go packages are not an API.** The module path stays `tenant`, and everything stays
   under `internal/`. Refactors (ARC-001, ARC-002) may move, split or rename any package.
4. **Operator interfaces are documented, not extension points.** These change with notes
   in the change records, but carry no API guarantee:
   - the CLI;
   - `config.json` and `settings.<agent>.json`;
   - the dashboard's HTTP endpoints;
   - the log book format.

## What is stable
- **MCP:** Tenant speaks the protocol version it advertises (`mcp.ProtocolVersion`) and
  negotiates as the specification says. These stay the same unless a change record says
  otherwise:
  - connector tools appear to the model as `mcp:<server>:<tool>`;
  - connector trust and tool rules keep their meaning.
- **Agent Skills:** Tenant reads and writes `SKILL.md` per the open format. Its own
  metadata (version, trust, provenance, required tools) lives in fields other tools
  ignore, so a package exported from Tenant opens anywhere and a standard package imports
  into Tenant.

## Consequences
- Third-party code never runs inside the Tenant process. Anything that is not first-party
  runs out of process, behind approval categories, and sandboxed where it runs locally.
- No plugin ABI to maintain, and no public Go API to version. Internal refactors cost
  what they cost inside this repository and nothing outside it.
- The README, `tenant --help`, the architecture doc and the plugin doc say "agent" or
  "appliance", not "framework".
- `docs/DESIGN.md` is kept as the historical design and not rewritten. This ADR
  supersedes what it says about Tenant being a framework.

## Alternatives considered
- **Framework:** publish a canonical module path (`github.com/Dog279/TENANT`), move the
  agent, model and plugin interfaces out of `internal/`, and version them. It is rejected
  for now:
  - it commits the project to API stability while the architecture work (ARC-001,
    ARC-002, ARC-003) still reshapes those interfaces;
  - it invites untrusted in-process code past the security boundaries;
  - no one outside this repository needs it today.
- **Both:** an appliance with an "experimental" public API. It is rejected: an API people
  build on is an API, whatever it is called.

Revisit this if someone needs to embed Tenant's runtime in their own program. That is the
case MCP cannot serve, and it needs a new ADR that names the module path, the public
packages and their versioning rules.
