# Resilient Launch + Live Model Management

> **STATUS: IMPLEMENTED (2026-06-08).** Built via plan → debate (7-agent workflow:
> explore abort points / recovery / multi-consumer propagation / resilience →
> design → adversarial review → doc) → implement → test. All gates green: full
> `go test ./...` (38 pkgs), gofmt, vet, `GOOS=windows`/`linux` cross-compile.
>
> **Shipped:**
> - `cmd/tenant/wiring.go`: `degradedState` (+`degradeClass`), `buildRouterResilient`,
>   `classifyDegrade`, `degradeBanner` — interactive-only echo fallback; `buildRouter`
>   unchanged (headless stays fail-fast).
> - `cmd/tenant/commands.go` cmdTUI: call-site swap (banner + `c.backend="echo"`
>   in-memory + echo-stub re-warn), shared gate threaded into the self-improve
>   scheduler (`sched.Paused`), cron engine (`SetPaused` + catch-up forced off),
>   relay (`degraded`), `modelControl.degraded`, and a reachability-only reconnect
>   kick (`reconnectMon.OnGenerationDown()`).
> - `internal/improve/scheduler.go`: `Paused func() bool` (RunDue+RunAll skip).
> - `internal/cron/engine.go`: `SetPaused` (runDue defers, no advance) + Prime
>   forces catch-up off while paused.
> - `cmd/tenant/modelctl.go`: clear the gate on a reachable (`✓`) `/model use`;
>   `modelInfos` flags the active row `Degraded`.
> - `internal/tui/tui.go`: `ModelInfo.Degraded` + `/model` list marker.
> - `cmd/tenant/discordrelay.go` / `discordmgr.go`: relay refuses turns while degraded.
> - Tests: `resilient_test.go` (degrade/classify/marker), scheduler + cron pause,
>   relay refusal. Status bar shows "echo" persistently (Backend/Model derive from
>   `c.backend`).
> Deferred (R-class, documented below): per-reply ECHO prefix (status bar + banner
> + /model marker cover visibility); embedder hot-swap on recovery (consolidation
> stays suppressed while the embedder is echo).

Implementation-ready design for Tenant's interactive TUI. Hand this to an engineer; every change is additive and Windows-stable by construction.

## Goal

Today an interactive launch (`tenant` / `tenant chat`) **aborts hard** if the configured model can't be built — a down vLLM box, a missing key, or a malformed `config.json` all kill the TUI before it opens, so the operator has no way to fix it live. The fix:

1. **Resilient launch (interactive only):** if the configured model fails to build, degrade to the pure-Go `echo` backend so the TUI still opens, surface an un-missable banner, and let the operator recover live with `/model use` / `/model add` — no restart.
2. **Keep headless strict:** `eval`, `serve`, `research`, `doctor`, `goalctl`, etc. continue to fail fast. No behavior change for any non-interactive caller.
3. **Live recovery propagates everywhere:** recovery via the single shared `*model.Router` already re-routes the main agent, cron, relay, and dashboard. Confirmed; no propagation fix needed.
4. **Do no harm while degraded:** autonomous background actors (self-improve, cron, Discord relay) must **not** write echo-derived garbage to durable stores or run side effects against a fake plan. This is the load-bearing addition the original design missed.

Non-goal: making `buildRouter` itself resilient. The resilience lives in a new wrapper used only by the interactive path.

## Current abort points

Every place an interactive launch can die on model build, and whether this plan fixes it.

