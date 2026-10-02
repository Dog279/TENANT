# Running Tenant 24/7 — `tenant serve` (TEN-194)

`tenant serve` runs the **full long-term brain headless**: the same agent, tools,
memory, self-improvement, cron jobs, federation peer listener, and comms relays the
interactive TUI drives — but with no terminal UI. You "hop in" through the **web
dashboard**, which is always on in serve mode and is the live control + chat +
approval surface.

```
tenant serve                      # loopback dashboard on 127.0.0.1:8770
tenant serve --dashboard-addr 0.0.0.0:8770   # LAN/tailnet — REQUIRES TLS + auth (see below)
tenant serve --self-improve=false # disable background distill/consolidate/profile
```

## What runs in serve mode

| Subsystem | Default | Notes |
|---|---|---|
| Agent + full tool surface | on | memory, skills, recall, sub-agent orchestration |
| Web dashboard | **on** | the only control surface — chat, tools, approvals, models, memory |
| Headless approval drain | on | gated "ask" actions wait on the dashboard's Approvals & safety page and at `/api/approvals` (no TUI to prompt) |
| Self-improvement scheduler | on | distill / skill-induction / consolidate / profile; `--self-improve=false` to disable |
| Cron jobs | on | recurring agent prompts; honors `cron.allow_exec` + timezone |
| Federation peer listener | if `peer.listen` set | paired peers reach this instance |
| Discord relay | if a token is configured **and** `relay.enabled` | drive the hub from your phone |
| Nightly eval / soul-nudge | config-gated, off by default | `improve.eval_every` / `improve.soul_nudge_every` |

While the model is **degraded to echo** (endpoint unreachable / misconfigured), all
autonomous work (cron, self-improve, relay turns) is suspended automatically — exactly
as in the TUI. Recover by fixing the endpoint/keys; the reconnect monitor restores it
without a restart.

## Concurrency

The shared brain is driven through a single **turn gate**: only one turn runs at a
time across all sources (the dashboard now, peer-delegated runs later), so unattended
traffic can never race the working set. `GET /api/status` reports `turn_active`,
`turn_age_secs`, and `pending_approvals` for liveness.

## Approvals (important for unattended runs)

There is no terminal to approve a dangerous action. A gated **ask**-mode call waits in
a queue: answer it on the dashboard's **Approvals & safety** page (it appears within a
second, with what the agent wants to do and why) or via REST. Requests raised in a
Discord conversation are answered with the buttons on the Discord card; the page only
says they are there. See [dashboard-runbook.md](dashboard-runbook.md) §4.

```
GET  /api/approvals                      # list pending: [{id, category, action, detail, age_secs}]
POST /api/approvals/<id>  {"decision":"approve"}          # or approve_session | approve_always | deny
```

The page offers Allow once and Deny, and "for the rest of this session" behind a
confirm step where that grant is safe to show. `approve_always` is REST-only: a
permanent change is a category's mode.

To run hands-off, set the categories you trust to **Allow** on the Approvals & safety
page, or at launch with the plugins' allow flags (`--os-allow-exec`, `--os-allow-write`,
`--gsuite-allow-send`, `--imessage-allow-send`, `--discord-allow-send`,
`--web-allow-interact`, …); a mode saved on the page wins over a flag. Anything left in
**ask** waits for a decision (fail-closed). Waiting never approves: a request is refused
when the job that asked ends (a scheduled job's run is capped at 15 minutes,
`cron.per_run_minutes`), and every pending one is denied on shutdown.

## Network exposure

The dashboard binds **loopback by default**. A non-loopback `--dashboard-addr` is
**refused at startup unless BOTH TLS and an auth token are set** (`dashboard.tls_cert`,
`dashboard.tls_key`, `dashboard.auth`). To reach it from elsewhere without managing
certs, prefer publishing the loopback dashboard onto your tailnet (`/tailscale serve`
from a TUI session, persisted) rather than binding a public interface.

## The dashboard token

Every dashboard request needs the bearer token in `<config>/dashboard-auth-token`
(created on first start), or `dashboard.auth` in config.json when you set one. Only
`GET /healthz` is open, for monitors; `HEAD /healthz` needs the token too.
`tenant serve` prints the token at start-up only when stdout is a terminal. Under
launchd or systemd, stdout goes to a file, so the start-up banner and the `system`
log line say where the token lives instead: `cat` that file to see it.
`tenant attach` and `tenant logs` read it themselves.
A browser presents the token once, on the sign-in form, and gets a random session id
in its cookie, never the token: the session lasts 7 days, ends on Log out, and every
session ends when serve restarts. Only the `Authorization: Bearer` header takes the
token itself; no path takes it from the URL (`/ws` included).

Builds before TEN-360 printed the token to stdout (launchd keeps it in `serve.log`,
which nothing rotates) and wrote it into `<data>/logs/system/*.log`. If you ran one,
rotate the token: stop serve, delete `<config>/dashboard-auth-token` (or change
`dashboard.auth`), and start serve again. Browsers then sign in with the new token.

