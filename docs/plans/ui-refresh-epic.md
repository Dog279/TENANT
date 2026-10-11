# UI refresh: a readable terminal app and an Intelligent-UI dashboard chat

Status: Epic, 2026-10-11. Jira Workstream [TEN-512](https://findtime.atlassian.net/browse/TEN-512) (To Do). Decisions D1, D3, D5 and D6 are made; D2 and D4 are open and explained below.

## Why

Tenant's two local surfaces show the same event stream (`agent.Event`: token, tool_call, tool_result, thinking and thought, approval, memory, final) and neither makes it easy to read. Claude Code, Charm's Crush, OpenCode and Codex CLI all read better, and OpenAI's ChatGPT now answers with components instead of prose when that fits.

**The terminal app (`internal/tui`, `tui.go` is 3.2k lines):**

- The answer is raw text. Markdown the model writes (headings, lists, fenced code, tables) shows as literal `#`, `*`, backticks and pipes. There is no renderer in `go.mod` (bubbletea, bubbles, lipgloss only).
- `wrap` and `wrapLine` break at the column, not the word, so words split mid-word on every long line.
- A turn is `you ` + text, `tenant ` + text, blank line. A long streamed answer is one undifferentiated block: no paragraph spacing, no section marks, no rule between turns. This is the "token chunks" problem: the stream lands as one slab.
- Tool calls never appear in the conversation. They live only in the feed, clipped to 60 characters of arguments and 70 of result, and nothing opens one. The one-third-width feed is mostly one dim colour.
- What works and stays: the 33 slash commands and the registry that drives `/help` (`cmd_registry.go`); the live activity feed and its lines (route, ctx assembled, `→ tool` / `✓ result`, `thought for Ns`, the turn summary, `cron:` and `[sub-agent]` prefixes); approval cards; interject while running; the timer, in/out token counter and context gauge; Ctrl+V images; the `/logs` viewer.

**The dashboard chat (`internal/dashboard/ssr_chat.go`, `templates/chat.html`):**

- Nothing streams. The page shows `thinking… 12s`, then the whole answer at once (`chatBubble` renders final, truncated and stuck only; token deltas are dropped by design).
- Bubbles are `white-space: pre-wrap` text: no markdown.
- Tool calls are inline monospace strips clipped at 120 and 200 characters, not collapsible.
- Leaving the page clears the transcript, and opening it shows nothing of the conversation so far.
- No activity view beside the chat (the Logs page replaced the Activity page).

**What the field did meanwhile.** OpenAI shipped Intelligent UI in ChatGPT on 2026-10-07: GPT-6 composes each answer from text, visuals and interactive parts, chosen per question, from a library of native streamable components compiled while the model generates. CopilotKit's OpenIntelligentUI (MIT) is the open reference: a router picks plain text, a native table (A2UI), or a generated chart, diagram, calculator or map (Open Generative UI); generated UI runs in a sandboxed iframe with a two-call host bridge (`sendPrompt`, `openLink`); a click inside a component starts a new agent turn carrying the selected values; each answer's UI is its own snapshot and is never patched later.

## Goal

Both surfaces read like a well-typeset document while the agent works: paragraphs, headings, lists, code and tables rendered; tool work folded into one-line rows you can open; the feed intact. The dashboard chat streams, keeps its transcript, and can answer with a component (table, steps, form, choices, and more) when that reads better than prose, with no JavaScript beyond Datastar and no change to the security posture.

**Keep:** the slash commands and `/help`; the activity feed; the dashboard design system (`internal/dashboard/DESIGN.md`: plain words, dark palette, Datastar as the only script, the CSP, confirm steps); approvals on both surfaces.

## Non-goals

- A JavaScript frontend (React, Next, CopilotKit). Tenant stays server-rendered plus Datastar.
- A second classifier model (OpenIntelligentUI's Jev). The main model picks a component by calling a tool.
- A new Go module. The markdown engine is an in-house copy (D1).
- New channels, voice, a desktop app.
- Diagrams and maps in v1.
- Syntax highlighting in v1 (a code block is a dim panel).

## The markdown engine (D1, decided: an in-house copy of goldmark)

One engine for both surfaces, under `internal/md`, with no module added to `go.mod`:

- `internal/md/goldmark/` is a copy of yuin/goldmark v2 at commit `cbf81e9` (2026-10-01), MIT. Its `LICENSE` stays beside the code, a `README.md` there pins the upstream commit and lists what was removed, and `README.md` at the repository root credits it. A future upstream fix is a diff against that commit. goldmark imports nothing outside the standard library (its `go.mod` has no requires), so the copy brings nothing else in.
- **Kept** (about 12k lines): `ast` (2.0k), `text` (2.1k), `util` (1.5k), `parser` (5.5k before the cuts below), `renderer` (0.4k), `renderer/html` (1.0k), the table extension (0.5k) with its AST nodes, and the task-list extension (0.3k), which gives `- [ ]` and `- [x]` for free.
- **Removed:** footnotes, definition lists, typographer, linkify, the GFM bundle, the `html_block` and `raw_html` parsers (540 lines), and the `WithUnsafe` and XHTML options. With the raw-HTML parsers gone, HTML in a model's answer is text, on both surfaces, by construction, not by option.
- **Added:** `internal/tui/markdown.go`, a terminal renderer over the same tree (about 300 lines): width, theme, hanging indents for lists, code panels kept whole, tables through `lipgloss/table` (already in the module cache). It lives in the terminal UI because only that package may draw with Charm libraries (`internal/archtest`). And `internal/md/md.go`, the facade: `Parse` for the tree and `HTML` for the dashboard.
- **Tests:** upstream's tests for the kept packages come with the copy; the ANSI renderer gets `View()` snapshots (A7).
- The copy and the ANSI renderer land in PR 1 with the terminal app. PR 2 only calls the HTML side.

## Part A: the terminal app

**A1. Markdown in the chat pane.** Headings, bold and italic, bullet and numbered lists with hanging indents, fenced code as a panel (left rule, dim background, kept whole across wraps), tables, blockquotes, links shown as text plus a dim URL. Word wrap at the pane width (never mid-word; code and URLs are the exception). While streaming, completed paragraphs render as they close and the tail stays plain text, so the renderer never re-renders the whole answer per token.

**A2. Turn blocks.** Each exchange is a block: a one-glyph gutter (`●` you, `●` tenant, like Claude Code's `⏺`), a dim time stamp and the model name on the header line, a rule between turns. The in-flight state lives in the block (`thinking… 8s`, `web_search…`) as well as in the footer timer. The final answer is visibly distinct from the streaming state.

**A3. Tool rows in the conversation.** A tool call is a collapsed one-line row inside its turn block: `⏺ web_search("tenant docs") ✓ 1.2s · 3 results`. Ctrl+O toggles the current turn's rows open (arguments and the result, paged), the way Claude Code's verbose toggle does. Errors stay open. The feed keeps the full chronological list; nothing is removed from it. The `/logs` detail view is the pattern for the paged result.

**A4. Feed readability.** Colour by kind (tool, ok, error, system, dim exist already), a turn-start header line with the time and the first words of the question, the turn's lines indented under it, the two `ctx assembled` lines folded into one (the static breakdown stays in the log book if it is recorded there; otherwise it goes behind `/feed verbose`). Relative time stamps. Cron and sub-agent prefixes unchanged.

**A5. Layout (D3, decided).** The 2/3 + 1/3 split stays. `/feed off|on` hides and shows the feed for a wider reading column, and Alt+F does the same (Ctrl+F is the textarea's cursor key); the choice persists like `/mouse`. Everything in the footer stays.

**A6. One theme.** The scattered styles (`cDim`, `cTool`, `cOK`, `cErr`, `cSys`, `cUser`, `cAgent`, `feedTitleStyle`) become one palette struct aligned to the dashboard's meanings: green is on or fine, amber needs you, red is an error, cyan is primary. Light-background terminals get a readable variant (lipgloss can detect the background).

**A7. Tests.** `View()` snapshots with ANSI stripped for a markdown answer, a turn with tool rows closed and open, and the feed grouping. The existing `tui_test.go` and `thinkfeed_test.go` patterns; no new test framework.

## Part B: the dashboard chat

**B1. Stream the answer.** The first token appends a bubble with a stable id; later tokens patch it, coalesced to about ten patches a second (the broker drops events for a slow subscriber, so per-token patches are a risk). Plain text while streaming, rendered markdown at final, as in A1. The `thinking…` row stays.

**B2. Markdown in bubbles.** Server-side rendering to HTML through `internal/md`, every text node escaped, no raw HTML (the parsers for it are gone). Code blocks are `pre` panels, tables go in `.table-wrap`, both already allowed to scroll inside their box.

**B3. Tool cards.** A tool call is a native `<details class="tool">`: the summary line is the name, a short argument, duration and a status pill; the body is the arguments and the result through `visibleText`, capped, with a link to the Logs page filtered by the turn's trace. No JavaScript: this is the `details.advanced` expander pattern DESIGN.md already uses. Errors render open.

**B4. Transcript on load (D5, decided).** Opening Chat shows the last 20 exchanges of the current conversation, rendered the same way, then the live stream continues. Needs one control method that returns the working set's recent exchanges (the terminal app holds them as `m.msgs`; serve holds the agent). Reload after a restart shows what the archive has.

**B5. Components, tier 1: the agent answers with a shape, Tenant draws it.** This is the Intelligent UI part, explained in plain words:

Today the model can only answer in prose. B5 gives it one more tool, `ui_render`, that says "show this as a table (or steps, or a form) with this data", and Tenant's own Go code draws it from a fixed template. The model never writes HTML or JavaScript; it fills in data. Ask "compare the three models on my server by speed and price" and the answer is a table instead of a paragraph of numbers. Ask "how do I put the dashboard on my tailnet" and the answer is a numbered checklist. Ask "split this bill" and the answer is a small form you fill in; Submit sends "Split $120 with 18% tip among 4" back to the agent as your next message, nothing more special than that. This is how OpenAI's version works too, as reported: a library of native components the model composes from.

The call rides the existing tool-call plumbing: the arguments are the component, the result is a one-line note for the model (`shown: table, 12 rows`), the archive keeps it, so the transcript re-renders it on load and the terminal app renders it too. One schema, two renderers. A component is a snapshot; a later turn never patches an earlier one. The model's contract is a short rule in the system prompt (use a component when the answer is a comparison, a table of more than three rows, a sequence of steps, or needs the operator's input; otherwise prose) plus the tool schema. Coverage goes through the eval harness (`internal/eval`) with a few prompts per kind. The tool is live-switchable like any built-in.

The seven kinds, each with an example and what it looks like on each surface:

| Kind | You ask | Dashboard | Terminal app |
|---|---|---|---|
| `table` | "Which models are configured and what do they cost?" | a real table (`.table-wrap`) | a boxed text table (`lipgloss/table`) |
| `compare` | "Discord relay or iMessage responder?" | two to four cards side by side, attributes down the side, the better value marked | aligned columns |
| `steps` | "Plan the backup and restore" | a numbered checklist with done and to-do marks | `✓` / `○` list |
| `stats` | "How is my agent doing today?" | a row of big-number tiles: tokens, turns, approvals waiting, uptime (`.cards`) | one line per tile |
| `chart` | "Tokens per day this week" | a bar or line chart as inline SVG drawn by Go, no chart library | block-character bars or a sparkline |
| `form` | "Split this bill" | inputs and a Submit that posts the filled message template to `/chat/send` | the existing `listPicker` and input prompt |
| `choices` | after a table of models | buttons for the likely next question: Show by price · Show by speed · Switch to the fastest; a tap sends that text as your message | numbered options you type from |

Field shapes: `table` has `columns[]`, `rows[][]`, `caption`, `source` (at most 12 columns and 200 rows, the caps A2UI uses); `compare` has `items[]` of name and attributes and an optional `best` per attribute; `steps` has `items[]` of title, detail, state; `stats` has `tiles[]` of label, value, delta; `chart` has `type` bar or line, `labels[]`, `series[]`; `form` has `fields[]` of text, number, select or range, `submit`, and a message template; `choices` has `options[]`.

**D4, open: which kinds go in the first components PR.** Options: (a) all seven; (b) four first, `table`, `steps`, `choices` and `form`, which need no drawing code and cover OpenAI's launch examples (comparisons, checklists, calculators), then `compare`, `stats` and `chart` in a second pass once the models' use of the first four has been seen; (c) a different subset. Recommendation: (b).

**B6. Components, tier 2: the model writes the page itself.** See D2 below. Nothing in B6 is built until D2 is decided.

**B7. Activity beside the chat (optional).** A `Show activity` toggle that reveals a rail of the same feed lines next to the chat, from the Logs page's live rows. The terminal app's feed is the one that must stay; this is the dashboard's equivalent if it costs little.

**B8. Mobile and tests.** Every component stacks at 390px; tables and charts scroll inside their box. Handler tests for the patches (`TestChat_*` beside `ssr_events_stream_test.go`), browser tests (chromedp) for streaming, a tool card opening, a `form` round trip and a `choices` tap, and the existing guards stay green: `TestTemplates_NoInlineJS`, `TestCSPInChrome`, the plain-language tests, `TestRouteInventory` for any new route.

## D2 explained: tier 2, custom sandboxed UI

**What it is.** Tier 1 (B5) is the model filling in forms Tenant designed. Tier 2 is the model writing the page: its own HTML, CSS and JavaScript for something no fixed template covers, which the dashboard then runs inside an iframe with the `sandbox` attribute. That is OpenIntelligentUI's `generateSandboxedUi`, and it is what makes their demos flashy: the 3D airplane with pitch, roll and yaw sliders, the coastal map with animated pins, a custom simulation. OpenAI's shipped Intelligent UI, as reported, is tier 1: a library of native components; it does not run model-written code in the browser.

**What it buys.** Anything the model can code: simulations, custom calculators, animations, small games, one-off visualisations. Unlimited variety, no template work on our side.

**What it costs.**

1. *Model-written code runs in the operator's browser.* The iframe sandbox gives it an opaque origin, so it cannot read the dashboard's cookies or page. Inside its frame it can still draw anything: a convincing "your session expired, sign in again" box in the middle of the chat, which a prompt injection in a web page the agent read could ask the model to draw. It can also try to reach the network unless its own CSP forbids that (`connect-src 'none'`, no remote images), which is how a frame could leak conversation text in a URL. This is a new attack surface in the surface Tenant calls its safest, and the competitive review scored security as Tenant's lead.
2. *It breaks the "Datastar is the only script" rule twice.* Once inside the frame (the model's JavaScript) and once on the dashboard page itself: a click inside the frame can only reach the chat through a host-side listener script, which DESIGN.md forbids today, or through a form posted from the frame's opaque origin, which `checkOrigin` would have to admit as an exception.
3. *Mechanics.* A `srcdoc` frame inherits the page's CSP, which forbids inline script, so each component needs its own route (`/ui/sandbox/<id>`) with its own CSP, a stored artifact, a size cap, a kill switch and Chrome tests. An L-size PR of its own plus a security review before it ships.
4. *Weaker models write worse front-end code.* On the local-model class Tenant runs, broken frames will be common, so a fallback to text is needed and the feature mostly pays off with the largest cloud models.
5. *Documentation and settings.* A `config.json` key (off by default), `reference/config.md`, `mcp/built-in-tools.md`, and a DESIGN.md exception.

**The options.**

- (a) **Never.** Tier 1 only. The chat answers with tables, comparisons, charts, steps, forms and buttons, all drawn by Tenant's code. No model-written code in the browser, no new attack surface, no exception to the script rule. The component set can still grow later with server-drawn kinds (a `diagram` as SVG, a `map` as static tiles) without crossing that line. This matches OpenAI's own approach.
- (b) **Later, behind a key, off by default.** Build tier 1, use it for a few weeks, then add tier 2 for operators who switch it on, with the sandbox CSP, the network block, the size cap and the kill switch. The decision can wait until tier 1 shows what is missing.
- (c) **Now, inside the components PR.** Doubles B5's size and puts the security review on the critical path.

Recommendation: (b), leaning (a). Decide after tier 1 has been in use.

## Decisions

| # | Decision | Answer (2026-10-11) |
|---|---|---|
| D1 | Markdown renderer | **Decided:** an in-house copy of goldmark, stripped, under `internal/md`; no module added. See "The markdown engine". |
| D2 | Tier 2 sandboxed custom UI | **Open.** Explained above; options (a) never, (b) later behind a key, (c) now. Recommendation (b), leaning (a). |
| D3 | Feed toggle | **Decided:** `/feed off|on` and Alt+F. |
| D4 | Component set for the first components PR | **Open.** Explained under B5; options (a) all seven, (b) `table`, `steps`, `choices`, `form` first. Recommendation (b). |
| D5 | Transcript on load | **Decided:** the last 20 exchanges. |
| D6 | PR shape | **Decided:** three PRs: A; B1 to B4; B5. B6 later, if ever, as its own PR. |

## Documentation that changes (CLAUDE.md table)

| Code | Page in findtime-net `tenant-docs/src/` |
|---|---|
| `internal/tui` rendering, keys, `/feed`, Alt+F, Ctrl+O | `first-launch/terminal-ui.md`; `reference/slash-commands.md` for `/feed` |
| `internal/dashboard` chat: streaming, cards, transcript, components | `dashboard/pages.md` (the Chat row), `dashboard/api.md` for any new route |
| `ui_render` built-in tool (and `ui_custom`, if D2 says so) | `mcp/built-in-tools.md` |
| a `config.json` key for tier 2, if D2 says so | `reference/config.md` |
| contributors | `internal/dashboard/DESIGN.md` (components, tool cards, streaming rules); `internal/md/goldmark/README.md` (what the copy is and what was removed); `CHANGELOG.md`; `README.md` (the surfaces, and the goldmark credit) |

## Acceptance

- Terminal app: an answer with a heading, a list, a fenced block and a table shows no literal `#`, `*`, backtick or pipe; no word is split at the wrap; a turn's tool calls are one row each, closed by default, open on Ctrl+O; `/feed off` and Alt+F hide the feed and the choice persists; the feed's lines are unchanged in content; all 33 slash commands work as before.
- Dashboard: the first token is visible within a second of the model's first token; the final answer is rendered markdown; tool cards open and close with no script; reloading shows the last 20 exchanges; each component kind chosen in D4 renders and a `form` submit or a `choices` tap starts a new turn; the CSP header is unchanged; the page works at 390px.
- `go.mod` is unchanged; `internal/md/goldmark` carries its LICENSE and pinned commit; no JavaScript beyond `datastar.js`; `go build ./...`, `go test ./...`, `go vet ./...` green locally before any push.

## Sizing

| Part | Size | PR |
|---|---|---|
| A: terminal app (A1 to A7) plus the `internal/md` copy and the terminal renderer | M to L | 1 |
| B1 to B4: streaming, markdown, cards, transcript | M | 2 |
| B5: components tier 1 (the D4 set), both surfaces, eval prompts | M with option (b), L with (a) | 3 |
| B6: tier 2 sandbox | L | only if D2 says (b) or (c) |
| B7: activity rail | S | folded into 2 or 3 if cheap |

## References

- OpenAI Intelligent UI (2026-10-07): [TechCrunch](https://techcrunch.com/2026/10/07/chatgpt-is-getting-a-lot-more-visual-with-the-launch-of-a-new-interface/), [MacRumors](https://www.macrumors.com/2026/10/07/chatgpt-intelligent-ui/), [ChatGPT release notes](https://help.openai.com/en/articles/6825453-chatgpt-release-notes)
- [CopilotKit/OpenIntelligentUI](https://github.com/CopilotKit/OpenIntelligentUI): `docs/architecture.md` (turn flow, `generateSandboxedUi`, host bridge, snapshots), `docs/visualization-routing.md` (router choices, A2UI table caps)
- Terminal references: Claude Code (turn bullets, collapsed tool rows, Ctrl+O, inline approvals), [Charm Crush](https://github.com/charmbracelet/crush) (bubbletea + glamour, the same stack), OpenCode, Codex CLI
- The engine being copied: [yuin/goldmark](https://github.com/yuin/goldmark) at `cbf81e9` (2026-10-01), MIT
- In this repository: `internal/tui/tui.go` (`renderChat`, `renderFeed`, `applyEvent`, `wrap`), `internal/dashboard/ssr_chat.go` (`handleEventsSSE`, `chatBubble`), `internal/dashboard/DESIGN.md`, `internal/agent/events.go`, the 2026-10-02 competitive review (`Tracking/competitive-reviews/2026-10-02/`)
