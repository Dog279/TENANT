package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"tenant/internal/memory/skills"
	"tenant/internal/model"
)

// cmdSkills dispatches `tenant skills [seed|list]`. Today seed is the only
// useful sub-command from the CLI; list is a convenience pass-through.
func cmdSkills(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: tenant skills seed <bundle>   (known bundles: gstack)")
	}
	switch strings.ToLower(args[0]) {
	case "seed":
		return cmdSkillsSeed(ctx, args[1:])
	default:
		return fmt.Errorf("usage: tenant skills seed <bundle>")
	}
}

// cmdSkillsSeed routes `tenant skills seed <bundle>` through the same
// AddSkill path the TUI uses, so we get consistent embedding + persistence.
func cmdSkillsSeed(ctx context.Context, args []string) error {
	// Strip a leading positional bundle name BEFORE flag.Parse — Go's flag
	// package stops at the first non-flag arg, which means
	// `skills seed gstack --backend X` leaves --backend in fs.Args().
	// Pull the positional off the front so the rest is pure flags.
	var bundle string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		bundle = rest[0]
		rest = rest[1:]
	}
	fs := flag.NewFlagSet("skills seed", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	// Allow either positional OR trailing args (`--backend X gstack`).
	if bundle == "" {
		bundle = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if bundle == "" {
		return fmt.Errorf("usage: tenant skills seed <bundle> [flags]   (known: gstack)")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	log := newLogger()
	router, err := buildRouter(c, log)
	if err != nil {
		return err
	}
	st, err := skills.Open(filepath.Join(c.dataDir, "skills.db"))
	if err != nil {
		return fmt.Errorf("open skills db: %w", err)
	}
	defer st.Close()
	emb, _, _ := router.EmbedderForRole(ctx, model.RoleEmbedder)

	sc := skillControl{st: st, emb: emb, agentID: c.agent}
	n, err := installSkillSeeds(bundle, sc)
	if err != nil {
		// Partial success is real here — keep going past per-skill errors.
		fmt.Printf("seeded %d skill(s) from %q (with errors: %v)\n", n, bundle, err)
		return err
	}
	fmt.Printf("seeded %d skill(s) from bundle %q\n", n, bundle)
	return nil
}

// skillAdder is the minimal interface installSkillSeeds needs — accept-
// interfaces-return-structs. Both the production skillControl and a test
// stub satisfy it without dragging the full SkillControl surface.
type skillAdder interface {
	AddSkill(name, description, recipe string) error
}

// installSkillSeeds is the bundle installer behind `/skills seed <bundle>`
// + `tenant skills seed <bundle>`. Routes each seed through the supplied
// skillAdder so the embedding + persistence path is identical to a
// manually-added skill. Returns the count installed + the first error
// encountered (best-effort: keeps going past per-skill errors so a single
// embedder hiccup doesn't drop the whole bundle).
func installSkillSeeds(bundle string, sc skillAdder) (int, error) {
	var seeds []gstackSeedSkill
	switch strings.ToLower(bundle) {
	case "gstack":
		seeds = gstackSeeds
	default:
		return 0, fmt.Errorf("unknown bundle %q (known: gstack)", bundle)
	}
	installed := 0
	var firstErr error
	for _, s := range seeds {
		if err := sc.AddSkill(s.Name, s.Description, s.Recipe); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("skill %q: %w", s.Name, err)
			}
			continue
		}
		installed++
	}
	return installed, firstErr
}

// GStack seed skills — port Garry Tan's operating discipline (the YC CEO's
// Claude Code skill bundle) into Tenant's native T4 skill library.
//
// These get installed via `/skills seed gstack` (TUI) or `tenant skills seed
// gstack` (CLI). Idempotent — re-running refreshes any prior installs (same
// name, same agent → upsert).
//
// Each skill is one recipe the agent retrieves into its prompt when the
// user's query embeds near it. Together they reproduce GStack's "founder
// taste + staff-engineer rigor" behavior without the external runtime.

// gstackSeedSkill is one row to install. Mirrors the SkillControl.AddSkill
// shape (the TUI/CLI plumbing path).
type gstackSeedSkill struct {
	Name        string
	Description string // one line — drives retrieval embedding
	Recipe      string // the full procedure the agent follows when retrieved
}

