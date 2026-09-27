package improve

import "tenant/internal/logs/event"

// Improve is the self-improvement scheduler's event provider (docs/EVENTS.md).
// Each job run is an activity, so the facts, skills and warnings it writes
// carry its activity id; its result names the runs it learned from.
var Improve = event.NewProvider("improve", "improve")

// Job runs (1000s).

var JobRun = Improve.Event(1001, event.Info, "Job run", "Self-improvement job {job}", "job").
	Doc("A self-improvement job ran: its start, then its stop with the duration, or its failure with the error.",
		"The scheduler found it due (serve, or the TUI with self-improvement on), or the operator asked for a run.",
		"None when it stops. On a failure, read the error, and the job's warnings under the same activity.")

var JobOutcome = Improve.Event(1002, event.Info, "Job run", "{job}: {summary}", "job", "summary", "changed", "source_runs").
	Doc("What one run of a self-improvement job did. source_runs are the turns (runs.db trace ids) it learned from.",
		"Every run that does not fail reports its result, whether it changed anything or not.",
		"None. To see what a run wrote, follow its activity to the memory and skills events it caused.")

// Warnings (2000s).

var StepFailed = Improve.Event(2001, event.Warning, "Job run", "{job}: {step} failed; the run went on: {err}", "job", "step", "err", "fact_id", "section", "size").
	Doc("A best-effort step of a self-improvement job failed, and the job skipped it.",
		"The model, or a memory database the step writes to, returned an error.",
		"None if it is rare: the next run tries again. If it repeats, check the model and the memory databases with tenant doctor.")

var MergeUnprotected = Improve.Event(2002, event.Warning, "Consolidation", "Consolidation could not tell which facts are protected, so it may merge any of {facts} facts: {err}", "facts", "err").
	Doc("Consolidation ran without leaving out pinned and protected facts, so one of them may be merged into a broader fact.",
		"The fact signals table in facts.db could not be read.",
		"Check facts.db with tenant doctor. A fact merged by mistake can be restored from its page in the dashboard.")

var ReflectFailed = Improve.Event(2003, event.Warning, "Reflection", "Reflection wrote nothing: the synthesis failed: {err}", "err").
	Doc("The reflection job did not update the project's SME document; the previous version stays in use.",
		"The proposer model failed, or answered with something that is not the expected JSON.",
		"Check the model set by improve.profile (or the main model). The next run tries again.")

var SectionTruncated = Improve.Event(2004, event.Warning, "Reflection", "Reflection cut SME section {section} to {cap} tokens; it was about {tokens}", "section", "tokens", "cap").
	Doc("A section of the SME document was longer than its cap, so its end was cut off.",
		"The model wrote more than the per-section limit allows.",
		"None usually. If details go missing from the section, raise its token cap.")

var SoulScoreFailed = Improve.Event(2005, event.Warning, "Soul nudge", "Soul nudge proposed nothing: the candidate could not be scored: {err}", "err").
	Doc("The soul-nudge job had a candidate, but could not run the fitness check that must pass before it is proposed.",
		"There is no fitness baseline yet, or the eval run failed.",
		"Capture a fitness baseline (tenant eval) if there is none; otherwise read the error. Nothing is proposed until it passes.")

var ProposerFallback = Improve.Event(2006, event.Warning, "Configuration", "improve.profile {profile} is not usable, so the jobs stay on the main model: {reason}", "profile", "reason", "err").
	Doc("The self-improvement jobs run on the main model instead of the profile improve.profile names.",
		"The profile is not in config.json, or its model could not be set up.",
		"Fix the profile's entry in config.json, or clear improve.profile.")