| Condition | Source | Fixed? |
|---|---|---|
| Configured model fails to build (any cause), interactive launch | `cmd/tenant/commands.go:1629-1632` (`buildRouter` → `return err`) | **Yes** — `buildRouterResilient` degrades to echo |
| vLLM endpoint set but model unknown (auto-detect failed, server down) | `cmd/tenant/wiring.go:323-326` | **Yes** — classified as *reachability*; degrade + reconnect polling |
| Anthropic provider missing API key | `cmd/tenant/wiring.go:348-351` | **Yes (degrade, no polling)** — classified as *credential*; banner says "add a key", no `OnGenerationDown` |
| Anthropic provider missing model | `cmd/tenant/wiring.go:352-354` | **Yes (degrade, no polling)** — *config* class |
| Unknown backend kind (corrupt/hand-edited config) | `cmd/tenant/wiring.go:365-367` | **Yes (degrade, no polling)** — *config* class; banner says "fix config.json / re-run `tenant setup`" |
| Embedder registry I/O failure (echo can't paper over) | `cmd/tenant/wiring.go:375-379` (`attachEmbedder`/`finishModelRouter`) | **No (intentional)** — echo fallback also fails here, so return the error and abort. Non-model failure. |
| Mid-session generation outage | `internal/tui/tui.go:1338-1342` (`turnDoneMsg`), `:1143-1150` (`ErrEndpointDown`) | **Already non-fatal** — `agent.Turn` returns errors as values (`internal/agent/agent.go:465`); `reconnectMonitor` polls and recovers |
| Headless `eval` / `serve` / `research` / `doctor` / `goalctl` model build failure | `eval.go`, `commands.go:1334` (serve), `research.go`, `doctor.go:114`, `goalctl.go` | **No (intentional)** — these keep calling `buildRouter` directly and stay fail-fast |

## Decisions

- **D1 — One additive wrapper, never touch `buildRouter`.** `buildRouterResilient` lives in `wiring.go` next to `buildRouter` and calls it. Headless callers keep calling `buildRouter` directly. Do **not** "simplify" by folding resilience into `buildRouter`; that would regress every headless command to silently degrade.
- **D2 — Reuse the existing echo precedent.** Embeddings already echo-fall-back inside `attachEmbedder` (`wiring.go:375`). Generation degrade mirrors that: same `backend = "echo"` registry/factory branch (`wiring.go:298-307`), pure in-memory, no network, no build tags.
- **D3 — In-place router mutation only; never `Agent.SetRouter`.** Recovery mutates the **single shared `*model.Router`** in place (`modelctl.go:93-102`: `RegisterBackend` + `SetProfiles`). Because the main agent, cron runner (`commands.go:2004`), Discord relay (`commands.go:2126`), and dashboard (shares `ag`) all hold the same pointer, recovery propagates to all of them on their next turn. Converting to `Agent.SetRouter` (dead code at `internal/agent/agent.go:143`) would re-point only the main agent and strand cron/relay. Leave `SetRouter` as-is.
- **D4 — One shared `*atomic.Bool` degraded gate is the single source of truth.** Created by `buildRouterResilient`, read by the TUI display, the self-improve scheduler, the cron engine, and the relay. Cleared only on a **reachable** `/model use`. This same object is both the "anti-masking display flag" and the "background-actor suppression gate" — not two flags. (Resolves review F4/F8.)
- **D5 — `c.backend = "echo"` is in-memory only and must never persist.** It exists solely so the **status bar** derivation (`commands.go:2232-2235`) honestly shows "echo". It is **not** read by `reconnectMonitor` (which reads the configured provider from `config.json`, see D6). The real provider stays in `config.json`, which `/model use` and reconnect need to target the real endpoint. (Corrects review F5.)
- **D6 — Classify the degrade cause.** Reachability faults → degrade + start reconnect polling. Credential / config faults → degrade but **do not** poll (the endpoint will never come up without operator action) and tailor the banner. (Resolves review F6.)
- **D7 — Suppress destructive background work while degraded.** Self-improve consolidation/profile jobs, the distill cursor advance, all non-interactive cron runs (incl. catchup and exec), and the Discord relay's autonomous replies are gated on D4. (Resolves review F1/F2/F3 — the original design's "no tweak needed" was wrong for background actors.)

## Design — resilient launch

### The degraded-state holder

