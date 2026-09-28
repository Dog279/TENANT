# Dashboard design guide

How the web dashboard looks, talks and behaves (TEN-211). Every page follows it; `ui_test.go` and `ui_browser_test.go` enforce what can be checked. The shared pieces live in `ui.go`, `templates/ui.html` and the `TEN-211 design system` block of `templates/styles.css`.

## 1. Who it's for

The operator is not a developer. They run Tenant on a home computer or a Windows server, and they open the dashboard on a laptop or a phone to answer five questions. Each question has a page that answers it, and the status strip at the top of every page answers the urgent ones without a click.

| Question | Where it's answered |
|---|---|
| Is my agent healthy? | Overview; the status strip (busy, queued, waiting) |
| What has it learned, and is it getting better? | Memory, Skills, Quality, Research |
| What can it do, and who can reach it? | Tools, Helpers, Remote services, Integrations, Access |
| Is anything waiting for my OK? | Approvals & safety; the strip's "N actions are waiting for your OK" on every page |
| Can I change this safely? | The confirm step and its consequence line (§5) |

## 2. Voice

- Say what happens, to whom, in everyday words. "Your agent can no longer use X", not "Provider removed".
- The consequence comes before the action: the confirm step says what changes, then offers "Yes, …".
- Say "the computer running Tenant", "your agent", "your main model", "helper", "instructions", "step", "Autopilot".
- Name safety categories by their title (`PermissionInfo.Title`, from `approval.CategoryPlain`): Run commands, Change files, Irreversible actions, Act on websites, Send messages, Share private data, Connect remote services, Leave the sandbox.
- A page's title is its nav label.
- No uppercase warnings, no emoji status markers. Use a pill (`.risk`, `.on-tag`, `.tag-new`) instead.

