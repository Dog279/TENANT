# UI refresh, the rest: tickets for TEN-512 after PR 1 and PR 2

Status: ticket spec, 2026-10-11. Parent: Jira Workstream TEN-512; the epic text is `ui-refresh-epic.md`. Tasks 1 to 3 (TEN-513, TEN-514, TEN-515) are built on `feat/ui-refresh-tui` and `feat/ui-refresh-dashboard` and wait for their PRs. This document specs what is left as five tickets: T4a and T4b (B5, components tier 1, split so the interactive half carries the security work alone), T5 (B5, the three drawn kinds), T6 (B7, activity rail) and T7 (B6, tier 2, a costed placeholder gated on D2). B8 (mobile and browser tests) is acceptance inside T4a, T4b and T5, not a ticket. Reviewed by GLM-5.3-Flash and Clef; the review log is at the end.

## What exists to build on (verified in the code)

- **Tools.** A built-in plugin is a `pluginDescriptor` (`cmd/tenant/plugindesc.go`) whose `Tools()` returns `model.ToolSpec` values (name, description, JSON-schema parameters); the tool mux (`internal/toolmux`) owns enable/disable and dispatch; `/tools`, `/enable`, `/disable` and the dashboard's Tools page follow for free.
- **The archive.** A tool call is already persisted: `working.Message.ToolCalls` carries the call's name and arguments, and the `role=tool` message carries its result. Nothing new is stored for a component: its arguments *are* the component, so the transcript re-renders it on load from what is already saved.
- **Two renderers of a tool call.** The terminal app draws `toolRow{id, name, args, result, …}` in `toolRowLines` (`internal/tui/tui.go`); the dashboard draws `toolCardData{id, name, args, result, trace, …}` in `toolCard` (`internal/dashboard/ssr_chat.go`) and streams it through `chatStream.patches`. A component is one more case in each: when `name == "ui_render"`, draw the component from `args` instead of the generic row or card.
- **Sending a message from the page.** The composer binds the box to a Datastar signal (`data-bind:text`) and `@post('/chat/send')` sends the signals; the handler reads `text` (`chatText`), starts the turn and appends the user bubble. Every POST passes the auth middleware's `checkOrigin` (`auth.go`), so a page on another origin cannot post to it; that is verified, not assumed.
- **Tool arguments arrive whole.** The agent emits `EventToolCall` once with the complete `Args` JSON (`agent.go`); nothing streams half an argument, so a component is drawn once, at the call, after validation.
- **The terminal has no pre-restart history.** It renders what happened in the session; it does not load the archive on start. The dashboard does (`transcriptHTML`, D5).
- **Slash commands.** The terminal treats input starting with `/` as a command (`tui.go`); the dashboard sends it to the agent as text.
- **Styles.** `templates/styles.css` already has `.table-wrap`, `.cards`, `.stat` and `details.tool`; DESIGN.md §4 lists the components and §8 the Datastar rules (no script but `datastar.js`, CSP unchanged).
- **The terminal picker.** `listPicker` (`internal/tui/tui.go`) is the arrow-key chooser `/model pick` and `/configure` use; `choices` and a `form`'s `select` field reuse it, and the setup-entry prompt (`setupEntryState`) is the typed-answer step `/judge use` uses.
- **Evals.** Tasks are YAML under `internal/eval/tasks/<subset>/`, embedded by `internal/eval/embed.go`; a new subset is a folder and one line in the embed directive.

## Tickets

### T4a (TEN-517). Components tier 1: `ui_render` with table and steps (B5, D4 option b, first half)

**Why.** Today the model can only answer in prose. This gives it one tool that says "show this as a table (or steps) with this data" and Tenant draws it from a fixed template on both surfaces. The model never writes HTML or JavaScript. Table and steps are pure rendering, so this half lands the plugin, the schema, the validation, both renderers and the transcript path without any interactive or send-side work.

**Scope.**