A tiny shared struct so the gate and its cause travel together. Put it in `wiring.go`.

```go
// degradeClass describes why generation degraded, which drives banner wording
// and whether reconnect polling makes sense.
type degradeClass int

const (
    degradeNone        degradeClass = iota
    degradeReachability              // server down / auto-detect failed → poll to recover
    degradeCredential                // missing API key → operator must add a key; do NOT poll
    degradeConfig                    // unknown backend / missing model → fix config.json; do NOT poll
)

// degradedState is the SINGLE source of truth for "running on echo fallback".
// One instance is shared by: the TUI model display, the self-improve scheduler,
// the cron engine, and the Discord relay. Cleared only on a reachable /model use.
type degradedState struct {
    on    atomic.Bool
    class degradeClass // written once at boot, read-only thereafter
    cause string       // original buildRouter error, for the banner
}

func (d *degradedState) Degraded() bool { return d != nil && d.on.Load() }
func (d *degradedState) clear()         { if d != nil { d.on.Store(false) } }
```

### The interactive-only seam

```go
// buildRouterResilient wraps buildRouter for INTERACTIVE launch only (TUI/chat).
// On a model-related failure it degrades to an echo router so the TUI still
// opens and the operator can recover live with /model. Returns the router, a
// shared *degradedState (nil when not degraded), a non-empty banner when
// degraded, and an error ONLY for non-model failures the echo branch can't
// paper over (e.g. embedder registry I/O).
//
// Never call this from a headless command. buildRouter stays fail-fast for
// eval/serve/research/doctor/goalctl.
func buildRouterResilient(c *commonFlags, log *slog.Logger) (r *model.Router, ds *degradedState, banner string, err error) {
    if r, err = buildRouter(c, log); err == nil {
        return r, nil, "", nil
    }
    cause := err.Error()
    class := classifyDegrade(c.backend, cause)

    // Degrade: echo generation. The echo branch is pure-Go (wiring.go:298-307),
    // never touches the network. Embeddings already echo-fall-back via
    // attachEmbedder, so they survive this path too.
    ec := *c
    ec.backend = "echo"
    er, eerr := buildRouter(&ec, log)
    if eerr != nil {
        // Echo couldn't build either (e.g. embedder registry I/O) — this is a
        // real, non-model failure. Abort, preserving the original cause.
        return nil, nil, "", fmt.Errorf("model unavailable and echo fallback failed: %w (cause: %s)", eerr, cause)
    }

    ds = &degradedState{class: class, cause: cause}
    ds.on.Store(true)
    banner = degradeBanner(c.genKind, class, cause)
    return er, ds, banner, nil
}

// classifyDegrade buckets a buildRouter error so the banner + reconnect policy
// match the actual fault. Substring matching on the known buildRouter error
// strings (wiring.go) — these are stable, operator-facing messages.
func classifyDegrade(backend, cause string) degradeClass {
    switch {
    case strings.Contains(cause, "needs an API key"):
        return degradeCredential
    case strings.Contains(cause, "unknown backend"),
        strings.Contains(cause, "needs a model"):
        return degradeConfig
    case strings.Contains(cause, "is the server up"),
        strings.Contains(cause, "could not determine"):
        return degradeReachability
    default:
        // Unknown model-build failure: degrade, treat as reachability so the
        // monitor at least tries; safe because polling is read-only.
        return degradeReachability
    }
}
```

### The banner (un-missable, honest, class-specific)

```go
func degradeBanner(genKind string, class degradeClass, cause string) string {
    const head = "⚠ model %q unavailable — running on ECHO (no real LLM, no tool execution). "
    switch class {
    case degradeCredential:
        return fmt.Sprintf(head+"Credential missing: add a key with `/model add` or "+
            "re-run `tenant setup`, then `/model use`. Reason: %s", genKind, cause)
    case degradeConfig:
        return fmt.Sprintf(head+"Config error: fix config.json or re-run `tenant setup`, "+
            "then `/model use`. Reason: %s", genKind, cause)
    default: // reachability
        return fmt.Sprintf(head+"Endpoint unreachable: start the server, then `/model use <provider>`. "+
            "Auto-reconnect is polling. Reason: %s", genKind, cause)
    }
}
```

