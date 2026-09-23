# 01 · Quickstart — your program logs into Tenant in five minutes

You have a Go program (a sidecar, a watcher, a cron job, a custom automation)
running on a box where `tenant serve` or `tenant tui --dashboard` runs. You
want its lines in Tenant's log book: a folder on disk and a tab on the
dashboard, without rebuilding `tenant`.

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
log := tenantlog.New("deploy")        // folder: <data>/logs/deploy/   tab: "deploy"

log.Info("build started", "host", "build-box", "commit", sha)
log.Warn("slow step", "step", "link", "took", 42*time.Second)
log.Error("build failed", "step", "link", "err", err)
```

Source names are lowercase `[a-z0-9._-]`, at most 40 characters; anything
else is normalized ("My Deploy" → `my-deploy`). Pick a name you would want to
see as a tab.

## 3. Run it and look

```bash
go run .
tenant log --list                     # your source is in the list
tenant attach --follow --source deploy
```

Open the dashboard → **Activity feed**: your source is a tab; click it to see
its lines and its files on disk. On disk:

```
~/Library/Application Support/tenant/logs/deploy/2026-09-22.log     (macOS)
%LOCALAPPDATA%\tenant\logs\deploy\2026-09-22.log                    (Windows)
~/.local/share/tenant/logs/deploy/2026-09-22.log                    (Linux)
```

```
2026-09-22 14:03:11.123 INFO  [deploy] build started	commit=abc123 host=build-box
2026-09-22 14:03:53.001 ERROR [deploy] build failed	err="exit status 1" step=link
```

## What happened underneath

- The file found your hub by reading Tenant's own `config.json`
  (`dashboard.addr`, `dashboard.auth`) — nothing to configure on a Tenant
  box. Override with `TENANT_URL`, `TENANT_TOKEN`, `TENANT_DATA`, or `Config`.
- Each line was `POST`ed to `/api/logs/deploy`. The hub wrote the file, kept
  the line for the feed, and pushed it to open dashboards and TUI feeds.
- If no hub had been reachable, the same line would have been appended to
  the same file directly, in the same format; a hub that starts later reads
  the folder's tail and follows it. Nothing is ever dropped.

Next: [02 · augmenting the file](02-augmenting.md).
