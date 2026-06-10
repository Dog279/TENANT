# iMessage via the openclaw approach — design + plan

Status: **IMPLEMENTED (Layer 1)** — unit-tested cross-platform; live read/send
gated on Full Disk Access + Automation (see below)  ·  Owner: Dylan  ·  Relates
to: TEN-68 (was: BlueBubbles config)

## Implementation status (2026-06-05)

Layer 1 (transport swap) is **implemented, additive, and Windows/Linux-green**.
All anti-loop primitives ship; the autonomous responder is the documented
Layer-2 follow-up.

**New files (all in `internal/plugins/imessage/`):**
- `transport.go` — the 5-method `transport` interface + compile-time proof
  `*Service` (BlueBubbles) still satisfies it (tag-free).
- `chatdb.go` — `chatReader` over an injected `*sql.DB`: `ListChats` /
  `ChatMessages` / `SearchMessages` / `MessagesSince`, Mac-time conversion,
  `attributedBody` fallback, handle/`is_from_me` mapping (tag-free).
- `attributedbody.go` — varint-aware streamtyped string extractor (UTF-8 +
  UTF-16LE fallback, `[unsupported message]` on failure) (tag-free).
- `applescript.go` — `sendToChatScript` / `sendToBuddyScript` argv builders
  (anti-injection: text travels in argv, never interpolated) (tag-free).
- `native.go` — `NativeConfig`, `Native` interface, `errMacOnly` (tag-free).
- `native_darwin.go` (`//go:build darwin`) — `OpenNative`: read-only chat.db
  open (`?mode=ro&_pragma=query_only(1)`, **no immutable=1**), FDA-error
  mapping, `osascript` send with a context timeout + Automation hint.
- `native_other.go` (`//go:build !darwin`) — `OpenNative` returns `errMacOnly`.
- `watch.go` — `Watcher` (is_from_me filter + monotonic ROWID cursor via a
  `cursorStore` satisfied by `*improve.Meta` + echo/dedup cache + allowlist),
  non-autonomous `Poll` consumer (tag-free).
- Tests: `chatdb_test.go`, `attributedbody_test.go`, `applescript_test.go`,
  `watch_test.go` — synthetic Apple-schema DB, golden blobs, anti-loop cases.

**Edited (additive only):**
- `dispatch.go` — `svc *Service` → `svc transport`; `NewDispatcher` widened;
  empty-guid send result handled for the native (AppleScript) path.
- `cmd/tenant/toolmux.go` + `commands.go` — transport selection: a BlueBubbles
  `--bb-url`/`$BLUEBUBBLES_URL` opts into the server bridge; otherwise the
  default is native (`OpenNative`). Native handle closed via `addCleanup`/defer.
- `cmd/tenant/doctor.go` — `checkIMessage`: native FDA probe (opens chat.db
  read-only) → WARN+fix when unreadable, OK when readable; BlueBubbles URL
  noted; SKIP off-darwin. **Verified live:** correctly WARNs today (FDA not yet
  granted) with the actionable Full Disk Access fix.

**Verified:** `go build ./...`, `GOOS=windows/linux go build ./cmd/tenant`,
`go vet`, `go test ./...` all green (Windows build byte-path unchanged). 28
imessage unit tests pass incl. the pre-existing BlueBubbles suite.

