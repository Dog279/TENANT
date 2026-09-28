# 03 · Hot-patch playbook — plug a new element into the live box

The situation: `tenant` is running (a server, a Mac, wherever) and you are
adding a custom element — a sidecar, a quality loop, a watcher, a scheduled
script — that should join the audited record now, without a `tenant` rebuild
or restart. The log book makes that a drop-in: **a log registers itself the
first time it is written.**

## Checklist

1. **Pick the source name.** Lowercase, `[a-z0-9._-]`; the element's lines
   are filed in the log `ext.<name>`. One element, one log (use `Sub` for a
   second one from the same program).
2. **Copy `tenantlog.go` in**, set the source (see [01](01-quickstart.md)).
3. **Decide how it reaches the hub:**
   - same box, same user → nothing to do; Tenant's config dir is read for
     the address and token;
   - same box, different user (a system service) → set `TENANT_URL` and
     `TENANT_TOKEN` in the service's environment (the token is
     `dashboard.auth` in Tenant's `config.json`, or on a default install the
     `dashboard-auth-token` file beside it; treat it like a password);
   - another box → `TENANT_URL=https://hub:8770` and the token; the
     dashboard must be exposed with TLS + auth (see `docs/serve-runbook.md`);
   - no network at all (a cron job that must not depend on the hub) →
     `FileOnly: true`: every line waits in the spool for Tenant.
4. **Build and start the element.** Nothing on the `tenant` side changes.
5. **Verify:**

   ```bash
   tenant logs                                   # ext.<name> is in the list
   tenant logs query log:ext.<name> --follow     # lines arrive live
   ```

   and open the dashboard → Logs → External. `Ping()` in the program tells
   you up front whether lines will go live or to the spool.
6. **Retention is automatic:** `ext.<name>` keeps the operational default
   (30 days, 64 MB). Change it with `tenant logs config ext.<name> --max-age 90d`.

## No code at all: write the files

Anything that can append text to a file is a source. A folder dropped under
the root is discovered within ~5 s and its newest file is followed:

```bash
d="$HOME/Library/Application Support/tenant/logs/backup"; mkdir -p "$d"
printf '%s INFO  [backup] snapshot ok\tsize=12G\n' "$(date '+%Y-%m-%d %H:%M:%S.000')" >> "$d/$(date +%F).log"
```

```powershell
$d = "$env:LOCALAPPDATA\tenant\logs\backup"; New-Item -Force -ItemType Directory $d | Out-Null
"$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss.fff') INFO  [backup] snapshot ok`tsize=12G" | Add-Content "$d\$(Get-Date -Format yyyy-MM-dd).log"
```

Lines in the format keep their level and fields; any other text is kept
verbatim as an INFO line stamped when it was read. `tenant log backup
"snapshot ok" size=12G` does the same with less typing.

## Over HTTP from anything

```bash
curl -s -X POST "http://127.0.0.1:8770/api/events" \
  -H "Authorization: Bearer $TENANT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"log":"backup","level":"info","msg":"snapshot ok","data":{"size":"12G"}}'
```

It is filed in `ext.backup`. A batch is one event per line with
`Content-Type: application/x-ndjson`. An event may set its log, level, time,
message, data, and `trace` and `activity` (from `TENANT_TRACE_ID` and
`TENANT_ACTIVITY_ID`); anything else is dropped, and a log or provider that is
Tenant's own is refused with HTTP 403. `GET /api/events?q=log:ext.backup`
reads it back. The full API table is in `docs/LOGGING.md`.

## Rolling back

Stop the element. Its log stays (it is the record) until retention ages it
out, or `tenant logs clear ext.<name>` empties it.

## Upgrading the copy later

`git pull`, then diff your copy against `reference/logging/tenantlog.go`. The
event JSON and the text format are the only parts that must match the hub;
the tests in this folder tell you if they drifted. A copy that only changed
`Defaults`, `Event` kinds or transport settings needs no update at all.

## When it is more than a sidecar

If the element should give the agent new **tools**, make it an MCP tool server
and let the agent adopt it live with `mcp_add` — see [`reference/mcp/`](../mcp/).
If it is really a new Tenant subsystem — it runs inside the binary, drives an
agent, or wants its lines styled like turns — see
[04 · inside the binary](04-inside-the-binary.md). That path needs a rebuild,
and its log is its own, not under `ext.`.