**Windows service.** Under a service account `<config>` is that account's
`%AppData%\tenant`: `C:\Windows\System32\config\systemprofile\AppData\Roaming\tenant`
for SYSTEM, `C:\Windows\ServiceProfiles\LocalService\AppData\Roaming\tenant` for LOCAL
SERVICE, unless the service passes `--config`. An administrator reads the token from an
elevated PowerShell with `Get-Content -LiteralPath "<config>\dashboard-auth-token"` on
builds that share a service's secrets with Administrators (September 2026); older
builds lock administrators out of the token and config.json alike. The steps, and
checking the folder with `icacls`, are in [dashboard-runbook.md](dashboard-runbook.md)
§2. A service has no screen, so a remote service's or an integration's browser sign-in
is finished in a browser on the server itself (dashboard-runbook.md §7).

## Graceful shutdown

`SIGTERM` / `SIGINT` shut down in order: stop accepting new work (peer listener →
Discord relay → dashboard), drain the in-flight turn and any running cron/improve job,
then close the stores last. Pending approvals are denied so blocked turns unblock.

## Run as a service

`tenant service install` writes the unit for this OS, enables it and starts it (TEN-498);
`tenant setup` offers it at the end. The unit pins `--config` and `--data` to the dirs the
command ran with and passes `--stop-budget`, the grace the OS grants a stop.

```
tenant service install [--print]   # --print shows the unit and changes nothing
tenant service status              # installed? running (version, turn_active)? where the logs are
tenant service restart [--now]     # waits for the running turn to finish (15 min ceiling) unless --now
tenant service start | stop
tenant service uninstall
```

`tenant doctor` warns when no service is installed or when the unit no longer matches the
dirs or binary this command uses; `--fix` rewrites it.

| | macOS | Linux | Windows | Windows `--system` |
|---|---|---|---|---|
| Unit | LaunchAgent `~/Library/LaunchAgents/com.tenant.serve.plist` | systemd user unit `~/.config/systemd/user/tenant.service` | Scheduled task `\Tenant\Serve` at logon, as you | Windows service `Tenant` as SYSTEM (elevated terminal; headless boxes) |
| Logs | `~/Library/Logs/tenant/serve.log`, `serve.err.log` | `journalctl --user -u tenant -f` | none for stdout | none for stdout |
| Stop budget | 90s | 90s | 4s | 120s |

Everything the hub does is in the log book either way (`tenant logs`, the dashboard's Logs page).

- **macOS (TCC):** `install` refuses a binary, config dir or data dir under `~/Desktop`, `~/Documents` or
  `~/Downloads`: macOS silently denies those to a background process. Good homes: `~/.local/bin`,
  `~/.config/tenant`, `~/Library/Application Support/tenant`.
- **Linux (linger):** the user manager stops at logout unless the account lingers; `install` prints
  `loginctl enable-linger <user>` when it is off. Run it once.
- **Windows:** the task keeps a hidden console, which is how logoff and shutdown reach the process;
  `restart` and `stop` end the task hard (hence the 4-second budget). `--system` is a real service with
  a Stop control and `sc failure` restarts; `sc`/the Services app work on it too.

## Upgrade and roll back

`tenant update` moves an install to the latest release with a way back (TEN-500). It downloads the
release archive for this OS and architecture, verifies its SHA-256 (and its signature when `cosign`
is on PATH), runs the new binary's `version` before touching anything, asks, then:

1. writes an unencrypted backup of the config and data dirs to `<data dir>-backups/pre-update-<old version>.tenant-backup`
   (credentials are left out; the previous pre-update backup is replaced);
2. swaps the binary in with no gap, keeping the old one as `tenant.prev`;
3. restarts the service (`tenant service restart`, which waits for the running turn unless `--now`);
4. polls `/api/status` for up to 90 s until it reports the new version; if it never does, the old binary
   goes back and the service is restarted again.

```
tenant update --check                 # one line, running vs latest; exit 10 when an update exists
tenant update [--version vX.Y.Z] [--yes] [--now] [--force]
tenant update --rollback              # put tenant.prev back; prints the data-restore command
```

**Schemas move forward only.** The log book refuses a database written by a newer version, so once
the new version has migrated a store, the old binary alone is not enough. Rollback is the old binary
plus the pre-update backup, restored by hand, never automatically:

```
tenant service stop && tenant backup restore <data dir>-backups/pre-update-<old version>.tenant-backup --yes && tenant service start
```

An OS stop in the middle leaves `<data>/update-state.json`: serve logs it at boot, `tenant doctor` warns,
and the next `tenant update` clears it when the new version is what runs, or offers `--rollback`.
serve also checks for a newer release once a day and files one log-book event per new tag
(`"update_check": false` in `config.json` turns that off); nothing is ever applied unasked.

## Restart-storm guard (24/7)

If you rely on cron, leave **`cron.catchup` OFF** (the default). With catchup on, a
restart after downtime fires every missed job at once. Federation resumes from its
persisted peer cursor, so a restart does not re-pull the world.

## Health

`tenant doctor` includes a **serve liveness** check: when a hub is reachable it probes
`/api/status` and WARNs on a stuck turn or a waiting approval queue; it SKIPs silently
when no hub is running (so a TUI-only setup isn't nagged). `tenant doctor --json` stays
CI-usable.
