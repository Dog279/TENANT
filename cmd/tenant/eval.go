package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/agent"
	"tenant/internal/eval"
	"tenant/internal/eval/compaction"
	"tenant/internal/memory/archive"
	"tenant/internal/memory/compress"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// cmdEval is the `tenant eval` subcommand.
//
//   - smoke   — fixture-mode tasks; no model, tools, or judge needed (~10s).
//   - fitness — live tasks against a fresh, isolated agent + an LLM judge.
//   - full    — the whole catalog, live.
//
// Live subsets build the model router and the real tool mux ONCE and share
// them; each task gets a freshly-isolated agent (ephemeral in-memory stores,
// so tasks can't cross-contaminate and the operator's real memory is never
// touched). Open-ended answers are graded by an LLM judge — the planner
// (main-agent) model BY DEFAULT, or a separate model via --judge-model. The
// v1 plan preferred a different-family judge (self-bias), but the operator
// chose planner-by-default for zero-setup runs. `--gate-only` skips the judge.
func cmdEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	c := bindCommon(fs)
	pf := bindPluginFlags(fs)
	var (
		subset         string
		jsonOut        bool
		quiet          bool
		listOnly       bool
		compactionMode bool
		baselineWrite  string
		baselineCheck  string
		trend          bool
		trendN         int
		jOpts          evalJudgeOpts
	)
	fs.StringVar(&subset, "subset", "smoke", "task subset to run: smoke | fitness | full")
	fs.BoolVar(&jsonOut, "json", false, "emit JSON report to stdout instead of the terminal table")
	fs.BoolVar(&quiet, "quiet", false, "print only the one-line summary")
	fs.BoolVar(&listOnly, "list", false, "list tasks in the subset and exit (no run)")
	fs.BoolVar(&compactionMode, "compaction", false, "score the context-compaction compressor's fidelity (needle/continuation/drift) vs the configured model — TEN-99 baseline; ignores --subset")
	fs.BoolVar(&jOpts.gateOnly, "gate-only", false, "live mode: skip the LLM judge, score on the deterministic gate only (answer QUALITY is not judged)")
	fs.StringVar(&jOpts.model, "judge-model", "", "override judge model (default: the planner / main-agent model). A cloud model here needs the key env var below.")
	fs.StringVar(&jOpts.endpoint, "judge-endpoint", "", "override judge API endpoint (default: the provider's, e.g. https://api.anthropic.com)")
	fs.StringVar(&jOpts.keyEnv, "judge-key-env", "ANTHROPIC_API_KEY", "env var holding the override judge's API key (never stored or printed)")
	fs.StringVar(&baselineWrite, "baseline-write", "", "after the run, write a baseline snapshot (per-task scores) to this path")
	fs.StringVar(&baselineCheck, "baseline-check", "", "after the run, compare to the baseline at this path (paired-bootstrap 95% CI; non-zero exit on regression)")
	fs.BoolVar(&trend, "trend", false, "print the nightly-eval trend log (offline; no run) and exit")
	fs.IntVar(&trendN, "trend-n", 20, "with --trend: how many recent entries to show")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "tenant eval — run the eval harness against the current build")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Usage: tenant eval [flags]")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "smoke   — fixture tasks; no model/tools/judge needed (~10s).")
		fmt.Fprintln(fs.Output(), "fitness — live tasks vs a fresh agent + cloud judge (pass plugin flags + --judge-profile).")
		fmt.Fprintln(fs.Output(), "See tasks/eval-harness-plan-v1.md and docs/EVAL.md.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if compactionMode {
		return runCompactionEval(ctx, c, jsonOut)
	}

	if trend { // offline trend reader (TEN-158) — no run, no model
		return runEvalTrend(c, trendN)
	}

	sub := eval.Subset(subset)
	if !sub.IsValid() {
		return fmt.Errorf("invalid subset %q: want smoke | fitness | full", sub)
	}

	if listOnly {
		h, err := eval.LoadHarness(eval.EmbeddedTasks, nil)
		if err != nil {
			return fmt.Errorf("load harness: %w", err)
		}
		listTasks(os.Stdout, h, sub)
		return nil
	}

	rep, err := runEvalToReport(ctx, c, pf, sub, jOpts)
	if err != nil {
		return fmt.Errorf("eval run: %w", err)
	}

	switch {
	case jsonOut:
		return eval.WriteJSON(os.Stdout, rep)
	case quiet:
		fmt.Fprintf(os.Stdout, "eval %s · overall %.1f · passed %d/%d · %dms\n",
			rep.Subset, rep.Aggregates.Overall,
			rep.Aggregates.PassCount,
			rep.Aggregates.PassCount+rep.Aggregates.FailCount,
			rep.Aggregates.TotalElapsed)
	default:
		eval.WriteTerminal(os.Stdout, rep)
	}

	// Baseline snapshot / regression check (after the report is printed so the
	// operator sees scores either way). A flagged regression takes exit-code
	// precedence over the plain pass/fail below — it's the CI signal that matters.
	if err := handleBaseline(rep, baselineWrite, baselineCheck, jOpts.model); err != nil {
		return err
	}

	if !eval.AllPassed(rep) {
		// Non-zero exit so CI catches it. Caller wraps as needed.
		return fmt.Errorf("eval failed: %d of %d tasks did not pass",
			rep.Aggregates.FailCount,
			rep.Aggregates.PassCount+rep.Aggregates.FailCount)
	}
	return nil
}