**Banned on pages** (checked on templates and on rendered pages by `TestTemplates_PlainLanguage` and `TestRendered_PlainLanguage`; the Logs page's filter language is exempt):

| Don't say | Say |
|---|---|
| soul | personality, instructions |
| compaction, working set | summaries |
| superseded | replaced |
| turn | message, conversation, step |
| posture, Read-only / Allow-send | What your agent may do: Look only / Look and act |
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

`ui-status` is the strip itself; only `/ui/status` renders it. Loading is a class, not a partial: `<span class="loading">Counting</span>`.

Text an agent or a request wrote goes through `visibleText` before it is shown (`{{visibleText .Detail}}`): characters that draw nothing or reorder text (controls, bidi and zero-width marks, tag characters, fillers, variation selectors) show as `⟨U+202E⟩`, and 3 or more blank lines as `⟨N blank lines⟩`. `waited` turns seconds into "waiting 3 min".

## 5. When to confirm

An action gets a confirm step when it:

- (a) loses something the page can't bring back: delete, remove, clear, or disconnect a service whose sign-in you'd have to redo;
- (b) cuts everyone off a channel;
- (c) lets the agent, or another person, do more without asking;
- (d) changes every answer.

No confirm on Deny, Stop, "Ask me again", tightening a mode, or "Keep them at Ask": friction goes on "go", never on "stop".

The step is the `ui-confirm` partial: a native `<details>` whose summary is the first button (it flips to Cancel), a consequence sentence, and a form that posts `confirm=yes`. It needs no JavaScript. Build it with `confirm` for (a) and (b), which is red (§3), or `confirmGo` for (c) and (d), which isn't. The server enforces it where the route is mounted:

```go
mux.HandleFunc("POST /models/remove", needConfirm(nil, s.handleModelRemoveForm))                      // always
mux.HandleFunc("POST /safety/mode", needConfirm(confirmUnless("mode", "ask", "deny"), s.handleMode)) // only when loosening
```

- Without `confirm=yes` (read with `r.FormValue`), the answer is 428 "Nothing changed: that needs a second click to confirm." and the handler never runs.
- `confirmUnless(key, safe…)` is fail-closed: the step is skipped only when the trimmed, lowercased value is one of the safe values. `ALLOW`, `" allow"`, `on`, `always`, `yes`, `1` or a missing field all confirm. A handler that sets a mode still rejects anything outside exactly ask, allow and deny.
- The gate runs before the handler's own checks, so with no model control wired, `POST /models/use` without `confirm=yes` is now 428 where it used to be a 303 to the page.
- Confirm is friction, not security. The session cookie, `checkOrigin` and `checkCSRF` are the boundary. `/api/*` isn't gated: a JSON client acts deliberately.

Every confirmed route is listed in plan §2.1 and checked by `TestNeedConfirm` and the route inventory.

## 6. States

Every page shows each of these that can happen to it, and a page test covers each (a states table in the page's own test file).

| State | How |
|---|---|
| off | the control is nil: `ui-off`, "<Feature> isn't available on this Tenant." |
| empty | `ui-empty`: what will appear here, and how to make it appear |
| loading | `.loading`, only where a result arrives after the page: the Quality check run, Research, and connecting or testing a remote service. Every other page renders complete. |
| error | `ui-flash` with `?err=` (`role=alert`) |
| success | `ui-flash` with `?msg=` (`role=status`) |
| busy | the status strip on every page, and `ui-busy` where a page has its own work (Research) |

**Errors (D21).** A handler shows a control's error only when it is a `PlainError`, which an adapter returns for the failures it can phrase ("A helper named X already exists."). Anything else shows the action's fixed plain message, and the raw error goes to the log:

```go
redirectFlash(w, r, "/agents", "", s.flashFor(r.Context(), "helper add", err, "Couldn't add that helper."))
```

Secret routes never put a control's error in a URL.

**Logging (D29).** New dashboard code never calls `s.log.Warn` or `s.log.Error` and adds no events: it reuses `ActionFailed` (2201, through `flashFor`) and `RenderFailed` (3001).

**The status strip.** `layout.html` holds `<div class="status-wrap" data-init="@get('/ui/status')" data-on-interval__duration.20s="@get('/ui/status')">`, which is never patched, around `#ui-status`. `statusView` fills it from the controls on each request: this dashboard's chat (Stop), a message queued behind the serve turn gate (Cancel, its place in line), what the gate is busy with, the approval count, and the new-safety-check notices. While busy the strip polls itself every 3s; idle, it is exactly `<div id="ui-status" class="status-strip"></div>` and CSS hides it. The poll is left out of request logging.

## 7. Mobile

- Every page works at 390px wide: no sideways scroll of the page, the sidebar or the menu, and every visible control ends inside the screen (`TestUIShellInChrome`, with long names, long URLs and many rows).
- Under 640px the nav is a drawer (the Popover API: Menu opens it, Esc or a tap outside closes it, Log out is at its bottom). The grid is `100dvh`, and the gutter is 16px.
- Long names and URLs break anywhere (`.page{overflow-wrap:anywhere}`). Only these inner scrollers may be wider than their box, and they scroll inside it: `.table-wrap`, `.lg-grid`, `pre`, `.diff`, `.q-detail`.
- Wide tables go in `<div class="table-wrap">`; the approval queue and the mode rows are cards.
- On touch screens buttons, nav links, the menu button and confirm summaries are at least 44px tall.
- Under 640px an open `.confirm-box` becomes a bottom sheet, 16px from the sides and the bottom.

## 8. Datastar

- Datastar (`/datastar.js`) is the only script. No hand-written JS, no inline `<script>`, no `on*=` attributes (`TestTemplates_NoInlineJS`).
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
- Patches are outer `datastar-patch-elements`, keyed on element IDs. The IDs are a contract: `#ui-status`, `#approval-queue` (items `#q-<id>`), `#approval-note`, `#research-live`, `#research-history`. There are no new event kinds.
- **F1:** no text from a request or an agent goes into a Datastar attribute other than `data-on:*`, and there only numbers or URLs built with `url.Values.Encode` or `url.PathEscape`. The browser decodes the attribute before Datastar evaluates it, so a quote in it would be script. Untrusted text goes in element content, where html/template escapes it. `template.HTML` is used only through `reportHTML`, which escapes first.

## 9. A new page

1. Go: an interface for its control, render-ready view structs, `SetX`, and `mountXSSR` called from `routes()`. Handlers are nil-safe: a nil control renders `ui-off`.
2. A template with `{{define "content"}}` that uses `ui-flash`, `ui-off`, `ui-empty`, and `ui-confirm` for every action §5 covers.
3. A nav line in its D15 slot in `layout.html`, a title equal to its label, and a `.Page` key.
4. Mutations are form POSTs answered with `redirectFlash`, each dangerous one wrapped in `needConfirm`, its errors through `flashFor`.
5. Styles only inside the page's marker block at the end of `styles.css`, using the classes above first.
6. Tests: register a fake from an `init()` (`harnessFakes`, `harnessPages` in `render_all_test.go`), a states table, the confirm rows, and the page passes the plain-language and 390px sweeps unchanged.
