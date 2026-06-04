# Tenant Eval Harness

Status: v1 (TEN-1 closed 2026-05-26)
Package: `internal/eval/`
CLI: `tenant eval [--subset smoke|fitness|full] [--json|--quiet|--list]`

The eval harness is the **fitness primitive** every self-improvement Job consumes (`SoulNudgeJob`, `SkillInductionJob`, `distill.Supersede`, future GEPA). It also gates CI regressions via the smoke subset on every PR and the full subset on a nightly cadence.

## Design summary

| Concern | v1 answer |
|---|---|
| Task spec format | YAML, embedded via `//go:embed`, strict unknown-field rejection |
| Scoring | Deterministic gate (`must_call`, `must_not_call`, `response_substring_any`) → anchored LLM rubric → linear-scale score |
| Detection floor | ~12% category regressions reliably caught at 50 tasks. Tracks toward 5% as catalog grows past 200 (TEN-21). |
| Statistical primitive | Paired bootstrap 95% CI on per-task Δ (1000 resamples default, 200 for in-Job fitness). |
| Flake handling | pass^k (k=5 rollouts default; τ-Bench MIT-licensed methodology) |
| Judge | Anchored rubric (score 1/3/5 reference examples) + length-bias countermeasure baked into prompt. Single judge in v1, multi-judge deferred to v2 if data shows single-judge variance dominates. |
| Concurrency | `Harness.RunWith` serializes via `runMu`; per-call factory + judge prevent racing other consumers. |
| Schema versioning | `Report.SchemaVersion = 1`, `Baseline.SchemaVersion = 1`. External consumers fail loud on mismatch. |

## Subsets

| Subset | Tasks | Wall time | Purpose | Cadence |
|---|---|---|---|---|
| `smoke` | 5 (fixture-mode, no LLM) | ~10s | Structural CI gate | Every PR |
| `fitness` | ~10 (live-mode, LLM-judged) | ~5 min | In-Job fitness signal | On-demand, in-Job calls |
| `full` | 50 tasks (grows per TEN-21) | ~30-125 min | Quality baseline | Nightly cron |

Subset selection in YAML: `subset: smoke | fitness | full`. The fitness mix (10 tasks) per TEN-14:

- 4 single-tool (web, sql, wiki, os — one each)
- 2 multi-tool chains
- 2 memory recall
- 1 safety/refusal
- 1 adversarial

## Judge configuration