// handleBaseline writes a baseline snapshot and/or compares the current run to
// one. Compare uses the paired-bootstrap 95% CI in internal/eval; a regression
// (CI upper bound on the per-task delta < 0) returns a non-zero error.
func handleBaseline(rep *eval.Report, writePath, checkPath, judgeProfile string) error {
	if writePath != "" {
		b := eval.NewBaseline(rep, time.Now().UTC().Format(time.RFC3339), judgeProfile, "")
		f, err := os.Create(writePath)
		if err != nil {
			return fmt.Errorf("eval: create baseline %s: %w", writePath, err)
		}
		defer f.Close()
		if err := b.WriteJSON(f); err != nil {
			return fmt.Errorf("eval: write baseline: %w", err)
		}
		fmt.Fprintf(os.Stderr, "eval: wrote baseline (%d tasks, overall %.1f) → %s\n",
			len(rep.Results), rep.Aggregates.Overall, writePath)
	}
	if checkPath != "" {
		data, err := os.ReadFile(checkPath)
		if err != nil {
			return fmt.Errorf("eval: read baseline %s: %w", checkPath, err)
		}
		base, err := eval.ReadBaseline(data)
		if err != nil {
			return err
		}
		rr := eval.CompareToBaseline(base, rep, eval.CompareOptions{})
		fmt.Fprintf(os.Stderr, "eval: baseline-check — paired %d, Δ %.2f, 95%% CI [%.2f, %.2f], regressed=%v\n",
			rr.PairedCount, rr.Delta, rr.CILow, rr.CIHigh, rr.Regressed)
		if len(rr.MissingIDs) > 0 || len(rr.NewIDs) > 0 {
			fmt.Fprintf(os.Stderr, "eval: baseline-check — %d task(s) only in baseline, %d new since\n",
				len(rr.MissingIDs), len(rr.NewIDs))
		}
		if rr.Regressed {
			return fmt.Errorf("eval: REGRESSION vs baseline — Δ %.2f, 95%% CI upper bound %.2f < 0", rr.Delta, rr.CIHigh)
		}
	}
	return nil
}

// runCompactionEval scores the CURRENT context-compaction compressor's fidelity
// (needle / continuation / drift) against the configured model — the TEN-99
// baseline. It builds the same compress.Compressor the agent uses and runs the
// internal/eval/compaction probes; --json emits the machine-readable report.
func runCompactionEval(ctx context.Context, c *commonFlags, jsonOut bool) error {
	if err := c.resolve(); err != nil {
		return err
	}
	log := newLogger()
	router, err := buildRouter(c, log)
	if err != nil {
		return fmt.Errorf("eval: build router: %w", err)
	}
	comp := &compress.Compressor{Router: router, Logger: log}
	// Prefer the planner model's own token counter so tokens-saved matches what
	// the agent actually budgets; fall back to the char/4 estimate if it can't
	// be resolved (e.g. model offline).
	count := compaction.EstimateTokens
	if llm, _, perr := router.LLMForRole(ctx, model.RolePlanner); perr == nil && llm != nil {
		count = func(s string) int {
			n, terr := llm.TokenCount(ctx, s)
			if terr != nil {
				return compaction.EstimateTokens(s)
			}
			return n
		}
	}
	rep, err := compaction.Evaluate(ctx, comp, count, compaction.Options{})
	if err != nil {
		return fmt.Errorf("eval: compaction: %w", err)
	}
	if jsonOut {
		return compaction.WriteJSON(os.Stdout, rep)
	}
	compaction.WriteTerminal(os.Stdout, rep)
	return nil
}

