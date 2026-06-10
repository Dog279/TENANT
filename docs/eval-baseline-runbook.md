# Eval baseline + nightly runbook (TEN-153)

The eval harness, baseline capture, paired-bootstrap regression check, and the
nightly job all already exist. This is the operator runbook to **arm** them: capture
the `fitness`/`full` baselines (needs a real model) and turn on the nightly run.
Once armed, the regression gate is what makes machine-authored self-improvement
(auto-accept skills TEN-152, SoulNudge TEN-16, distill.Supersede TEN-35, GEPA
TEN-18) safe to enable.

## What exists
- **Subsets:** `smoke` (5 tasks, **no LLM**, ~10s), `fitness` (~10 tasks, ~5min),
  `full` (~50 tasks, ~30min). Corpus: `internal/eval/tasks/{smoke,fitness,adversarial}`.
- **Capture / check:** `tenant eval --subset <s> --baseline-write <path>` snapshots
  per-task scores; `--baseline-check <path>` compares a fresh run to the baseline
  with a paired-bootstrap 95% CI and **exits non-zero on a regression**.
- **Nightly:** `evalNightlyJob` (`cmd/tenant/eval_job.go`) rides the self-improve
  scheduler. Enable with `--self-improve --eval-every 24h`; it writes a timestamped
  JSON artifact under `<data>/eval-artifacts` and, when `baselines/full.json`
  exists, checks for a regression each run.
- **Committed today:** only `baselines/smoke.json`. `fitness.json` / `full.json`
  are **not** captured yet — that's the gap this runbook closes.

## Step 1 — verify the loop offline (no model)
`smoke` needs no LLM, so the capture→check mechanism is verifiable on the echo
backend:

```sh
tenant eval --subset smoke --baseline-write /tmp/smoke.json     # capture
tenant eval --subset smoke --baseline-check baselines/smoke.json # check (exit 0 = no regression)
```

## Step 2 — capture the real baselines (MODEL-GATED)
`fitness`/`full` score real model output, so they need a real provider configured
(e.g. `zai` + `ZAI_API_KEY`, or any wired backend) — **they cannot be produced on
the echo offline backend.** Run once on a known-good model, then commit:

```sh
# with a real provider active (see `tenant setup` / `/setup`):
tenant eval --subset fitness --baseline-write baselines/fitness.json
tenant eval --subset full    --baseline-write baselines/full.json
git add baselines/fitness.json baselines/full.json && git commit -m "eval: capture fitness/full baselines"
```

Judge note: the eval uses a different-family judge by default (the planner model);
override with `--judge` / the `/judge` TUI control (TEN-91) if needed. Record which
judge profile produced the baseline (it's stamped into the baseline JSON).

## Step 3 — arm the nightly gate
Launch the live agent with the self-improve scheduler + a 24h eval cadence:

```sh
tenant tui --self-improve --eval-every 24h     # (or chat/serve)
```

With `baselines/full.json` present, each nightly run checks for a regression and
drops a JSON artifact under `<data>/eval-artifacts`. A regression surfaces in the
self-improvement feed.

### Persisting the cadence (TEN-157)
`--eval-every` is flag-only and dies with the process. For a 24/7 appliance,
persist it in config.json so nightly survives restarts:
```json
{ "improve": { "eval_every": "24h" } }
```
Precedence: an explicit `--eval-every` flag wins; else `improve.eval_every`; an
empty or **malformed** value fails closed to off (a typo can't brick launch).

## Trend log (TEN-158)
Each nightly run appends one line to `<data>/eval-artifacts/trend.jsonl`
(`{ts, subset, overall, passed, total, has_baseline, regressed, delta, ci_high, artifact}`)
— the durable record of each run's regression verdict that the heavy per-run
artifacts don't keep. View it (offline, no model):
```sh
tenant eval --trend          # last 20 runs, newest first
tenant eval --trend --trend-n 50
```
This is the series that tells you whether quality is trending and where the
ceiling is (prompt vs routing vs memory) — the data that gates SoulNudge (TEN-16)
and GEPA (TEN-18).

## Preflight gate (TEN-159)
The offline "smoke on PR" check for a repo with no remote — run before pushing
(or wire as a pre-push hook):
```sh
bash scripts/preflight.sh
# optional: ln -sf ../../scripts/preflight.sh .git/hooks/pre-push
```
Runs gofmt (changed files only), `go vet`, `go test ./...`, Windows+Linux
cross-compile, and the smoke eval + `baselines/smoke.json` regression check — all
offline. Operator-optional; not wired into the build.

## Downstream: SoulNudge (TEN-16)
The first eval-gated *improver*. Enable with `improve.soul_nudge_every` (config
duration, e.g. `"168h"`; off by default). Each run reviews recent ack/undo
feedback, asks the model to propose refined operating instructions, **A/B-scores
the candidate against `baselines/fitness.json`**, and queues only non-regressing
ones via the soul proposal flow for **human review** (`/memory soul` →
accept/reject). It **never auto-applies** (soul edits hit every turn), fails
closed without a fitness baseline, and is suppressed while the model is degraded.
So it too is gated on capturing `baselines/fitness.json` above.

## Re-capture cadence
Re-capture baselines deliberately — after an **intentional** model swap, prompt/soul
change, or skill-set change that you've confirmed is an improvement. Never
auto-recapture (that would launder a regression into the new baseline).

## Status (2026-06-09)
- [x] Harness, capture/check commands, nightly job — built.
- [x] `baselines/smoke.json` — committed; offline loop verified.
- [x] Persisted cadence (`improve.eval_every`), trend log + `tenant eval --trend`, preflight gate — TEN-34/157/158/159.
- [ ] `baselines/fitness.json` / `baselines/full.json` — **operator step, model-gated** (above).
- [ ] Set `improve.eval_every: "24h"` (or `--eval-every 24h`) in the live launch profile.
