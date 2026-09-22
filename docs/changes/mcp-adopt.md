# Change: live tool-server adoption — `mcp_add`, per-connector trust, no more one-add-per-session

**Branch:** `feat/logs-hub` · **Status:** implemented, tested, not yet deployed.

## What changed
The agent can adopt a Model Context Protocol tool server it built (or any other) into its own
tool surface at runtime, the operator decides its trust with the same ask/allow/deny vocabulary
as every other permission, and a long-running hub can do this indefinitely.

- **Agent tools** `mcp_list` / `mcp_add` / `mcp_remove` / `mcp_trust` (`cmd/tenant/mcptools.go`),
  mounted in serve and the TUI, cut from cron runners. `mcp_add url=… [trust=allow] [reason=…]`
  is gated by a new permission category **`mcp`** (ask by default; `allow` = hands-off adoption;
  `deny` = forbidden) — visible in `/permissions`, `/relay permissions`, `/imessage permissions`.
  When the agent asks for `trust=allow`, a second prompt lets the operator decide whether that
  connector's tools run without per-call approval.
- **Per-connector trust, live** (`mcpremote.Trust*`): `ask` (default — every gated call prompts;
  read-only-annotated tools skip the gate when `--mcp-trust-annotations` is on, as before),
  `allow` (its tools run unprompted), `deny` (refused and hidden). Pushed into the running
  dispatcher — no reconnect, no restart. Persisted per URL in `config.json` (`mcp_trust`) and
  re-applied when the connector reconnects at launch. Surfaces: `/mcp trust <label> <mode>`
  (+ `/mcp allow|ask|deny <label>`), the dashboard's Remote services page (a per-server select),
  and `mcp_trust`.
- **The callback port is bound only during a sign-in.** Before, an interactive connect held
  `127.0.0.1:8765` for the connector's lifetime, so a hub that had added one connector could
  not add a second without a restart. Sign-ins now bind → open the browser → capture the code →
  release, serialized process-wide; a server that never answers 401 (a local tool server) never
  touches the port at all.
- **Labels keep the port** (`mcp:127.0.0.1:9000`), and a URL that would still collide gets its
  path appended — several local servers on one host coexist. Hosted services' labels are
  unchanged (`mcp:mcp.atlassian.com`).
- **Docs + reference:** `reference/mcp/toolserver` is a complete tool server (two tools, no
  sign-in, logs into the log book) with the adoption loop written up in `reference/mcp/README.md`.

## Why
The report of 2026-09-22 found the architecture already supported "an LLM builds a tool, Tenant
picks it up live" through MCP, but three things stopped it from being one step: the agent had no
way to connect a server itself, every call to a new server prompted as `destructive` with only a
launch flag to relax it, and the OAuth callback port made the second live add of a session fail.
Dylan runs the hub for months at a time; restarts are not an option.

## Evidence
`internal/plugins/mcpremote`: lazy fetcher holds the port only during a sign-in, serializes
concurrent sign-ins, honors a cancelled ctx; trust modes in the dispatcher. `cmd/tenant`: the
adoption flow with a fake control (approved+trusted, trust declined, not requested, declined,
fail-closed with no confirm, bad input, connect error), trust/remove/list, category + cron cut,
and an **end-to-end test with a real go-sdk Streamable-HTTP server** — adopted with no browser
and no callback port, gated under ask, unprompted under allow, hidden under deny, a second local
server with its own label, and persisted trust applied on silent reconnect. Label rule, trust
persistence. Dashboard trust form. Full `cmd/tenant`, `dashboard`, `tui`, `mcpremote` suites green.

## Operator notes
- Nothing changes for existing connectors: they start with trust `ask`, which is today's
  behavior. `--mcp-trust-annotations` still works.
- To let the agent adopt tools hands-off: `/permissions set mcp allow` (and on the relay for
  Discord turns). Each adopted server still starts at `ask` unless the agent requested `allow`
  and that category auto-approved it.
- Tool names are global in the mux: two servers offering the same tool name → the first keeps it.