The judge runs the anchored-rubric scoring on live-mode tasks. **Self-bias is real and linearly correlated with self-recognition** ([arXiv 2508.06709](https://arxiv.org/pdf/2508.06709)), so the judge must be a different model family than the planner.

Recommended pairings:

| Planner | Recommended judge |
|---|---|
| Qwen 3.6-X | Gemma 4 |
| Gemma 4-X | Qwen 3.6 |
| Llama 4-X | Mistral Large |
| Mistral Large | Qwen 3.6 |

Set up via a regular profile YAML in `%APPDATA%\tenant\profiles\judge-XXX.yaml` (or your OS equivalent). The role field must be `role: judge` (added in TEN-3, see `internal/model/llm.go`).

Same-family judging is permitted but emits a warning at eval-startup time. The warning cites the self-bias paper so the operator can decide whether to override consciously.

## Adding a task

Tasks live in `internal/eval/tasks/{smoke,fitness,full,adversarial}/*.yaml`. Example fixture-mode task (smoke):

```yaml
id: smoke-NNN-short-description
category: smoke/scoring
subset: smoke
description: |
  One-paragraph explanation of what this task exercises and why.
prompt: "What the agent should be asked."
mode: fixture
fixture:
  tool_calls:
    - tool: tool_name
      args: '{"key":"value"}'
  response: "Pre-canned agent response."
expected:
  must_call:
    - tool: tool_name
      args_contain: ["value"]
  must_not_call: [destructive_tool]
  response_substring_any: ["substring1", "substring2"]
```

Example live-mode task (fitness):

```yaml
id: fitness-NNN-short-description
category: fitness/single-tool/web
subset: fitness
description: |
  What this exercises end-to-end through the agent.
prompt: "Operator question."
mode: live
expected:
  must_call:
    - tool: web_navigate
      args_contain: ["example.com"]
  rubric:
    criterion: "What constitutes a good response."
    anchors:
      1: "Concrete bad example"
      3: "Concrete acceptable example"
      5: "Concrete ideal example"
    length_normalized: true
  rubric_min_score: 3
weight: 1.0     # default; set 2.0 for adversarial-category tasks
```

Adversarial tasks (category `adversarial/*`) ship with `weight: 2.0` per TEN-12. Standing rule: every model-behavior bug from `tasks/lessons.md` → one new adversarial task.

## Job-callable API

The Phase 5 surface is `Harness.RunWith`:

```go
func (h *Harness) RunWith(
    ctx context.Context,
    sub Subset,
    factory AgentFactory,
    judge Judge,
) (*Report, error)
```

### Worked example: SoulNudgeJob

```go
// SoulNudgeJob proposes a Soul edit, then verifies it doesn't regress
// the fitness subset before queuing the proposal for human review.
func (j *SoulNudgeJob) Run(ctx context.Context) (improve.JobResult, error) {
    candidate := j.proposeEdit() // candidate Soul derived from recent episodes

    // Build two factories: one returns an agent with the CURRENT soul,
    // one returns an agent with the CANDIDATE soul.
    currentFactory := func(ctx context.Context, taskID string) (eval.AgentRunner, func() error, error) {
        return j.buildRunner(ctx, j.CurrentSoul), nil, nil
    }
    candidateFactory := func(ctx context.Context, taskID string) (eval.AgentRunner, func() error, error) {
        return j.buildRunner(ctx, candidate), nil, nil
    }

    currentRep, err := j.Harness.RunWith(ctx, eval.SubsetFitness, currentFactory, j.Judge)
    if err != nil { return improve.JobResult{}, err }
    candidateRep, err := j.Harness.RunWith(ctx, eval.SubsetFitness, candidateFactory, j.Judge)
    if err != nil { return improve.JobResult{}, err }

    // Compare via paired bootstrap. Reject the proposal if candidate
    // shows a regression beyond the CI bounds.
    baseline := eval.NewBaseline(currentRep, time.Now().Format(time.RFC3339), j.JudgeProfile, j.TenantVersion)
    cmp := eval.CompareToBaseline(baseline, candidateRep, eval.CompareOptions{
        BootstrapN: 200, // 200 is fine for in-Job — see TEN-11 decision
    })
    if cmp.Regressed {
        return improve.JobResult{
            Summary: fmt.Sprintf("rejected proposal (regression: Δ=%.2f CI=[%.2f, %.2f])",
                cmp.Delta, cmp.CILow, cmp.CIHigh),
        }, nil
    }
    // Otherwise queue for human review via soul.ProposeEdit.
    j.SoulEditor.ProposeEdit(candidate)
    return improve.JobResult{
        Changed: true,
        Summary: fmt.Sprintf("queued proposal (Δ=%.2f, gate-and-rubric clean)", cmp.Delta),
    }, nil
}
```

Same shape works for `SkillInductionJob` (factory swaps Skills), `distill.Supersede` upgrade (factory swaps semantic store contents), and future GEPA (factory swaps system prompt).

## Interpreting baseline.json

`Baseline.SchemaVersion` MUST equal `eval.BaselineSchemaVersion` or `ReadBaseline` errors. Future schema bumps require explicit migration.

Per-task scores are kept in `task_scores` so paired bootstrap has the raw data. Don't post-process this map — let the Job pass it through `eval.CompareToBaseline`.

`captured_at` is informational only (the regression test uses task-ID pairing, not timestamps). `judge_profile` and `tenant_version` let you spot baselines that drifted because something changed underneath (model swap, tenant upgrade).

## Detection floor

50 tasks gives roughly **12% category-regression sensitivity at 95% confidence** (sample-size math: [arXiv 2410.03492](https://arxiv.org/pdf/2410.03492); [Wolfe](https://cameronrwolfe.substack.com/p/stats-llm-evals)). Smaller regressions get lost in flake.

Path to 5% sensitivity: ~200 tasks. The standing rule from TEN-21 ("every shipped feature ≥ 1 new eval task") is the cheapest way there. Reviewed quarterly.

## Failure modes & their handling

| Failure | What happens |
|---|---|
| Task YAML has unknown field | Load fails at startup with the file path + field name |
| Live-mode task missing rubric | Validator rejects at load |
| AgentFactory not configured | Each live task fails with "AgentFactory not configured" — no silent skip |
| Judge LLM returns malformed output | Parser extracts JSON from fences; falls back to integer-scrape; finally returns score=0 with reasoning explaining the failure |
| Judge returns score > 5 or < 0 | Clamped to [0, 5] |
| Ctx cancellation mid-run | Returns partial Report with already-completed task results; no panic |
| Concurrent RunWith calls | Serialized via `runMu`; each gets its own *Report; Harness defaults restored after each |

## References

The v1 design pulls directly from these sources (see also `tasks/eval-harness-plan-v1.md` Appendix B):

- BFCL v3 — [Patil et al., ICML 2025](https://proceedings.mlr.press/v267/patil25a.html)
- τ-Bench `pass^k` methodology — [arXiv 2406.12045](https://arxiv.org/pdf/2406.12045) (Sierra, MIT-licensed)
- LLM-as-judge self-bias — [arXiv 2508.06709](https://arxiv.org/pdf/2508.06709) ("Play Favorites")
- LLM-as-judge length bias — [arXiv 2509.26072](https://arxiv.org/pdf/2509.26072) ("Silent Judge")
- Paired bootstrap CI — Efron 1979; sample-size guidance [arXiv 2410.03492](https://arxiv.org/pdf/2410.03492)
