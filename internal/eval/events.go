package eval

import "tenant/internal/logs/event"

// Events is the eval harness's event provider, writing the eval log. IDs
// follow the blocks in internal/logs/event's package doc. `tenant eval`'s
// table and summary stay the command's output; these are the record.
var Events = event.NewProvider("eval", "eval")

// Runs.

// Run is the activity of one eval run, nightly or manual: start, then stop
// with its scores. Its tasks are child activities.
var Run = Events.Event(1001, event.Info, "Run", "Eval run of {subset}, {tasks} tasks", "subset", "tasks", "overall", "passed", "failed", "ungraded", "skipped").
	Doc("An eval run: start, then stop with its overall score and counts, or fail when it was cancelled.",
		"The nightly schedule (improve.eval_every or eval_at) came due, someone ran it now (the dashboard, /eval), `tenant eval` ran, or the soul nudge scored a candidate.",
		"None. Compare overall across runs in the trend (`tenant eval --trend`); a regression is reported on its own (eval/2003).")

var TaskRun = Events.Event(1002, event.Info, "Task", "Eval task {task}", "task", "category", "mode", "result", "score").
	Doc("One task of an eval run: start, then stop with its result (pass, fail, ungraded or skipped) and score.",
		"Its run reached it.",
		"For a failing task, open its report in <data>/eval-artifacts, or run `tenant eval --baseline-diff` for the movers.")

var TrendUpdated = Events.Event(1003, event.Info, "Trend", "Eval trend: {subset} scored {overall}, {passed}/{total} passed", "subset", "overall", "passed", "total", "regressed", "delta", "artifact").
	Doc("A finished run's line was added to the trend (trend.jsonl), with its verdict against the baseline.",
		"A nightly run finished, or `tenant eval --append-trend` ran.",
		"None. `tenant eval --trend` shows the series.")

// Warnings.

var TaskUngraded = Events.Event(2001, event.Warning, "Task", "Eval task {task} was not graded ({class}): {reason}", "task", "class", "reason").
	Doc("A task was left out of the score because the harness, the model provider or the Quality model failed, not the agent.",
		"class says which: harness (the task's agent couldn't be built), quota, endpoint, ratelimit or provider (the model), or judge (the Quality model, the grader, was unusable after its retry).",
		"Fix the named piece (a provider's quota or endpoint, the Quality model) and run again. Many ungraded tasks make a run's score unreliable.")

var BaselineMissing = Events.Event(2002, event.Warning, "Regression", "No eval baseline at {path}: regressions are not checked", "path").
	Doc("A run finished with no baseline to compare against, so the regression gate is off.",
		"No baseline was ever captured, or it lives somewhere the run doesn't look.",
		"Capture one from a good run: `tenant eval --baseline-from <data>/eval-artifacts/eval-<time>.json`.")

var Regression = Events.Event(2003, event.Warning, "Regression", "Eval {subset} regressed against its baseline: Δ {delta}, 95% CI upper bound {ci_high}", "subset", "delta", "ci_high", "overall", "artifact").
	Doc("A run scored worse than its baseline, beyond chance (the paired bootstrap's 95% interval is below zero).",
		"A change to the prompt, the model, routing, the tools or memory made the agent worse, or the environment lost something tasks need.",
		"Run `tenant eval --baseline-diff` to see which tasks moved and why; fix the cause, or capture a new baseline if the change is intended.")

var BaselineUnreadable = Events.Event(2004, event.Warning, "Regression", "Eval baseline {path} could not be read: {err}", "path", "err").
	Doc("The baseline exists but doesn't parse, so this run has no regression verdict.",
		"The file was edited by hand, truncated, or written by an incompatible version.",
		"Capture it again: `tenant eval --baseline-from <a good artifact>`.")

var NotSaved = Events.Event(2005, event.Warning, "Run", "Eval {record} was not saved: {err}", "record", "path", "err").
	Doc("A run finished, but its report artifact or its trend line didn't reach <data>/eval-artifacts.",
		"The data directory is full, unwritable or locked.",
		"Check the data directory's free space and permissions. The run's scores are still in this log (eval/1001).")

var ScheduleIgnored = Events.Event(2006, event.Warning, "Schedule", "Schedule {value} is malformed and ignored: {effect}", "value", "effect", "err").
	Doc("An eval cadence setting didn't parse, so it was ignored rather than guessed at.",
		"improve.eval_at is not HH:MM, or an improve.*_every duration is not like 24h.",
		"Set it again with /eval every <duration> or /eval at HH:MM (applies live), or fix it in config.json.")

// Errors.

var ClassifierPanicked = Events.Event(3001, event.Error, "Task", "The route classifier panicked in an eval task: {panic}", "panic", "stack").
	Doc("A bug in the route classifier was caught while a route task measured it; that sample counts as a failed classification.",
		"A programming error in the classifier.",
		"Report it with the stack. The rest of the run goes on.")
