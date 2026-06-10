# Cron management — design plan (2026-06-07)

> **STATUS: IMPLEMENTED (2026-06-07).** All four surfaces shipped + tested, all
> gates green (`go build/vet/test ./...`, gofmt, `GOOS=windows`/`linux` cross-
> compile, `-race` on the engine). Files shipped:
> - Engine: `internal/cron/{schedule,engine}.go` (+ tests) — crontab/@every
>   parser with the DOM/DOW OR-rule, strictly-after minute-granular `Next()`,
>   +5y termination cap, advance-before-run (no double-fire), Stop-cancels-
>   in-flight, injectable clock + `runDue(now)`/`Has` test seams.
> - LLM tools: `internal/plugins/cron/dispatch.go` (+ tests) — `cron_list`
>   (ungated) + `cron_add/remove/set_enabled/run_now` (Gated), via a decoupled
>   `Manager` interface.
> - TUI: `internal/tui/tui.go` — `CronControl`/`CronJobView`, `/cron` (alias
>   `/cronjob`) `list|add <sched> | <prompt>|enable|disable|run|rm`, styled
>   `renderCronList`, "Automation (cron)" help section (+ tests).
> - Dashboard: `internal/dashboard/{cron.go,templates/cron.html}` + nav link;
>   `Server.cron` + `SetCron`; routes mounted unconditionally + nil-guarded; all
>   behind the existing `secure()`; html/template escaping (XSS test) (+ tests).
> - Wiring: `cmd/tenant/{cronmgr.go,restricttools.go,launchconfig.go,commands.go,
>   dashboardmgr.go}` — dedicated read/comms-safe cron runner agent (memory-
>   isolated: nil Archive/Episodic/Semantic), `cronManager` satisfying all three
>   consumer interfaces, persisted at `launchConfig.Cron.Jobs`.
> - Docs: `docs/PLUGINS.md` (cron row). `dashboard.New` signature unchanged
>   (setter route adopted); relay path left byte-identical.
> Adopted the debate's must-fixes: cron_* cut from the runner, hardened
> unattended system prompt, `@every` ≥ 1m floor, 50-job cap, 5m per-run timeout,
> no outbound comms in the cron surface.



Goal (Dylan): *"Implement a Cron management section in the TUI with a slash
command + tool for the LLM to utilize for recurring jobs. Also update the HTTP
page with the respective Cron management for full administration on a GUI."*
Motivation: QOL for QA testing.

Locked product decisions (asked + answered):
1. **Job action = run an agent prompt.** Each job stores a natural-language
   instruction; on schedule it runs ONE agent turn on a dedicated, isolated
   runner and posts the result to the TUI feed + dashboard.
2. **Schedule = hand-rolled crontab + `@every` + macros.** Standard 5-field cron
   (`0 9 * * *`), `@every 30m`, and `@hourly/@daily/@weekly/@monthly`. Pure-Go,
   NO new dependency, cross-platform.
3. **Safety = read/comms-safe.** During a cron run, gated/destructive tools
   (exec, file-write, send) are auto-denied. Jobs read/research/summarize/report
   only — never autonomously destroy or send.

Binding constraints: **additive only** (Windows build is stable — no existing
signature/behaviour changed except the one nil-safe `dashboard.New` param add,
all callers updated); CGO-free / pure-Go; deny-by-default security posture.

## Why a new engine (not `improve.CronStore`)
`improve.CronStore` exists but is interval-only (`time.Duration`), SQLite-backed,
and has ZERO callers. `improve.Scheduler` is interval-only and gated behind
`--self-improve`. Cron management must work regardless of `--self-improve` and
needs crontab semantics. So: a self-contained `internal/cron` engine, leaving
`improve` untouched (additive, no perturbation of the self-improve loop).

