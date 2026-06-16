# Plan — Dashboard Activity Tab Overhaul + Modernization

Status: PLAN (awaiting operator decisions in §5). Relates to TEN-199 (Dashboard
v2), TEN-211 (design system), TEN-234 (the live feed events). Likely a new ticket.

Operator requirements (verbatim intent):
- The activity feed "tends to not update if the tab isn't selected" on macOS — fix it.
- "All events prior showing in the tab … even if I haven't been on the dashboard
  earlier" — backfill the whole backlog on load, not just events since connect.
- "An easy way to troubleshoot issues without diving into log files."
- "Modernize the dashboard a bit, keep a congruent theme based off the Skills and
  Memory tabs," and "ensure the activity tab reflects all that has happened"
  (calls, tool-uses, cross-agent bus, ingest, errors).

---

## 1. Root cause (grounded)

Two independent defects, one shared fix.

**(A) No backfill.** `agent.Broker` (`internal/agent/broker.go`) is pure live
fan-out with ZERO retention: `Subscribe()` (broker.go:54) mints a fresh channel
at call time; `Publish()` (broker.go:39) iterates only current subscribers and
drops on a full buffer. `handleEventsSSE` (ssr_chat.go:33) subscribes *after*
headers and loops on new events only — nothing replays prior state. Everything
before you open the tab is gone. (The broker was designed as a live feed, not an
audit log — that's the design choice we're now changing.)

**(B) Stalls when the tab is unfocused, and loses the gap on reconnect.**
- `datastar.js` defaults `openWhenHidden:false`; on `visibilitychange→hidden` it
  aborts the SSE fetch, and macOS throttles/suspends background tabs so it doesn't
  re-establish until focus returns. `activity.html` sets `retryMaxCount` but not
  `openWhenHidden:true`.
- No replay on reconnect: `sse.go` `setSSEHeaders` has no `Last-Event-ID`
  semantics and `handleEventsSSE` never reads it. Each reconnect `Subscribe()`s
  fresh → starts from "now" → every event during the gap is lost.

Both fixed by the same primitive: **a retained, cursor-indexed event log**
(backfill on load + replay-from-cursor on reconnect) + keeping the SSE alive while
hidden. The repo already has the exact lossless pattern to mirror:
`orchestra.Bus.Since(cursor)` (bus.go:130) over an append-only log + coalescing
`Notify()` (bus.go:148). We replicate it for `agent.Event`.

---

## 2. Activity-completeness architecture (core)

Goal: show ALL events from agent start — backfill on first load, live tail,
gap-free catch-up after any reconnect — WITHOUT touching the TUI feed or chat.

**(a) New `internal/agent/eventlog.go` — `EventLog`.** A bounded ring with a
monotonic `uint64` Seq cursor (eviction never corrupts cursors, unlike a slice
index): `Append(ev)`, `Since(cursor) ([]seqEvent, head)` (lossless replay),
`Snapshot() ([]seqEvent, head)` (full backlog). Mirrors `orchestra.Bus`.
Write-time denylist of the noisy kinds (`EventToken/EventUsage/EventAssistant/
EventMemory`, same set as `activityRelevant` ssr_chat.go:113) so the ring holds
real activity, not token flood.

**(b) Additive single tap.** The agent Observer is `broker.Publish` today; wrap it
to fan to BOTH sinks: `obs := func(ev){ broker.Publish(ev); evlog.Append(ev) }`.
No `broker.go` change, no TUI-feed change, no chat change → Windows-stable /
additive. The EventLog is a parallel sink only the dashboard reads.

**(c) `GET /activity` renders the backlog server-side** (from `evlog.Snapshot()`)
into `#activity-feed`, stamped with the head cursor; the SSE then tails from that
cursor. Instant history on load, no round-trip.

**(d) SSE replays-from-cursor then tails** (`handleEventsSSE`): resolve start
cursor from `Last-Event-ID` header (sent automatically on reconnect) or `?cursor=`
(first load); `evlog.Since(cursor)` → write missed rows tagged `id:<Seq>`; then
drive the live tail from the EventLog (`Since`+notify, like the orchestrator) so
the activity path has no drop-on-full failure mode. Add an optional `id` param to
`patchElements` (sse.go:45; empty ⇒ omit, callers unaffected). Add
`openWhenHidden:true` to the `@get('/events')` options. Gap-free by construction.

**(e) Event kinds = "all calls."** Retained + surfaced: tool calls/results
(`EventToolCall/Result/Validation/Retry/ToolCatalog`), lifecycle
(`TurnStart/Final/Truncated/Compact/Interject/Skills`), errors (`EventError` +
`IsErr` results), cross-agent (`Agent != ""`), bus (`EventBus` 🔀), ingest
(`EventIngest` 📥). Denylisted: token/usage/assistant/memory (unchanged TEN-234).

