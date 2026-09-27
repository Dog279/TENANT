# 01 · Quickstart — your program logs into Tenant in five minutes

You have a Go program (a sidecar, a watcher, a cron job, a custom automation)
running on a box where `tenant serve` or `tenant tui --dashboard` runs. You
want its lines in Tenant's log book, as a log of its own in the dashboard,
without rebuilding `tenant`.

## 1. Copy the file

Two ways, pick one:

**Beside your `main.go`** (simplest for a single-file tool):

```bash
cp reference/logging/tenantlog.go ./tenantlog.go
sed -i '' 's/^package tenantlog$/package main/' tenantlog.go      # macOS sed; on Linux drop the ''
```

**As a package** in your module (better once the program has more than one file):

```bash
mkdir -p tenantlog && cp reference/logging/tenantlog.go tenantlog/
```

and `import "yourmodule/tenantlog"`.

The file imports only the standard library. Go 1.21 or newer.

## 2. Name your source and log

```go
log := tenantlog.New("deploy")        // filed in the log ext.deploy

log.Info("build started", "host", "build-box", "commit", sha)
log.Warn("slow step", "step", "link", "took", 42*time.Second)
log.Error("build failed", "step", "link", "err", err)
```

Source names are lowercase `[a-z0-9._-]`; anything else is normalized
("My Deploy" → `my-deploy`). Everything from outside the binary is filed
under `ext.`, so `deploy` becomes the log `ext.deploy`, marked `external`:
a script can never write into one of Tenant's own logs.

## 3. Run it and look

```bash
go run .
tenant logs                              # ext.deploy is in the list
tenant logs query log:ext.deploy --follow
```

Open the dashboard: `ext.deploy` is under **External** in the Logs tree.

```
1042 2026-09-22 14:03:11.123 INFO  [ext.deploy] build started	commit=abc123 host=build-box
1043 2026-09-22 14:03:53.001 ERROR [ext.deploy] build failed	err="exit status 1" step=link
```

## What happened underneath

- The file found your hub by reading Tenant's own config dir: `config.json`
  (`dashboard.addr`, `dashboard.auth`), and on a default install, which keeps
  the token out of `config.json`, the `dashboard-auth-token` file beside it.
  Nothing to configure on a Tenant box. Override with `TENANT_URL`,
  `TENANT_TOKEN`, `TENANT_DATA`, or `Config`.
- Each line was `POST`ed to `/api/events` as one event: its time, log,
  level, message and fields. The hub filed it in `logbook.db` and pushed it
  to open viewers.
- If no hub had been reachable, the event would have been dropped into the
  spool, `<data>/logs/spool/`, as a file of its own (written as `.tmp`, then
  renamed `.ndjson`); the next Tenant that runs files it. Nothing is ever
  dropped.
- A program Tenant starts (a cron job's command, a script the agent runs)
  has `TENANT_TRACE_ID` and `TENANT_ACTIVITY_ID` in its environment. The
  file sends them with every line, so the lines appear in that turn's or
  job's timeline (`tenant logs trace <id>`).

Next: [02 · augmenting the file](02-augmenting.md).
