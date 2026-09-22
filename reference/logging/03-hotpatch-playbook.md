# 03 · Hot-patch playbook — plug a new element into the live box

The situation: `tenant` is running (the R640, a Mac, wherever) and you are
adding a custom element — a sidecar, a quality loop, a watcher, a scheduled
script — that should join the audited record now, without a `tenant` rebuild
or restart. The log book makes that a drop-in: **a source registers itself the
first time it writes.**

## Checklist

1. **Pick the source name** — the folder and the tab. Lowercase, `[a-z0-9._-]`,
   ≤ 40 chars, not one of the built-ins. One element, one source (use `Sub`
   for a second folder from the same program).
2. **Copy `tenantlog.go` in**, set the source (see [01](01-quickstart.md)).
3. **Decide how it reaches the hub:**
   - same box, same user → nothing to do; `config.json` is read for the
     address and token;
   - same box, different user (a system service) → set `TENANT_URL` and
     `TENANT_TOKEN` in the service's environment (the token is
     `dashboard.auth` in Tenant's `config.json`; treat it like a password);
   - another box → `TENANT_URL=https://hub:8770` and the token; the
     dashboard must be exposed with TLS + auth (see `docs/serve-runbook.md`);
   - no network at all (a cron job that must not depend on the hub) →
     `FileOnly: true`, or just run `tenant log …` from the script.
4. **Build and start the element.** Nothing on the `tenant` side changes.
5. **Verify:**

   ```bash
   tenant log --list                            # the source appears, "external" or "tenant"
   tenant attach --follow --source <name>       # lines arrive live
   ls "<data>/logs/<name>/"                     # the day's file exists
   ```

   and open the dashboard → Activity → the new tab. `Ping()` in the program
   tells you up front whether lines will go live or to the file.
6. **Retention is automatic:** the hub deletes files older than
   `logs.retention_days` (30) from every folder under its root, including
   yours. Copy a file elsewhere if it must outlive that.

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
curl -s -X POST "http://127.0.0.1:8770/api/logs/backup" \
  -H "Authorization: Bearer $TENANT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"level":"info","msg":"snapshot ok","fields":{"size":"12G"}}'
```

`GET /api/logs` lists sources; `GET /api/logs/<name>` lists files;
`GET /api/logs/<name>/<file>` returns one. The full API table is in
`docs/LOGGING.md`.

## Rolling back

Stop the element. Its folder and files stay (they are the record); the tab
stays as long as the folder exists. Delete the folder to remove the tab —
the hub notices on its next scan.

## Upgrading the copy later

`git pull`, then diff your copy against `reference/logging/tenantlog.go`. The
format section is the only part that must match the hub; the tests in this
folder tell you if it drifted. A copy that only changed `Defaults`, `Event`
kinds or transport settings needs no update at all.

## When it is more than a sidecar

If the element should give the agent new **tools**, make it an MCP tool server
and let the agent adopt it live with `mcp_add` — see [`reference/mcp/`](../mcp/).
If it is really a new Tenant subsystem — it runs inside the binary, drives an
agent, or wants its lines styled like turns — see
[04 · inside the binary](04-inside-the-binary.md). That path needs a rebuild,
but the folder and tab work exactly the same way.