---

## 3. Modernization / theme congruence

Today `activity.html` is a 4-line stub; rows are a flat `.ev`/`.tg`/`.detail`
stream (ssr_chat.go:121). Adopt the Memory + Skills idiom using the SAME
`styles.css` tokens (congruent by construction, no new color system):

- **Filter sub-nav** like Memory's `.memtabs` (styles.css:69) — pill links with the
  active `.on` gradient. Filters are query-param driven (`?kind=&agent=&q=&err=1`),
  server-rendered, zero-JS, refresh-safe, testable (mirrors Memory's `urlWith`).
- **Section chrome:** wrap the feed in `.card`/`.card-head` ("ACTIVITY").
- **Row redesign** adapting Memory's `.ep` episode card (styles.css:106): relative
  timestamp (`.ep-when` monospace + absolute on hover), a semantic kind badge
  (`.tag` normal, `.risk` for validation/retry, red for errors — free error
  highlighting), an agent `.chip` for cross-agent/bus, tool name in `.nm`, and
  **expandable args/results** (`?expand=<Seq>` → full `Args`/`Result` in a nested
  `.prov` block — Memory's inline-expand pattern, no JS).
- **Grouping by turn:** `TurnStart…Final/Truncated` boundaries (+ `ev.Iter`)
  rendered as `.tgroup` with a `.ghead` header → scannable "what happened this turn."
- **Troubleshooting affordances** (replace reading logs): filter by kind/agent,
  error-only toggle, search over tool/text/result, expandable detail, timestamps,
  card empty states.
- **CSS:** reuse existing classes; APPEND any new rules to styles.css — never edit
  existing (shared by every tab).

---

## 4. Phasing + file-level steps

**Phase 1 — Retained event log + lossless replay (the functional fix).** Additive.
- `internal/agent/eventlog.go` (NEW) — the ring + Append/Since/Snapshot + write-time denylist.
- `internal/agent/eventlog_test.go` (NEW) — backfill, gap-free Since, eviction, cursor-older-than-ring, denylist.
- `cmd/tenant/commands.go` (EDIT ~1843) — `evlog := agent.NewEventLog(0)`; Observer fans to broker + evlog; pass evlog to `dashboard.New`.
- `internal/dashboard/server.go` (EDIT ~81/96) — `evlog` field + `New(...)` param (nil-safe default like broker).

**Phase 2 — Backfill + replay-from-cursor in the dashboard.** Dashboard-only.
- `internal/dashboard/sse.go` (EDIT line 45) — optional `id` param.
- `internal/dashboard/ssr_chat.go` (EDIT line 30) — Last-Event-ID/?cursor; `Since` replay; drive tail from EventLog; stamp `id:<Seq>`. Chat stays on the broker.
- `internal/dashboard/ssr_activity.go` (NEW or extend) — `handleActivityPage` renders backlog + cursor + filters.
- `internal/dashboard/templates/activity.html` (EDIT) — server-side backlog; `openWhenHidden:true`; `?cursor={{.Cursor}}`.
- Tests: extend `ssr_events_stream_test.go` (backlog + reconnect catch-up); `templates_datastar_test.go` (openWhenHidden).

**Phase 3 — Modernization / theme.** Templates + activityRow; CSS append-only.
- `activity.html` (EDIT) — `.memtabs` filters, `.card` sections, `.tgroup` grouping, empty states.
- `ssr_chat.go` activityRow (EDIT line 121, or split to `ssr_activity.go`) — `.ep`-card layout, badges, agent chip, expandable detail.
- `styles.css` (APPEND only if needed).

**Phase 4 (OPTIONAL, gated) — cross-restart disk history.**
- `eventlog.go` — JSONL persistence (`<data>/activity/feed.jsonl`, O_APPEND, tail-load on start), behind a flag, off by default.

Additive summary: Phases 1–3 add new files + edit dashboard SSR + the Observer tap
only. No `broker.go` / TUI / chat change → Windows build untouched.

---

## 5. Decisions for the operator

1. **History scope** — *Recommend: in-memory ring only (Phases 1–3).* Satisfies
   "all prior events while the dashboard is on, even if I wasn't watching" within a
   run; matches the single-binary, Bus-in-memory precedent. Trade-off: history
   resets on agent restart. Opt into Phase 4 (JSONL) for cross-restart history at
   the cost of a hot-path disk write + an audit-log/PII liability.
2. **Retention size** — Default 2000 post-denylist events (~hours; low-MB).
   Exposable later as `dashboard.activity_retain`.
3. **Modernization breadth** — *Recommend: activity-tab-only now* (reuses
   Memory/Skills classes; additive, low-risk). A cross-tab design-system pass is
   TEN-211 / TEN-199 territory — keep it there to avoid broadly editing shared
   styles.css under the additive constraint.