The banner explicitly says **"no real LLM, no tool execution"** (echo has no tools — `echo.go:50-93`), correcting the original wording. (Resolves review F7/F10 banner note.)

### Call site

Replace `cmd/tenant/commands.go:1629-1632`:

```go
router, degraded, degradedBanner, err := buildRouterResilient(c, log)
if err != nil {
    return err
}
if degraded.Degraded() {
    pushSys(degradedBanner)
    // In-memory ONLY — makes the status bar (commands.go:2232) honestly show
    // "echo". MUST NOT persist: no lc.save runs here, so config.json keeps the
    // real provider, which /model use and reconnect both target. Do NOT add a
    // save on this path.
    c.backend = "echo"
    // Re-emit the echo-stub warning AFTER degrading. healthCheck ran before the
    // degrade (commands.go:1625) against the real backend, so it never printed
    // the "responses are deterministic stubs" line for this path.
    pushSys("echo: responses are deterministic stubs — not a real model.")
    // Start reconnect polling only for transient (reachability) faults. A
    // missing key / corrupt config will never recover by polling.
    if degraded.class == degradeReachability {
        // wired below where Reconnect is constructed; see Mid-session resilience.
    }
}
```

`pushSys` (`commands.go:1616`) already feeds the TUI's system channel — same plumbing as `ensureSetup`/`healthCheck`, so the banner lands in-pane at startup with zero new wiring.

`cmdChat` (`commands.go:121`) already continues past turn errors and has no `/model` UI, so the banner alone is the win there. Applying the seam to `cmdChat` is **optional** and out of scope for v1.

## Live recovery & router propagation

**Recovers live, no restart:**

- **Generation.** `/model use <provider>` and `/model add` → `modelControl.UseModel` (`modelctl.go:59`) mutates the shared router in place: `RegisterBackend` for the new factory + `SetProfiles` (`internal/model/router.go:83-95`), which invalidates cached LLMs and re-points `rolePref`. Echo→real recovers with no restart.
- **Propagation to all actors.** The router is built once (`commands.go:1629`) and passed into the main agent, cron deps (`commands.go:2004`), relay deps (`commands.go:2126`), and the dashboard (shares `ag`). All hold the **same pointer**, so the in-place swap re-routes every actor on its next turn. **No propagation gap; no holder needed for recovery.** (Verified.)

**Does NOT recover live (accepted v1 limitations):**

- **Embedder role (pre-existing GAP 1).** `UseModel` re-points generation roles only; the embedder stays on echo after recovery. Accepted for v1. Compounding note: consolidation must remain suppressed while the embedder is echo (see Background-actor safety, F11) so we don't write real merged text with `echo-embedder` vectors that can't be searched.

**The one additive propagation fix — clearing the degraded gate:**

Recovery of generation is automatic, but the **degraded gate (D4) must be cleared** so background actors resume and the display flag clears. Clear it **only on a reachable swap**:

- `UseModel` already calls `probeSwap` (`modelctl.go:109-140`), which returns a status line leading with `✓` (reachable) or `⚠` (swapped-but-unreachable). The router swaps either way (advisory probe).
- Give `modelControl` a `degraded *degradedState` field (the shared object). In `UseModel`, after `probeSwap`, clear the gate **only when the status leads with `✓`**:

```go
status := probeSwap(name, active, c2)
if mc.degraded != nil && strings.HasPrefix(status, "✓") {
    mc.degraded.clear() // reachable real model → resume background actors
}
return status, active, nil
```

This guards review F4's trap: a swap to a still-down endpoint returns `⚠`, leaves the gate set, and keeps destructive jobs suppressed.

