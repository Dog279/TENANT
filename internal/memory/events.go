package memory

import "tenant/internal/logs/event"

// Memory is the memory tiers' event provider (docs/EVENTS.md): changes to
// facts, compaction, distillation's warnings and retrieval diagnostics. Fact
// events carry fact_id, the id the dashboard's provenance inspector opens at
// /memory/facts/{fact_id}. A fact's text is personal content: it is kept as a
// sensitive field, never in the message.
var Memory = event.NewProvider("memory", "memory")

// Facts (1000s).

var FactAdded = Memory.Event(1001, event.Info, "Fact", "Fact {fact_id} added by {job}", "fact_id", "job", "agent", "source_runs", "fact").
	Sensitive("fact").
	Doc("A fact was written to long-term memory (facts.db). source_runs are the turns it was learned from.",
		"Distillation or consolidation learned it, the agent or the operator saved it, an import brought it, or an edit or restore made a new version.",
		"None. To see where it came from, open `/memory/facts/<fact_id>` in the dashboard.")

var FactSuperseded = Memory.Event(1002, event.Info, "Fact", "Fact {fact_id} superseded by fact {by}", "fact_id", "by").
	Doc("A fact was replaced by a newer one. It stays in facts.db for its history, but recall no longer uses it.",
		"Distillation learned something that contradicts it, consolidation merged it, or it was edited or resolved as a duplicate.",
		"None. Its versions are on `/memory/facts/<fact_id>` in the dashboard, where an earlier one can be restored.")

var FactTombstoned = Memory.Event(1003, event.Info, "Fact", "Fact {fact_id} removed", "fact_id").
	Doc("A fact was removed from memory: recall no longer uses it, but it can still be restored.",
		"The operator or the agent forgot it.",
		"None. To undo it, restore it from the dashboard's removed-facts page.")

var FactRestored = Memory.Event(1004, event.Info, "Fact", "Fact {fact_id} restored", "fact_id").
	Doc("A removed fact was brought back, and recall uses it again.",
		"The operator restored it.",
		"None.")

// Compaction (1100s).

var Compacted = Memory.Event(1101, event.Info, "Compaction", "Compacted a conversation from {before} messages to {after}", "session", "before", "after", "origin", "source_messages").
	Doc("The oldest turns of a conversation were summarized into one message, so it fits the model's context again. origin says whether the summary was made from the archive's raw events or from the working set.",
		"The conversation filled most of its share of the context window, or the operator ran /compress.",
		"None. If answers lose the thread after this, lower the model's context use or raise its context length.")

// Distillation warnings (2000s).

var DistillBatchSkipped = Memory.Event(2001, event.Warning, "Distillation", "Distillation skipped {episodes} episodes from #{first_episode} at {step}: {err}", "step", "first_episode", "episodes", "agent", "err").
	Doc("Distillation could not learn facts from a batch of turns. It moved past them, so their facts are not extracted.",
		"The summarizer model failed or answered with something that is not the expected JSON (extract), or embedding or writing the facts failed (persist).",
		"Check the summarizer model (tenant doctor). If it keeps happening, the batch size may be too large for the model.")

var DistillStepFailed = Memory.Event(2002, event.Warning, "Distillation", "Distillation: {step} failed for fact {fact_id}: {err}", "step", "fact_id", "by", "err").
	Doc("A best-effort write after distilling a fact failed; the fact itself was kept.",
		"facts.db refused the write: it is busy, full or damaged.",
		"If it repeats, check facts.db with tenant doctor.")

var BorderlineUndecided = Memory.Event(2003, event.Warning, "Distillation", "Could not tell whether a new fact repeats fact {fact_id}; added it anyway: {err}", "fact_id", "err").
	Doc("Distillation added a fact that may repeat or contradict an existing one, because the model could not judge them.",
		"The summarizer model failed or gave an answer that could not be read.",
		"None usually: consolidation merges duplicates later. If it repeats, check the summarizer model.")

// Retrieval diagnostics (9000s).

var Retrieved = Memory.Event(9001, event.Info, "Retrieval", "Retrieved {facts} facts and {episodes} episodes for the turn", "facts", "episodes", "fact_ids", "episode_ids", "truncations").
	Diagnostic().
	Doc("What long-term memory put into a turn's context, after its budget cut: the facts and past turns by id.",
		"Every turn retrieves memory.",
		"None. When an answer ignores something memory holds, check whether its fact was retrieved here.")

var Recalled = Memory.Event(9002, event.Info, "Retrieval", "memory_recall found {facts} facts, {episodes} episodes and {archived} archive events", "query", "scope", "facts", "episodes", "archived", "err").
	Sensitive("query").
	Diagnostic().
	Doc("The agent searched its memory with memory_recall. err holds a store's search error, which the agent never sees.",
		"The agent decided it needed something from earlier conversations.",
		"None; on an err, check the memory databases with tenant doctor.")
