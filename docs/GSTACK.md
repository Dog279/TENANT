# GStack → Tenant — adopting Garry Tan's operating discipline

GStack is Garry Tan's (YC CEO) Claude Code skill bundle. It's not a tool. It's
an **operating discipline** baked into prompts, hooks, and workflows that
makes Claude behave like a founder/staff engineer instead of a chatbot.

This document reverse-engineers what GStack actually does, picks the patterns
that transfer to Tenant, and ships them as native Tenant primitives (skills,
souls, commands) — so a Tenant operator gets the same "CEO-grade builder"
behavior without depending on an external runtime.

## What GStack actually is

A collection of skills loaded into Claude Code, each with a tightly-specified:

- **Voice** — direct, builder-to-builder, concrete file paths and commands.
  Banned em-dashes, banned AI vocabulary (`delve`, `crucial`, `robust`,
  `comprehensive`, `pivotal`, `landscape`, `tapestry`, `foster`, etc.),
  banned phrases (`here's the kicker`, `let me break this down`).
- **Completeness Principle (Boil the Lake)** — when AI makes a complete
  implementation near-free, always do the complete thing. Always recommend
  the complete option over the shortcut. Cap on options to compare.
- **Iron Law of Debugging** — NO FIXES WITHOUT ROOT CAUSE INVESTIGATION
  FIRST. 4 phases (investigate → analyze → hypothesize → implement). 3-strike
  rule: 3 failed hypotheses → STOP and question architecture.
- **AskUserQuestion conventions** — re-ground (project + branch + task),
  simplify (explain to a smart 16-year-old), recommend (with completeness
  scoring 0-10), lettered options.
- **Cascading Reviews** — CEO review (scope expansion / strategy), Eng
  review (architecture, tests), Design review (visual hierarchy, slop
  patterns), DevEx review (TTHW, error messages, onboarding). Each rates
  dimensions 0-10 and tells you what would make it a 10.
- **Plan Mode Default** — for any non-trivial task, write a plan first.
  Plans get a `GSTACK REVIEW REPORT` footer auto-appended.
- **Contributor Mode** — when tooling fails the operator, file a field
  report (`~/.gstack/contributor-logs/{slug}.md`) so the experience
  improves.
- **Telemetry + Trends** — log skill runs (skill, duration, outcome) to
  `~/.gstack/analytics/skill-usage.jsonl`; trend tracking across audit runs.
- **Hooks integration** — pre-tool hooks block destructive ops, post-deploy
  canary monitoring, scheduled retros.
- **Status escalation** — every workflow ends with a status (DONE,
  DONE_WITH_CONCERNS, BLOCKED, NEEDS_CONTEXT) — never silently bail.

The throughline: Garry Tan is encoding **founder taste + staff-engineer
rigor** into the prompt layer so the agent makes the calls a partner-level
human would make, not the calls a fresh grad would make.

## What transfers to Tenant

Tenant already has many of the load-bearing pieces:

| GStack pattern              | Tenant equivalent today           |
|-----------------------------|-----------------------------------|
| Skills                      | T4 skill cards (`/skills`)        |
| Voice / soul                | Soul system (`/memory soul`)      |
| Plan-mode → review → ship   | `/research` + (missing: review)   |
| Hooks for destructive ops   | Permission broker (ask/allow/deny)|
| Telemetry / analytics       | Episodic + analytics log (`distill`)|
| Sub-agents with own souls   | `/agents add` (per-model + soul)  |
| Autonomous loop on a goal   | `/goal <condition>` (DONE)        |
| Long-running structured run | `/research history`               |

What's MISSING vs GStack:

1. **No cascading review pipeline** — Tenant can run an orchestrator + sub-
   agents, but there's no canned "CEO review → eng review → design review"
   chain that produces decision-grade output.
2. **No completeness/escalation discipline in the soul** — Tenant's agents
   will happily produce shortcut answers without flagging the completeness
   tradeoff.