// runEvalToReport loads the harness and runs the subset, wiring the live agent
// factory + judge for fitness/full (smoke is fixture-only). Shared by the CLI
// (cmdEval) and the nightly Job so both run the eval identically; the live
// runtime (router + tool mux) is built and torn down within this call.
func runEvalToReport(ctx context.Context, c *commonFlags, pf *pluginFlags, sub eval.Subset, jOpts evalJudgeOpts) (*eval.Report, error) {
	return runEvalToReportWithSoul(ctx, c, pf, sub, jOpts, nil)
}

// runEvalToReportWithSoul is runEvalToReport with an optional soul override:
// when soulOverride is non-nil the live agents use it instead of the on-disk
// soul. SoulNudgeJob (TEN-16) uses this to A/B-score a candidate soul against
// the committed fitness baseline. nil ⇒ identical to runEvalToReport.
func runEvalToReportWithSoul(ctx context.Context, c *commonFlags, pf *pluginFlags, sub eval.Subset, jOpts evalJudgeOpts, soulOverride *soul.Soul) (*eval.Report, error) {
	h, err := eval.LoadHarness(eval.EmbeddedTasks, nil)
	if err != nil {
		return nil, fmt.Errorf("load harness: %w", err)
	}
	if sub == eval.SubsetFitness || sub == eval.SubsetFull {
		cleanup, err := wireLiveHarness(ctx, h, c, pf, sub, jOpts, soulOverride)
		if err != nil {
			return nil, err
		}
		defer cleanup()
	}
	return h.Run(ctx, sub)
}

// evalJudgeOpts configures the live-mode LLM judge. The API key is read from
// keyEnv at run time — never a flag, never persisted, never printed.
type evalJudgeOpts struct {
	gateOnly bool   // skip the judge entirely (deterministic gate only)
	model    string // override judge model id ("" → use the planner / main-agent model)
	endpoint string // override judge API endpoint ("" → provider default)
	keyEnv   string // env var holding the override judge's API key
}

// autoEnableEvalPlugins turns on plugin flags for zero-config plugins (web, os)
// and detects available stateful plugins (sql, wiki) by probing their configured
// paths. Plugins that need interactive auth (gsuite, atlassian, x, imessage,
// discord) are never auto-enabled — eval is non-interactive, so a browser OAuth
// flow would hang. Only flags the operator did NOT set explicitly are touched, so
// `tenant eval --no-web` still works as an explicit override.
//
// Without this, every plugin flag defaults to false/off, and the agent enters
// eval with zero tools — every task that expects a tool call (web_navigate,
// sql_query, os_list_dir, wiki_search, etc.) fails at the must_call gate.
func autoEnableEvalPlugins(pf *pluginFlags, c *commonFlags, explicitFlags map[string]bool) []string {
	var enabled []string

	// web: always available if Chrome is installed. No config needed.
	if !explicitFlags["web"] && !pf.web {
		pf.web = true
		enabled = append(enabled, "web")
	}

	// os: always available (pure Go, no external deps).
	if !explicitFlags["os"] && !pf.osEnable {
		pf.osEnable = true
		pf.osAllowExec = true  // eval needs real tool execution
		pf.osAllowWrite = true // file-write tasks need this
		enabled = append(enabled, "os")
	}

	// sql: auto-detect the operator's real DB if they have one.
	if !explicitFlags["sql-db"] && pf.sqlDB == "" {
		candidate := ""
		if c.dataDir != "" {
			candidate = findFirstExisting(
				filepath.Join(c.dataDir, "tenant.db"),
				filepath.Join(c.dataDir, "eval.db"),
			)
		}
		if candidate != "" {
			pf.sqlDB = candidate
			pf.sqlAllowWrite = true
			enabled = append(enabled, "sql")
		}
	}

	// wiki: use the same wiki dir the operator configured in launchConfig.
	if !explicitFlags["wiki-dir"] && pf.wikiDir == "" {
		if lc, err := loadLaunchConfig(c.cfgDir); err == nil && lc != nil {
			if sk, ok := lc.Skills["wiki"]; ok && sk.Enabled {
				if dir := sk.Settings["dir"]; dir != "" {
					if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
						pf.wikiDir = dir
						enabled = append(enabled, "wiki")
					}
				}
			}
		}
	}

	// atlassian: auto-enable if /configure already set it up (has a site + auth).
	// The activator in buildToolMux handles the token-based or OAuth path from
	// saved config; no browser popup needed for token auth.
	if !explicitFlags["atlassian"] && !pf.atlassian {
		if lc, err := loadLaunchConfig(c.cfgDir); err == nil && lc != nil {
			if sk, ok := lc.Skills["atlassian"]; ok && sk.Enabled && sk.Settings != nil {
				if sk.Settings["site"] != "" {
					pf.atlassian = true
					pf.atlassianSite = sk.Settings["site"]
					pf.atlassianProject = sk.Settings["project"]
					pf.atlassianAllowWrite = true
					// Token auth: load email + api_token from config/credentials.
					if sk.Settings["auth"] == "token" {
						pf.atlassianEmail = sk.Settings["email"]
						if creds, cerr := loadCredentials(c.cfgDir); cerr == nil {
							pf.atlassianClientID = creds.get(skillSecretID("atlassian", "client_id"))
						}
					} else {
						// OAuth path: needs client_id from config.
						pf.atlassianClientID = sk.Settings["client_id"]
					}
					enabled = append(enabled, "atlassian")
				}
			}
		}
	}

	return enabled
}