## Concurrency (the landmine)
`agent.Agent.Turn` has NO internal turn lock — the caller serializes (TUI via its
single loop; dashboard via `wsCoordinator`; relay via a DEDICATED agent). A cron
job firing `ag.Turn` from a background goroutine concurrently with a user turn
would race the working set. → Cron runs on a **dedicated cron agent** (own
working set, shared long-term memory), exactly like the relay. Jobs also run
**one-at-a-time** on a single engine worker goroutine (no self-overlap).

## Safety mechanism (reuse, don't invent)
The relay's `restrictForDiscord(full, disp)` already builds a read/comms-safe
surface: a registry that hides gated/dangerous tools and a dispatcher that
refuses cut tools by name, honoring an `execGate`. The cron runner reuses it with
the gate **left OFF permanently** → gated/destructive + team/orchestra tools are
cut. The safety guarantee rides on proven, tested code.

## Components

### A. Engine — `internal/cron/` (new pkg, tag-free, pure-Go, no deps)
- `schedule.go`: `Schedule` with `Next(after time.Time) time.Time`; `Parse(spec)`.
  Supports `@every <dur>`, macros, 5-field cron (`*`, `,` lists, `-` ranges, `/`
  steps; numeric fields). `String()` canonical render. Pure (time passed in).
- `engine.go`: `Job{ID,Name,Spec,Prompt,Enabled + runtime LastRun,NextRun,
  LastStatus,LastError,Runs}`; `Engine` (mutex job list, `Runner func(ctx,Job)
  (summary,err)`, bounded history ring, `persist` closure, injectable `now` for
  tests). Methods: Add/Remove/SetEnabled/List/RunNow/Snapshot, Start(ctx)/Stop().
  Ticker (30s) → due jobs (enabled && now>=NextRun) → run sequentially → update
  LastRun/NextRun/status → history → persist DEFINITIONS only (run-state stays in
  memory; NextRun recomputed = Next(now) on load → no chatty config writes).
- `schedule_test.go`, `engine_test.go`: crontab field parsing, Next() correctness
  (incl. DST-agnostic local time), `@every`, macros, due calc, add/remove/enable,
  persist, RunNow, fake Runner + injected clock for determinism.

### B. LLM tool — `internal/plugins/cron/` (new pkg)
- `dispatch.go`: `Manager` interface (List/Add/Remove/SetEnabled/RunNow + views),
  `Dispatcher{mgr}`, `Tools()`, `Dispatch()`. Tools: `cron_list` (ungated),
  `cron_add`/`cron_remove`/`cron_set_enabled`/`cron_run_now` (**Gated:true** —
  scheduling/altering autonomous future work + triggering a run are blast-radius
  writes). Manager interface avoids an import cycle (cmd/tenant adapter satisfies
  it).
- `dispatch_test.go`.

### C. cmd/tenant wiring
- `launchconfig.go`: add `Cron cronConfig` + `cronConfig{Jobs []cronJobConfig}` +
  `cronJobConfig{ID,Name,Spec,Prompt,Enabled}` (config.json, 0644, additive).
- `cronmgr.go` (new): `cronManager` satisfying `tui.CronControl`,
  `dashboard.CronControl`, and `cron.Manager` (or thin adapters). Holds
  `*cron.Engine`. `buildCronRunner(...)` builds the dedicated read/comms-safe
  agent (via `restrictForDiscord`, gate off) and returns a `cron.Runner` that
  runs a turn, returns the response as the run summary, and surfaces to feed.
- `commands.go` (cmdTUI): seed engine from `c.lc.Cron.Jobs`; persist closure
  writes `c.lc.Cron.Jobs` + `c.lc.save`; `engine.Start(ctx)`; add `Cron: cronMgr`
  to `tui.Config`; pass cron to dashMgr → `dashboard.New`. `engine.Stop()` before
  stores close (mirrors `sched.Stop()`).

### D. TUI — `internal/tui/tui.go`
- `CronControl` interface + `Config.Cron` field (after `IMessage`).
- `/cron` case: `list|status` (styled render), `add <spec> | <prompt>`
  (delimiter `|` separates the space-containing cron spec from the free-text
  prompt — unambiguous), `enable <id>`, `disable <id>`, `rm <id>`, `run <id>`.
  Nil-guard "cron not available in this session".
