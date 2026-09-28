package main

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/wiki"
)

// wikiPlugin declares the wiki plugin: the agent answers from a markdown
// knowledge base, and saves notes into it (wiki_save) when the operator
// approves. Configured by path (--wiki-dir, or `tenant setup`).
func wikiPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:     "wiki",
		Events: wiki.Events,
		External: func(settings map[string]string) map[string]string {
			return map[string]string{settings["dir"]: "wiki vault"}
		},
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
	ix, err := buildWikiIndex(ctx, e.c, e.router, f.dir)
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

// checkWikiDir — only meaningful when an operator configured a wiki dir
// for the wiki plugin. Skips silently when none is set; warns if set but
// missing/unreadable.
func checkWikiDir(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "no wiki configured"}
	}
	wcfg := e.lc.Skills["wiki"]
	if wcfg == nil || wcfg.Settings == nil {
		return checkResult{Status: statusSkip, Detail: "no wiki configured"}
	}
	dir := expandPath(strings.TrimSpace(wcfg.Settings["dir"]))
	if dir == "" {
		return checkResult{Status: statusSkip, Detail: "no wiki dir configured"}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("wiki dir %s: %v", dir, err),
			Fix:    "create it or update the wiki.settings.dir in config.json"}
	}
	if !info.IsDir() {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("wiki path %s is not a directory", dir),
			Fix:    "point wiki.settings.dir at an actual directory"}
	}
	// Count .md files. Empty dir is a configuration-not-yet-done state,
	// not a failure — but it silently breaks fitness-004/015/017 and any
	// wiki_search/wiki_list user query, so surface as WARN with a
	// remediation. fitness-016 (no-result) actually passes on an empty
	// wiki, but the 3-of-4 silent-failure case is what we're warning on.
	// TEN-24 motivation: operator runs `tenant eval --subset=fitness`,
	// most wiki tasks fail, no obvious why. Doctor catches it upfront.
	files, _ := os.ReadDir(dir)
	mdCount := 0
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Name()), ".md") {
			mdCount++
		}
	}
	if mdCount == 0 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("wiki dir %s exists but contains no markdown files — wiki_search/wiki_list will return empty", dir),
			Fix:    "drop .md notes into the dir, OR point wiki.settings.dir at an existing notes directory"}
	}
	return checkResult{Status: statusOK,
		Detail: fmt.Sprintf("wiki dir %s OK (%d markdown file(s))", dir, mdCount)}
}

// checkWikiFreshness — TEN-48 minimal diagnostic. Detects the "wiki
// file on disk is newer than the index sidecar" condition that caused
// the 2026-05-26 lost-context bug (TEN-43). Compares each .md file's
// mtime to the sidecar's mtime; reports a WARN if any file is newer
// (the next wiki_search will lazy-reindex, but until then the file is
// dark to retrieval). TEN-44 fixed the proximate cause for research
// deposits; this check catches the GENERAL case (manual `cp` to wiki,
// editor saves, future producers that bypass the deposit path).
func checkWikiFreshness(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "no wiki configured"}
	}
	wcfg := e.lc.Skills["wiki"]
	if wcfg == nil || wcfg.Settings == nil {
		return checkResult{Status: statusSkip, Detail: "no wiki configured"}
	}
	dir := expandPath(strings.TrimSpace(wcfg.Settings["dir"]))
	if dir == "" {
		return checkResult{Status: statusSkip, Detail: "no wiki dir configured"}
	}
	// Sidecar path: mirror the formula used in toolmux.go::buildWikiIndex
	// and commands.go::cmdWiki (one sidecar per vault, hashed by abs path).
	absVault, _ := filepath.Abs(dir)
	h := fnv.New64a()
	_, _ = h.Write([]byte(absVault))
	sidecar := filepath.Join(e.c.dataDir, "wiki", fmt.Sprintf("%x.json", h.Sum64()))
	sideInfo, err := os.Stat(sidecar)
	if err != nil {
		// No sidecar yet → first launch / wiki never indexed. Not a failure;
		// the index builds on first wiki_search.
		return checkResult{Status: statusOK,
			Detail: "wiki sidecar not present (will be built on first search)"}
	}
	sideMtime := sideInfo.ModTime()

	// Walk wiki dir for .md files; count those newer than the sidecar.
	var staleFiles []string
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // tolerant: skip unreadable entries
		}
		if d.IsDir() {
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir // skip hidden dirs (.git, .obsidian)
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().After(sideMtime) {
			rel, _ := filepath.Rel(dir, path)
			staleFiles = append(staleFiles, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("wiki freshness walk failed: %v", err)}
	}
	if len(staleFiles) == 0 {
		return checkResult{Status: statusOK,
			Detail: fmt.Sprintf("wiki index fresh (sidecar mtime %s)", sideMtime.Format("2006-01-02 15:04:05"))}
	}
	// Cap the rendered list so a huge dir doesn't blow the doctor output.
	preview := staleFiles
	const maxShown = 5
	suffix := ""
	if len(preview) > maxShown {
		preview = preview[:maxShown]
		suffix = fmt.Sprintf(" (+%d more)", len(staleFiles)-maxShown)
	}
	return checkResult{Status: statusWarn,
		Detail: fmt.Sprintf("%d file(s) on disk newer than wiki index%s: %s — search will return stale results until next wiki_search re-indexes",
			len(staleFiles), suffix, strings.Join(preview, ", ")),
		Fix: "run `tenant wiki reindex` OR fire any wiki_search call to trigger lazy reindex"}
}

// buildWikiIndex constructs the wiki index for the TUI. Unlike the
// `tenant wiki` command (where a dead embedder is fatal — wiki is the
// whole point there), here it must NOT crash the TUI: `wiki.New` is
// offline-safe (validates the dir + loads the sidecar, no network), the
// embedder fingerprint falls back to the profile's declared dim if the
// probe fails, and the initial reindex is best-effort (wiki.Search
// auto-reindexes lazily on first use, so it self-heals when the
// embedder comes back). A search while the embedder is down surfaces a
// clean tool error in the feed instead of a startup crash.
func buildWikiIndex(ctx context.Context, c *commonFlags, router *model.Router, dir string) (*wiki.Index, error) {
	emb, embProfile, err := router.EmbedderForRole(ctx, model.RoleEmbedder)
	if err != nil {
		return nil, fmt.Errorf("wiki plugin: resolve embedder: %w", err)
	}
	dim := embProfile.EmbedDim // declared; preferred = actual (probed) when reachable
	if probe, perr := emb.Embed(ctx, []string{"wiki embedder fingerprint probe"}); perr == nil && len(probe) == 1 && len(probe[0]) > 0 {
		dim = len(probe[0])
	} else {
		wiki.EmbedderProbeFailed.Log(ctx, "declared_dim", dim, "err", perr)
	}
	embedID := embProfile.Model + "/" + strconv.Itoa(dim)
	absVault, _ := filepath.Abs(dir)
	h := fnv.New64a()
	_, _ = h.Write([]byte(absVault))
	sidecar := filepath.Join(c.dataDir, "wiki", fmt.Sprintf("%x.json", h.Sum64()))
	ix, err := wiki.New(dir, sidecar, embedID, emb)
	if err != nil {
		return nil, fmt.Errorf("wiki plugin: %w", err)
	}
	if _, _, rerr := ix.Reindex(ctx); rerr != nil {
		// Embedder down at launch — don't crash; lazy reindex on first search.
		wiki.ReindexFailed.Log(ctx, "err", rerr)
	}
	return ix, nil
}