## Mid-session resilience

Already non-fatal; confirmed, no core change:

- TUI routes `Turn` errors through `turnDoneMsg` (`tui.go:1338-1342`).
- `ErrEndpointDown` triggers `reconnectMonitor` (`tui.go:1143-1150`); `reconnectMonitor.activeEndpoint()` reads the **configured** active provider from `config.json` (`reconnect.go:85-101`) — **not** `c.backend`. So after a degrade it correctly polls the real (down) endpoint, and `c.backend = "echo"` does not mislead it.
- `agent.Turn` returns errors as values (`agent.go:465`), so an outage never panics the loop.

With degraded launch: the first message on echo returns echo output; once the operator runs `/model use`, the next turn routes to the real provider.

**One required wiring (class-gated polling):** when the degrade class is `degradeReachability`, kick the monitor at startup so it polls without waiting for a failed live turn. `reconnectMonitor` is constructed at `commands.go:2292`; after construction, if `degraded.class == degradeReachability`, call its generation-down entry point (e.g. `Reconnect.OnGenerationDown()`). For `degradeCredential` / `degradeConfig`, **do not** start polling — those need operator action, and polling a key-less endpoint is misleading. (Resolves review F6 mis-targeted polling.)

## Making degraded state un-missable (anti-masking)

Echo replies render in the same chat-pane styling as real replies (`buildReply`, `echo.go:69`), so passive signals alone let echo masquerade as the model. Layered defenses, all reading the single shared `degradedState` (D4):

1. **Startup banner** via `pushSys` (above) — class-specific, says "no real LLM, no tool execution".
2. **Re-emitted echo-stub warning** after degrade (`healthCheck` ran pre-degrade against the real backend and never printed it for this path).
3. **Status bar** shows "echo" via `c.backend = "echo"` (D5) — `commands.go:2232-2235` derives `modelName` from `c.backend`.
4. **`/model list` + `/model show` degraded marker.** Add `Degraded bool` to `tui.ModelInfo` (`internal/tui/tui.go`, the struct used at `:441`). The renderer flags the active row `(degraded — echo fallback)`. Source of truth is the shared gate, **not** config inference:
   - Add `degraded *degradedState` to `modelControl` (`modelctl.go:23`).
   - Plumb it through `ModelList()` → `modelInfos(lc, degraded)` (`modelctl.go:32,42`). Set `Degraded: ds.Degraded() && info.Active` on the active provider's row. `config.json` still names the real provider (we never persisted echo), so the gate — not config — is the only correct signal. (Resolves review F8.)
5. **Recommended per-reply marker (in scope for v1).** While the gate is set, prefix streamed assistant output (or persistently tag the status bar) with an `ECHO` marker so each reply is self-evidently fake, not just the startup banner. Implement at the TUI's stream-render boundary, conditioned on the shared gate. (Addresses review F7.)

## Background-actor safety while degraded

**The load-bearing addition.** Two schedulers and the relay run autonomously against the shared router, independent of any user turn. While degraded they would write echo-derived garbage to durable stores or run real side effects from a fake plan. All are gated on the **single shared `degradedState`** (D4) via a `func() bool` predicate.

The cleanest additive seam: a `paused func() bool` predicate (returns `degraded.Degraded() || embedderIsEcho`) passed into `improve.Scheduler` and `cron.Engine`; both consult it before running a job. The relay consults the same gate on its send path.

### Self-improve scheduler (`commands.go:2186-2230`)

Pass the predicate into `improve.NewScheduler` (or set a field). At each tick, **skip-and-log** while paused. Per-job severity:

