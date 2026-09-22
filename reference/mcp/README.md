# reference/mcp — a tool server Tenant adopts live

[`toolserver/main.go`](toolserver/main.go) is a complete Model Context Protocol
tool server (Streamable HTTP, no sign-in, two example tools, logging into the
log book). Run it, and Tenant's agent — or you — adopts it **without a rebuild
or a restart**.

## The loop

```bash
go run ./reference/mcp/toolserver -addr 127.0.0.1:9000     # 1. start it
```

Then, in any Tenant turn, the agent calls:

```
mcp_add  url=http://127.0.0.1:9000/mcp  trust=allow  reason="example tools"
```

2. You get an approval prompt for the **adoption** (permission category `mcp`,
   in the TUI, the dashboard, or on Discord/iMessage). Approve.
3. The server's tools are live. Because the agent asked for `trust=allow`, you
   get **one more prompt**: let this server's tools run without per-call
   approval? Approve → trust `allow`. Deny → trust `ask` (each gated call
   prompts you, exactly like `exec` or `send`).
4. The agent can use `echo` and `write_note` from the next turn on. Their calls
   show in the log book under `turns/`; the server's own lines under its own
   folder and tab (`example-tools`).

The connection and its trust are persisted: after a Tenant restart the
connector reconnects on its own if the server is running.

As the operator you can do the same by hand (`/mcp add http://127.0.0.1:9000/mcp`,
the dashboard's Remote services page) and change trust any time:

```
/mcp trust mcp:127.0.0.1:9000 allow      # or ask | deny
```

## Making adoption hands-off

`/permissions set mcp allow` (or the same on `/relay permissions` for Discord
turns): adoption and trust prompts auto-approve, so an agent that builds a tool
can wire it in and use it in one turn. `deny` forbids adoption entirely.
Unattended cron jobs never get the `mcp_*` tools regardless.

## Per-tool rules (for tools nobody pre-programmed)

Trust is per server; rules are per tool, and they win over both the category
and the trust mode:

```
/permissions add write_note allow          # this tool, on any server, never prompts
/permissions add delete_* deny             # any tool named delete_… is refused
/permissions add mcp:127.0.0.1:9000:* allow   # everything this server offers
/permissions rm delete_*
```

The same rules are one JSON object in `settings.<agent>.json`:

```json
"tool_rules": {"write_note": "allow", "delete_*": "deny"}
```

Edit the file and the running hub applies it within seconds; the dashboard's
Tools page has the same list. A tool with no rule follows its server's trust
(`ask` = prompt; `allow` = run), exactly as above.

## Writing your own

- Keep it Streamable HTTP (the SDKs' default). Tenant connects only over HTTP;
  a server that never answers 401 needs no OAuth setup at all.
- Bind loopback unless the server should be reachable from other machines.
- Tool **names are global** in Tenant's mux: if two servers both offer `echo`,
  the first one connected keeps the name and the second's is ignored. Prefix
  your tools (`disk_usage`, `disk_prune`).
- Mark read-only tools with `ReadOnlyHint` so an operator running with
  `--mcp-trust-annotations` gets them unprompted even under trust `ask`.
- Log through `tenantlog` ([`reference/logging`](../logging/)) so the server
  has its own folder and tab; its tool calls are already recorded by Tenant.
- Copying out of the repo: this file + `reference/logging/tenantlog.go`,
  `go mod init`, `go get github.com/modelcontextprotocol/go-sdk`.
