# 02 · Augmenting `tenantlog.go` — making the copy yours

The file is meant to be edited. Each recipe below is a small, local change;
none touches the wire format or the hub. Function names refer to
[`tenantlog.go`](tenantlog.go).

## Stamp fields on every line

Host, version, pid — anything you would otherwise repeat:

```go
host, _ := os.Hostname()
log, _ := tenantlog.NewWithConfig(tenantlog.Config{
    Source:   "deploy",
    Defaults: map[string]string{"host": host, "version": buildVersion, "pid": strconv.Itoa(os.Getpid())},
})
```

Per-line fields override a default with the same key.

## Route your existing `slog` output

No call-site changes: install the handler as the default logger.

```go
slog.SetDefault(slog.New(log.Handler()))
slog.Info("cache warmed", "entries", n)        // → the deploy folder + tab
```

`With(...)` attributes become fields; `WithGroup("req")` prefixes keys
(`req.id`). Records below `Config.MinLevel` (default Info) are dropped so a
chatty debug logger does not turn into a POST per line; set
`MinLevel: tenantlog.Debug` when you want everything.

Have an older `log.Printf` program? Point it at the handler too:

```go
log.SetOutput(&lineWriter{l})   // write a 6-line io.Writer that calls l.Info(strings.TrimSpace(string(p)))
```

## Event kinds and placed fields

The feed places a few keys specially on any row — `tool` in mono, `agent` as
a chip — and shows the other fields as `key=value`. `Event` files a line with
a kind:

```go
_ = log.Event("tool_call",   "tool call disk_check", map[string]string{"tool": "disk_check", "args": `{"path":"/"}`})
_ = log.Event("tool_result", "tool result disk_check", map[string]string{"tool": "disk_check", "result": "91% used"})
_ = log.Event("error",       "watcher crashed", map[string]string{"text": err.Error()})   // red row
```

A kind named `error` or ending in `_failed` is filed at ERROR level. The kind
itself travels in the `ev` field, and a hub drops `ev` from every line it
receives over the API: only Tenant files turn events, so an outside line can
never pass for a tool call or an approval. Such a row shows its level instead
of the kind. The kind stays only in a line appended to the file directly
(file-only mode, or while no hub was reachable).

## Several folders from one program

```go
audit := log.Sub("deploy-audit")      // same hub, token, data dir — different folder + tab
audit.Info("verdict recorded", "id", id, "by", operator)
```

## File-only mode (cron scripts, or a hub you know is down)

```go
log, _ := tenantlog.NewWithConfig(tenantlog.Config{Source: "nightly-backup", FileOnly: true})
```

The line goes straight to `<data>/logs/nightly-backup/<day>.log`. A running
hub notices new content within about a second and shows it live; a hub that
starts later backfills the file's tail. This is also what a shell script gets
with `tenant log nightly-backup "snapshot ok" size=12G` — same file, same
format, no Go needed.

## Tune the transport

| Field | Default | Change it when |
|---|---|---|
| `Timeout` | 3 s | the hub is across a slow link |
| `Retry` | 30 s | after a connection failure, how long to stay on the file before trying the hub again; raise it for a program that logs in bursts while the hub is rebooting |
| `URL` / `Token` | from `config.json` | the hub runs on another box (`https://hub:8770`), or the program runs as a different user that cannot read Tenant's config |
| `Stderr` | off | `os.Stderr` while developing — every line is also printed in the on-disk format |

## Raise or remove the value cap

`maxFieldBytes` (4096) cuts one value with a `…(+N bytes)` marker — the hub's
own default, so a rejected-then-filed line looks the same either way. A
program that logs whole documents can raise it; remember the hub still caps
lines it receives over the API at its configured `logs.max_field_bytes`.

## Windows

Paths, the data dir and the file fallback are all `filepath`-based and the
data dir rule matches `tenant.exe` (`%LOCALAPPDATA%\tenant`). Lines end with
`\n`; the hub also accepts `\r\n` from other tools. Nothing else differs.

## Never break these

- The line format (`lineFor`, `quoteValue`, `LevelName`, `timeLayout`) and
  `NormalizeSource` must stay identical to the hub's — the tests in this
  folder compare them byte-for-byte. Edit anything else freely.
- Keep the file free of imports outside the standard library, or the copy
  stops being a copy.
- Do not use one of Tenant's own source names (`turns`, `subagents`,
  `approvals`, `discord`, `imessage`, `cron`, `eval`, `improve`, `model`,
  `route`, `memory`, `mcp`, `peering`, `dashboard`, `sandbox`, `privacy`,
  `shutdown`, `system`): the hub refuses them with HTTP 403.

Next: [03 · hot-patch playbook](03-hotpatch-playbook.md).