- **`ConsolidationJob`** (`internal/improve/consolidatejob.go:124-176`) — CRITICAL. It summarizes via the router (echo), then `Semantic.Insert`s a merged fact built from echo text and `Semantic.Supersede`s the real source facts. Echo returns a canned non-JSON string (`echo.go:62-78`), so real facts get **irreversibly superseded by garbage**. **Hard-block while degraded.** Also keep blocked while the embedder role is echo (F11) — it calls `embedder.Embed` (`consolidatejob.go:152`) and would write `echo-embedder` vectors.
- **`profileJob` → `refreshProfile`** (`commands.go:1705-1714`) — CRITICAL. On `changed` it does `prof.Update(md); prof.Save(...)`, persisting an echo-synthesized profile over the real one. **Hard-block while degraded.**
- **`DistillJob`** (`internal/improve/distilljob.go:57-75`) — partially protected (echo returns `{"facts":[]}` for JSON-schema requests, so it inserts nothing) but it still **advances the distill cursor** (`distilljob.go:68-69`) past episodes never really processed, silently skipping them forever once a real model returns. **Do not advance the cursor while degraded** — simplest: skip the job entirely.
- **`SkillInductionJob`, nightly eval** — skip while degraded for consistency (eval against echo is meaningless).

Log line on skip: `self-improve: paused — model degraded (echo)`.

### Cron engine (`commands.go:2003-2065`, `internal/cron/engine.go`)

The original design never mentioned cron. `cronExecGate` (`commands.go:2015`) may be ON from persisted config (`c.lc.Cron.AllowExec`). A degraded launch means a scheduled job's agent gets an echo "plan" and may run shell/tool side effects from a hallucinated stub. Worse, `Prime(... Catchup: cronCatchup ...)` (`commands.go:2043-2050`) can fire **missed jobs immediately at startup**, before the operator even sees the banner.

- Pass the same predicate into `cron.NewEngine` (or `Prime`). `runDue`/`execute` (`internal/cron/engine.go:415-545`) **skip-and-defer** every due job while paused — do **not** advance schedule state in a way that consumes the slot; treat as deferred so they run after recovery.
- **Suppress catchup while degraded** explicitly: if paused at `Prime` time, force `Catchup: false` for that boot.
- Surface it: `cron: paused while model degraded (N jobs deferred)` via `SetNotify(pushSys)`. Never let `AllowExec` + echo + catchup combine silently. (Resolves review F1/F2.)

### Discord relay (`commands.go:2125-2164`)

The relay shares the degraded router and answers a **remote** user who never sees the terminal banner — exactly the "operator away, local box rebooted" scenario where degrade is most likely. (Resolves review F3.)

- Plumb the shared gate into the relay's send path. While degraded, either **refuse** with a clear message ("model unavailable, recovering — try again shortly") or **prefix** every reply with the degraded warning. Refuse-or-prefix, never a silent echo stub presented as real.

## Additivity & cross-platform

- **Additive seam summary:** new `buildRouterResilient` + `degradedState` + `classifyDegrade` + `degradeBanner` (all `wiring.go`); ~12-line call-site swap (`commands.go:1629`); one `Degraded bool` on `tui.ModelInfo`; one `degraded *degradedState` field on `modelControl` + the reachable-clear in `UseModel`; a `paused func() bool` predicate threaded into `improve.Scheduler`, `cron.Engine`, and the relay send path; class-gated `OnGenerationDown` kick.
- **No `buildRouter` change.** Headless callers (`eval.go`, `serve` at `commands.go:1334`, `research.go`, `doctor.go:114`, `goalctl.go`) keep calling `buildRouter` directly and stay fail-fast. Hold this line in review. (Verified F9.)
- **Windows-stable by construction.** All new code is pure-Go: no build tags, no `GOOS` branches. The echo branch (`wiring.go:298-307`) and `echo.New`/`Generate`/`GenerateStream` (`echo.go:45-93`) do no I/O and never touch the network. (Verified F10.) `atomic.Bool` is stdlib and platform-neutral.

## Test plan

Unit + integration, all runnable on Mac/Windows with no live server (echo only).