**Still gated on Dylan (can't run without TCC):** one live chat.db read + one
live osascript send, after granting Full Disk Access (terminal/binary) and
Automation (Messages). The reader/sender/watcher logic is otherwise fully
exercised against a synthetic DB.

## Drive-allowlist (security gate, 2026-06-05 — implemented, additive)

Added ahead of Layer 2 so inbound never opens unrestricted: a **deny-by-default
allowlist** of phone numbers / emails permitted to *drive* the agent over
iMessage (i.e. let a texter invoke the agent's full tool set — the "MCP
framework"). The operator manages it; the inbound responder enforces it.

Designed via plan → 2 unbiased reviewers (security + Go-architecture) → build →
document. The reviewers surfaced one structural risk and one scope risk:

- **(security) empty-list semantics must DIFFER for "observe" vs "drive."** The
  Watcher's optional `WatchConfig.AllowFrom` treats empty as *observe everyone*.
  Reusing that field for drive would mean a fresh install lets **anyone** drive
  the agent. Fix: a **separate type** `imessage.AllowList` whose `Allows` returns
  `false` on an empty list — empty = nobody. The permissive observe default can
  never leak into the drive gate.
- **(architecture) don't ship a config nothing reads.** The Watcher has no live
  runtime caller yet (Layer 2 is the consumer). So we ship the **tested
  enforcement primitive** now (`AllowList`, deny-by-default, re-normalizes input)
  + the management UI + persistence, and Layer 2 wires the single call site.

**New / edited (all additive):**
- `internal/plugins/imessage/allow.go` — `AllowList` (immutable, normalized,
  deny-by-default) + exported `NormalizeHandle` (one-line wrapper over the
  internal `normalizeHandle`, so cmd/tenant stores handles in the exact
  canonical form the matcher compares). `allow_test.go` covers deny-by-default,
  re-normalization, dedupe/sort, immutable With/Without.
- `cmd/tenant/launchconfig.go` — additive `imessageConfig{ AllowFrom []string }`
  on `launchConfig` (`json:"imessage"`), persisted via the existing atomic
  `save`. Lives next to `relayConfig` (operator policy, configured once — not
  hot per-poll state like the Watcher's ROWID cursor in `improve.Meta`).
- `cmd/tenant/imessageallow.go` — `imessageAllowManager` (implements
  `tui.IMessageControl`; mirrors `dashboardManager`/`discordRelayManager` with a
  persist closure; persist-then-swap so a write failure never desyncs the UI).
  `imessageallow_test.go` covers add/dedupe/deny/clear/persist-failure-rollback.
- `internal/tui/tui.go` — `IMessageControl` interface + `Config.IMessage` field +
  `/imessage` (alias `/imsg`) slash command `list|allow|deny|clear`, rendered
  with an explicit "(empty) — deny-by-default" notice; `/help` rows under
  **Safety & approvals** (it's an access-control gate, like `/permissions`).
  `tui_test.go` drives the command against a fake control.
- `cmd/tenant/commands.go` — wires the manager into `tui.Run(... IMessage: ...)`,
  seeded from `lc.IMessage.AllowFrom`, persisting back through `lc.save`.

**Layer-2 enforcement requirements (recorded from the security review — MUST be
honored when the inbound responder lands):**
1. Gate **every** inbound message on `AllowList.Allows(handle)` at the drive
   point (after is_from_me/dedup, before any tool runs). Read the persisted list
   as source of truth; re-normalize the handle at the check (don't trust caches).
2. **iMessage-only:** require `service = 'iMessage'` (not SMS) for an allowed
   handle — green-bubble SMS sender IDs are trivially spoofable. (chat.db has the
   column; `MessagesSince` would need to select it.)
3. **Group chats:** refuse to drive from group chats (1:1 only) — or require ALL
   participants allowlisted — since replies broadcast to everyone in the room.
4. **Audit** denied drive attempts (handle, chat, time).
5. **Unicode:** `normalizeHandle` lowercases but does not NFC/casefold — harden
   before trusting confusable emails. (Left as-is now: changing it would alter
   the stable Watcher matcher; additive constraint.)

`config.json` stays `0644` (the allowlist is policy, not a secret — secrets live
in `credentials.json` `0600`); changing the global perms would be a non-additive
change to stable code. Noted for the operator.

## Goal

Replace the BlueBubbles-backed iMessage transport with an **openclaw-style native
macOS integration**:

- **Read** incoming messages straight from `~/Library/Messages/chat.db` (SQLite),
  using the `modernc.org/sqlite` driver we already depend on (pure-Go, CGO-free).
- **Send** via AppleScript (`osascript` → Messages.app) — no external server.
- **Don't loop**: port openclaw's anti-loop layer — `is_from_me` filtering, a
  monotonic `ROWID` cursor, and an echo/dedup cache.
- **Axe BlueBubbles** as the live path while keeping the Windows build green.

## Why openclaw's design

openclaw connects to iMessage with: a watcher over `chat.db` keeping a per-account
cursor (`lastSeenRowid`/`lastSeenMs`), `is_from_me` filtering, an inbound-dedupe
cache, and AppleEvents/AppleScript for basic sends (private-API dylib only for
reactions/edits, which we do **not** need). That's a proven, server-less design
that fits Tenant's single-binary philosophy. We adopt the *design*, implemented
natively in Go — we do **not** shell out to openclaw's `imsg` binary.

## Hard constraints

1. **Additive / Windows-stable.** Tenant is stable on Windows. All new code is
   **additive** and **`//go:build darwin`-gated**; non-darwin builds get a stub
   that compiles and returns a clear "macOS only" error. The Windows build path
   is byte-for-byte unchanged.
2. **CGO-free.** Use `modernc.org/sqlite` (already vendored). No cgo, no
   `mattn/go-sqlite3`.
3. **Blast-radius gating unchanged.** Sends stay behind the existing `Policy`
   (`AllowSend` + `Confirm`). The model can't self-approve.

## macOS realities discovered

- **Full Disk Access (TCC) is mandatory** to open `chat.db`. Verified: our shell
  gets `authorization denied` today. The `tenant` binary (or the terminal that
  launches it) must be added to System Settings → Privacy & Security → Full Disk
  Access. We must detect this and emit an actionable error + a `doctor` check.
- **Automation permission** is required for `osascript`→Messages sends; the first
  send triggers a TCC prompt.
- **`message.text` is frequently NULL on modern macOS**; the real text lives in
  `attributedBody` (a serialized `NSAttributedString`/typedstream BLOB). The
  reader must fall back to extracting the string from `attributedBody`.
- **`message.date` is Mac absolute time** (nanoseconds since 2001-01-01 UTC on
  High Sierra+). Convert: `unix = date/1e9 + 978307200`.

## Architecture

Today the `Dispatcher` holds a concrete `*Service` (BlueBubbles). We make the
dispatcher transport-agnostic and add a native transport.

### Change 1 — introduce a `transport` interface (minimal, low-risk)

The dispatcher already calls exactly five methods. Define an interface they all
satisfy and widen the field type from `*Service` to the interface. `*Service`
(BlueBubbles) satisfies it **unchanged**, so existing behavior is preserved.

```go
// transport.go  (platform-neutral)
type transport interface {
    ListChats(ctx context.Context, limit int) ([]Chat, error)
    ChatMessages(ctx context.Context, chatGUID string, limit int) ([]Message, error)
    SearchMessages(ctx context.Context, text string, limit int) ([]Message, error)
    SendText(ctx context.Context, chatGUID, text string) (string, error)
    NewChat(ctx context.Context, address, text string) (string, error)
}
```

`Dispatcher.svc *Service` → `Dispatcher.tx transport`; `NewDispatcher(svc *Service,…)`
→ `NewDispatcher(tx transport,…)`. Two-line change, no behavior change.

### Change 2 — native transport (darwin-only, additive files)

- `chatdb_darwin.go` — opens `chat.db` read-only (`?mode=ro&immutable=1`),
  implements `ListChats`/`ChatMessages`/`SearchMessages` via SQL over Apple's
  schema (`message`, `chat`, `handle`, `chat_message_join`, `chat_handle_join`),
  with `attributedBody` fallback + Mac-time conversion. Maps TCC "unable to open"
  → a friendly "grant Full Disk Access" error.
- `applescript_darwin.go` — `SendText`/`NewChat` via `osascript` with text passed
  through `on run argv` (no string interpolation → no AppleScript injection).
- `native_other.go` (`//go:build !darwin`) — `openNative()` returns
  `errMacOnly`; keeps Linux/Windows compiling.
- `attributedbody.go` — pure-Go extractor for the typedstream string (with tests).

### Change 3 — anti-loop watcher (the "not looping" patch)

`watch_darwin.go` — `Watcher` that polls `chat.db` for `message.ROWID > cursor`
and yields only **actionable** inbound messages:

1. **`is_from_me = 0` filter** — never act on our own / the operator's sends.
2. **Monotonic ROWID cursor** — `WHERE ROWID > :lastSeenRowid ORDER BY ROWID ASC`;
   advance only on successful dispatch (failed rows retried, capped).
3. **Echo/dedup cache** — every text we send is recorded (chat+text+ts, short
   TTL); a matching inbound row is dropped even if Apple surfaces it.
4. **Allowlist (optional)** — `allowFrom` handles; default off = read-only watch.

Cursor + dedup persist in the Tenant data dir (new tiny table in the existing
`tenant_meta.db`, or a JSON sidecar — decide in review).

### Change 4 — wiring (additive, transport selection)

- `toolmux.go` / `commands.go`: add a `--imessage-native` selector (or auto:
  prefer native on darwin, fall back to BlueBubbles if `--bb-url` given). Default
  on macOS = **native**. BlueBubbles flags remain but are no longer the default.
- `tenant doctor`: add `checkIMessage` — FDA probe + Automation probe.
- Config wizard (TEN-68): replace the "BlueBubbles url+password" prompt with an
  FDA/Automation readiness check (no secrets needed for native).

### Scope: two layers

- **Layer 1 (this PR): transport swap.** Native read + osascript send behind the
  existing 5 tools. Fully unit-testable against a synthetic Apple-schema DB.
- **Layer 2 (follow-up): autonomous responder.** Wire `Watcher` into a relay loop
  that auto-replies (mirrors the Discord relay workstream TEN-114…125). Ship the
  anti-loop primitives now; wire the loop after Layer 1 is verified live.

## Testing

- **Unit (no FDA needed):** build a synthetic `chat.db` with Apple's schema +
  rows (NULL-text/attributedBody cases, Mac-time dates, group vs 1:1, is_from_me
  mix). Assert `ListChats`/`ChatMessages`/`SearchMessages` shape + ordering.
- **attributedBody:** golden-blob → expected-string tests.
- **Watcher anti-loop:** feed synthetic rows; assert is_from_me skipped, cursor
  monotonic, dedup drops echoes, retry cap honored.
- **osascript:** unit-test the AppleScript **builder** (argv form) without sending;
  one manual live send to Dylan's own number after Automation is granted.
- **Cross-build:** `GOOS=windows go build` and `GOOS=linux go build` stay green
  (stub path). `go vet ./...`, `go test ./...`.

## Verification gated on Dylan

Live `chat.db` read can't be tested until **Full Disk Access** is granted to the
terminal/binary. Plan: implement + unit-test now; do the live read/send check
together once FDA + Automation are granted.

## Decisions (post-review debate — 3 independent reviewers)

Three unbiased reviewers (scope/risk, macOS-SQLite domain, Go architecture)
critiqued the draft. Outcomes:

1. **CRITICAL FIX — drop `immutable=1`.** `chat.db` is WAL-mode; `immutable=1`
   makes SQLite ignore the `-wal` file, so a watcher would read a stale snapshot
   and never see new messages until a checkpoint. Open with **`?mode=ro`** only +
   `PRAGMA query_only=1`. WAL readers don't block the live writer.
2. **Seam placement (highest architectural risk).** Put SQL/query logic + the
   `Chat`/`Message` types + the AppleScript *builder* in **tag-free** files that
   operate on an injected `*sql.DB`. Only `~/Library/Messages` path resolution,
   the read-only open, and the `osascript` exec are `//go:build darwin`; a
   `//go:build !darwin` stub returns `errMacOnly`. `cmd/tenant` calls only a
   neutral constructor — no darwin symbol may leak (Windows must compile).
   Reader/attributedBody/watcher tests then run cross-platform.
3. **attributedBody — minimal typedstream parser, not a naive heuristic.** Text is
   NULL on modern macOS; the string lives in a NeXTSTEP `streamtyped` archive (not
   a plist/NSKeyedArchiver). Parse the class records + variable-width length
   (`<128` = 1 byte; `0x81` = 2-byte LE length) and decode NSString (UTF-8) vs
   NSUnicodeString/NSMutableString (UTF-16). Naive byte-grab breaks on emoji.
   Golden-blob tests (ASCII + emoji + mention). Fall back to `[unsupported
   message]` on decode failure rather than erroring the whole read.
4. **Persistence — reuse `tenant_meta`.** `internal/improve/meta.go` already stores
   scheduler cursors in a KV `tenant_meta` table. Store `imessage_cursor_<account>`
   there. No new DB file, no JSON sidecar.
5. **Keep BlueBubbles, retire it as default.** It's the only path that works before
   FDA is granted and the only cross-macOS-version-tested sender; deleting a
   working transport to verify an unverified one is backwards, and removal would be
   non-additive. "Axe BlueBubbles" = remove from the default/wizard flow; native is
   the default on darwin, BlueBubbles only if `--bb-url` is given.
6. **Scope.** Ship the native transport (reads + sends behind the existing 5
   tools) now. Build the anti-loop **Watcher** (is_from_me + monotonic ROWID
   cursor + echo/dedup cache) with a **non-autonomous `watch`/tail consumer** that
   surfaces new actionable inbound messages but does NOT auto-reply. The full
   autonomous responder (auto-reply + approval routing, mirroring the Discord relay
   workstream) is the documented follow-up.

### Other gotchas to handle (from review)
- `message.date`: integer math `date/1e9 + 978307200`; legacy rows may be seconds
  (`if date < 1e11` treat as seconds).
- Sender of an inbound msg = `handle.id` via `message.handle_id → handle.ROWID`;
  for `is_from_me=1`, `handle_id` is unreliable → `From="me"`.
- `chat.guid`: 1:1 = `iMessage;-;+15551234567`, group = `iMessage;+;chatNNN`; both
  are valid `chat id` targets for AppleScript send. Service prefix may be `SMS;…`.
- Normalize phone handles toward E.164 for `new_chat` + dedup matching.
- `osascript` first run triggers an Automation TCC prompt that can hang → run it
  under a context timeout.
- Pass send text via `osascript … on run argv` (no string interpolation) — sound
  anti-injection (confirmed).
- AppleScript `send` is historically fragile on Ventura+/Sonoma+, esp. groups and
  the account selector — expect to verify live and possibly add fallbacks.
