package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"tenant/internal/agent"
	"tenant/internal/improve"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/userprofile"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// memControl implements tui.MemoryControl: inspect the memory tiers,
// search, and the limited editing surface (forget; soul view + .md
// import). The soul is view-only here — updated by importing a file or
// editing the TOML in the OS — per the chosen design.
type memControl struct {
	episodic *episodic.Store
	semantic *semantic.Store
	skills   *skills.Store
	embedder model.Embedder
	distill  *improve.DistillJob
	cfgDir   string
	agentID  string
	// soulLive is the SAME live-soul holder the agent reads each turn.
	// Soul edits swap in a fresh *soul.Soul (pointer swap, not in-place
	// mutation) so they take effect next turn without a restart and
	// without racing the assembler's mid-turn Render.
	soulLive *soul.Live

	// profile is the SAME *userprofile.Profile the agent renders each turn;
	// profileRefresh re-synthesizes it (shared with the background job, so
	// it serializes internally). Both optional.
	profile        *userprofile.Profile
	profileRefresh func(context.Context) (bool, error)

	// working is the SAME *working.Set the agent's turn loop appends to —
	// read (Len) for the T1 count on the dashboard status page. Optional.
	working *working.Set

	// expand rehydrates the latest compaction summary's source span from the
	// archive for the dashboard's Compaction provenance page (TEN-104). Set
	// AFTER the agent is constructed (memControl is built before the agent);
	// nil = the page renders "nothing compacted yet."
	expand func(context.Context) (*agent.CompactionExpansion, error)
}

// ProfileView shows the synthesized always-on user model (T0-adjacent).
func (c memControl) ProfileView() string {
	if c.profile == nil || !c.profile.HasContent() {
		return "no user profile yet — it's synthesized from learned facts in the background. " +
			"Run /memory distill to learn facts, then /memory profile refresh."
	}
	return "user profile (learned, injected every turn):\n\n" + c.profile.Body()
}

// ProfileRefresh re-synthesizes the user profile from current facts now.
func (c memControl) ProfileRefresh() string {
	if c.profileRefresh == nil {
		return "user-profile synthesis is not available in this session"
	}
	changed, err := c.profileRefresh(context.Background())
	if err != nil {
		return "profile refresh failed: " + err.Error()
	}
	if !changed {
		return "no change — no new facts to fold into the profile yet"
	}
	return "user profile refreshed:\n\n" + c.profile.Body()
}

// profileJob adapts the shared profile-refresh closure to improve.Job so
// the scheduler re-synthesizes the user model on a cadence.
type profileJob struct {
	refresh func(context.Context) (bool, error)
}

func (j profileJob) Name() string { return "user-profile" }

func (j profileJob) Run(ctx context.Context) (improve.JobResult, error) {
	changed, err := j.refresh(ctx)
	if err != nil {
		return improve.JobResult{}, err
	}
	if !changed {
		return improve.JobResult{Summary: "user profile: no new facts"}, nil
	}
	return improve.JobResult{Changed: true, Summary: "user profile refreshed"}, nil
}

func (c memControl) Stats() string {
	ctx := context.Background()
	ep, _ := c.episodic.Count(ctx, false)
	fa, _ := c.semantic.Count(ctx, false, false)
	sk := 0
	if c.skills != nil {
		sk, _ = c.skills.Count(ctx, c.agentID)
	}
	soulInfo := "(none)"
	if sl, err := soul.Load(c.cfgDir, c.agentID); err == nil {
		soulInfo = fmt.Sprintf("v%d, %d instruction(s), %d user fact(s)", sl.Meta.Version, len(sl.Instructions.Items), len(sl.User.Facts))
	}
	return fmt.Sprintf("memory — agent=%s\n  T0 soul:     %s\n  T2 episodes: %d\n  T3 facts:    %d\n  T4 skills:   %d",
		c.agentID, soulInfo, ep, fa, sk)
}

func (c memControl) embed(q string) []float32 {
	if c.embedder == nil {
		return nil
	}
	if v, err := c.embedder.Embed(context.Background(), []string{q}); err == nil && len(v) == 1 {
		return v[0]
	}
	return nil
}

