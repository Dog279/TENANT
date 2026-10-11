# Judge rework: the Quality model and the Judge (TEN-509)

Status: plan, decided 2026-10-11 (Q1 to Q5 answered by Dylan, see the end). Lands on `feat/turn-judge` as the last commits before that branch's one PR, after TEN-510's judge loop. Jira: TEN-516.

## The problem

Three things answer to "judge" today, on two code paths and in three sets of words:

| Today | What it is | Where |
|---|---|---|
| `/judge set <kind> <model>`, `/judge clear`, `/judge status` | the LLM that **grades Quality eval answers** (TEN-91); default: the main model grades itself | `improve.judge`, `judge_kind`, `judge_endpoint`, `judge_key_env` in `config.json`; `tui.JudgeControl` → `cmd/tenant/judgectl.go`; the **Quality** page (`POST /eval/judge`, `/eval/judge/clear`); `tenant eval --judge-model`, `--gate-only` ("skip the LLM judge") |
| `/judge loop on <endpoint> [model]`, `/judge loop off` | the **decision model in the loop** (TEN-510): Clef triages each prompt and checks a task's answer, sending it back up to 3 times | `judge_loop` in `config.json`; `cmd/tenant/judgeloop.go`; the **Model** page card (`POST /models/judge`); `/model` names it; `⚖ judge …` feed lines; `internal/agent/judge.go`, `internal/model/systemone` |
| "the judge's last verdict" in `/goal show` | `/goal`'s own completion check, an LLM call (`GoalControl.Judge`) | unchanged by this plan except its words in the docs |

One slash command carries two unrelated settings; the Model page and the Quality page each call a different thing "judge"; and the loop's decision model is hard-wired to one server shape with no notion of a provider.

## The target

- **Quality model**: the model that grades Quality evals. It is the old eval judge, renamed everywhere an operator can see it. It stays an LLM grader for now; a later task (TEN-511, retitled) lets it be the Judge instead.
- **Judge**: the decision model in the loop of a turn. It gets a **kind** so it can be Clef today, Jev when a live call has verified it (Q5), and another System One server later by adding a row, not a feature. Setting it is a short prompt: the server's address, then its key, masked; a blank key means none, which is how a local Clef works.
- The goal's check keeps its code and its name in code; the docs call it "the goal's check", not a judge.

## Design

### 1. Configuration

`config.json` gains one `judge` section and the Quality model's keys move under their own name:

```json
"judge": {
  "enabled": true,
  "kind": "clef",                      // the server's dialect; "clef" in this PR, "jev" once verified (Q5)
  "endpoint": "http://host:30000",     // always overridable; clef has no default, jev defaults to https://api.typesafe.ai
  "model": "clef-flash",               // clef default clef-flash; jev default jev-latest
  "rounds": 3                          // send-backs per turn, 1 to 5
},
"improve": {
  "quality_model": "claude-sonnet-4-20250514",
  "quality_model_kind": "anthropic",
  "quality_model_endpoint": "",
  "quality_model_key_env": "ANTHROPIC_API_KEY"
}
```