1. A built-in plugin `ui` with one tool, `ui_render`. Parameters: `{kind, data}`. In this ticket `kind` is `table | steps`; T4b adds `choices | form`, T5 adds the rest. Shapes:
   - `table`: `columns[]` (strings), `rows[][]` (strings), optional `caption`, optional `source` (plain text, never a link). Caps: 12 columns, 200 rows, 200 characters per cell.
   - `steps`: `items[]` of `{title, detail?, state: todo | doing | done}`. Caps: 50 items, 200 characters per title or detail.
   - **One total cap** on the arguments, 32 KB, on top of the per-field caps, because the arguments ride back into the model's context and into every transcript load.
   The caps are in the JSON Schema (`maxItems`, `maxLength`, `enum`) so a strict model self-corrects before calling, and the handler enforces them again. A rejected call gets a one-line error the model can act on ("table: 14 columns, the limit is 12"); a **live** call with an unknown field renders what it can and warns in the result note ("ignored unknown field: colums"), so a typo gets feedback; an **archived** call with an unknown kind or field renders as the ordinary tool card, never an error, so arguments saved under a later schema still render (the versioning rule; no `v` field). The result note carries the component id: `shown: table <call id>, 12 rows`; the id is the tool call's own id, the one the agent emits with `EventToolCall` and keeps on the assistant message, unique within the conversation; a call with no id is not drawn as a component. The tool is a read tool, never gated. Its description is the only prompt change (the epic's "short rule in the system prompt" is dropped: the description is loaded only when the tool is on, the system prompt costs every turn): "use ui_render when the answer is a comparison, a list of steps, a set of next actions, or needs the user's input; otherwise answer in prose".
2. **Dashboard rendering**, in `toolCard`'s path: when the call's name is `ui_render` and its arguments validate, the component is drawn in place of the card, once, at the tool-call event (the arguments are complete there). `table` is `.table-wrap` with a `<table>` (caption, header row, scroll inside the box); `steps` is an ordered list with a done/doing/todo mark. Every value is escaped text; no attribute is ever built from model data (a template test asserts no `href` or `src` comes from a component). A call that fails validation is the ordinary error card with the error.
3. **Terminal rendering**, in `toolRowLines`' path: `table` through `lipgloss/table` with the cell-aware wrapping the Markdown renderer uses; `steps` as `✓` / `◐` / `○` lines. A component is always open (it is the answer, not a tool detail); Ctrl+O keeps toggling the other rows.
4. **Transcript on load.** The dashboard's `transcriptHTML` renders a saved `ui_render` call from its arguments like a live one. In the terminal, components render within the session (there is no pre-restart history to load).
5. **Evals.** A new subset `ui` (`internal/eval/tasks/ui/`): two prompts that should produce a table, two a steps list, four that should stay prose. The gate checks that `ui_render` was called with the expected `kind` **and that the arguments validated** (a malformed call fails the case), one prompt feeds the model a rejection to see it retry or fall back to prose, and one has a typo'd field to see the warning land. Runs against live models like `fitness`, with `fitness`'s pass threshold and retry policy; `smoke` is untouched.
6. **Mobile and tests (B8).** Both kinds stack at 390px; the table scrolls inside its box. Handler tests beside `ssr_chat_stream_test.go` for each kind's HTML, a bad shape, an unknown kind, an unknown-field warning, and the 32 KB cap; terminal `View()` snapshots for a table and a steps list; the existing guards stay green (`TestTemplates_NoInlineJS`, `TestCSPInChrome`, the plain-language tests, `TestRouteInventory`).

**Not in scope.** `choices` and `form` (T4b); `compare`, `stats`, `chart` (T5); any model-written markup (T7); a component outside the chat.

**Docs.** Site: `mcp/built-in-tools.md` (the `ui` plugin and `ui_render`), `dashboard/pages.md` (Chat: components), `first-launch/terminal-ui.md`, `models/eval.md` (the `ui` subset). Repo: CHANGELOG, `internal/dashboard/DESIGN.md` §4 (component kinds), the CLAUDE.md table row for `cmd/tenant/plugin_ui.go`.

**Size.** M. One PR, stacked on PR 2 until that merges.

**Acceptance.** "Which models are configured and what do they cost?" answers with a table on both surfaces; "plan the backup and restore" answers with steps; a 14-column table is refused with the one-line error and the model recovers; reloading Chat shows every component again; the CSP header is unchanged and no script but `datastar.js` is served; the prose prompts in the `ui` eval stay prose with the main model.

### T4b (TEN-518). Components tier 1: `choices` and `form`, the send path and its guards (B5, second half)

**Why.** The two kinds that let the operator answer with a tap or a filled form instead of typing. They post a message as the operator, so this ticket carries the security review: model-authored text must never pass as something the operator typed.

**Scope.**

