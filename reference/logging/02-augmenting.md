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
slog.Info("cache warmed", "entries", n)        // → the log ext.deploy
```

`With(...)` attributes become fields; `WithGroup("req")` prefixes keys
(`req.id`). Records below `Config.MinLevel` (default Info) are dropped so a
chatty debug logger does not turn into a POST per line; set
`MinLevel: tenantlog.Debug` when you want everything.

Have an older `log.Printf` program? Point it at the handler too:

```go
log.SetOutput(&lineWriter{l})   // write a 6-line io.Writer that calls l.Info(strings.TrimSpace(string(p)))
```

## Event kinds

`Event` files a line with a kind, kept in its `kind` field:

```go
_ = log.Event("tool_call",   "tool call disk_check", map[string]string{"tool": "disk_check", "args": `{"path":"/"}`})
_ = log.Event("error",       "watcher crashed", map[string]string{"text": err.Error()})
```

A kind named `error` or ending in `_failed` is filed at ERROR level. Tenant's
own turn events carry their kind in `ev`, and the hub drops `ev` (and every
other field named like an event's own attributes: `event`, `trace`,
`session`, `approval`, …) from what arrives from outside, so an outside line
can never pass for a tool call or an approval. Filter on the kind with
`data.kind:tool_call`.

## Several logs from one program

```go
audit := log.Sub("deploy-audit")      // same hub, token, data dir — the log ext.deploy-audit
audit.Info("verdict recorded", "id", id, "by", operator)
```

## Spool-only mode (cron scripts, or a hub you know is down)

```go
log, _ := tenantlog.NewWithConfig(tenantlog.Config{Source: "nightly-backup", FileOnly: true})
```

Each line goes straight into the spool, `<data>/logs/spool/`, and a running
Tenant files it at its next housekeeping pass. A shell script can use
`tenant log nightly-backup "snapshot ok" size=12G` instead.

## Correlation

`Trace` and `Activity` default to `$TENANT_TRACE_ID` and
`$TENANT_ACTIVITY_ID`, which Tenant sets for the programs it starts. Set them
yourself to join lines to a turn or operation you know the ID of; each is 32
lowercase hex characters, or the hub refuses the line.

## Tune the transport

| Field | Default | Change it when |
|---|---|---|
| `Timeout` | 3 s | the hub is across a slow link |
| `Retry` | 30 s | after a connection failure, how long to stay on the spool before trying the hub again; raise it for a program that logs in bursts while the hub is rebooting |
| `URL` / `Token` | from `config.json` | the hub runs on another box (`https://hub:8770`), or the program runs as a different user that cannot read Tenant's config |
| `Stderr` | off | `os.Stderr` while developing — every line is also printed in the log book's text format |

## Raise or remove the value cap

`maxFieldBytes` (4096) cuts one value with a `…(+N bytes)` marker — the hub's
own default. A program that logs whole documents can raise it; remember the
hub still caps what it receives at its configured `logs.max_field_bytes`.

## Windows

Paths, the data dir and the spool are all `filepath`-based and the data dir
rule matches `tenant.exe` (`%LOCALAPPDATA%\tenant`). A spool file is renamed
only once it is closed, so Tenant never reads one half-written. Nothing else
differs.

## Never break these

- The event JSON (`json`, `eventLevel`, `LogName`) and the text format
  (`lineFor`, `quoteValue`, `LevelName`, `timeLayout`) must stay identical to
  the hub's — the tests in this folder compare them byte-for-byte. Edit
  anything else freely.
- Keep the file free of imports outside the standard library, or the copy
  stops being a copy.
- Field keys are letters, digits, `.`, `_` and `-` (`cleanKey`); the hub
  refuses others.

Next: [03 · hot-patch playbook](03-hotpatch-playbook.md).
