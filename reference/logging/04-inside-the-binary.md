# 04 · Inside the binary — a new Tenant subsystem gets its folder and tab

For code that lives in this repository (a new package under `internal/`, a
new relay, a new job). This path needs a rebuild, but no dashboard or
registration work: the hub creates the folder on first write and the Activity
tab row is read from the hub on every render.

## Plain log lines: `logs.For`

```go
package mysubsystem

import "tenant/internal/logs"

var log = logs.For("mysubsystem")   // safe at package init; files once the hub is installed

func Start() {
    log.Info("started", "port", 8080)          // → <data>/logs/mysubsystem/<day>.log + a "mysubsystem" tab
    log.Warn("slow upstream", "took", d)
    log.Error("sync failed", "err", err)
}
```

`logs.For` resolves the process-wide hub lazily (`logs.Install` runs in
`serve`/`tui` startup), so nothing has to be threaded through constructors.
Until a hub exists (one-shot commands, tests) records go to stderr at WARN.

Already receiving a `*slog.Logger` from the wiring? Pin it at the boundary
instead, the way `cmd/tenant/serve.go` does for every subsystem:

```go
relay := newRelay(log.With("source", "myrelay"))
```

A `source` attribute names the folder and is consumed (not filed as a field).
Without either, a message's `tag: …` prefix is looked up in `prefixTable`
(`internal/logs/slog.go`) and unknown tags fall to `system` — add your tag
there if you prefer the prefix convention over a pinned logger.

## Turn events from an agent you drive

Mirror the agent's events into the feed and name the folder on each event,
like the cron / Discord / eval observers do:

```go
obs := func(e agent.Event) {
    switch e.Kind {
    case agent.EventToolCall, agent.EventToolResult, agent.EventFinal, agent.EventError:
        e.Agent, e.Source = "tenant-myrelay", "myrelay"
        emit(e)   // the serve/tui emit: broker (live feeds) + hub.Log(agent.EntryFor(e))
    }
}
ag, err := agent.New(agent.Config{ /* … */, Observer: obs })
```

`agent.EntryFor` renders the event as a readable line with the kind in the
`ev` field, so the dashboard styles the row (tool in mono, agent chip, error
rows red). Without `Source`, a sub-agent's events file under `subagents` and
the primary agent's under `turns`.

## Writing entries directly

For something that is neither slog nor an agent event:

```go
if h := logs.Default(); h != nil {
    h.Log(logs.Entry{Source: "myrelay", Level: slog.LevelWarn, Msg: "queue backlog",
        Fields: []logs.Field{{Key: "depth", Value: strconv.Itoa(n)}}})
}
```

## Tests to add

- A unit test that your package logs under its source:
  `h := logs.NewMemory(10); logs.Install(h); defer logs.Install(nil)` then
  assert on `h.Snapshot()` (`Source`, `Msg`, `Field("k")`, `IsErr()`).
- If you map events, a table test on `agent.EntryFor` for your `Source`
  (see `internal/agent/logentry_test.go`).
- Nothing on the dashboard side: `TestActivity_TabsFollowSources` already
  proves a new source becomes a tab.

## What you never have to do

- Add a tab, a filter, a route or a template — the source list is live.
- Register the source anywhere — first write creates the folder.
- Think about retention, rotation or the on-disk format — the hub owns them
  (`logs.*` in `config.json`; `docs/LOGGING.md`).