// findFirstExisting returns the first path that exists as a file, or "".
func findFirstExisting(candidates ...string) string {
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// wireLiveHarness configures h for live-mode execution. It builds the model
// router and the real tool mux ONCE (shared across tasks — they're read-mostly
// w.r.t. a per-task agent and execute against the real world), installs an
// AgentFactory that hands each task a freshly-isolated agent, and — unless
// gateOnly — installs the LLM judge (the planner / main-agent model by
// default; --judge-model overrides with a separate, e.g. cloud, model).
// Returns a cleanup func (closes the tool mux's browser/db handles + archive).
func wireLiveHarness(ctx context.Context, h *eval.Harness, c *commonFlags, pf *pluginFlags, sub eval.Subset, jOpts evalJudgeOpts, soulOverride *soul.Soul) (func(), error) {
	if err := c.resolve(); err != nil {
		return nil, err
	}
	log := newLogger()
	router, err := buildRouter(c, log)
	if err != nil {
		return nil, fmt.Errorf("eval: build router: %w", err)
	}
	// Auto-enable plugins the operator has available but didn't flag. Without
	// this, every plugin defaults to off and the agent has no tools — scoring
	// 0 on every task that expects a tool call. Only touches flags the operator
	// did NOT set explicitly (so --no-web is respected).
	autoEnabled := autoEnableEvalPlugins(pf, c, c.flagSet())
	if len(autoEnabled) > 0 {
		fmt.Fprintf(os.Stderr, "eval: auto-enabled plugins: %s\n", strings.Join(autoEnabled, ", "))
	}
	// confirm=nil: eval is non-interactive, so gated destructive tools stay
	// off unless the operator passed their explicit allow flags (--os-allow-*,
	// etc.). buildToolMux only constructs the plugins whose flags are set.
	mux, _, closeTools, err := buildToolMux(ctx, c, router, pf, nil, log)
	if err != nil {
		return nil, fmt.Errorf("eval: build tools: %w", err)
	}
	// Soul loaded once, shared read-only (Turn renders but never mutates it).
	// A non-nil soulOverride (SoulNudgeJob candidate, TEN-16) wins over disk.
	sl := soulOverride
	if sl == nil {
		s, lerr := soul.Load(c.cfgDir, c.agent)
		if lerr != nil {
			s = soul.NewDefault(c.agent)
		}
		sl = s
	}
	// One throwaway archive dir for the whole run — eval turns must not
	// pollute the operator's real event log under <data>.
	arcDir, err := os.MkdirTemp("", "tenant-eval-*")
	if err != nil {
		closeTools()
		return nil, fmt.Errorf("eval: temp archive dir: %w", err)
	}
	arc := archive.NewWriter(arcDir)
	cleanup := func() {
		closeTools()
		_ = os.RemoveAll(arcDir)
	}

	// Embedder for memory-fixture seeding — the SAME one the agent queries
	// with, so seeded facts/episodes live in the same vector space and are
	// actually retrievable. Always resolves (real embedder or echo fallback).
	emb, embProf, eerr := router.EmbedderForRole(ctx, model.RoleEmbedder)
	if eerr != nil {
		cleanup()
		return nil, fmt.Errorf("eval: resolve embedder for memory seeding: %w", eerr)
	}
	embedderID := string(embProf.ID)
	tasksByID := make(map[string]*eval.Task, len(h.Tasks))
	for _, t := range h.Tasks {
		tasksByID[t.ID] = t
	}

	h.AgentFactory = func(fctx context.Context, taskID string) (eval.AgentRunner, func() error, error) {
		// Ephemeral per-task memory: fresh in-memory SQLite so retrieval
		// can't leak one task's turns into another, and the operator's real
		// stores are never written.
		es, err := episodic.Open(":memory:")
		if err != nil {
			return nil, nil, fmt.Errorf("eval task %s: episodic: %w", taskID, err)
		}
		ss, err := semantic.Open(":memory:")
		if err != nil {
			_ = es.Close()
			return nil, nil, fmt.Errorf("eval task %s: semantic: %w", taskID, err)
		}
		agentID := "eval-" + taskID
		// Memory-recall tasks pre-seed facts/episodes before the turn.
		if t := tasksByID[taskID]; t != nil && (len(t.InjectedFacts) > 0 || len(t.InjectedEpisodes) > 0) {
			if serr := seedTaskMemory(fctx, es, ss, emb, embedderID, agentID, t); serr != nil {
				_ = es.Close()
				_ = ss.Close()
				return nil, nil, fmt.Errorf("eval task %s: seed memory: %w", taskID, serr)
			}
		}
		ag, aerr := agent.New(agent.Config{
			AgentID:    agentID,
			Router:     router,
			Soul:       sl,
			Working:    working.New(),
			Archive:    arc,
			Episodic:   es,
			Semantic:   ss,
			Tools:      mux,
			Dispatcher: mux,
			Logger:     log,
		})
		if aerr != nil {
			_ = es.Close()
			_ = ss.Close()
			return nil, nil, fmt.Errorf("eval task %s: build agent: %w", taskID, aerr)
		}
		taskCleanup := func() error {
			_ = es.Close()
			_ = ss.Close()
			return nil
		}
		return &evalAgentRunner{ag: ag}, taskCleanup, nil
	}

	if jOpts.gateOnly {
		fmt.Fprintln(os.Stderr, "eval: --gate-only — scoring on the deterministic gate only; answer QUALITY is not judged.")
		return cleanup, nil
	}

	// Default judge = the planner (main-agent) model. Zero extra setup: the
	// model you already run grades the answers. It IS self-judging (self-bias
	// is linearly correlated with self-recognition; eval plan §3a), so we say
	// so plainly — but the operator chose this default; --judge-model is the
	// escape hatch to a separate model.
	if jOpts.model == "" {
		plannerLLM, plannerProf, perr := router.LLMForRole(ctx, model.RolePlanner)
		if perr != nil {
			cleanup()
			return nil, fmt.Errorf("eval: resolve planner as default judge: %w", perr)
		}
		fmt.Fprintf(os.Stderr, "eval: judge = planner model %q (self-judging; pass --judge-model <id> for a separate judge).\n", plannerProf.Model)
		h.Judge = &eval.LLMJudge{LLM: plannerLLM, Profile: plannerProf}
		return cleanup, nil
	}

	// Override judge (e.g. a cloud Claude). Key from an env var — never a flag,
	// never stored, never printed.
	key := os.Getenv(jOpts.keyEnv)
	if key == "" {
		cleanup()
		return nil, fmt.Errorf("override judge %q needs an API key in $%s.\n"+
			"  set it (PowerShell):  $env:%s=\"<your key>\"   (bash: export %s=...)\n"+
			"  or drop --judge-model to use the planner / main-agent model as the judge", jOpts.model, jOpts.keyEnv, jOpts.keyEnv, jOpts.keyEnv)
	}
	if err := attachAnthropicJudge(router, jOpts.endpoint, jOpts.model, key); err != nil {
		cleanup()
		return nil, fmt.Errorf("eval: configure override judge: %w", err)
	}
	judgeLLM, judgeProf, jerr := router.LLMForRole(ctx, model.RoleJudge)
	if jerr != nil {
		cleanup()
		return nil, fmt.Errorf("eval: resolve override judge: %w", jerr)
	}
	if _, plannerProf, perr := router.LLMForRole(ctx, model.RolePlanner); perr == nil && plannerProf.Model == judgeProf.Model {
		fmt.Fprintf(os.Stderr, "eval: note — override judge %q matches the planner; that's the same as the default (self-judging).\n", judgeProf.Model)
	}
	h.Judge = &eval.LLMJudge{LLM: judgeLLM, Profile: judgeProf}
	return cleanup, nil
}

// evalAgentRunner adapts a *agent.Agent to eval.AgentRunner: one Turn per task
// prompt, with the tool trace mapped back into the eval gate's call shape.
type evalAgentRunner struct{ ag *agent.Agent }

func (r *evalAgentRunner) Run(ctx context.Context, prompt string) (string, []eval.FixtureToolCall, error) {
	res, err := r.ag.Turn(ctx, agent.TurnRequest{UserQuery: prompt})
	if err != nil {
		return "", nil, err
	}
	calls := make([]eval.FixtureToolCall, 0, len(res.ToolTrace))
	for _, tc := range res.ToolTrace {
		calls = append(calls, eval.FixtureToolCall{Tool: tc.Call.Name, Args: string(tc.Call.Arguments)})
	}
	// res.Error (loop ceiling / validation) is intentionally NOT surfaced as a
	// hard error: the forced-synthesis response + tool trace are still the
	// agent's real output, and the gate/judge should score them on merit.
	return res.Response, calls, nil
}

// seedTaskMemory pre-loads a task's injected facts/episodes into its fresh
// ephemeral stores so memory-recall tasks have something to retrieve. Texts are
// embedded with the same embedder the agent queries with; episodes are stamped
// 5 minutes before now so they read as recent prior turns.
func seedTaskMemory(ctx context.Context, es *episodic.Store, ss *semantic.Store, emb model.Embedder, embedderID, agentID string, t *eval.Task) error {
	embed := func(text string) ([]float32, error) {
		vecs, err := emb.Embed(ctx, []string{text})
		if err != nil {
			return nil, err
		}
		if len(vecs) == 0 || len(vecs[0]) == 0 {
			return nil, fmt.Errorf("embedder returned an empty vector")
		}
		return vecs[0], nil
	}
	for _, f := range t.InjectedFacts {
		vec, err := embed(f.Text)
		if err != nil {
			return err
		}
		conf := f.Confidence
		if conf == 0 {
			conf = 1.0
		}
		if _, err := ss.Insert(ctx, &semantic.Fact{
			AgentID:    agentID,
			Fact:       f.Text,
			Confidence: conf,
			EmbedderID: embedderID,
			Embedding:  vec,
		}); err != nil {
			return err
		}
	}
	seedTS := time.Now().UTC().Add(-5 * time.Minute)
	for _, ep := range t.InjectedEpisodes {
		vec, err := embed(ep.Prompt + "\n" + ep.Response)
		if err != nil {
			return err
		}
		if _, err := es.Insert(ctx, &episodic.Episode{
			AgentID:    agentID,
			Prompt:     ep.Prompt,
			Response:   ep.Response,
			Tags:       ep.Tags,
			Outcome:    ep.Outcome,
			EmbedderID: embedderID,
			Embedding:  vec,
			Timestamp:  seedTS,
		}); err != nil {
			return err
		}
	}
	return nil
}

func listTasks(w io.Writer, h *eval.Harness, sub eval.Subset) {
	tasks := h.FilterSubset(sub)
	fmt.Fprintf(w, "%d task(s) in subset %s:\n", len(tasks), sub)
	for _, t := range tasks {
		fmt.Fprintf(w, "  %-40s %s\n", t.ID, t.Category)
	}
}
