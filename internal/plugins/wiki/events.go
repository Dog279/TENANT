package wiki

import (
	"tenant/internal/logs/event"
	"tenant/internal/pluginreg"
)

// Events is the wiki's event catalog (TEN-393), declared by its descriptor.
var Events = pluginreg.NewEvents("wiki", "Wiki")

var EmbedderProbeFailed = Events.Provider.Event(2101, event.Warning, "Index", "Wiki embedder unreachable at start; using the declared dimension {declared_dim}: {err}", "declared_dim", "err").
	Doc("The embedding model did not answer when the wiki started, so the index uses the dimension its profile declares and reindexes on the first search.",
		"The embedder's server is down, still loading, or unreachable.",
		"Check the embedder role's backend (`tenant doctor`). The wiki heals on its own once it answers.")

var ReindexFailed = Events.Provider.Event(2102, event.Warning, "Index", "Wiki initial reindex failed; it reindexes on the first search: {err}", "err").
	Doc("The notes changed since the index was saved, and they could not be embedded at start, so searches may miss them until the first search reindexes.",
		"The embedder is down, or a note could not be read.",
		"Check the embedder role's backend; or run `tenant wiki reindex` once it answers.")