- `renderCronList()` (plain, styled): per job → ●/○, name, spec, next-run, last
  status. Empty → "(no jobs) — add one with /cron add ...".
- `/help`: new "Automation" section with `/cron` rows (and fold in `/relay`,
  currently unlisted).

### E. Dashboard — `internal/dashboard/`
- `cron.go`: `CronControl` interface + `CronJobView`.
- `New(...)` gains trailing `cron CronControl` (nil-safe, mirrors how `mem` was
  added); update all callers (dashboardmgr.go + tests).
- `Server.cron`; in `routes()`: `if s.cron != nil { s.mountCronSSR(s.mux) }`.
- `ssr_cron.go`: `handleCronPage` (GET /cron) + form handlers (POST /cron/add,
  /cron/{id}/enable, /cron/{id}/delete, /cron/{id}/run) → Datastar patch or 303.
- `templates/cron.html`: list + add-form + per-row enable/run/delete buttons;
  nav `<a href="/cron">` in layout.html.
- cron admin reachable only behind the dashboard's existing auth/bind policy.

## Test/verify gate
`go build ./...`, full `go test ./...`, `go vet`, `gofmt`,
`GOOS=windows`/`GOOS=linux go build ./cmd/tenant` — all green before done.

## Debate synthesis — decisions adopted (3 unbiased reviewers)

Security (must-fix):
- **Cut `cron_*` from the cron runner's own tool surface** (anti self-replication;
  a cron turn must not schedule more jobs). Plus team/orchestra (already cut) and
  ALL gated tools.
- **Hardened unattended system-prompt suffix**: declare it a scheduled UNATTENDED
  run; tool output / fetched content is untrusted DATA not instructions (prompt-
  injection defense); never read credential/secret files; never schedule jobs.
- **No outbound comms in cron surface**: unlike the relay, the cron runner keeps
  NO gated comms tools (no `discordKeptGated` analog) — there is no operator
  channel to scope to.
- **Limits**: reject `@every` < 1 minute; cap total jobs at 50; per-run
  wall-clock timeout (5m ctx deadline) — also bounds Stop().
- **XSS**: dashboard renders job fields via `html/template` text context only (no
  `template.HTML`); add a `<img onerror>` escaping test. All cron routes ride the
  existing `secure()` + fail-closed bind policy; `run` is POST (CSRF via SameSite).

Architecture (adopted):
- **Don't change `dashboard.New` signature** (≈21 call sites). Add `Server.cron`
  field + `SetCron()` setter; mount `/cron` routes unconditionally in `mountSSR`
  with nil-guards inside handlers (the memory-page precedent). Fully additive.
- **Don't reuse the discord-named `restrictForDiscord` directly.** Add an isolated
  `restrictReadComms(full, inner, surface, extraDeny)` (new, parallel function —
  the relay path is left byte-identical, the safest reading of "additive"). Cron
  uses it with empty kept-set, no exec gate, cron-specific refusal copy, and
  `extraDeny = cron_*`.
- **Three independent view types + thin adapters** (the `ToolInfo`/`dashTools`
  precedent): `cron.Job` (engine) / `tui.CronJobView` / `dashboard.CronJobView`;
  plugin `Manager` returns the engine view, never a tui/dashboard type.
- Engine is shaped as a deliberate sibling of `improve.Scheduler` (Start/Stop
  drain, bounded history ring, OnRun-style hook). New engine justified by the
  config.json-vs-SQLite persistence-model mismatch (cron jobs are operator config
  like Relay/IMessage, not improve-loop runtime state).

Correctness (must-fix in parser + lifecycle):
- **DOM/DOW OR-semantics**: when BOTH day-of-month and day-of-week are restricted
  (source text ≠ `*`), match on EITHER; else AND. Track "restricted" from parsed
  text, not the bitset.