1. **Shapes.** `choices`: `options[]` of `{label, send?}`, 2 to 6 options, labels of 80 characters, `send` (the message a tap sends; defaults to the label) of 200 characters. `form`: `fields[]` of `{name, label, type: text | number | select | range, options?, min?, max?, default?}` (8 fields; `name` matches `^[a-z][a-z0-9_]{0,30}$`, so it is a valid signal name), `submit` (button text), `template` (the message to send, 1 KB, with `{name}` placeholders). Validation also **rejects a `send` or `template` that starts with `/`** (it would run a slash command in the terminal) or contains a newline.
2. **The send path (D8, made concrete).** Both kinds post to the existing `POST /chat/send` with Datastar signals: `component` (the `ui_render` call's id) and, for a form, one signal per field (`f_<name>`); a `choices` tap sends `choice` (the option index). When `component` is present, `text` is ignored. **The server fills the message**: `handleChatSend` looks the call up by id in the running process's working set, the only store there is (it is in memory, created fresh at launch, and compaction trims it), takes the `send` or the `template` from the saved arguments, validates the inputs (`choice` within the options' bounds, `select` among the options, `number` and `range` parsing and sitting within `min`/`max`, every field present), fills `{name}` and starts the turn with that text. A call no longer in the working set (a restart, a compaction) gets a system row in the chat, "that form is gone, ask again": no turn, no tag. A second submit while a turn runs gets today's "still answering" row (the coordinator already refuses it). The page never supplies the template, so a page cannot forge one, and the request still passes `checkOrigin` like every POST. No new route.
3. **Marked as sent from a component.** The user message gets `Meta{"source": "ui_render", "component": id, "kind": "choices" | "form"}`. Both surfaces show it: the dashboard's user bubble carries a small "via choices" / "via form" tag, the terminal's turn header says the same, and the transcript on load keeps the tag. The model sees the component id in its result note (T4a) and the filled text as the next user message, so a reply correlates to the component that caused it.
4. **Dashboard rendering.** `choices` is a row of buttons, each `@post('/chat/send')` with its `component` and `choice` signals; `form` is native inputs (`text`, `number`, `select`, `range` with its value shown) bound to signals and a Submit that posts them. Buttons and inputs are built from escaped text only. A component from an earlier exchange still works on load while its call is in the working set; otherwise the "gone" row above. `choices` buttons and the form stack at 390px.
5. **Terminal rendering.** `choices` as numbered options the operator picks with the arrow keys (`listPicker`) or types a number for; `form` as one prompt per field (`setupEntryState`; `listPicker` for `select`), ending in the filled template sent as the next message **through the agent's message path, never through the slash-command parser**. The terminal calls the same Go fill-and-validate helper the server uses (one function in the `ui` plugin package) and sets the same `Meta`, so the "via" tag cannot go missing on one surface. **A component takes input only once the turn is complete**; streaming output never steals the prompt, a pending approval card comes first, and Esc or `/cancel` leaves it with nothing sent. Only the latest turn's component is interactive in the terminal; older ones render static.
6. **Tests.** Handler tests: a choices tap and a form submit each start a turn with the filled text and the `Meta` tag; `choice` out of bounds (99, -1); a `select` value not among the options; a number outside `min`/`max`; a missing field; a bad field name; an unknown component id (the "gone" row, no turn); a `send` starting with `/`; `component` plus `text` (text ignored); a second submit during a turn; a cross-origin POST to `/chat/send` is rejected (asserting today's guard holds). chromedp: a choices tap and a form submit start a new turn and the transcript shows the "via" tag; the form at 390px. Terminal tests: the form prompt sequence, Esc mid-form sends nothing, a choice picked while the model is still streaming waits. The `ui` eval subset gains two choices prompts, two form prompts and one form round trip.

**Not in scope.** Persisting form input anywhere; multi-step forms; file inputs; a component outside the chat.

**Docs.** The T4a pages, extended; `first-launch/terminal-ui.md` gets the form and choices flow; `dashboard/pages.md` gets the "via form" tag; DESIGN.md §4 and §5 (when a tap counts as the operator speaking).

**Size.** M. One PR, after T4a merges.

**Acceptance.** A `choices` tap and a `form` submit each start a new turn whose user message is the filled text, tagged "via choices" or "via form" on both surfaces and after reload; a `send` beginning with `/` is refused at validation; `choice=99` is refused; a form submit from another origin is rejected; a submit for a call that is gone shows the system row and starts nothing; the terminal never runs a component's text as a slash command.

### T5 (TEN-519). Components tier 1, second pass: compare, stats and chart

**Why.** The three kinds that need drawing code, added once T4 has shown how models use the first four (which kinds they reach for, how often the shape is wrong, whether 200 rows is too many).

**Scope.** `compare` (`items[]` of `{name, attributes{}}`, optional `best{}` per attribute; 2 to 4 items, 12 attributes): cards side by side on the dashboard, aligned columns in the terminal. `stats` (`tiles[]` of `{label, value, delta?}`; up to 8, 40 characters each): `.stat` tiles in a `.cards` row, one line per tile in the terminal. `chart` (`type: bar | line`, `labels[]`, `series[]` of `{name, values[]}`; 2 series, 52 points): an inline SVG drawn by Go with no chart library, sized by the box, with a text table of the values under it for screen readers and for the terminal, which draws block-character bars or a sparkline. The `ui` eval subset gains three component prompts and two prose ones.

Labels, names and values are escaped text; no attribute comes from model data (the T4a template test covers these kinds too).

**Not in scope.** Interactive charts, images, maps, anything the model draws itself.

**Docs.** The same pages as T4a, extended. **Size.** M. One PR. **Starts when** T4a and T4b are merged and two runs of the `ui` eval with the main model, a week apart, show kind accuracy of 80% or better and under 10% malformed calls; below that, fix the tool description first. The trigger is eval-only on purpose: nothing collects runtime kind accuracy.

### T6 (TEN-520). Activity beside the dashboard chat (B7)

**Why.** The terminal app's feed is the thing operators asked to keep; the dashboard has the same lines on the Logs page but not next to the chat.

**Scope.** A `Show activity` toggle on the Chat page that reveals a rail of the live feed lines (the same `GET /events` stream the chat already subscribes to, filtered to the feed kinds), newest at the bottom, capped at 200 lines in the page. The toggle is a Datastar signal remembered with Datastar's `data-persist` (its own localStorage plugin, so no script of ours and no server state). At 390px the rail is below the chat, collapsed by default. No new route. Tests: a handler test for the filtered rows; chromedp: toggle on, reload, the rail is still on and does not flash before the stream connects.

**Not in scope.** Filtering, search, or anything the Logs page already does better.

**Docs.** `dashboard/pages.md` (Chat). **Size.** S. Its own small PR, after T4a: a rail that rides another PR "if cheap" ships half-tested.

### T7 (TEN-521). Components tier 2: the model writes the page itself (B6, gated on D2)

Filed as a placeholder so the decision has a concrete cost next to it; nothing is built until D2 says yes. If D2 is "never", the ticket closes won't-do.

**What it would take** (from the epic's D2 section, named `ui_custom` as the epic does): a `ui_custom` tool whose argument is a complete HTML document with a required text fallback; a stored artifact with a size cap (64 KB), an unguessable id and a kill switch; its own route `/ui/custom/<id>` serving it with `Cache-Control: no-store` and its own CSP (`default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'none'; form-action 'none'`), so the frame cannot reach the network; the chat embeds it in `<iframe sandbox="allow-scripts">` with no `allow-same-origin`, so the frame has an opaque origin and cannot read the dashboard, and **that CSP is the only line of defense once scripts are allowed**; no host-side listener (display only, so DESIGN.md's script rule holds on the host page); a `config.json` key `dashboard.custom_ui` off by default; a security review before it ships; chromedp tests that the frame cannot fetch, cannot post, cannot read the parent, and that the kill switch blanks every frame. Size L.

**Docs it would need.** `reference/config.md` (`dashboard.custom_ui`), `mcp/built-in-tools.md` (`ui_custom`), `dashboard/pages.md`, a DESIGN.md exception for the frame, `reference/environment.md` if a kill-switch variable exists.

**Decision D2, recommendation.** (a) never, unless an operator-facing gap that tier 1 cannot cover is written down after four weeks of T4 and T5 in use; then (b), behind the key, with the security review as the gate. Not (c). The decision is the operator's; this ticket waits in To Do until it is made.

## Decisions this spec finalizes

| # | Decision | Answer |
|---|---|---|
| D4 | Kinds in the first components PRs | (b) `table`, `steps` in T4a and `choices`, `form` in T4b; `compare`, `stats`, `chart` in T5 |
| D7 | How a component is stored | Nothing new: the `ui_render` call's arguments are the component; the dashboard transcript re-renders from the archive it already has; an unknown kind or field renders as the ordinary card, never an error, so older arguments survive schema changes |
| D8 | How a `form` or `choices` sends | `POST /chat/send` with `component` and field signals (`text` ignored when `component` is present); the server looks the call up in the working set, validates every input including the choice index, fills the message, tags it `via form` / `via choices`, refuses slash-prefixed text, and answers a "gone" row for a call that is no longer held; `checkOrigin` already guards the route; the terminal uses the same helper |
| D9 | B8 (mobile and browser tests) | Folded into T4a, T4b and T5 as acceptance, not a ticket |
| D10 | B7 (activity rail) | Its own ticket T6 and its own small PR |
| D11 | B6 (tier 2) | Placeholder ticket T7; built only if D2 says so |
| D12 | Ticket shape for tier 1 | Two tickets and two PRs: T4a (rendering) then T4b (interaction and its guards), so the security work is reviewed on its own |
| D13 | Prompting | The tool description only; no system-prompt rule (it would cost every turn, the description costs only when the tool is on) |

## Review log

**GLM-5.3-Flash on the first draft (2026-10-11), ten findings:**

| # | Finding | Disposition |
|---|---|---|
| 1 | Nobody fills the form template: the browser has no script for it, the server had no design | Accepted: the server fills it (T4b §2), looking the call up by component id in the working set; the page never supplies the template |
| 2 | Model-authored `send` / `template` text is posted as the operator's words: an injection channel | Accepted: tagged `via choices` / `via form` on both surfaces and in the archive (`Meta`), `send` capped at 200 and `template` at 1 KB, slash-prefixed text refused, a chromedp test for the tag |
| 3 | Streaming would render half-written arguments as flickering error cards | Held with evidence: `EventToolCall` carries the complete arguments once; the spec now says a component is drawn once, at the call, after validation |
| 4 | A cross-origin form POST could drive `/chat/send` | Held with evidence: every POST passes `checkOrigin` in the auth middleware; T4b adds a test that asserts it |
| 5 | D7's holes: schema versioning, the terminal has no archive load, interactive replay of old components | Accepted: unknown kind or field renders as the ordinary card (the versioning rule); the terminal renders within the session only; older terminal components are static, only the latest turn's is interactive |
| 6 | T4 is L, not M: the interactive terminal flow plus the security work hide inside one ticket | Accepted: split into T4a (table, steps, the plugin, both renderers, transcript) and T4b (choices, form, the send path and its guards), each its own PR |
| 7 | Caps incomplete; the model cannot correlate a reply to a component | Accepted: a 32 KB total cap, per-string caps everywhere, caps in the JSON Schema, a component id in the result note and in the send tag |
| 8 | `source` wants to be a link; escaping asserted, not designed | Accepted: `source` is plain text; a template test asserts no `href` or `src` is built from model data |
| 9 | D2 "leaning" is not a decision; T7 lacked docs, a consistent tool name, `allow-scripts`, `no-store`, unguessable ids | Accepted for T7's text (`ui_custom`, docs, `allow-scripts` named as the point where the CSP is the only defense, `no-store`, unguessable ids); the D2 answer is sharpened to "(a) unless a written gap after four weeks, then (b) behind the key with a security review", and stays the operator's call |
| 10 | The epic's system-prompt rule dropped silently; T7 numbering gap; T5 had no trigger; T6 "if cheap"; eval gate only checked `kind`; terminal form vs streaming unstated | Accepted: D13 records the prompt decision; tickets renumbered T4a, T4b, T5, T6, T7; T5 starts on an eval threshold; T6 is its own PR; the eval gate checks validation and includes a rejection and a round trip; a component takes input only once the turn is complete |

**GLM debate round on the revision (same day):** all ten conceded (3 and 4 recorded as its own errors). Seven new points and four gaps, all accepted:

| # | Point | Disposition |
|---|---|---|
| N1 | Two stores named for the lookup ("working set" and "archive") with different lifetimes | One store: the running process's working set, in memory, created fresh at launch (verified: `working.New()` at every launch, nothing saved); a missing call gets the "gone" system row, no turn; a submit-after-restart test |
| N2 | The component id had no provenance | It is the tool call's own id, the one `EventToolCall` carries and the assistant message keeps; a call without one is not drawn as a component |
| N3 | `choice` index unchecked | Bounds-checked against the saved options; tests for 99 and -1 |
| N4 | Field names unconstrained; `text` + `component` precedence unstated | `^[a-z][a-z0-9_]{0,30}$`; `component` wins and `text` is ignored, with a test |
| N5 | Unknown-field leniency blinds a live call | Live calls warn in the result note; the leniency is for archived calls only; a typo prompt in the eval |
| N6 | T5's trigger was unmeasurable | Eval-only: two runs a week apart, by design (nothing collects runtime accuracy) |
| N7 | Two fill implementations would let the "via" tag drift | One Go helper in the `ui` plugin package, used by the server and the terminal |
| gaps | T4b had no 390px line; "gone" behaviour unstated; concurrent submits; `data-persist` reload flash | All four added (T4b stacks at 390px; the "gone" row starts nothing; the coordinator's "still answering" row covers a second submit, with a test; T6 asserts the rail survives a reload without flashing) |

**Clef (System One, structured questions):** the server accepted TCP but closed every HTTP request during this pass (both addresses), so its scores are pending; they are appended here when it answers. The Jira tickets TEN-517 to TEN-521 were filed from this revision.