func (c memControl) Search(query string) string {
	ctx := context.Background()
	emb := c.embed(query)
	var b strings.Builder
	fmt.Fprintf(&b, "search %q\n", query)
	fHits, _ := c.semantic.Search(ctx, semantic.Query{AgentIDs: []string{c.agentID}, Embedding: emb, Keywords: ftsTokens(query), K: 6})
	fmt.Fprintf(&b, "FACTS (%d):\n", len(fHits))
	for _, h := range fHits {
		fmt.Fprintf(&b, "  - [#%d %.2f] %s\n", h.Fact.ID, h.Score, clip(h.Fact.Fact, 90))
	}
	eHits, _ := c.episodic.Search(ctx, episodic.Query{AgentIDs: []string{c.agentID}, Embedding: emb, Keywords: ftsTokens(query), K: 6})
	fmt.Fprintf(&b, "EPISODES (%d):\n", len(eHits))
	for _, h := range eHits {
		fmt.Fprintf(&b, "  - [ep:%d %.2f] %s -> %s\n", h.Episode.ID, h.Score, clip(h.Episode.Prompt, 50), clip(h.Episode.Response, 50))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (c memControl) Facts(query string) string {
	ctx := context.Background()
	var facts []*semantic.Fact
	if strings.TrimSpace(query) != "" {
		hits, _ := c.semantic.Search(ctx, semantic.Query{AgentIDs: []string{c.agentID}, Embedding: c.embed(query), Keywords: ftsTokens(query), K: 20})
		for _, h := range hits {
			facts = append(facts, h.Fact)
		}
	} else {
		facts, _ = c.semantic.List(ctx, semantic.ListFilter{AgentIDs: []string{c.agentID}, Limit: 20})
	}
	if len(facts) == 0 {
		return "no facts yet (they're distilled from episodes — run /memory distill after some turns)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "facts (%d):\n", len(facts))
	for _, f := range facts {
		fmt.Fprintf(&b, "  - [#%d conf %.2f] %s\n", f.ID, f.Confidence, f.Fact)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (c memControl) Recent(n int) string {
	if n <= 0 {
		n = 10
	}
	eps, _ := c.episodic.List(context.Background(), episodic.ListFilter{AgentIDs: []string{c.agentID}, Limit: 300})
	if len(eps) > n { // List is chronological; take the most recent tail
		eps = eps[len(eps)-n:]
	}
	if len(eps) == 0 {
		return "no episodes yet"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "recent episodes (%d)  [✓ acked · ✗ undone]:\n", len(eps))
	for i := len(eps) - 1; i >= 0; i-- { // newest first
		e := eps[i]
		fmt.Fprintf(&b, "  %s [ep:%d %s] %s -> %s\n", feedbackMark(e.UserFeedback), e.ID, e.Timestamp.Format("01-02 15:04"), clip(e.Prompt, 50), clip(e.Response, 60))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (c memControl) Forget(target string) string {
	ctx := context.Background()
	switch {
	case strings.HasPrefix(target, "fact:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(target, "fact:"), 10, 64)
		if err != nil {
			return "bad fact id: " + target
		}
		if err := c.semantic.Tombstone(ctx, id); err != nil {
			return "forget failed: " + err.Error()
		}
		return fmt.Sprintf("forgot fact #%d", id)
	case strings.HasPrefix(target, "ep:"):
		id, err := strconv.ParseInt(strings.TrimPrefix(target, "ep:"), 10, 64)
		if err != nil {
			return "bad episode id: " + target
		}
		if err := c.episodic.Tombstone(ctx, id); err != nil {
			return "forget failed: " + err.Error()
		}
		return fmt.Sprintf("forgot episode #%d", id)
	default:
		return "usage: /memory forget fact:<id> | ep:<id>"
	}
}

func (c memControl) SoulView() string {
	p := soul.Path(c.cfgDir, c.agentID)
	sl, err := soul.Load(c.cfgDir, c.agentID)
	if err != nil {
		return "(no soul yet — seeded on first run)\nfile: " + p
	}
	return "soul file (edit here or import): " + p + "\n\n" + sl.Render()
}

// soulImportCap caps per-file size when importing a folder — the soul
// rides ReserveSoul (~2K tokens), so a huge file (e.g. a 17KB CLAUDE.md)
// is skipped rather than blowing the budget.
const soulImportCap = 12000

// SoulImport replaces the soul's operating Instructions with markdown
// content. The path may be a single .md file OR a directory (imports
// every .md/.txt in it, skipping oversized ones). View-only otherwise —
// this is the edit path.
func (c memControl) SoulImport(path string) string {
	path = expandPath(path)
	info, err := os.Stat(path)
	if err != nil {
		return "import failed: " + err.Error()
	}
	var files []string
	if info.IsDir() {
		entries, _ := os.ReadDir(path)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := strings.ToLower(e.Name())
			if strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".markdown") || strings.HasSuffix(n, ".txt") {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
		sort.Strings(files)
	} else {
		files = []string{path}
	}

	var items, imported, skipped []string
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		if info.IsDir() && fi.Size() > soulImportCap {
			skipped = append(skipped, fmt.Sprintf("%s (%dB)", filepath.Base(f), fi.Size()))
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		items = append(items, mdParagraphs(string(raw))...)
		imported = append(imported, filepath.Base(f))
	}
	if len(items) == 0 {
		return "import found no usable .md/.txt content at " + path
	}
	sl, err := soul.Load(c.cfgDir, c.agentID)
	if err != nil {
		sl = soul.NewDefault(c.agentID)
	}
	sl.Instructions.Items = items
	if err := sl.Save(c.cfgDir); err != nil {
		return "import save failed: " + err.Error()
	}
	// Swap the agent's live soul so it applies next turn (no restart).
	// Pointer swap, not in-place mutation — a turn mid-Render keeps its
	// snapshot, the next turn sees this one.
	c.soulLive.Store(sl)
	msg := fmt.Sprintf("imported %d block(s) from %s — live next turn", len(items), strings.Join(imported, ", "))
	if len(skipped) > 0 {
		msg += "\n  skipped (too big for the small soul budget): " + strings.Join(skipped, ", ")
	}
	return msg
}

// RulesView shows the agent's operating rules — the soul's Instructions
// block, which is what import populates and what's injected every turn.
func (c memControl) RulesView() string {
	sl, err := soul.Load(c.cfgDir, c.agentID)
	if err != nil || len(sl.Instructions.Items) == 0 {
		return "no operating rules set. Import a rules file: /memory rules import <path.md|folder>"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("operating rules (%d, injected every turn):\n", len(sl.Instructions.Items)))
	for i, it := range sl.Instructions.Items {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, it)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (c memControl) Distill() string {
	if c.distill == nil {
		return "distillation not available"
	}
	res, err := c.distill.Run(context.Background())
	if err != nil {
		return "distill failed: " + err.Error()
	}
	return "distilled: " + res.Summary
}

// mdParagraphs splits markdown into blank-line-separated paragraphs,
// collapsing internal whitespace and stripping leading bullet/heading
// markers — each becomes one operating instruction.
func mdParagraphs(s string) []string {
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		p := strings.TrimSpace(para)
		p = strings.TrimLeft(p, "#-*> \t")
		p = strings.Join(strings.Fields(p), " ")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