- `config.Judge` replaces `config.JudgeLoop` (fields: `Enabled`, `Kind`, `Endpoint`, `Model`, `Rounds`). `Improve.Judge*` become `Improve.QualityModel*`. One word for the one knob: `rounds` in config, in `/judge rounds` and on the dashboard card, clamped to 1..5 wherever it is set and when the file is read (TEN-510's D2, "at most 3", is superseded by a default of 3). There is no `key_env` key: the kind table names the variable, and nothing on any surface would set it (YAGNI).
- **Migration in `Launch.Migrate()`** (persist.go). `Load` calls `Migrate` on every load, and `Persist` re-loads before every save, so a migrated key cannot come back through a later save from `/judge on`, a dashboard form or `/eval model set`. A saved `judge_loop` becomes `judge` with `kind: "clef"` (its `max_rounds` becomes `rounds`); saved `improve.judge*` become `improve.quality_model*`. The legacy fields are ordinary struct fields named `LegacyJudgeLoop`, `LegacyJudge…`, tagged with the old JSON names and `omitempty`; `Migrate` reads them into the new fields and zeroes them, so the next save writes only the new keys. **Precedence:** when a hand-edited file carries both the old and the new key, the new one wins and the old is dropped; a test covers it. The file-format golden (`internal/config/testdata/keys.golden`) changes with `-update`, and a test asserts the golden contains neither `judge_loop` nor `improve.judge`. The legacy fields are removed in the first release after this one ships (a follow-up task, filed when this merges).
- **The Judge's key** lives in `credentials.json` under the id `judge` (`config.Credentials`, the store every provider key uses), written by the prompt or the dashboard card and never shown again; it is sent as `Authorization: Bearer` when set. No environment variable: Dylan's Q3. The Quality model keeps its env-var keys as today; aligning it is not this task.
- **Judge kinds** are one small table in `cmd/tenant/judgeloop.go` (not a new package): `kind → {default endpoint, default model}`. The kind is the server's **dialect**, not where it runs: the endpoint is always just an endpoint, defaulted per kind and overridable for both, so a self-hosted Jev or a gateway in front of Clef is a different endpoint, not a new kind. `clef`: no default endpoint (the operator's server), `clef-flash`. `jev`: `https://api.typesafe.ai`, `jev-latest`. The System One client gains an optional bearer key, nothing else. An unknown kind in the file is a load error naming the valid values. A third System One dialect is a row; a grader that is not a System One server (TEN-511's LLM-scored kind) is a second implementation of the agent's `Judge` interface, which is the seam, not this table.
- **Jev is unverified, and there is no key to verify it (Q5).** Everything here about Jev comes from third-party write-ups of its API ([LiteLLM](https://docs.litellm.ai/docs/pass_through/typesafe), [a usage guide](https://dev.to/valyuai/how-to-use-jev-a-practical-guide-to-typesafes-system-one-model-g5e)), not from a call. So this PR ships `clef` alone: the table has the one row, `/judge use jev` is not offered, and the dashboard's kind select has one entry. The `jev` row is a follow-up task that starts with one live call capturing the request, the response envelope and the usage fields (Clef bills input only and returns `output_tokens: 0`; the accounting must not assume that for Jev). The key prompt and the bearer header ship now, so a hosted server needs only its row.
- Setting the Judge probes the server with one triage question before saving (TEN-510's check-before-save rule), from `/judge` and from the dashboard card alike; a server that refuses the key, or asks for one that is blank, fails that probe with the server's own answer.

### 2. Commands

`/judge` is the decision model's command and nothing else:

```text
/judge                                  status: kind, model, endpoint, whether a key is saved, on or off, rounds
/judge on | off                         switch the loop (on asks the server one question first, as today)
/judge use clef [model]                 set it up: prompts for the server's address (the saved one offered), then
                                        its key (masked; blank = none), asks the server once, saves, switches on
/judge rounds <n>                       send-backs per turn, 1 to 5 (default 3)
```

The two prompts reuse the `/setup` step-entry flow (`setupEntry`: the next line is the value, the key step masked, `/cancel` or Esc leaves everything as it was). Each later kind adds nothing to the flow, only its defaults.

The Quality model moves under the eval family, which already owns the Quality page's commands:

```text
/eval model                             which model grades eval answers (default: the main model grades itself)
/eval model set <kind> <model> [endpoint]
/eval model clear
```

`/judge loop …` and `/judge set|clear|status` are removed, not aliased (Q2). Help rows and the registry change with them (`cmd_models.go`), so `/help` and the docs' command index follow.

`tenant eval --judge-model` becomes `--quality-model`; `--gate-only`'s help says "skip the Quality model". `cmd/tenant/cliregistry.go` lists the flag.

### 3. Controls and the dashboard

- `tui.JudgeControl` → `tui.QualityModelControl` (`Config.Quality`), implemented by `judgectl.go` renamed `qualitymodel.go`. `tui.JudgeLoopControl` (the loop's TUI hook) is `tui.JudgeControl` now: `Status()`, `Set(on bool)`, `Use(kind, endpoint, key, model string)` (an empty key clears the saved one), `Rounds(n int)`, `Saved() (endpoint string, hasKey bool)` for the prompt's defaults.
- `dashboard.EvalControl`: `JudgeStatus/SetJudge/ClearJudge` → `QualityModel/SetQualityModel/ClearQualityModel`; the Quality page's card is titled **Quality model**, routes `POST /eval/quality-model` and `POST /eval/quality-model/clear` (replacing `/eval/judge*` in `reviewedMutations` and the inline-forms tests).
- `dashboard.ModelControl`: `JudgeLoop/SetJudgeLoop` → `Judge() (on bool, kind, endpoint, model string, rounds int, hasKey bool)` and `SetJudge(on bool, kind, endpoint, key, model string, rounds int)`. The Model page card is titled **Judge**: a kind select (Clef, one entry for now), the server box, a key box (`type="password"`, "only if the server asks for one", emptied when the reply arrives as the page's other key boxes are, never pre-filled; "a key is saved" shown as text), the model box, a rounds box (1 to 5), on/off. Route `POST /models/judge` unchanged; it is a key-carrying form, so the secret-route rules apply (no key in a URL or a flash).
- `/model` keeps naming the Judge in front of the main model, now with its kind: `judge: clef-flash (clef) at http://… · on`.
- Feed lines `⚖ judge …` and the `turns` events are unchanged.
- Internal names follow where the rename is mechanical: `eval.Judge` (the grader interface) becomes `eval.Grader`, `evalJudgeOpts` → `qualityOpts`, `judgeCtl` → `qualityModelCtl`. `internal/agent/judge.go` keeps its name: it is the Judge.

### 4. Documentation (findtime-net, per the CLAUDE.md table)

| Page | Change |
|---|---|
| `models/eval.md` | "A separate judge" → "The Quality model": `/eval model …`, the `improve.quality_model*` keys, `--quality-model` |
| `models/tuning.md` | "The judge loop" → "The judge": kinds Clef and Jev, `/judge …`, `judge` in config |
| `reference/slash-commands.md` | the `/judge` rows and the `/eval model` row |
| `reference/config.md` | the `judge` section and the `quality_model*` rows |
| `reference/cli.md` | `--quality-model` |
| `setup/directories.md` | `credentials.json` holds the Judge's key under `judge` |
| `dashboard/pages.md` | Quality page: "Quality model"; Model page: "Judge" |
| `models/teams.md`, `models/helpers.md`, `first-launch/terminal-ui.md` | "the judge's last verdict" and similar `/goal` wording → "the goal's check" |
| this repository | `CHANGELOG.md` (TEN-510's entry rewritten in the new words), `docs/QUICKSTART.md` (the "Judge loop" section TEN-510 added), `docs/EVENTS.md`, `internal/dashboard/DESIGN.md` if it names the cards; `docs/plans/turn-judge-plan.md` gets a pointer to this plan |

The docs branch is `docs/tenant-paste-judge` in findtime-net, already carrying TEN-510's pages; these edits go on it. **Gate before the PR:** `git grep -n "judge loop\|/judge set\|/judge status\|/judge clear\|--judge-model\|judge_loop\|judge_kind"` over this repository's Markdown and the site's `tenant-docs/src` returns nothing (today it finds `models/eval.md`, `models/tuning.md`, `reference/cli.md`, `reference/config.md`, `reference/slash-commands.md`, `dashboard/pages.md`, `CHANGELOG.md`, `docs/QUICKSTART.md`, `docs/EVENTS.md`). The plain-language test stays what it is: this repository's templates and rendered pages; the site's words are checked by that grep.

### 5. Tests

- Config: migration from `judge_loop` and from `improve.judge*`; both keys present (the new wins); legacy keys gone after a save; the golden has no legacy key; `rounds` clamped on read; an unknown kind is a load error.
- Judge setup: the two-step prompt saves the endpoint and the key (blank clears), probes before saving, `/cancel` leaves everything as it was; the client sends `Authorization` only when a key is set; the key is in `credentials.json`, never in `config.json`, a flash or a log line; `clef` reuses the saved endpoint as the prompt's default.
- Commands: `/judge use jev`, `/judge rounds 2`, `/judge on` with a fake server; `/eval model set` round-trips; `/judge loop` and `/judge set` are unknown.
- Dashboard: the Quality model form and the Judge form answer in place (`TestEval_InlineAnswersEveryForm`, the Model page's), `TestRouteInventory` for the renamed routes, the plain-language test (no "judge" where the Quality model is meant).
- The existing TEN-510 tests (`internal/agent/judge_test.go`, `cmd/tenant/judgeloop_test.go`, `internal/tui/judgeloop_test.go`) keep passing under the new names.

### 6. Delivery

Commits on `feat/turn-judge`, in order: config + migration; the Judge kinds and the System One key; the commands and controls; the dashboard; docs. Each commit builds and passes the suite on its own, so any one can be reverted alone. One PR carrying TEN-510 and this task, both numbers in its description (the fewer-PRs rule is Dylan's; the reviewer sees the net diff, which has only the final names), when Dylan says go. Jira: a Task for this rework under TEN-509; TEN-511 retitled to "Quality model can be the Judge" (its kind becomes `judge`: the configured decision model grades with `score` questions); TEN-510 unchanged.

## Out of scope

- Making the Quality model a decision model (TEN-511).
- Using the Judge for `/goal`'s check or for helpers (later, under TEN-509).
- Any other System One kind than Clef and Jev; the kinds table is where one goes.

## Decisions (Dylan, 2026-10-11)

| # | Question | Decision |
|---|---|---|
| Q1 | Where the Quality model's command lives | `/eval model …` |
| Q2 | Aliases for the old spellings | none; the config migration covers saved files |
| Q3 | The Judge's key | setting the Judge prompts for the server's address, then the key; a blank key means none (a local Clef). The key goes to `credentials.json`, not an environment variable |
| Q4 | TEN-511 | retitled "Quality model can be the Judge" |
| Q5 | Jev | no TypeSafe key: `clef` ships alone, the key prompt and bearer header ship with it, `jev` is a follow-up that starts with one live call |

## Review log

**Clef (decision model, `POST /v1/systemone`, 2026-10-11), the whole plan as state:**

| Question | P(yes) |
|---|---|
| Covers every surface the problem table lists | 0.89 |
| An operator would know which setting grades evals and which checks turns | 0.95 |
| The migration keeps saved `judge_loop` and `improve.judge*` working | 0.95 |
| The Judge kind table is specified enough to implement and test | 0.92 |
| Conflicts with TEN-511 (rather than leaving room for it) | 0.09 |
| Removing the old spellings without aliases is acceptable | 0.82 |
| Where the Quality model's command lives: `/eval model` 0.98, new `/quality` 0.01, under `/judge` 0.01 | choice |
| Overall risk of regression or confusion: low 0.57, moderate 0.36, high 0.07 | score |

**GLM-5.3-Flash (adversarial, 2026-10-11), ten findings, each checked against the branch:**

| # | Finding | Disposition |
|---|---|---|
| 1 | Jev is designed but never verified | Accepted: a live call is a gate on the `jev` row; `clef` alone otherwise (Q5) |
| 2 | Migration runs only at launch; other save paths could resurrect legacy keys | Held in part: `Load` migrates on every load and `Persist` re-loads before saving, so they cannot. Accepted: precedence when both keys are present, a test for it, and plain words for the legacy fields |
| 3 | No assertion that the golden has no legacy key; legacy fields with no removal date | Accepted: both added |
| 4 | `key_env` is a config key with no surface | Accepted: dropped; the kind table names the variable |
| 5 | `kind` conflates dialect with hosting; `/judge use clef` without an endpoint | Accepted: kind is the dialect, the endpoint is always overridable, the saved endpoint is reused |
| 6 | `rounds` unbounded and two names for one knob | Accepted: 1 to 5, one name, D2 superseded |
| 7 | QUICKSTART and other pages with the old spellings | Accepted: `docs/QUICKSTART.md`, `docs/EVENTS.md`, `CHANGELOG.md` added, and a grep gate before the PR |
| 8 | The kinds table is the wrong seam for TEN-511 | Accepted: the agent's `Judge` interface is the seam; said so |
| 9 | One PR for three features; the Jira number mismatch | Held: one PR is Dylan's standing rule; the PR names both tasks and each commit is green on its own |
| 10 | Unknown kind at load, the dashboard's key check, the plain-language test's scope, `max_rounds` vs `rounds` | Accepted: all four specified above |
