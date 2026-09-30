# Change: thinking time, the classifier on the route line, and cron tool calls (TEN-340, TEN-342)

**Branch:** `feat/think-route-cron-visibility` · **Status:** implemented, tested, not yet deployed.

## What changed

### Thinking vs tool time (TEN-342)
A slow model used to look the same as a hung one: the feed showed tool calls,
and only the turn's timer moved in between. Now:

- **Live, while the model answers.** The TUI's timer reads `⏱ 1:05 · thinking… 45.0s`.
  While tool calls run it reads `· tools… 3.1s` instead. The dashboard's Chat shows a
  `thinking…` row that counts up each second.
- **After each model call.** `thought for 8.2s` appears in the TUI feed and in Chat,
  and ends with `(the call failed)` when the call errored. A sub-agent's call shows as
  `[writer] thought for 2.5s`. The forced synthesis at the loop ceiling counts as a call too.
- **At the turn's end.** `turn 4:12 — thinking 3:40 · tools 32.0s` appears in the TUI
  feed, and `turn 4m12s — thinking 3m40s · tools 32s` in Chat.
- **On the record.** The turn's end event (`turns/1`) carries `think` and `tools`.
  runs.db stores them as `think_ms` and `tools_ms`, which also appear in the JSONL export
  and in `tenant logs trace`, as `(thinking N ms, tools N ms)`. An older runs.db gains the
  two columns when it is opened.

`tools` is the wall time of each batch of tool calls. Calls in a batch run in parallel, so
a batch counts once. The per-call marks are live only and are not filed in the log book,
because the `model` log already times every call.

### Which classifier the router asked (TEN-340)
- A route decision now records the classifier it asked: `endpoint` (host and port only,
  never credentials, path or query), `model` and `latency_ms`. They are on the
  route-decisions.jsonl row and on events `route/1001` and `route/2001`.
- The feed's route line ends with `via <host> (<model>)`, for example
  `route: shadow · small · complexity 2 · general · 85ms via 10.0.0.5:11434 (Supra-Router-51M-gguf:Q4_K_M)`.
  A failed decision names the classifier it failed to reach.
- A decision made when a sub-agent is spawned now reaches the live feed too, as
  `route: spawn · …`. It was already in the route log.

### Scheduled jobs' tool calls read `cron:<tool>`
A cron job's tool calls used to be filed only in the `cron` log, and the TUI feed
dropped them. Now:

- The TUI feed shows them as `→ cron:web_search {…}` and `✓ cron:web_search …`, then
  `cron: job done`, or `cron error: …` when the job fails.
- The log views (the TUI's `/logs`, the dashboard's Logs grid and detail, and
  `tenant attach`) name a cron job's tool rows the same way, for example
  `Tool cron:web_search finished in 1.2s`.
- Only the display changes. The stored `tool` field keeps the plain name, so
  `tool:web_search` still finds both a turn's call and a job's.

## Files
- `internal/agent`:
  - `EventThinking` and `EventThought`, and `Think` and `Tools` on `EventTurnEnd`
    (events.go);
  - timing around each planner call, the forced synthesis and each tool batch (agent.go);
  - `CronTool` and `RowMsg`, with the thinking marks kept out of the log book
    (logentry.go).
- `internal/runtrace`: `Span.AddThink`, `AddToolTime` and `Phases`; the `think_ms` and
  `tools_ms` columns.
- `internal/route`: `Signal` and `Decision` gain `Endpoint` and `Model`; the summary gains
  `via`; the event fields.
- `cmd/tenant/routewire.go`: spawn decisions go to the live feed (`routeRuntime.feed`).
- `internal/tui`:
  - the feed lines, the timer's phase and cron tool calls (tui.go);
  - `cron:` labels in `/logs` (cmd_logs.go).
- `internal/dashboard`:
  - Chat's thinking row and timing lines (ssr_chat.go);
  - `cron:` labels on the Logs page (ssr_logs.go).
- `internal/logs/event`: `Def.Render`, so a view can render an event again.
- `cmd/tenant/attach.go`, `cmd/tenant/logsctl.go`: `cron:` labels, and the time split in
  the trace header.

## Not done
- The dashboard's busy strip doesn't show the thinking/tools split yet. The Chat row
  covers the live view.
- The feed's cron lines don't name the job. TEN-441, on hold, covers the job name and
  the rest of cron visibility.
