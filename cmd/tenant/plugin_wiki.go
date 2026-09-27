package main

import (
	"context"
	"flag"
	"os"
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/wiki"
)

// wikiPlugin declares the wiki plugin: the agent answers from a markdown
// knowledge base, and saves notes into it (wiki_save) when the operator
// approves. Configured by path (--wiki-dir, or `tenant setup`).
func wikiPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "wiki",
		Flags:       bindWikiFlags,
		On:          func(pf *pluginFlags) bool { return wikiFlagsOf(pf).dir != "" },
		ApplyConfig: applyWikiConfig,
		ExpandPaths: expandWikiPaths,
		Launch:      launchWiki,
		Prepare:     prepareWiki,
		Eval:        evalWiki,
		Title:       "Wiki (markdown knowledge base)",
		Version:     1,
		Tools: func() []model.ToolSpec {
			return append(wiki.NewDispatcher(nil).Tools(), (&wikiSaveDispatcher{}).Tools()...)
		},
		Configure: func() skillKind {
			return skillKind{ID: "wiki", Label: "Wiki (markdown knowledge base)", Wired: true, Fields: []skillKindField{
				{Key: "dir", Prompt: "Wiki markdown directory"},
			}}
		},
		SetupOnly:  true,
		SetupOrder: 1,
		Actions:    map[string]string{"wiki_save": catDestructive},
		StubHint:   "relaunch with --wiki-dir <dir>",
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

// wikiFlags are the wiki plugin's launch flags.
type wikiFlags struct {
	dir string
}

func wikiFlagsOf(pf *pluginFlags) *wikiFlags { return flagsOf[wikiFlags](pf, "wiki") }

func bindWikiFlags(fs *flag.FlagSet) any {
	f := &wikiFlags{}
	fs.StringVar(&f.dir, "wiki-dir", "", "enable the wiki plugin over this markdown dir")
	return f
}

func applyWikiConfig(pf *pluginFlags, s savedConfig) {
	if f := wikiFlagsOf(pf); !s.set["wiki-dir"] {
		f.dir = firstNonEmpty(f.dir, s.get("dir"))
	}
}

func expandWikiPaths(pf *pluginFlags) { f := wikiFlagsOf(pf); f.dir = expandPath(f.dir) }

// launchWiki indexes the vault named by --wiki-dir and hands the index to the
// callers that reindex after writing (TEN-44).
func launchWiki(ctx context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := wikiFlagsOf(e.pf)
	if f.dir == "" {
		return nil, nil, nil
	}
	ix, err := buildWikiIndex(ctx, e.c, e.router, f.dir, e.log)
	if err != nil {
		return nil, nil, err
	}
	e.wikiIndex = ix
	return []plugin{
		wiki.NewDispatcher(ix),
		// Gated local-write counterpart so a fetched peer note (or research)
		// can be ingested into the wiki on the user's direction. (TEN-244)
		newWikiSaveDispatcher(f.dir, ix, e.confirm),
	}, nil, nil
}

func prepareWiki(pf *pluginFlags, ensureDir func(label, dir string), lines *[]string) {
	if f := wikiFlagsOf(pf); f.dir != "" {
		ensureDir("wiki dir", f.dir)
		seedWikiNote(f.dir, lines)
	}
}

// evalWiki uses the wiki dir the operator configured, when it exists.
func evalWiki(pf *pluginFlags, c *commonFlags, explicit map[string]bool) bool {
	f := wikiFlagsOf(pf)
	if explicit["wiki-dir"] || f.dir != "" {
		return false
	}
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil || lc == nil {
		return false
	}
	sk, ok := lc.Skills["wiki"]
	if !ok || !sk.Enabled {
		return false
	}
	dir := sk.Settings["dir"]
	if fi, err := os.Stat(dir); dir == "" || err != nil || !fi.IsDir() {
		return false
	}
	f.dir = dir
	return true
}
