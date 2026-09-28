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

## Supervision

### macOS — launchd

`~/Library/LaunchAgents/com.tenant.serve.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.tenant.serve</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/tenant</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <!-- Restart on crash, but stay stopped after a clean operator stop. -->
  <key>KeepAlive</key>
  <dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>/Users/YOU/Library/Logs/tenant/serve.log</string>
  <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/tenant/serve.err.log</string>
  <!-- Prefer credentials.json over env vars so secrets aren't in the plist. -->
</dict>
</plist>
```

Load it:

```
mkdir -p ~/Library/Logs/tenant
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.tenant.serve.plist
launchctl kickstart -k gui/$(id -u)/com.tenant.serve   # restart after an edit
launchctl bootout gui/$(id -u)/com.tenant.serve         # stop
```

### Linux — systemd (user unit)

`~/.config/systemd/user/tenant.service`:

```ini
[Unit]
Description=Tenant headless hub
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/tenant serve
Restart=on-failure
RestartSec=5
# stderr (TENANT_LOG-filtered) goes to the journal: journalctl --user -u tenant -f
# The full per-subsystem record is the log book: <data>/logs/<source>/YYYY-MM-DD.log
# (see docs/LOGGING.md); `tenant logs query log:approvals --follow` tails one.

[Install]
WantedBy=default.target
```

Enable it:

```
systemctl --user daemon-reload
systemctl --user enable --now tenant.service
journalctl --user -u tenant -f
```

`ExecStop` is unnecessary — systemd sends `SIGTERM`, which Tenant handles gracefully.

## Restart-storm guard (24/7)

If you rely on cron, leave **`cron.catchup` OFF** (the default). With catchup on, a
restart after downtime fires every missed job at once. Federation resumes from its
persisted peer cursor, so a restart does not re-pull the world.

## Health

`tenant doctor` includes a **serve liveness** check: when a hub is reachable it probes
`/api/status` and WARNs on a stuck turn or a waiting approval queue; it SKIPs silently
when no hub is running (so a TUI-only setup isn't nagged). `tenant doctor --json` stays
CI-usable.
