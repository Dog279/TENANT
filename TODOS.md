# TODOS

Deferred work with context. Each entry: What / Why / Pros / Cons / Context / Depends on.

## Migrate legacy SSE gateway to authenticated streamable-HTTP

- **What:** Replace the legacy two-endpoint SSE binding on `--sse-addr` (internal/mcp/transport/sse.go, served from cmd/tenant/commands.go ~404) with the go-sdk streamable-HTTP server + bearer auth, for operator MCP clients (Cursor, Zed remote, Claude Desktop).
- **Why:** After TEN-184 ships, Tenant maintains two server transports; the legacy one is the weaker (no auth, sleep-fragile SSE — the TEN-180 failure class).
- **Pros:** One transport to maintain; auth on every network surface; retires the last persistent push stream.
- **Cons:** Breaking for existing operator-client configs (URL/auth changes); adds zero new capability.
- **Context:** Eng review of the federation epic (2026-06-11) decided 1A (new separate peer listener, legacy untouched) for blast-radius reasons, plus 5A (`--insecure-lan` guard on the legacy bind) as the stopgap. This TODO is the eventual consolidation. Only sensible after TEN-184's go-sdk server binding is proven in real use.
- **Depends on:** TEN-184 shipped and stable.

## Per-peer rate limiting on the peer listener

- **What:** Token-bucket request budget per peer identity (e.g. N req/min) on the TEN-184 peer listener; 429 on excess; loud log on throttle.
- **Why:** A misbehaving/compromised peer or a bridge bug can hammer search/bus endpoints; nothing bounds it but CPU.
- **Pros:** Cheap blast-radius cap; keeps the hub's interactive loop responsive; makes the TEN-193 threat model's "queue flooding" line enforceable in Go.
- **Cons:** Another knob; limits set too low cause mysterious bridge stalls (hence the mandatory loud throttle log).
- **Context:** The bridge's adaptive 2s-30s polling (TEN-190) is cooperative, not enforced; this is the enforcement side. Deliberately kept out of TEN-184 to keep the wedge small — mutual-consent pairing means peers are hand-trusted today, so risk grows with fleet size, not from day one.
- **Depends on:** TEN-184 (listener + per-connection peer identity).
