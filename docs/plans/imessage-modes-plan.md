# iMessage on a Mac: passive reading + operator response modes

Status: **IMPLEMENTED locally** (2026-10-01, branch `feat/imessage-modes`, not pushed) · Jira TEN-484 (TEN-485/486/487) · Builds on TEN-68 / TEN-230 / TEN-267

## What already exists (don't rebuild)

Most of the openclaw/hermes-style native path shipped in TEN-68/230/267:

| Piece | Where | State |
|---|---|---|
| Native chat.db reader (read-only, WAL-safe, attributedBody, Mac time) | `internal/plugins/imessage/chatdb.go`, `native_darwin.go` | done |
| AppleScript send (argv, no injection) | `applescript.go`, `native_darwin.go` | done |
| BlueBubbles transport (5 tools) | `bluebubbles.go`, `client.go` | done |
| Transport choice: `--bb-url` ⇒ BlueBubbles, else native | `cmd/tenant/plugin_imessage.go` | done (logic copied in 4 places) |
| Anti-loop Watcher (is_from_me, ROWID cursor, echo cache) | `watch.go` | done |
| Autonomous responder, live `/imessage on\|off`, own permission broker, operator `Y <nonce>` approvals | `cmd/tenant/imessagerelay.go`, `imessageconfirm.go` | done, **native only** |
| Drive allowlist (deny-by-default), operator handle | `allow.go`, `config.IMessage` | done |

## The gaps

1. **No passive mode.** The responder hands `AllowFrom` to the Watcher, so a text
   from anyone off the list is dropped silently. It's never logged, never shown
   in the feed, and the agent never sees it. The only way to "see" a text is to
   let that person drive the agent.
2. **Open security-review items from TEN-68 are still open:** no SMS rejection
   (green-bubble sender IDs are spoofable), no group-chat refusal (a reply goes to
   the whole room), and denied drive attempts aren't audited. The allowlist is
   also read once at `Start`, not on each message.
3. **The responder is native-only.** BlueBubbles gives you tools but no inbound
   loop. Native also can't run on a non-Mac host (the R340).
4. **Copy and setup still say "iMessage (via BlueBubbles)"**, and the setup
   wizard asks for a BlueBubbles URL even though native needs nothing.

## Design (smallest change that closes the gaps)

**One mode knob plus the existing lists.** No new per-handle policy table.

```
imessage.enabled = on/off  (unchanged)      imessage.passive = true ⇒ read only
```
(Built as a `passive` bool next to the existing `enabled`, not a new `mode`
string: no migration, and old configs mean "respond".)

| Sender / chat | `passive` | `respond` |
|---|---|---|
| `operator` handle, 1:1 iMessage | observe | **drive** (+ approves `Y <nonce>`) |
| `allow_from` handle, 1:1 iMessage | observe | **drive** |
| anyone else | observe | observe |
| any group chat, or any `SMS;…` chat | observe | observe |

- **Observe** means: emit `imessage.Observed` (it lands in the `imessage` logs-hub
  source, so the existing `tenant logs alerts` rules can fire on it), push to the
  activity feed, and never reply. The agent reads the details on demand with the
  `imessage_*` tools that already exist. chat.db is the store, and nothing is
  copied out of it.
- **Drive** works the same as the current responder turn.
- The Watcher is built with **no allowlist** (observe all), and the drive gate
  moves into `imessageResponder.handle`. It reads the live `AllowList` on each
  message, so it no longer depends on a snapshot taken at `Start`.
- Service and group come from the chat GUID: `iMessage;-;<handle>` is the only
  drivable shape. That's one helper, with no schema change.

---

## PR 1: Inbound modes + drive gate (core, needed for the Mac test)

Ticket: *iMessage: passive/respond modes; drive gate on 1:1 iMessage only*

- `internal/config/config.go`: add `IMessage.Mode string` (`off|passive|respond`).
  On load, treat legacy `enabled:true` with no mode as `respond` (back-compat).
- `internal/plugins/imessage/allow.go`: add `Drivable(chatGUID) bool`, which is
  true only for the `iMessage;-;` prefix. Add the `Observed` event in `events.go`
  (fields `from, chat, reason=passive|not_allowed|group|sms`, plus the text).
- `cmd/tenant/imessagerelay.go` `handle()`: order is approval-reply → `canDrive(m)`
  → turn, else observe. `canDrive` = mode is respond, `Drivable`, and either
  `AllowList.Allows(from)` or from is the operator.
- Manager: `Start(mode)` replaces the on/off bool. `/imessage passive|respond|off`
  (keep `on` as an alias for `respond`). Dashboard Access page: a mode select in
  place of the toggle (`internal/dashboard/access.go`, `cmd/tenant/access_dashctl.go`).
- The TUI (`commands.go`) and `serve_subsystems.go` `buildFn` both stop passing
  `AllowFrom` to the Watcher.
