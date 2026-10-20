# Judge defaults: a bounded state and a patient client

Status: spec, written 2026-10-12 from a live diagnosis of the Clef judge on the R640 (`inference`,
2× Tesla T4). Not started. One PR when Dylan says go; the findtime-net docs change ships with it.

## The problem

The judge in the loop (TEN-510) fails open several times a day on long turns, and every failure
looks like an outage: `judge unavailable; answers go out unchecked … Client.Timeout exceeded while
awaiting headers`. The server is not down. The request is too big for the client's patience.

What was measured (Tenant on Dylan's Mac → Clef-flash on the R640, over the tailnet, direct path):

| Judge state | Input tokens | Server time | Client limit |
|---|---|---|---|
| triage ("hello") | 195 | 0.3 s | 10 s |
| a mid-size check | 3,114 | 3.3 s | 10 s |
| the check Tenant abandoned at 10:09 on 2026-10-12 | 9,976 | 11.1 s | **10 s** |
| Tenant's worst case (100 tool calls, 20 outputs, 4,000-char request and answer) | 16,384 (server cap) | 21 s | 10 s |
| two of those at once | | 22 s and 44 s | 10 s |

The server costs about 1.1 ms per input token and serves one request at a time; that is the
hardware floor for a 9B model in fp16 on two power-capped T4s (the Triton kernels that would
speed up its linear-attention layers were tried and are slower on Turing). Three things on
Tenant's side turn that into a failure:

1. `internal/model/systemone` has a hard-coded 10 s HTTP client timeout. The setup probe already
   allows 20 s ("a cold server took ~15 s").
2. `judgeState` in `cmd/tenant/judgeloop.go` can send about 56,000 characters: `judgeMaxCalls`
   100 × `judgeInputChars` 300, `judgeKeepOutputs` 20 × `judgeOutputChars` 600, plus
   `judgeTextChars` 4,000 for the request and 4,000 for the answer. Anything past roughly 9,000
   tokens blows 10 s on its own; a second caller (a cron turn, another Tenant) doubles the wait.
3. The state is a `map[string]any`, so Go marshals its keys alphabetically:
   `earlier_tool_calls_not_listed`, `final_answer`, `tool_calls`, `user_request`. Clef truncates
   the state from the right at 16,384 tokens, so an oversized state loses the **user's request**
   first, and the judge grades an answer against nothing.

A timed-out check costs the turn nothing (fail open), but it pauses judging for a minute, so in
practice every long turn goes out unchecked, which is the opposite of what the loop is for.

## The target

A check Tenant sends must finish comfortably inside the client's timeout on hardware like the
R640's, and the request and the answer must never be the part that gets cut. Operators running
a faster judge (Workers AI, a bigger GPU) just see faster checks.

## Design

### 1. A state budget (`cmd/tenant/judgeloop.go`)

Replace the four count caps with one budget and smaller per-item caps:

| Constant | Today | New | Why |
|---|---|---|---|
| `judgeTextChars` | 4,000 | 3,000 | request and answer, each |
| `judgeInputChars` | 300 | 200 | a tool call's arguments |
| `judgeOutputChars` | 600 | 400 | a tool call's result, newest calls only |
| `judgeKeepOutputs` | 20 | 8 | calls whose output is shown |
| `judgeMaxCalls` | 100 | removed | the budget bounds the list |
| `judgeStateChars` | — | 12,000 | the whole serialized state |

`judgeState` builds the tool list from the **newest call backwards**, adding a call while the
running total (request + answer + calls so far) stays under `judgeStateChars`, then reverses
the kept calls so the judge reads them in order. Calls that didn't fit are counted in
`earlier_tool_calls_not_listed`, as today. Worst case is 12,000 characters, about 4,000 tokens
of JSON-heavy text: ~4.5 s on the R640, sub-second on a hosted Clef.

Why 12,000 and not less: the judge's `done`/`every_part` scores need to see the recent tool
calls to tell "did it" from "said it did"; 8 outputs at 400 characters plus ~20 more call names
and arguments is what a long turn's last stretch looks like. If accuracy suffers on short turns
it won't be from this (short turns fit entirely).

### 2. An ordered state (`cmd/tenant/judgeloop.go`)

`judgeState` returns a struct, not a map:

```go
type judgeStateJSON struct {
    UserRequest string      `json:"user_request"`
    FinalAnswer string      `json:"final_answer"`
    ToolCalls   []judgeCall `json:"tool_calls"`
    Skipped     int         `json:"earlier_tool_calls_not_listed,omitempty"`
}
```

Request first, answer second, tool calls last. If a server ever truncates again (a smaller cap
than ours), it loses old tool calls, never the question. `agent.Exchange` and the questions are
unchanged.

### 3. A patient client (`internal/model/systemone`)

`defaultHTTP` timeout 10 s → **30 s**. That covers the new worst case, one request queued
behind it, and a cold server (15 s), with margin. `judgeProbeTimeout` (20 s) stays: the probe's
context bounds it, and a setup that takes longer than 20 s to answer "hello" should fail. No new
config key: this is a shipped default, and nothing measured needs a knob. If one is ever wanted,
it is `judge.timeout_ms` in `config.Judge`, read into the client in `judgeStateFrom`.

### 4. Say how long a check took (`internal/agent/judge.go`)

The `⚖ judge` event for a check carries its cost, so the next slow-judge report is diagnosable
from Tenant's own logs: `done ✓ every_part 0.91 · 3.1k tokens, 3.4 s` and the same suffix on
`sent back (1/3): …` and `not confirmed (…): …`. `judgeFinal` times the `Check` call; the token
count is already in `Verdict.Use.InputTokens`. One `fmt.Sprintf`, no new fields.

### 5. Tests

- `TestJudgeState` (exists): 200 tool calls with 1,000-character arguments and results → the
  serialized state is ≤ `judgeStateChars`; the newest call is present and the oldest absent;
  `earlier_tool_calls_not_listed` equals the number dropped; the raw JSON starts with
  `{"user_request":` and `"final_answer":` comes before `"tool_calls":`; the rune-boundary check
  stays.
- `internal/agent/judge_test.go`: the check event's text ends with ` tokens, ` + a duration.
- `internal/model/systemone`: nothing new; the timeout is a constant.

### 6. Documentation (same PR, per CLAUDE.md)

- findtime-net `tenant-docs/src/models/judge.md`, under "If the judge is down": a check waits up
  to 30 s; what the judge is sent is bounded (the request and the answer at 3,000 characters
  each, then the newest tool calls, 12,000 characters in all); on a pair of T4s that is about
  4 s at worst. The activity-feed paragraph gains the `· 3.1k tokens, 3.4 s` suffix.
- `CHANGELOG.md`, Unreleased → Fixed: the judge timed out on long turns and skipped them.
- `docs/ARCHITECTURE.md` docfacts: regenerate from a clean detached worktree if the counts move
  (constants and one struct shouldn't).

## Not in this change

- Server-side concurrency on the R640 (overlapping two requests across its two pipeline stages
  would roughly double throughput, not latency). Noted in `/opt/clef/server.py`'s docstring as
  "single-threaded on purpose"; revisit only if two Tenants share one judge.
- The triage questions, rounds, or the judge's wording.
- A faster model or GPU for the R640.

## What the R640 got on 2026-10-12 (already done, for the record)

`/opt/clef/server.py` answers HTTP 400 for a malformed question (a `choice` whose `criteria` is a
list crashed the handler 61 times and closed the socket with no response) and 500 for anything
else, and logs the client address per request. `flash-linear-attention` was installed, measured
1.4× slower than the reference path on T4, and removed. Backup: `/opt/clef/server.py.bak-2026-10-12`;
bench harness: `/opt/clef/bench/`.

## Rollout

One branch, `fix/judge-defaults`, one PR, the findtime-net docs PR referencing it (merged after).
`go build ./... && go test ./... && go vet ./...` locally before pushing. Operators get it with
the next binary; no config migration.