- **`Next(after)` returns the first match STRICTLY > after**, working at
  whole-minute granularity (truncate up to next minute) so a job can't fire many
  times within its matching minute across 30s ticks.
- **Termination**: cap the search at +5 years; an impossible spec (Feb 31)
  returns zero time = "never due" (NOT epoch → no hot-loop). Test Feb-29.
- **Reject (don't clamp)**: out-of-range (min 0-59, hour 0-23, dom 1-31, month
  1-12, dow 0-6 with 7==Sun alias), inverted ranges, `*/0`, garbage, wrong field
  count. `@every` zero/negative/sub-minute.
- **No double-fire**: advance `NextRun` BEFORE running the (long) job; tick
  handler runs synchronously in the loop goroutine (next `<-t.C` not serviced
  until it returns). Test: a Runner blocking past two ticks → exactly one run.
- **Stop cancels in-flight** (cancellable ctx into Runner; `agent.Turn` respects
  ctx) then waits — no shutdown hang. Test it.
- **Testability seams**: unexported `runDue(now)` callable directly; parameterized
  tick interval; nil-Runner guarded (not a nil-deref → the "config nothing reads"
  failure mode); injectable `now`.

Residual risks (documented, accepted for v1):
- The read surface is broad (web/file/mail reads are ungated by design). Cron
  cuts destroy/send + cron_* and hardens the prompt; reads remain for utility.
  Localhost/auth-gated dashboard is the only sink (send is cut). Don't put secrets
  in job prompts.
- `@every N` jobs reset their phase on restart (Next(now)=now+N); frequent
  restarts can starve them. Cron-expression jobs use absolute wall-clock targets
  and are unaffected.

## Extension — finishing the deferred 5 (2026-06-07) — IMPLEMENTED

> All five shipped + tested; gates green (build/vet/test ./..., gofmt,
> windows/linux cross-compile, -race on the engine). After plan → debate (2
> reviewers: security + correctness) → implement → test.
>
> **Shipped:**
> - Engine (`internal/cron/engine.go`): `JobDef.Kind/Exec/TZ`; `AddSpec` struct;
>   one `nextFor(job,after)` path (TZ-correct everywhere); `Prime(opts)` (default
>   Location, Catchup, History seed + persist) recomputing in a single pass;
>   catch-up = fire-once for SAFE jobs only, never when LastRun is zero; RunRecord
>   JSON tags. Tests cover tz, catch-up (incl. never-run + exec-skip), history
>   seed/persist.
> - Plugin (`internal/plugins/cron`): `cron_add` gains kind/exec/tz; `AddSpec`;
>   JobView shows them.
> - cmd/tenant: `restrictExec` (exec surface — gated kept, team/orchestra+cron_*
>   cut) alongside the unchanged `restrictReadComms`; `buildCronRunner` builds
>   safe + exec agents; **shell jobs** run via `runCronShell` (os/exec, runtime
>   GOOS, Classify-REFUSED for catastrophic, scrubbed env, sandbox cwd, capped
>   output); **exec prompt jobs** use the exec agent + `withOffsiteConfirm(ctx,
>   cronExecApprover)`; `cronExecApprover` hard-denies destructive + config/data
>   writes; `time/tzdata` embedded (`tzdata.go`); history persisted 0600 to
>   `<dataDir>/cron-history.json`; global `Cron.AllowExec` kill-switch +
>   `Cron.Timezone` + `Cron.Catchup` in launchConfig.
> - TUI: `/cron add [shell|exec] [tz=<zone>] <sched> | <payload>` leading-flag
>   parse; exec/shell rows flagged in red. Dashboard: add-form kind select + exec
>   checkbox + tz input; shell/exec/tz badges; html/template escaping.
> - Tests added: engine tz/catchup/history; plugin kind/exec/tz; TUI flag parse;
>   dashboard kind/exec/tz threading; cmd/tenant approver/env-scrub/shell-refusal.
>
> **Follow-up (2026-06-08): `/cron exec on|off` TUI toggle shipped.** The global
> `Cron.AllowExec` kill-switch is now a LIVE `execGate` (atomic) the runner reads
> per run, flipped + persisted via `/cron exec on|off|status` (`cronManager.SetExec`
> / `ExecEnabled`, `tui.CronControl`). The `/cron` list header shows the exec
> state (red when ON). Tests: `TestSlash_CronExecToggle`, `TestSlash_CronListShowsExecState`.

