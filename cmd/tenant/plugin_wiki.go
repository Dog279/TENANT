package main

import (
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/wiki"
)

// wikiPlugin declares the wiki plugin: the agent answers from a markdown
// knowledge base, and saves notes into it (wiki_save) when the operator
// approves. Configured by path (--wiki-dir, or `tenant setup`).
func wikiPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:      "wiki",
		Title:   "Wiki (markdown knowledge base)",
		Version: 1,
		Tools: func() []model.ToolSpec {
			return append(wiki.NewDispatcher(nil).Tools(), (&wikiSaveDispatcher{}).Tools()...)
		},
		Configure: func() skillKind {
			return skillKind{ID: "wiki", Label: "Wiki (markdown knowledge base)", Wired: true, Fields: []skillKindField{
				{Key: "dir", Prompt: "Wiki markdown directory"},
			}}
		},
		SetupOnly: true,
		Setup:     1,
		Actions:   map[string]string{"wiki_save": catDestructive},
		StubHint:  "relaunch with --wiki-dir <dir>",
		Doctor: []doctorCheck{
			// The configured wiki dir is reachable.
			{"wiki dir (if configured)", checkWikiDir},
			// TEN-48: detect "file on disk newer than sidecar index" — the
			// proximate diagnostic for the 2026-05-26 lost-context bug
			// (TEN-43). Independent of TEN-44's fix because that only
			// covered the research deposit path; manual `cp` to wikiDir or
			// future producers can still leave the index stale.
			{"wiki freshness", checkWikiFreshness},
		},
		CLI: []*cliCommand{
			{Names: []string{"wiki"}, Section: "plugins", Run: cmdWiki,
				Summary: "the agent answers from a markdown knowledge base",
				Rows:    []cmdreg.Row{{Syntax: `wiki "<q>" --dir DIR`, Desc: "the agent answers from a markdown knowledge base (read-only)"}}},
		},
	}
}
