# Judge in the loop (TEN-510, workstream TEN-509)

**Status:** implemented, rev 2 (2026-10-09). **Branch:** `feat/turn-judge`, stacked on `fix/tui-image-paste`: one PR.

## Goal

A judge sits in front of the main model. When the main model gives its final answer to a task, the judge checks
that the ask was actually done. If not, the work goes back to the main model with a specific nudge, up to three
times. The judge is a **decision model**: Cloudflare's Clef-flash, which answers typed questions with
probabilities through the System One API (`POST /v1/systemone`), never with text. It is fast, cheap and can't be
argued with.

## Decisions (Dylan, 2026-10-09)

| # | Decision |
|---|---|
| D1 | **Which turns:** the judge triages every prompt (chat, question or task) while the main model is already working, so triage adds no wait. Only tasks get the done-check. |
| D2 | **Rounds:** at most **3** send-backs per turn. |
| D3 | **Out of rounds:** deliver the best answer with a visible note: `judge: not confirmed (done 0.31)`. |
| D4 | **Scope:** the turn loop only. Clef as the eval judge is TEN-511, a later PR. |

## Verified (live against a self-hosted Clef-flash, 2026-10-09)

- **API:** `POST /v1/systemone` only (every other route 404s).
  - **Request:** `{"model":"clef-flash","state":<string|object>,"questions":{<id>:{"type":"noul"|"choice"|"score","instructions":"…","criteria":{…}|[…]}}}`.
  - **Response:** `{"model","answers":{<id>:{"type":"noul","noul":P(true)} | {"type":"choice","choice","confidence","probabilities"} | {"type":"score","score","confidence","legend","probabilities"}},"usage":{"input_tokens","output_tokens":0}}`.
  - **No key.**
- **Latency:** 0.2–0.7 s per call once warm (the first calls after the server started took about 15 s).
- **Triage** (one `choice` question: chat, question or task): 6 of 6 right, confidence 0.85–0.98. "thanks, that's great!" → chat; "what's the difference between TCP and UDP?" → question; "check the disk usage and clean up anything over 1GB" → task.
- **Done-check:** four `noul` questions on a **structured** state (`{"user_request","tool_calls":[{tool,input,output}],"final_answer"}`):