// gstackSeeds is the five-card bundle the GSTACK design doc spec'd. Each
// one is small + composable; the agent retrieves whichever ones match the
// current task. Add to this list to extend the bundle.
var gstackSeeds = []gstackSeedSkill{
	{
		Name:        "investigate-systematically",
		Description: "before fixing any bug, error, or unexpected behavior, investigate root cause first (4 phases, 3-strike rule)",
		Recipe: `Iron Law: NO FIXES WITHOUT ROOT CAUSE INVESTIGATION FIRST.

Four phases:
1. INVESTIGATE — collect symptoms (error messages, stack traces, repro
   steps). Read the code path from symptom back to potential causes.
   Check recent changes (git log on the affected files). Reproduce
   deterministically. Output: "Root cause hypothesis: ..." (a specific,
   testable claim).

2. ANALYZE — match against known patterns: race condition (intermittent,
   timing-dependent), nil propagation (TypeError/NoMethodError), state
   corruption (inconsistent data, partial updates), integration failure
   (timeout, unexpected response), config drift (works locally, fails
   in prod), stale cache (shows old data, fixes on cache clear).
   Search related TODOs and prior fixes in the same area — recurring
   bugs in the same files are an architectural smell.

3. HYPOTHESIZE — verify the hypothesis with a log statement or assertion
   BEFORE writing any fix. 3-STRIKE RULE: if 3 hypotheses fail, STOP
   and question the architecture, don't keep guessing.

4. IMPLEMENT — minimal diff that fixes the root cause (not the symptom).
   Regression test that FAILS without the fix and PASSES with it. Run
   the full test suite.

Red flags: "quick fix for now" (no such thing), proposing a fix before
tracing data flow (guessing), each fix reveals a new problem elsewhere
(wrong layer).

Output a DEBUG REPORT: Symptom / Root cause / Fix (with file:line) /
Evidence / Regression test / Status (DONE | DONE_WITH_CONCERNS | BLOCKED).`,
	},
	{
		Name:        "boil-the-lake-completeness",
		Description: "when AI makes a complete implementation cheap, recommend the complete option over shortcuts (with completeness scoring)",
		Recipe: `Completeness Principle: AI makes completeness near-free. Always
recommend the COMPLETE option over the shortcut when delta is minutes
with AI assist.

When offering choices:
- Score each option 0-10 on completeness (10 = all edge cases, 7 = happy
  path, 3 = shortcut that defers significant work).
- If both options are 8+, pick the higher. If one is ≤5, flag it.
- Show effort in TWO scales when relevant: human-team-hours vs.
  AI-assisted-minutes. The delta is the whole reason to pick complete.

Effort reference:
  Boilerplate: 2 days human  → 15 min AI  (~100x)
  Tests:       1 day  human  → 15 min AI  (~50x)
  Feature:     1 week human  → 30 min AI  (~30x)
  Bug fix:     4 hrs  human  → 15 min AI  (~20x)

A "lake" (100% coverage, all edge cases) is boilable. An "ocean" (full
rewrite, multi-quarter migration) is not. Boil lakes, flag oceans.

When recommending, format:
  RECOMMENDATION: Choose [X] because [one-line reason]
  Option A: <description>  (Completeness: N/10)
  Option B: <description>  (Completeness: N/10)`,
	},
	{
		Name:        "structured-ask",
		Description: "when asking the user a clarifying question, format as: re-ground, simplify, recommend, options",
		Recipe: `ALWAYS follow this structure for clarifying questions:

1. RE-GROUND (1-2 sentences) — state the project, the current branch, the
   current task. The user hasn't looked at this window in 20 minutes.

2. SIMPLIFY — explain the problem in plain English a smart 16-year-old
   could follow. No raw function names, no internal jargon. Say what it
   DOES, not what it's called. Use concrete examples and analogies.

3. RECOMMEND — "RECOMMENDATION: Choose [X] because [one-line reason]".
   Always prefer the complete option (see boil-the-lake-completeness).
   Include "Completeness: X/10" for each option.

4. OPTIONS — Lettered: A) ... B) ... C) ... When an option involves
   effort, show both scales: "(human: ~Xh / AI: ~Ym)".

Assume the user doesn't have the code open. If you'd need to read the
source to understand your own explanation, it's too complex — rewrite.`,
	},
	{
		Name:        "founder-voice",
		Description: "when writing prose for the user (summaries, reports, docs), strip AI vocabulary and use the builder voice",
		Recipe: `Voice rules:
- Lead with the point. Say what it does, why it matters, what changes.
- Short paragraphs. Mix one-sentence and 2-3 sentence runs.
- Name specifics. Real file names, real function names, real numbers.
- Be direct: "well-designed" or "this is a mess" — don't dance around.
- End with what to do. Give the action.

Banned words (do NOT use): delve, crucial, robust, comprehensive,
nuanced, multifaceted, furthermore, moreover, additionally, pivotal,
landscape, tapestry, underscore, foster, showcase, intricate, vibrant,
fundamental, significant, interplay.

Banned phrases: "here's the kicker", "here's the thing", "plot twist",
"let me break this down", "the bottom line", "make no mistake", "can't
stress this enough".

Style:
- No em-dashes. Use commas, periods, or "..." instead.
- No filler / throat-clearing preambles. Just answer.
- Real numbers over qualitative claims: not "this might be slow" but
  "N+1 query → ~200ms per page load with 50 items".
- file:line over "in the auth flow".

Audit your own draft against these rules before sending. If you used a
banned word, rewrite the sentence.`,
	},
	{
		Name:        "status-escalation",
		Description: "every workflow ends with a status token (DONE / DONE_WITH_CONCERNS / BLOCKED / NEEDS_CONTEXT) — never silently bail",
		Recipe: `Every workflow you complete must end with one status token:

- DONE — all steps complete, evidence provided for each claim.
- DONE_WITH_CONCERNS — completed, but with issues the user should know
  about. List each concern explicitly.
- BLOCKED — cannot proceed. State what's blocking and what was tried.
- NEEDS_CONTEXT — missing information required to continue. State exactly
  what you need.

Escalation triggers:
- 3 failed attempts → STOP and escalate, don't keep guessing.
- Security-sensitive change you're uncertain about → escalate.
- Scope of work exceeds what you can verify → escalate.

Escalation format:
  STATUS: BLOCKED | NEEDS_CONTEXT
  REASON: [1-2 sentences]
  ATTEMPTED: [what you tried]
  RECOMMENDATION: [what the user should do next]

It is always OK to say "this is too hard for me" or "I'm not confident
in this result." Bad work is worse than no work. You will NOT be
penalized for escalating — you WILL be penalized for shipping broken
work with a "DONE" stamp.`,
	},
}