3. **No voice rules** — Tenant's main agent writes prose that includes the
   banned AI vocabulary (we've seen `comprehensive`, `crucial`, em-dashes).
4. **No structured ASK format** — `AskUserQuestion`-grade prompts don't
   exist; the agent just types follow-up questions.
5. **No status escalation** — agents finish whatever they finish; "DONE
   WITH CONCERNS" / "BLOCKED" categorization isn't surfaced.

## Implementation plan

The goal is to give a Tenant operator the **GStack behavior** without
needing a separate runtime. Three layers:

### Layer 1 — Voice + Discipline (the SOUL)

A reusable soul file at `cmd/tenant/builtinsouls/founder.md` operators can
import via `/memory soul import` to put the main agent in "founder mode".

Contents (high-level):
- Voice rules (no em-dashes, banned AI vocabulary, short paragraphs)
- Completeness Principle (always boil the lake when AI makes it cheap)
- Iron Law (no fixes without root cause; 3-strike escalation)
- Status protocol (DONE / DONE_WITH_CONCERNS / BLOCKED / NEEDS_CONTEXT)
- Concrete-over-vague rule (file:line, exact commands, real numbers)

### Layer 2 — Reusable Recipes (T4 SKILLS)

Save five core GStack patterns as Tenant skills (`/skills add`):

1. **`investigate-systematically`** — recipe: investigate → analyze → 
   hypothesize → implement, with the 3-strike rule. Triggered by bug
   reports, error messages, "why is X broken".
2. **`boil-the-lake-completeness`** — recipe: when AI makes the marginal
   cost near-zero, recommend the complete option over shortcuts. Score
   options 0-10 on completeness, prefer 8+.
3. **`structured-ask`** — recipe for the AskUserQuestion format:
   re-ground, simplify, recommend, options with completeness scoring.
4. **`founder-voice`** — recipe: banned words/phrases checklist. Triggered
   when writing prose for the user (reports, summaries, docs).
5. **`status-escalation`** — recipe: every workflow ends with a status
   token (DONE | DONE_WITH_CONCERNS | BLOCKED | NEEDS_CONTEXT). Never
   silently bail.

These are retrieved by the agent's existing T4 retrieval (top-K by query
embedding) so they fire when the work matches.

### Layer 3 — Cascading Reviews (NEW COMMAND)

A new `/review <plan.md>` command that runs the GStack-style review chain
against a plan file:

```
/review docs/some-plan.md
→ spawns named sub-agents:
   • ceo-reviewer    (rates scope/strategy 0-10, suggests expansions)
   • eng-reviewer    (rates architecture/tests/edges 0-10)
   • design-reviewer (rates UX/spacing/hierarchy 0-10, only if plan has UI)
→ each runs against the plan with its own soul (founder voice)
→ orchestrator collates the three reviews into a structured report:
   ## GSTACK REVIEW REPORT
   | Review | Verdict | Findings |
   | CEO    | 7/10    | ...     |
   | Eng    | 9/10    | ...     |
   | Design | n/a     | ...     |
→ appended to the plan file
```

This uses Tenant's existing `agentControl` + `TeamRuntime` (sub-agents with
per-agent profiles + souls). The three reviewer profiles are pre-registered
on first use. Same lifecycle as `/research` (persisted to `<data>/reviews/<id>/`).

## Files this design adds/changes

| File                                              | Purpose                                |
|---------------------------------------------------|----------------------------------------|
| `docs/GSTACK.md` (this file)                      | design doc                             |
| `cmd/tenant/builtinsouls/founder.md`              | soul template for `founder` mode       |
| `cmd/tenant/builtinskills/gstack.go`              | seed the 5 T4 skill cards on first run |
| `cmd/tenant/review.go`                            | `/review <plan.md>` command            |
| (TUI) `internal/tui/tui.go`                       | `/review` dispatcher + help entry      |
| `tasks/lessons.md`                                | "founder-mode" lessons entry           |

## What this gives Tenant operators

After running:
```
/memory soul import cmd/tenant/builtinsouls/founder.md
/skills seed gstack             # one-time
/agents add ceo-reviewer dgx aeon-ultimate -- scope + strategy reviewer
/agents add eng-reviewer dgx aeon-ultimate -- architecture + tests reviewer
```

The main agent talks like a builder (voice rules), refuses to ship symptomatic
fixes (Iron Law), recommends complete options over shortcuts (boil the lake),
and `/review docs/some-plan.md` produces a multi-perspective decision-grade
review — the same review you'd get hiring three Garry-Tan-shaped consultants.

That's the business edge: the operator gets founder-grade thinking baked
into every turn, without paying for or waiting on consultants.

## Status

This document is the **plan**. Implementation lands in subsequent commits:
soul template, skill seeds, /review command, tests, live verification.

GSTACK REVIEW REPORT
| Review        | Trigger              | Why                              | Runs | Status | Findings |
|---------------|----------------------|----------------------------------|------|--------|----------|
| CEO Review    | `/plan-ceo-review`   | Scope + ambition check           | 0    | —      | —        |
| Eng Review    | `/plan-eng-review`   | Architecture + tests (required)  | 0    | —      | —        |
| Design Review | `/plan-design-review`| UI gaps (n/a — no UI here)       | 0    | —      | —        |

**VERDICT:** NO REVIEWS YET — design doc is the entry point. The /review
command this plan describes is the mechanism to populate this table going
forward.