| case | done | every_part | claims_without_tool | gave_up |
|---|---|---|---|---|
| did it all | 0.90 | 0.88 | **0.10** | 0.02 |
| claimed a delete it never ran | 0.93 | 0.90 | **0.82** | 0.02 |
| did half (reported, didn't delete) | **0.21** | **0.09** | 0.05 | 0.29 |
| asked instead of doing | **0.04** | 0.10 | 0.02 | **0.79** |

  A flat-text state caught the false claim only weakly (0.31 against 0.15). The structured state with the
  "claims an action with no matching tool call" wording separates it cleanly, so the state is always structured.

## Design

### 1. The System One client: `internal/model/systemone` (commit A)

- **API:** `Client{Endpoint, Model, HTTP}` and `Decide(ctx, state any, qs map[string]Question) (map[string]Answer, Usage, error)`.
- **`Question`:** `{Type, Instructions, Criteria any}`.
- **`Answer`:** `{Type, Noul *float64, Choice string, Confidence float64, Score float64, Probabilities map[string]float64}`.
- **Transport:** stdlib `net/http` only; a 10 s default timeout, a 1 MB response cap, and an error that keeps the server's message.
- **Archtest:** under `internal/model/**`, so the existing rule covers it.

### 2. Configuration, live (commit A)

- **`config.json`:** `"judge_loop": {"enabled": false, "endpoint": "", "model": "clef-flash", "max_rounds": 3}`. Off by default. No key, since System One servers are self-hosted (a hosted gateway is TEN-511's business).
- **Live state:**
  - **Swap:** `modelControl.SetJudgeLoop(on, endpoint, model)` saves `config.json` (the `/route` `SetMode` pattern: `cfgMu`, load, mutate, save) and swaps an atomic the hook reads every turn, as the repeat guard does (TEN-506).
  - **Check before saving:** turning it on runs one triage call first and refuses with the server's error if that fails.

### 3. The agent hook (commit B)

**The hook:** `agent.Config.Judge Judge`, an interface, so the agent package imports nothing from `cmd`:

```go
type Judge interface {
    Triage(ctx context.Context, prompt string) (task bool, err error)
    Check(ctx context.Context, ex Exchange) (Verdict, error)
}
type Exchange struct { Request string; Tools []ToolCallResult; Answer string }
type Verdict struct { Done bool; Failed []string; Scores map[string]float64; Usage int }
```

**At turn start** (`agent.go` around 744):
- Triage runs in a goroutine when the hook is set, the judge is enabled (`Judge.Triage` returns `ErrJudgeOff` otherwise, so a live toggle applies), and the turn is judgeable.
- **Judgeable:** not a sub-agent (`ParentTraceID` empty) and not `req.NoJudge`. `/goal` turns set `NoJudge`, since `/goal` judges itself.
- The result lands on a buffered channel.

**At the normal final** (`agent.go:1134`, before `result.Response`, `EventFinal` and `persistEpisode`):
1. Read triage, waiting at most 5 s. If it isn't a task, or triage errored, deliver as today.
2. If `rounds < max`, call `Check`.
   - **Pass:** deliver.
   - **Fail:** append a user message to working and the archive, the interjection pattern from `agent.go:987-1000`, with one nudge per failed check. Emit `EventJudge`, do `rounds++`, and `continue` the loop, which re-plans with tools. The rejected draft stays in context, so the model can see what it said.
3. If the rounds are used up and the last check failed: deliver the answer with `\n\n— judge: not confirmed (done 0.31)` appended. Log `turns/2005 JudgeNotConfirmed`.
4. **Judge unavailable** (network error or 5xx): fail open and deliver. Log `turns/2111 JudgeUnavailable` once per outage, not every turn.

**Not judged:**
- The forced-synthesis paths (ceiling, repeat guard, ctx-cancel salvage) and empty-final recovery: tools are already off there.
- The loop ceiling still bounds everything: a nudge round counts iterations like any other.

**The questions:**
- **Triage:** one `choice` question, `kind`: chat, question or task, with the verified criteria.
- **Check:** `done`, `every_part`, `claims_without_tool` and `gave_up`, as `noul` questions with the verified wording. Pass when `done ≥ 0.5`, `every_part ≥ 0.5`, `claims_without_tool < 0.5` and `gave_up < 0.5`. Thresholds are constants, not config (YAGNI).

**Nudges** (one per failed check, deduplicated):
- **done / every_part:** "A check found the request isn't fully done. Re-read what the user asked, finish what's missing, then answer."
- **claims_without_tool:** "Your answer says things were done that no tool call shows. Do them with tools now, or say plainly they weren't done."
- **gave_up:** "You handed the work back to the user. Do it yourself unless you truly need their input; if you do, say exactly what you need."

**State:**
- `user_request` is this turn's text plus any interjections.
- `tool_calls` holds the last 20 calls, each with its input and output cut to 600 characters.
- `final_answer` is cut to 4,000 characters.
- The whole state stays under about 16K tokens, Clef's default prompt cap.
- Images are not sent (Clef could take them; later).

### 4. Accounting and events (commit B)

- **Usage:** Clef reports `input_tokens`. Each call does `span.AddUsage` and emits `EventUsage` with the judge's model name, so `runs.db`, the TUI totals and a team governor all count it. This avoids the gap `/goal` and `synthesizeFinal` have.
- **`EventJudge`** (new `EventKind`), with `Detail`: `task`, `checked ✓ done 0.90`, or `sent back: every_part 0.09 (round 1/3)`. The TUI activity feed renders it as `⚖ …`. It needs an `EntryFor` mapping and a `Retained` decision.
- **Catalog:**
  - `turns/1006 Judged` (info: verdict, scores, round).
  - `turns/2005 JudgeNotConfirmed` (warning).
  - `turns/2111 JudgeUnavailable` (warning, deduplicated).
  - `docs/EVENTS.md` regenerated.

### 5. Controls (commit C)

- **TUI:**
  - `/judge loop` shows the state.
  - `/judge loop on [<endpoint> [<model>]]` turns it on; the endpoint and model default to the saved ones.
  - `/judge loop off` turns it off.
  - `/judge` alone stays the eval judge.
- **`/model`:** lists `judge: clef-flash (loop on) → main: <model>` when the loop is on.
- **Dashboard:** the Model page's Advanced card gains a Judge loop row (on or off, endpoint, model, and the last check). It is a form posting to `/models/judge`, in the shape of the repeat-guard form.
- **Wiring:** on the main agents only, TUI (`commands.go:2263`) and serve (`serve.go:479`). Relays share those agents.

### 6. Docs (commit D)

- **This repository:** QUICKSTART "Judge loop", CHANGELOG, `docs/EVENTS.md` and docfacts.
- **The site** (findtime-net, the same piece of work):
  - `models/judge-loop.md` (new) or a section in `models/tuning.md`;
  - `reference/slash-commands.md`;
  - `reference/config.md` (`judge_loop`);
  - `dashboard/pages.md`;
  - the image-paste docs branch already pending.

## Testing

| Layer | Test |
|---|---|
| systemone | httptest server: request shape, the noul/choice/score answers decode, a 404 or 500 keeps the error message, timeout |
| agent | fake Judge: chat isn't checked; a task fails then passes (one nudge appended, a second plan, then delivered); three fails deliver with the note; Check erroring fails open; a sub-agent and `NoJudge` skip it; the usage event is emitted |
| wiring | the live toggle flips the atomic and saves `config.json`; turning on against a dead endpoint is refused |
| TUI | `/judge loop on/off/status`; the `⚖` feed line; `/model` shows the judge |
| live | against the self-hosted Clef with GLM as the main model: "check disk usage in /tmp and list the 3 biggest files" is triaged task and checked; a deliberately partial run gets sent back |

## Risks

- **False rejects** waste at most 3 rounds, then deliver with the note. Wording and thresholds come from the live test.
- **Prompt caching:** a nudge appends a message, so the prefix stays stable.
- **Streaming:** the TUI streamed the rejected draft; `finalizeAssistant(res.Response)` replaces it at turn end, and the `⚖ sent back` line explains why it changed.
- **A judge outage** fails open, with one warning per outage.

## Commits

A: the systemone client and config. B: the agent hook, accounting and events. C: controls (TUI, dashboard, `/model`) and wiring. D: docs, site and docfacts.

## Review log

**Rev 2 (2026-10-09), GLM-5.3-Flash adversarial review of rev 1: 5/10.** Verified each finding against the code.

Fixed in the implementation:
- **H1:** a send-back took an iteration, so a long task's last check fell into the ceiling's forced synthesis. A
  send-back now gets a free iteration (the interjection pattern); the rounds cap bounds it.
- **H2:** answers the loop forced (ceiling, repeat guard, an empty final) were unchecked; they're the unfinished
  ones. They are now checked and get the note (no round left to send them back in).
- **M1:** the 5 s triage wait skipped the check after a cold start; the final waits on the triage, which its
  client timeout bounds.
- **M2:** an unreachable judge stalled every task turn; a failure pauses judging for a minute (the same atomic
  dedupes the log).
- **M4:** a last-20 window could hide an early action the answer mentions; every call is listed by name and input,
  outputs only for the last 20, at most 100.
- **L1:** settings swap as one immutable struct behind one atomic pointer.
- **L2:** the "on" probe has a 5 s timeout.
- **L3:** peer and research turns aren't judged.
- **L6:** the note is for the reader; episodic memory keeps the model's words.

Rejected after checking the code:
- **M3** (triage usage from a goroutine): usage returns on the channel and is counted on the turn's thread.
- **M5** (two files): `loadLaunchConfig` is `config.Load`, i.e. config.json; one file, one writer
  (`persistLaunchConfig`).
- **L4** (image-only prompts triage as chat): accepted; the judge doesn't see images, so it couldn't judge them.
- **L5** (EventUsage has no model): accepted; usage is filed under the launch model, as for every call today.

Found live (GLM main, Clef judge, the real TUI):
- **Denials:** a turn whose rm the operator denied was sent back three times, asking the operator four
  times. Tools report a safety boundary or a denial as "blocked: …" (all 30 sites); a turn with one is never
  sent back.
- **Fresh setup:** no `judge_loop` in config.json left the judge without a client; `/judge loop` would panic.
  Caught by the switch test.
- **Pre-existing TUI bug (out of scope, task filed):** an unbracketed burst of input drops every "delete".