- **Check:** one table test in `imessagerelay_test.go` covering allowlisted 1:1
  drives, a stranger observed, an allowlisted group observed, an allowlisted SMS
  observed, passive mode with an allowlisted sender observed, the operator nonce
  consumed, and no `SendText` for any observed row.

## PR 2: One transport selector + BlueBubbles as a responder source

Ticket: *iMessage: responder over BlueBubbles; single transport selection*

- Add `openIMessageTransport(url, pass, private)` in `plugin_imessage.go`, used by
  `launchIMessage`, `cmdIMessage`, `checkIMessage` and both responder `buildFn`s.
  This removes four copies of the "url set → BB else native" logic. Add an
  optional `imessage.transport = auto|native|bluebubbles` (auto = today's rule)
  so a Mac that also runs BlueBubbles can pick.
- `client.go`: `(*Service).MessagesSince` and `LatestRowID` via BlueBubbles
  `POST /api/v1/message/query` (`where message.ROWID > :n`, `sort ASC`,
  `with: [chat, handle]`), mapping `originalROWID → RowID`. `*Service` then
  satisfies `messageSource` and `imsgSender`, so the responder runs unchanged.
  **Verify the field names live against your BlueBubbles server.**
- `Drivable` already covers BlueBubbles chat GUIDs (same format).
- **Check:** an `httptest` BlueBubbles stub test in `imessage_test.go` checking
  that the cursor advances, is_from_me is skipped, and the order is ASC.

## PR 3: Mac-first setup, doctor, docs

Ticket: *iMessage: Mac-first setup + doctor + docs*

- Title and labels become "iMessage". Setup fields: `transport` (Options), and
  `url`/`password` only when the transport is bluebubbles; `mode` (Options).
- Doctor: report the mode. Add a native Automation probe (`osascript -e 'tell
  application "Messages" to get name'` under a 5s timeout) and warn on a timeout
  or denial.
- `docs/PLUGINS.md` + CHANGELOG + regenerate docfacts. Retire the stale
  "Layer 2 is follow-up" text in `docs/dev/imessage-openclaw-plan.md`.

---

## As built: what was skipped

- `imessage.transport` override: a BlueBubbles URL still means BlueBubbles.
  Add it when a Mac with both wants native.
- Doctor Automation probe: running `osascript` from doctor can launch Messages
  and prompt. The Delivery event's fix text covers a failed send.

## First-party test runbook (your Mac)

1. **Full Disk Access gotcha:** an unsigned `go build` binary gets a new cdhash on
   every rebuild, and that can drop its TCC grant. For the test week, grant FDA to
   your **terminal app** and run `tenant` from it. Children inherit the grant.
   Under `serve`/launchd, grant FDA to the installed binary path and re-grant
   after each rebuild.
2. `tenant doctor`: iMessage should be OK (chat.db readable).
3. `/imessage passive`. Text the Mac from another phone, and from a group. Both
   should show in the feed and in `tenant logs` under `imessage`, with no reply.
   Ask the agent "what did X text me" and it should answer via `imessage_read_chat`.
4. `/imessage allow <your number>`, then `/imessage respond`. Your 1:1 iMessage
   drives a turn. The same text sent as SMS or in a group gets observed only.
5. Set `imessage.operator`, put a category on `ask`, trigger it, and reply
   `Y <nonce>`.
6. BlueBubbles: set its URL + password (`/configure imessage`), `/imessage off`
   then `on`, and repeat steps 3–4. Check the `originalROWID` cursor against
   your server (verified only against a stub).

## Decisions (Dylan, 2026-10-01)

- **D1 — log bodies.** Passive mode logs the message text to the local
  `imessage` log source. This may change later, so keep the text in one field of
  the `Observed` event; switching to metadata only is then a one-line change.
- **D2 — group chats never drive.** This is permanent, not a v1 limit. `Drivable`
  stays a hard rule with no config knob.
- **D3 — observed texts are not put into the agent's context by default.**
  Follow-up idea, once this is live: asks for FindTime LLC become Jira tickets
  (ask + due date) automatically. That needs no context injection, because the
  pieces already exist:
  `tenant logs alerts add findtime-asks '<filter on imessage Observed>' --action cron --target '<prompt>'`
  fires a cron prompt on matching texts. The prompt reads the chat with
  `imessage_read_chat` and files or updates the ticket via the Atlassian tools.
  A plain periodic cron that sweeps the `imessage` log since its last run also
  works. Pick one when it's live. Nothing is built for it in PRs 1–3.
- **D4 — not a migration.** The 2026-09-28 "dead" call was about migrating
  Tyler's Hermes setup to Tenant. This plan is the generalized, use-from-day-one
  iMessage path for any Tenant install on a Mac (native) or any host (BlueBubbles).
  It gets a new workstream, and the old cutover tickets stay closed.
