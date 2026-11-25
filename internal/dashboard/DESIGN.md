# Dashboard design guide

How the web dashboard looks, talks and behaves (TEN-211). Every page follows it; `ui_test.go` and `ui_browser_test.go` enforce what can be checked. The shared pieces live in `ui.go`, `templates/ui.html` and the `TEN-211 design system` block of `templates/styles.css`.

## 1. Who it's for

The operator is not a developer. They run Tenant on a home computer or a Windows server, and they open the dashboard on a laptop or a phone to answer five questions. Each question has a page that answers it, and the status strip at the top of every page answers the urgent ones without a click.

| Question | Where it's answered |
|---|---|
| Is my agent healthy? | Overview, Logs; the status strip (busy, waiting) |
| What has it learned, and is it getting better? | Memory, Skills, Quality, Research |
| What can it do, and who can reach it? | Tools, Helpers, Remote services, Fleet, Integrations, Access |
| Is anything waiting for my OK? | Approvals & safety; the strip's "N actions are waiting for your OK" on every page |
| Can I change this safely? | The confirm step and its consequence line (§5) |

## 2. Voice

- Say what happens, to whom, in everyday words. "Your agent can no longer use X", not "Provider removed".
- The consequence comes before the action: the confirm step says what changes, then offers "Yes, …".
- Say "the computer running Tenant", "your agent", "your main model", "helper", "instructions", "step", "Autopilot".
- Name safety categories by their title (`PermissionInfo.Title`, from `approval.CategoryPlain`): Run commands, Change files, Irreversible actions, Act on websites, Send messages, Share private data, Connect remote services, Leave the sandbox.
- A page's title is its nav label.
- No uppercase warnings, no emoji status markers. Use a pill (`.risk`, `.on-tag`, `.tag-new`) instead.