1. **`buildRouterResilient` degrades on model failure.** With a vllm backend pointed at a dead endpoint + empty `--vllm-model`, assert it returns a non-nil router, `degraded.Degraded() == true`, a banner containing "ECHO" and "no tool execution", and `err == nil`.
2. **Non-model failure aborts.** Force the echo fallback build to fail (inject an `attachEmbedder` registry error); assert `buildRouterResilient` returns a non-nil error and nil router.
3. **`classifyDegrade` mapping.** Table test: each known `buildRouter` error string → expected class (`needs an API key` → credential; `unknown backend` / `needs a model` → config; `is the server up` / `could not determine` → reachability; unknown → reachability).
4. **Headless stays strict.** Assert `buildRouter` (not the wrapper) is still the only call in `eval`/`serve`/`research`/`doctor`/`goalctl` paths (compile-level / grep guard test), and that it returns the original error for a dead endpoint.
5. **`c.backend = "echo"` does not persist.** After the degraded call site runs, load `config.json` and assert the real provider/kind is still recorded (no echo written).
6. **Status bar + `/model list` reflect degraded.** With the shared gate set, `modelInfos` flags the active row `Degraded: true`; the inactive rows stay false; config still names the real provider.
7. **Reachable `/model use` clears the gate.** With a fake `probeSwap` returning `✓...`, assert `degraded.Degraded()` flips to false. With `⚠...`, assert it stays true.
8. **Consolidation hard-blocked while degraded.** Seed two overlapping facts; run the scheduler tick with the gate set; assert **no** `Supersede`/`Insert` occurred and the original facts are intact. Clear the gate; assert it now runs.
9. **Distill cursor frozen while degraded.** With the gate set, run the distill tick; assert the cursor did not advance. Clear; assert it advances.
10. **Profile not overwritten while degraded.** With the gate set, run `profileJob`; assert `prof.Save` was not called.
11. **Cron suppressed + catchup forced off while degraded.** Prime with `Catchup: true` and a missed job while the gate is set; assert the missed job did not fire at boot and is deferred; a `cron: paused ...` notice was emitted. Clear; assert it runs on the next due tick.
12. **Relay refuses/prefixes while degraded.** With the gate set, drive a relay message; assert the reply is the refusal/prefixed warning, not a bare echo stub.
13. **End-to-end recovery.** Degraded launch → send a turn (echo reply) → `/model use` to a reachable echo "provider" returning `✓` → assert gate cleared, background actors resume on next tick.

## Out of scope

- Making `buildRouter` resilient for headless commands (intentional fail-fast; D1).
- Re-pointing the **embedder** role on `/model use` (pre-existing GAP 1). Mitigated by keeping consolidation suppressed while the embedder is echo; full embedder hot-swap is a separate change.
- Adopting `Agent.SetRouter` (`agent.go:143`) — would strand cron/relay (D3). Left as dead code.
- Applying the resilient seam to `cmdChat` (`commands.go:121`) — chat already survives turn errors and has no `/model` UI; optional, deferred.
- Persisting an "echo fallback" state across restarts — degrade is a per-launch condition derived from the live build; nothing to persist.

### Residual risks (accepted)

- **R1 — Post-recovery embedder.** After generation recovers, the embedder stays echo until restart (GAP 1). Consolidation stays suppressed while the embedder is echo, so no `echo-embedder` vectors are written; the cost is consolidation simply doesn't run until a real embedder is present. Accepted for v1.
- **R2 — `classifyDegrade` is substring-based.** It matches stable, operator-facing error strings from `wiring.go`. If those messages change, update `classifyDegrade` in lockstep (covered by test 3). An unrecognized cause defaults to reachability (degrade + poll), which is safe because polling is read-only.
- **R3 — Window before first background tick.** A destructive job whose cadence elapsed *exactly* at boot is gated by D4 before it runs, so there is no race; the gate is set inside `buildRouterResilient` before the scheduler/cron/relay are constructed (`commands.go:2186+`, `:2003+`, `:2125+`).
