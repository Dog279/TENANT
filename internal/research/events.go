package research

import "tenant/internal/logs/event"

// Events is the /research pipeline's event provider. Its sub-agents' events
// are the subagents provider's, related to the run.
var Events = event.NewProvider("research", "research")

// RunActivity is one research pass, as an activity: start with the question,
// then stop or fail with its duration. run is its ID in the research store
// (tenant research show <run>), when it has one.
var RunActivity = Events.Event(1, event.Info, "Research run", "Research run {run}", "run", "question").
	Sensitive("question").
	Doc("A research pass started, or ended: a stop or a fail, with its duration. Its sub-agents' activities name it as related.",
		"The operator ran /research or tenant research, or replayed a run. It fails when the planner can't be reached or no sub-agent found anything usable.",
		"None when it stops. When it fails, read the error; tenant research show <run> has each sub-agent's findings.")

var RecordFailed = Events.Event(2001, event.Warning, "Record", "Research record not saved: could not {op}: {err}", "run", "op", "err").
	Doc("The research ran, but its record in the research store is missing or incomplete, so tenant research list/show and replay won't have all of it.",
		"The research directory under the data directory could not be written: the disk is full, or its permissions are wrong.",
		"Check free space and the data directory's permissions.")

var WikiFailed = Events.Event(2002, event.Warning, "Wiki", "Research report not saved to the wiki: could not {op}: {err}", "op", "err").
	Doc("The report was written, but not deposited in the wiki, or not indexed there, so wiki_search won't find it yet.",
		"The wiki directory could not be written, or its index could not be rebuilt.",
		"Check the wiki directory (--wiki or the wiki plugin's configuration); an unindexed report is picked up at the next wiki search.")