**Banned on pages** (checked on templates and on rendered pages by `TestTemplates_PlainLanguage` and `TestRendered_PlainLanguage`; the Logs page's filter language is exempt, and so are the log records it shows as written, "Sub-agent …" and "cron-exec turned on" included: they are the record, not the page's words. The page's own labels around them follow this list):

| Don't say | Say |
|---|---|
| soul | personality, instructions |
| compaction, working set | summaries |
| superseded | replaced |
| turn | message, conversation, step |
| posture, Read-only / Allow-send | Tools that change things: All off / All on (Overview: What it may do: Look only / Change things) |
| gated | asks you first |
| cron, cron-exec | scheduled jobs; "Scheduled jobs may make changes" |
| allowlist, handles | who can message your agent; phone numbers or emails |
| exec, exec mode | run commands; "Let Discord messages make changes" |
| egress | Share private data |
| mcp, toolmux | remote services, tools |
| planner, sub-agent, wiki | your main model, helper, your agent's notes |
| broker, episodic, cycle, wave, SSE, CSRF, bearer, credentials.json | say what it does for the operator, or leave it out |
| slash commands, backticked commands | a button, or a plain instruction |

Error text follows the same voice (§6): a control's raw error never reaches a page.

## 3. Palette

Dark only. Contrast measured against `--card` (#111a2e) and `--card2` (#0e1626); WCAG AA needs 4.5:1 for body text.

| Token | Value | On card / card2 | Use |
|---|---|---|---|
| `--fg` | #e6edf6 | 14.71 / 15.33 | body text |
| `--dim` | #8595b0 | 5.72 / 5.96 | secondary text |
| `--label` | #7788a6 | 4.84 / 5.04 | small uppercase labels (`.card-head`, `.stat-label`, `.nav-group`, table heads) |
| `--cyan` | #22d3ee | 9.60 / 10.01 | links, primary, active (with `--violet` in gradients) |
| `--green` | #34d399 | 9.02 / 9.41 | on, ok, success |
| `--amber` | #fbbf24 | 10.39 / 10.83 | needs you: waiting, busy, new |
| `--red` | #fb7185 | 6.44 / 6.72 | error, destructive |

Meaning is fixed: green = on or fine, amber = needs you, red = error or can't be undone, cyan-violet = the primary action or the current page.

## 4. Components

Cards and layout: `.card` (with `.card-head`), `.cards` of `.stat` cards, `.row` (a name on the left, `.actions` on the right), `.actions` (buttons that wrap), `.cinput` (`.wide` for full width), `.page h2`.

Pills: `.on-tag` (green, "active"), `.risk` (amber, "can change things"), `.tag` (cyan), `.tag-new` (amber, "New").

Buttons: `.btn`, `.btn.sm`, `.btn.on` (primary), `.btn.danger`.

Banners: `.banner.ok`, `.banner.err`, `.banner.info`, `.banner.warn` (the ui-flash and status-strip partials make them).

Components the agent answers with (TEN-517): a `ui_render` tool call is drawn as `.comp-wrap` in the chat instead of a tool card, by `internal/uicomp` from the call's arguments. `.comp.comp-table` is a `.table-wrap` holding a `<table>` with an optional `<caption>` and a `.comp-source` line; `.comp.comp-steps` is an `<ol>` of `.step.todo|doing|done` with a `.mark`. Every string is escaped text and no attribute is ever built from model data, so a component carries no link, image or script.

Two kinds answer back (TEN-518): `.comp.comp-choices` is a row of `.btn.sm` buttons, and `.comp.comp-form` a `<form>` of `.field` labels with `.cinput` inputs and a `.btn.sm.on` submit. Both post to `/chat/send` through Datastar signals (`component`, `choice`, `f_<id>_<name>`); the server fills the message from the saved call and tags it, and the user bubble shows `.via` ("via form"). The only variable attribute values are the call id (`[A-Za-z0-9_-]`) and field names (`[a-z][a-z0-9_]*`), both checked before they are written; an id that fails the check draws the component static.

Advanced options hide behind an expander. It is used for the Research options ("Research exactly what I typed") and the Helpers instructions editor:

```html
<details class="advanced"><summary>Advanced</summary>…</details>
```

The partials in `templates/ui.html` are parsed into every page set (`parseSSR` and `logsTmpl`):

```html
{{template "ui-flash" .}}                      <!-- .Msg → banner ok role=status; .Err → banner err role=alert -->
{{template "ui-off" "Helpers aren't available on this Tenant."}}   <!-- a nil control -->
{{template "ui-empty" "Nothing is waiting for you. When your agent needs your OK, it appears here right away."}}
{{template "ui-busy" .Busy}}                   <!-- a BusyView: dot, title · since, detail, Stop/Cancel -->
{{template "ui-confirm" (confirm "/models/remove" "Remove" (printf "Your agent can no longer use %s." .Name) "name" .Name)}}
{{template "ui-confirm" (confirmGo "/models/use" "Use this model" (printf "Every answer from now on comes from %s." .Name) "name" .Name)}}  <!-- a "go": not red -->
```

`ui-status` is the strip itself: `/ui/status` renders it, and so do every in-place answer (`formReply`) and Chat's send. Loading is a class, not a partial: `<span class="loading">Counting</span>`.

Text an agent or a request wrote goes through `visibleText` before it is shown (`{{visibleText .Detail}}`): characters that draw nothing or reorder text (controls, bidi and zero-width marks, tag characters, fillers, variation selectors) show as `⟨U+202E⟩`, and 3 or more blank lines as `⟨N blank lines⟩`. `waited` turns seconds into "waiting 3 min".

The activity rail (TEN-520): `.chat-row` holds the chat and `details.activity`, a native toggle whose `.rail` receives one `.line` per event the terminal app's feed would show (`k-turn`, `k-tool`, `k-ok`, `k-err`, `k-sys`), appended by the events stream and capped at 200 by removing the oldest. No script: the toggle is the browser's; the state is not remembered across a reload.

## 5. When to confirm

An action gets a confirm step when it:

- (a) loses something the page can't bring back: delete, remove, clear, or disconnect a service whose sign-in you'd have to redo;
- (b) cuts everyone off a channel;
- (c) lets the agent, or another person, do more without asking;
- (d) changes every answer.

No confirm on Deny, Chat's Stop, Autopilot's Stop, "Ask me again", tightening a mode, or "Keep them at Ask": friction goes on "go", never on "stop". Research's Stop is the exception: it throws the run away (a), so it confirms (D20).

The step is the `ui-confirm` partial: a native `<details>` whose summary is the first button (it flips to Cancel), a consequence sentence, and a form that posts `confirm=yes`. It needs no JavaScript. Build it with `confirm` for (a) and (b), which is red (§3), or `confirmGo` for (c) and (d), which isn't. The server enforces it where the route is mounted:

```go
mux.HandleFunc("POST /models/remove", needConfirm(nil, s.handleModelRemoveForm))                      // always
mux.HandleFunc("POST /safety/mode", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleMode)) // only when loosening
```

- Without `confirm=yes` (read with `r.FormValue`), the answer is 428 "Nothing changed: that needs a second click to confirm." and the handler never runs.
- `confirmUnless(key, safe…)` is fail-closed: the step is skipped only when the trimmed, lowercased value is one of the safe values. `ALLOW`, `" allow"`, `on`, `always`, `yes`, `1` or a missing field all confirm. A handler that sets a mode still rejects anything outside exactly ask, allow and deny.
- The gate runs before the handler's own checks, so with no model control wired, `POST /models/use` without `confirm=yes` is now 428 where it used to be a 303 to the page.
- Confirm is friction, not security. The session cookie (a random session id with a 7-day life that Log out ends, never the key: cookies reach every port on the host), `checkOrigin` and `checkCSRF` are the boundary. `/api/*` isn't gated: a JSON client acts deliberately.

Every route that changes something is in `reviewedMutations` (security_test.go), the TEN-213 ledger, with when it confirms; `TestRouteInventory` posts each one on the real mux, every spelling of "more" included, and fails on a route nobody classified.

### Confirming a fleet machine from another computer (TEN-545)

Confirming a pending fleet machine is the one action that is not open to "any signed-in browser". The six digits tie the record to the machine the person is looking at. By default they are also taken only at the computer running Tenant (`atThisComputer`: loopback peer, loopback name, no proxy header, a live session, never the API key), which ties the person to the master: a session cookie that leaked, a browser left signed in somewhere, or someone on the tailnet who should only look cannot confirm a machine of their own.

A master with no screen (Server Core, a headless box) needs a console for every machine under that rule. Decision, 2026-11-24: the operator can turn on confirming from a signed-in browser elsewhere. What stands in for "at this computer":

- **Off by default, and switched only at the master's terminal.** `fleet.remote_confirm` in `config.json`, set by `tenant fleet remote-confirm on|off` or `/fleet remote-confirm`. There is no switch on the dashboard: a session elsewhere must not open the door it then walks through. The running Tenant reads the file again at each confirmation, in the handler and in the control behind it; a file that can't be read is off.
- **The sign-in key again, every time.** The form from a browser elsewhere has a key field (`keyAgain`). It is compared as sign-in compares it, taken from the body only, and wrong keys are slowed per address by sign-in's own count. A session alone is not enough.
- **The page's own form only.** A live session and the dashboard's Origin (`signedInBrowser`). The API key confirms nothing, on or off.
- **The per-machine wait is unchanged**, and attempts are still taken one at a time.
- **The records say where.** Every refusal and the confirmation itself carry the surface `dashboard-remote` with the session's tag and address (`security/4323`, `security/4324`, `fleet/1002`, `fleet/2102`). Turning the setting on or off is a Security record too.

Weighed and not built: a list of addresses that may confirm. The LAN listener already refuses every source outside the private ranges and the operator's list before sign-in (`lan.go`); behind `tailscale serve` or a proxy every request arrives from loopback, so such a list would decide by a header the proxy wrote. The key is the check that holds on every route.

What nothing stands in for: with this on, the digits no longer show that the person typing them is at the master. They still have to come from the machine's own screen, and the form says so. Leave it off on a master you can sit at.


A tap on a component's button or a form's Send counts as the operator speaking: it starts a turn like a typed message, and never asks first. It is still a message the model proposed, so the bubble says "via choices" or "via form", and a component can never send a slash command.

## 6. States

Every page shows each of these that can happen to it, and a page test covers each (a states table in the page's own test file).

| State | How |
|---|---|
| off | the control is nil: `ui-off`, "<Feature> isn't available on this Tenant." |
| empty | `ui-empty`: what will appear here, and how to make it appear |
| loading | `.loading`, only where a result arrives after the page: the Quality check run, Research, connecting or testing a remote service, and Model's server check (shown while its post is out: `data-indicator:_checking` on the form, `data-show` on the text). Every other page renders complete. |
| error | `ui-flash` with `?err=` (`role=alert`); on a page whose forms don't reload (§8), an error toast |
| success | `ui-flash` with `?msg=` (`role=status`); on a page whose forms don't reload (§8), a toast |
| busy | the status strip on every page, and `ui-busy` where a page has its own work (Research) |

**Errors (D21).** A handler shows a control's error only when it is a `PlainError`, which an adapter returns for the failures it can phrase ("A helper named X already exists."). Anything else shows the action's fixed plain message, and the raw error goes to the log:

```go
redirectFlash(w, r, "/agents", "", s.flashFor(r.Context(), "helper add", err, "Couldn't add that helper."))
```

Secret routes never put a control's error in a URL.

**Logging (D29).** New dashboard code never calls `s.log.Warn` or `s.log.Error` and adds no events: it reuses `ActionFailed` (2201, through `flashFor`) and `RenderFailed` (3001).

**The status strip.** `layout.html` holds `<div class="status-wrap" data-init="@get('/ui/status')" data-on-interval__duration.20s="@get('/ui/status')">`, which is never patched, around `#ui-status`. `statusView` fills it from the controls on each request: this dashboard's chat (Stop), the approval count, and the new-safety-check notices. The queued ("#N in line", Cancel) and "busy with" lines read serve's turn gate, but today only the dashboard's own chat enters it: scheduled jobs, Discord, iMessage and alert investigations run their own agents alongside and never block Chat, and the terminal app has no gate. Those two lines are future work (ticket: one turn gate for every surface); until then only "Your agent is answering a message" can show. While busy the strip polls itself every 3s; idle, it is exactly `<div id="ui-status" class="status-strip"></div>` and CSS hides it. The poll is left out of request logging.

## 7. Mobile

- Every page works at 390px wide: no sideways scroll of the page, the sidebar or the menu, and every visible control ends inside the screen (`TestUIShellInChrome`, with long names, long URLs and many rows).
- Under 640px the nav is a drawer (the Popover API: Menu opens it, Esc or a tap outside closes it, Log out is at its bottom). The grid is `100dvh`, and the gutter is 16px.
- Long names and URLs break anywhere (`.page{overflow-wrap:anywhere}`). Only these inner scrollers may be wider than their box, and they scroll inside it: `.table-wrap`, `.lg-grid`, `pre`, `.diff`, `.q-detail`, `.bubble.md table`, `.tool-io`.
- Wide tables go in `<div class="table-wrap">`; the approval queue and the mode rows are cards.
- On touch screens buttons, nav links, the menu button and confirm summaries are at least 44px tall.
- Under 640px an open `.confirm-box` becomes a bottom sheet, 16px from the sides and the bottom. While a step is open the page gets half a screen of room at its end, so the last step's Cancel can be scrolled above its sheet.

## 8. Datastar

- Datastar (`/datastar.js`) is the only script. No hand-written JS, no inline `<script>`, no `on*=` attributes (`TestTemplates_NoInlineJS`). The browser enforces it: every answer carries `Content-Security-Policy: frame-ancestors 'none'; script-src 'self' 'unsafe-eval'; object-src 'none'; base-uri 'none'` (D31; Datastar evaluates its attributes, hence `unsafe-eval`) and `X-Content-Type-Options: nosniff`. An inline script, an `on*=` handler or a `javascript:` link doesn't run (`TestCSPInChrome`).
- Use the colon syntax: `data-on:click`, `data-bind:text` (`TestTemplates_NoDeadHyphenDatastarAttrs`).
- A `data-init` or `data-on-interval` that opens a GET goes only on a wrapper that is never patched; otherwise every patch opens another request. The one exception is the busy strip's own 3s poll, a one-shot GET.
- Stream openers use `{retryMaxCount:1000000, retryMaxWait:5000}` (Research adds `openWhenHidden:true`).
- A stream whose loss the operator must see (the approval queue, Research) shows the paused banner. The opener's own fetch events drive it, not `data-indicator`, which stays on through every retry of a dropped stream:

  ```html
  <div data-signals:_live="true" data-on:datastar-fetch="evt.detail.el === el && ($_live = !['error', 'retrying', 'retries-failed', 'finished'].includes(evt.detail.type))" data-init="@get('/events?only=approvals', {…})">…</div>
  <p class="banner warn live-off" style="display:none" data-show="$_live === false">Live updates paused. Reload the page to see new requests.</p>
  ```

  A dropped connection, a refusal or the stream ending shows it at once, and the stream's next patch hides it. So such a stream sends a patch as soon as it opens (a snapshot); otherwise a reconnect leaves the banner up. `TestLiveBannerInChrome` checks it.
- A Datastar request answered 401 means the sign-in has gone stale (the key changed, or another Tenant on this host signed the browser in): the status wrapper in `layout.html` sends the page to the sign-in form, which brings it back. One refused request, not one every 20s in the Security log.
- Buttons that send a decision carry `data-indicator:_sending` and `data-attr:disabled="$_sending"`, so a double tap sends once.
- Patches are outer `datastar-patch-elements` (the toast's is `replace`), keyed on element IDs. The IDs are a contract: `#ui-status`, `#toast`, `#approval-queue` (items `#q-<id>`), `#approval-note`, `#research-live`, `#research-history`, Chat's `#chat-cur` (the answer being written) and `#tool-<tag>-<turn>-<id>` (a tool call's card), the Logs page's `#lg-tree`, `#lg-props`, `#lg-clear`, `#lg-grid` (rows `#lg-rows`, paging `#lg-more`), `#lg-count`, `#lg-detail`, and the form regions listed under "Forms that don't reload". There are no new event kinds.
- **F1:** no text from a request or an agent goes into a Datastar attribute other than `data-on:*`, and there only numbers or URLs built with `url.Values.Encode` or `url.PathEscape`. The browser decodes the attribute before Datastar evaluates it, so a quote in it would be script. Untrusted text goes in element content, where html/template escapes it. `template.HTML` is used only through `reportHTML`, which escapes first.

### Chat

The Chat page (TEN-515) opens with the conversation so far: the last 20 exchanges of the agent's working set (`SetTranscript`), each message you sent, the answers, the tool calls as settled cards, and a dim row where compaction left a summary. Then `/events` streams:

- A stream opens with a sweep: it removes `#chat-cur`, `#chat-thinking` and every `details.tool.running` a previous connection left on the page (Datastar reconnects in place, so a reconnect is a fresh stream over the old elements). Each is a no-op on a fresh page.
- The answer being written is one bubble, `#chat-cur`, appended on the first token and morphed by id at most every 100 ms (`streamEvery`) with the plain text so far; a fast model can't flood the page or fill the broker's buffer behind a slow browser. The final answer replaces it (`mode replace`) rendered from Markdown through `internal/md`: headings, lists, code, tables and task boxes. HTML inside the answer is text (the copy has no raw-HTML parser) and script links are dropped; the text goes through `visibleText` first. A streaming answer renders as plain text until it is complete, so a half-written list or fence never flickers. Text written before a tool call settles as its own bubble, so the card sits where the call happened.
- A tool call is a native `<details class="tool">` card, `#tool-<tag>-<turn>-<id>` (the connection's tag, its turn count, and the model's call id or one the stream makes up and matches by name, so a reused id or a reconnect over a page that still holds the old cards never doubles an id; a transcript rendered on load uses `#tool-<id>`): the summary is the name, a short argument and a pill (`running…`, `ok 1.2s`, `error`); the body is what was asked and what came back, capped, through `visibleText`, with "open in Logs" filtered by the message's trace. A card carries `running` until its result arrives; the result morphs the card by id, and a result whose call this connection never saw (the broker dropped it, or the page opened mid-turn) is appended on its own. A failed call is open. No script: the expander is the browser's.
- `.bubble.md table` scrolls inside its box like `.table-wrap`.

### Forms that don't reload

A form post answered with `redirectFlash` reloads the page, which jumps back to the top. Every page's forms post in place instead: the page stays where it was, the row shows its new state, and a toast says what happened. A form keeps a full navigation only where going to another page is the point, or where the answer can't be shown in place:

- Logs: "Delete this view" (the page it's on is the view it deletes, so it goes to the Summary), and a plain post of "Save as view", which opens the new view.
- Memory: "Keep which fact?" (the picker's state is in the URL, which the answer has to clear), and Edit a fact and "Make this current again" (each makes a new version, and its page is the next step).
- Research, on a run's own page: Run again and Delete (both end on `/research`).
- Fleet, on a machine's own page: both Revoke steps and "Delete its record" (the page it's on can be the machine it deletes, so the answer is the list).
- Remote services: Connect and Reconnect (the connection is made after the reply, the instruction to approve the sign-in on the computer running Tenant has to stay on screen, and the reload clears an address that can hold a key).
- Not converted yet: Tools' "Turn on all / Turn off all" (its URL names the plugin; it needs a fixed route with the plugin as a field), a tool's own on/off (`toolrow.html`, a button whose URL names the tool; it patches its own `#tool-<name>` row and has no throttle, and needs a fixed route with the name and on/off as fields) and the status strip's Stop (`ui-busy`; Autopilot writes its busy line out so its own Stop posts in place).
- Sign in and Log out.

The rules:

- The form keeps `method="POST" action="…"`, so it still works before Datastar loads, and adds `data-on:submit__prevent__throttle.1s="@post('/access/discord/permission', {contentType:'form', retryMaxCount:0})"`. The URL is a literal (F1), and a row's values are form fields, never part of the expression. Datastar sends the form's fields form-encoded, with the `Origin` header a browser always sends on a POST, so `secure()`'s checks are unchanged. A confirm step opts in with `inline`: `{{template "ui-confirm" (inline (confirmGo "/access/discord/permission" "Allow" …))}}`.
  - `throttle.1s`: the morph reuses a row's form for the row that moves into its place, so without it a double tap on Remove removes two people.
  - `retryMaxCount:0`: otherwise Datastar sends a post again after a dropped connection, up to 10 times over about 4 minutes, and a change could land minutes after the operator gave up on it. A button that posts (a tool's on/off, the approval queue's decisions, Chat's Stop) carries it too, or the tap guard would stay up through every retry.
- **No form field is named `id`.** An input named `id` hides the form's own `id` from Datastar's morph, which then replaces the form and resets its throttle. Scheduled jobs send `job`, Research `run`, Memory `fact` and `item`, keys `service`, integrations `integration` (`TestRendered_NoFieldNamedID`). An integration's fields come from the settings catalog, so none may be named `id` there either.
- The throttle holds only while the morph keeps the same form under the finger:
  - A row a form changes has no `id` (Skills, Tools), so the morph matches rows by position and the next row reuses the form.
  - A container gets an `id` when something appears above it after a change (a fact's `#fact-actions`), so the morph doesn't match the wrong one.
  - Two buttons that take turns on one spot are one form on one route (a fact's Remove and Restore, `/memory/facts/removal`; No longer true and It's true again, `/memory/facts/valid`).
- The handler answers with `formDone` wherever it would call `redirectFlash`:

  ```go
  func (s *Server) formDone(w http.ResponseWriter, r *http.Request, page, msg, errMsg string, region func() (string, error))
  ```

  A plain POST still gets the 303 and its flash. A Datastar POST gets three patches: `#toast` (mode `replace`, so a new toast starts its fade again), `#ui-status`, and `region()`, the page's changed part rendered from a `{{define}}` with a stable ID (`access-body` renders `#access-body`), which Datastar morphs. The scroll, the focus and text typed in an input stay; an open confirm step closes, because the new HTML has no `open`. An error changed nothing, so it leaves `region()` out: an open step stays open with what was typed in it. Errors still go through `flashFor`. The region holds the page's `ui-flash`, so a banner from a plain post goes with the first in-place answer.
  - `formWarn(w, r, page, warn, region)` is for a change that happened but needs the operator's eye (the model switched, but it can't answer yet): an alert toast, and the region, because it changed.
  - `createDone` (Helpers' Add, Research's start, Model's server add) replaces the region whole instead of morphing it, so the form comes back empty. `createWarn` is the same with an alert toast: a server added whose tool calls weren't confirmed.
- The regions, each a `{{define}}` of the same name: Access `#access-body`; Model `#models-body`; Provider keys `#keys-body`; Integrations `#integrations-body`; Skills `#skills-body`; Remote services `#mcp-body`; Tools `#tools-body` (a tool's own toggle patches its `#tool-<name>` row); Scheduled jobs `#jobs-flash`, `#jobs-add` (after an add, with a new form ID so its boxes are empty) and `#jobs-body`; Quality `#eval-body`; Memory `#mem-soul-body`, `#mem-facts-body`, `#mem-removed-body` (a Restore answers with its row alone, `#removed-<id>`, so the list doesn't get shorter under the finger) and `#mem-fact-body`; Approvals & safety `#safety-modes`; Helpers `#agents-body`; Research `#research-ask`, `#research-live` and `#research-history`; Autopilot `#goal-body`; Fleet `#fleet-body` (the list, or one machine's page); Logs below.
- A region may be several elements, each with its ID, when a change touches parts that sit apart. Logs: saving a view redraws `#lg-tree`; a log's properties redraw `#lg-props` and `#lg-clear`, which sit inside their `<details>`, so an open section stays open; a clear also redraws `#lg-tree`, `#lg-grid`, `#lg-count` (counted now, not the page's `data-init`) and `#lg-detail`. A form carries what its answer needs to draw them (the log, the view and the grid's filter) as fields. Keep an element that opens a GET on `data-init` out of a region.
- A form that holds a key (Model's "Add a cloud model" and "Add a model from a server on your network", a key's Add or Replace, an integration's Save) empties its key boxes when the reply arrives, after an error too, so a typed key never stays on the page; what else was typed stays. The server never puts a key into an input:

  ```html
  data-on:datastar-fetch="evt.detail.el === el && evt.detail.type === 'finished' && el.querySelectorAll('input[type=password]').forEach(i => { i.value = '' })"
  ```
- **The tap guard.** A double tap's second tap can land on what the answer moved under the finger: the next row, the page under a phone's confirm sheet that just closed, the Turn on step where Turn off was. That is a different form, which its throttle doesn't cover. So `layout.html`'s `.app` carries one guard for every page: from the moment a form with an `action`, or a button, posts (`datastar-fetch` `started`) until its answer is 400ms old (`finished`), a capture-phase click filter drops every tap in `.app`. A tap that opens a confirm step starts it for 400ms too: on a phone the sheet opens under the finger, and a double tap would otherwise press its Yes.
  - An open confirm step's Cancel still works, and the toasts, `#toast-fail` included, sit outside `.app`.
  - Datastar sends `finished` from a `finally` block, and no post retries, so an error, a refusal or a lost connection ends it at once.
  - The chat box (a form with no `action`) and the GET streams don't start it. Without JavaScript there is none.
  - The per-form throttle and `retryMaxCount:0` stay as a second line.
  - A browser test that taps soon after an answer, or right after it opens a step, waits it out first (`tapsOpen`).
- The toast is `ui-toast` in `ui.html`, patched into `#toast` inside `layout.html`'s never-patched `.toast-wrap`, which is `aria-live="polite"`: a screen reader reads a change inside a live region that was already there, not a new element's own `role=status`. Success fades after about 3 seconds (CSS only). An error is `role=alert` and stays until its close button or the next toast. It sits top right on a desktop, and across the top on a phone, left of Menu, clear of the rows the thumb works on.
- Datastar patches nothing for a reply that isn't 200 (a 403, a 428 from a stale page, a 500) or for a dropped connection. `.toast-wrap` watches the posts of forms and buttons and shows `#toast-fail`, a fixed "That didn't go through. Reload the page and try again.", until the next post starts or it's closed. `needConfirm` still answers 428 without `confirm=yes`, so a stale page gets that alert, never the change.
- The Activity page is gone: the Logs page replaced it. `GET /activity` redirects to Logs with its filter (`?trace=X` becomes `trace:X`, `?source=` `log:`, `?level=error` or `warn` `level>=error` or `level>=warning`). `GET /api/activity` stays, for `tenant attach`.
- Tests: per page, a handler test that a Datastar POST gets the three patches and a plain one the 303 (`TestAccess_InlineAnswersEveryForm` and its siblings), and a browser test that the scroll and the page survive and a double tap sends once (`TestAccessInlineFormsInChrome`, which also covers `#toast-fail` on a stale page). `TestTapGuardInChrome` shows the guard alone stops a double tap that crosses rows on Access or presses the Yes of the sheet it opened, that Cancel and the toast's close still work while it is up, and that a lost connection, for a form or a button, shows `#toast-fail` and ends it. Logs: `TestLogs_InlineForms` and `TestLogsInlineFormsInChrome`.

### Another computer's words (Fleet, TEN-533)

What a fleet machine reports about itself (its system, the account it runs as, its version, sandbox and limit), the address it connected from and why its link ended are another computer's text. They reach the page already escaped by the control: printable ASCII, at most 64 characters, in quotes the text cannot close (`fleetQuote` in `cmd/tenant`, the same rule as the terminal's screens). The page shows them:

- in element content only, in a `.fl-said` cell, never in an attribute or a Datastar expression (F1);
- under words that say whose they are: the list's starred columns and their footnote, and the machine page's "Reported by the machine" card.

A request for an action on a machine is titled "On the machine "<name>": your agent wants to …", gives the machine's id on its second line, and offers no session grant. The name there is the label the broker escaped (`control.PendingApproval.HostName`).

A machine is named in a form by its id alone, in a field called `machine`, and a handler uses it only once it is one of the fleet's.

**Where a machine may be confirmed.** `POST /fleet/confirm` is the one dashboard route that works only for a browser signed in on the computer running Tenant (`atThisComputer`): a loopback peer address, a loopback name in `Host`, no proxy header (`Forwarded`, `Via`, `X-Forwarded-*`, `X-Real-*`, `Tailscale-*`, `Cf-*`), and the session cookie, never the API key. Anything else gets 403 and a Security record (4324); the page shows such a browser where to confirm instead of the form. Attempts are taken one at a time, and after five failures for one machine each further one doubles a wait, up to a minute, as sign-in does per address. There is no `/api` row that changes the fleet.

## 9. A new page

1. Go: an interface for its control, render-ready view structs, `SetX`, and `mountXSSR` called from `routes()`. Handlers are nil-safe: a nil control renders `ui-off`.
2. A template with `{{define "content"}}` that uses `ui-flash`, `ui-off`, `ui-empty`, and `ui-confirm` for every action §5 covers.
3. A nav line in its D15 slot in `layout.html`, a title equal to its label, and a `.Page` key.
4. Mutations are form POSTs answered in place with `formDone` (§8; `redirectFlash` only where a full navigation is the point), each dangerous one wrapped in `needConfirm`, its errors through `flashFor`.
5. Styles only inside the page's marker block at the end of `styles.css`, using the classes above first.
6. Tests: register a fake from an `init()` (`harnessFakes`, `harnessPages` in `render_all_test.go`), a states table, the confirm rows, and the page passes the plain-language and 390px sweeps unchanged.

## 10. Assets and caching

- `layout.html` links the stylesheet and Datastar by content hash, `{{stylesURL}}` and `{{datastarURL}}` (`/styles.css?v=<16 hex of SHA-256>`), computed once at startup (`newAsset` in `ssr.go`). The hashed URL is `Cache-Control: private, max-age=31536000, immutable`, so a browser fetches each once per build: the second page takes 0 bytes of them (`TestAssetsCachedInChrome`). A new build's bytes get a new hash.
- Any other URL (no `?v=`, or an old one from a page opened before a restart) is `private, no-cache` and revalidates: its `ETag` gets a 304 while the bytes are the same.
- `private`: the assets need the sign-in. The sign-in page links neither; it carries its own `<style>`.
- Each asset is gzipped once at startup and sent with `Content-Encoding: gzip` and `Vary: Accept-Encoding` to a client that takes gzip, as is otherwise. The two have different ETags.
- Nothing else is compressed. A page mixes the operator's secrets with text a request put there, so compressing it leaks the secrets by size (BREACH), and gzip would buffer the streams.