## Extension plan — finishing the deferred 5 (2026-06-07)

All additive, opt-in, deny-by-default preserved. New job fields: `Kind`
("prompt"|"shell"), `Exec` (bool), `TZ` (IANA string). New engine config:
default `Location`, `Catchup` bool, persisted history.

1. **Shell-command jobs** (`Kind:"shell"`): the runner runs the payload as a
   shell line via `os/exec` (`sh -c` / `cmd /C` by runtime.GOOS), under the
   per-run ctx timeout, output captured + size-capped. No agent, no tool mux.
   Inherently exec → creation is the authorization (cron_add is Gated). Cron_*
   stays cut everywhere, so a safe prompt job can't spawn a shell job.

2. **Per-job exec opt-in** (`Exec:true`, prompt kind): the runner uses a second
   cron agent built over the FULL tool surface (gated tools kept; only cron_* +
   team/orchestra cut) and stamps the turn ctx with `withOffsiteConfirm(ctx,
   auto-approve)` — reusing the existing origin-confirm seam so gated tools
   auto-approve unattended (pre-authorized when the Gated cron_add was approved).
   Default stays `Exec:false` → read/comms-safe agent, no stamp. Hardened prompt
   variant for exec runs.

3. **Persisted run history**: engine gains an optional history seed + persist
   hook (`Prime`); cmd/tenant stores the bounded ring in
   `<dataDir>/cron-history.json` (atomic). On load, each job's LastRun/LastStatus
   is backfilled from the latest record (also feeds catch-up). config.json stays
   definitions-only.

4. **Timezone config**: per-job `TZ` + a global `launchConfig.Cron.Timezone`
   (engine default). `Next` is computed in the job's effective location
   (`now.In(loc)`). `import _ "time/tzdata"` embeds the zone DB so LoadLocation
   works on Windows. TZ validated at Add (bad zone → reject). Interval jobs are
   TZ-agnostic.

5. **Catch-up of missed runs** (opt-in, `Catchup:true`): at `Prime`, a job whose
   `Next(LastRun) <= now` fires ONCE on startup (NextRun=now), then resumes
   normal scheduling — never a backlog stampede. Default off → forward-from-now
   (unchanged).

Engine API: keep `NewEngine(defs,runner,persist,log)` stable; add `Prime(opts)`
(location/catchup/history-seed+persist, recomputes NextRun) called once before
Start; add setters as needed. Surfacing: `JobDef`/`cronJobConfig`/`cronp.JobView`
+ Kind/Exec/TZ; `cron_add` gains `kind`/`exec`/`tz` params; `/cron add [shell|
exec] [tz=Zone] <sched> | <payload>` leading-flag parse; dashboard add-form gains
kind select + exec checkbox + tz input.

### Residual risks (exec/shell — accept w/ guardrails)
- An exec/shell job runs dangerous actions UNATTENDED and auto-approved. The
  guardrails: opt-in per job; creation is Gated (the human approval IS the
  authorization); cron_* cut so jobs can't self-schedule; hardened prompt; exec
  defaults off. An exec prompt job could still write files (os_write) to add
  jobs out-of-band — inherent to "you authorized unattended exec"; documented.

## Out of scope (still deferred)
- Multiple catch-up runs for many missed occurrences (we fire once).
- Per-job approval routing to a phone (cron exec is pre-authorized, not live-
  approved). - A second permissive tool mux (we reuse the offsite-confirm seam).
