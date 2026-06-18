package improve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

// ConsolidationJob is a self-improvement job that merges overlapping facts in
// the semantic store. The distiller only dedups near-identical embeddings on
// write; paraphrases ("Tenant is a Go MCP framework" said three ways) and
// subset/superset facts (granular facts subsumed by a comprehensive one) slip
// through and accumulate as noise.
//
// Two grouping strategies:
//   - cosine (default): cluster facts by embedding similarity, then ask the
//     summarizer to merge each cluster. Cheap, but misses paraphrases that
//     embed far apart because they lead with different specifics.
//   - Holistic: hand the whole fact list to the summarizer and let it group by
//     MEANING. Catches semantic duplicates cosine can't, at the cost of one
//     larger prompt (fine at personal scale; pre-cluster first for huge stores).
//
// Either way it inserts the canonical merged fact and Supersedes the originals
// (kept for audit, filtered from Search).
type ConsolidationJob struct {
	Semantic *semantic.Store
	Router   *model.Router
	// SummarizerRouter, when non-nil, resolves the merge SUMMARIZER LLM instead
	// of Router — used to pin reflection work to a stronger reasoning model
	// (TEN-195). The EMBEDDER is always resolved off Router (the main router) so
	// the embedding space stays consistent regardless of which model summarizes.
	// nil ⇒ Router (today's behavior).
	SummarizerRouter *model.Router
	AgentID          string
	SummarizerRole   model.Role
	EmbedderRole     model.Role
	// ClusterThreshold is the cosine cutoff for the cosine strategy. 0 →
	// DefaultClusterThreshold. Ignored when Holistic is set.
	ClusterThreshold float64
	// MaxClusterSize bounds a cosine cluster (cosine strategy only).
	MaxClusterSize int
	// Holistic selects the meaning-based grouping strategy (one LLM call over
	// all facts) instead of cosine clustering.
	Holistic bool
	// DryRun computes proposed merges but writes nothing.
	DryRun bool
	Logger *slog.Logger
}

const (
	DefaultClusterThreshold = 0.83
	DefaultMaxClusterSize   = 8
	// holisticMaxFacts caps how many facts go into one holistic prompt.
	holisticMaxFacts = 80
)

// mergeGroup is a set of facts to collapse into one canonical fact.
type mergeGroup struct {
	members []*semantic.Fact
	text    string
}

// Name implements Job.
func (j *ConsolidationJob) Name() string { return "consolidate" }

// Run implements Job: group overlapping facts (by cosine clustering or, when
// Holistic, by a meaning-based LLM pass), merge each group into one canonical
// fact, and supersede the originals.
func (j *ConsolidationJob) Run(ctx context.Context) (JobResult, error) {
	log := j.Logger
	if log == nil {
		log = slog.Default()
	}
	if j.Semantic == nil {
		return JobResult{}, fmt.Errorf("consolidate: nil Semantic store")
	}
	if j.Router == nil {
		return JobResult{}, fmt.Errorf("consolidate: nil Router")
	}
	if j.AgentID == "" {
		return JobResult{}, fmt.Errorf("consolidate: empty AgentID")
	}

	facts, err := j.Semantic.List(ctx, semantic.ListFilter{AgentIDs: []string{j.AgentID}})
	if err != nil {
		return JobResult{}, fmt.Errorf("consolidate: list facts: %w", err)
	}
	// Exclude merge-protected facts (pinned / explicitly protected / high-
	// importance AND actually-used) from merge candidacy, so the holistic
	// pass can no longer eat load-bearing nuance (design §7). Strictly
	// safer: it only REMOVES facts from candidacy. Fails open (no filter)
	// on a signals-load error so consolidation never silently stops.
	totalFacts := len(facts)
	facts, protectedExcluded := j.filterMergeProtected(ctx, facts, log)
	if len(facts) < 2 {
		return JobResult{Summary: fmt.Sprintf("consolidate: %d fact(s) after %d protected excluded, nothing to merge", len(facts), protectedExcluded)}, nil
	}

	summarizer, sumProf, err := j.summarizerRouter().LLMForRole(ctx, j.summarizerRole())
	if err != nil {
		return JobResult{}, fmt.Errorf("consolidate: resolve summarizer: %w", err)
	}
	if j.SummarizerRouter != nil && j.SummarizerRouter != j.Router && j.Logger != nil {
		// Provenance: which model is doing the merge reasoning (TEN-195).
		j.Logger.Info("consolidate: summarizing on pinned proposer model", "model", sumProf.Model)
	}
	embedder, embProfile, err := j.Router.EmbedderForRole(ctx, j.embedderRole())
	if err != nil {
		return JobResult{}, fmt.Errorf("consolidate: resolve embedder: %w", err)
	}

	// Build merge groups via the configured strategy.
	var groups []mergeGroup
	if j.Holistic {
		groups, err = j.holisticGroups(ctx, summarizer, facts)
		if err != nil {
			return JobResult{}, fmt.Errorf("consolidate: holistic grouping: %w", err)
		}
	} else {
		threshold := j.ClusterThreshold
		if threshold <= 0 {
			threshold = DefaultClusterThreshold
		}
		maxCluster := j.MaxClusterSize
		if maxCluster <= 0 {
			maxCluster = DefaultMaxClusterSize
		}
		for _, cluster := range clusterFacts(facts, threshold, maxCluster) {
			if len(cluster) < 2 {
				continue
			}
			if err := ctx.Err(); err != nil {
				return JobResult{}, err
			}
			text, ok, merr := j.mergeCluster(ctx, summarizer, cluster)
			if merr != nil {
				log.Warn("consolidate: merge failed", "agent", j.AgentID, "size", len(cluster), "err", merr)
				continue
			}
			if !ok {
				continue // summarizer judged the cluster genuinely distinct
			}
			groups = append(groups, mergeGroup{members: cluster, text: text})
		}
	}
	if len(groups) == 0 {
		return JobResult{Summary: fmt.Sprintf("consolidate: %d facts, no merges found", len(facts))}, nil
	}

	// Apply the merges (or just count them in dry-run).
	var mergedGroups, factsSuperseded int
	previews := make([]string, 0, len(groups))
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return JobResult{}, err
		}
		previews = append(previews, fmt.Sprintf("%d→1: %s", len(g.members), clip(g.text, 90)))
		if j.DryRun {
			mergedGroups++
			factsSuperseded += len(g.members)
			continue
		}
		vecs, eerr := embedder.Embed(ctx, []string{g.text})
		if eerr != nil || len(vecs) == 0 {
			log.Warn("consolidate: embed merged fact failed", "err", eerr)
			continue
		}
		newID, ierr := j.Semantic.Insert(ctx, &semantic.Fact{
			AgentID:        j.AgentID,
			Visibility:     g.members[0].Visibility,
			Fact:           g.text,
			SourceEpisodes: unionSources(g.members),
			Confidence:     maxConfidence(g.members),
			EmbedderID:     string(embProfile.ID),
			Embedding:      vecs[0],
		})
		if ierr != nil {
			log.Warn("consolidate: insert merged fact failed", "err", ierr)
			continue
		}
		for _, f := range g.members {
			if serr := j.Semantic.Supersede(ctx, f.ID, newID); serr != nil {
				log.Warn("consolidate: supersede failed", "old", f.ID, "new", newID, "err", serr)
				continue
			}
			factsSuperseded++
		}
		mergedGroups++
	}

	// Heat-reset: halve the hottest facts' access_count so a few popular
	// facts can't permanently dominate ranking (MemoryOS promotion-reset).
	// Runs in this always-on cadence so the guard is live in Phase 1, not
	// gated behind the optional reflection job (review finding 4). Best-effort.
	if !j.DryRun {
		if derr := j.Semantic.DampenHeat(ctx, j.AgentID, heatDampenTopN); derr != nil {
			log.Warn("consolidate: dampen heat failed", "agent", j.AgentID, "err", derr)
		}
	}

	live, _ := j.Semantic.Count(ctx, false, false)
	summary := fmt.Sprintf("merged %d group(s), superseded %d fact(s); %d live facts remain (%d merge-protected, excluded)",
		mergedGroups, factsSuperseded, live, protectedExcluded)
	if j.DryRun {
		summary = "[dry-run] " + summary
	}
	return JobResult{
		Summary: summary,
		Changed: !j.DryRun && mergedGroups > 0,
		Details: map[string]any{
			"merged_groups":      mergedGroups,
			"facts_superseded":   factsSuperseded,
			"live_facts":         live,
			"holistic":           j.Holistic,
			"dry_run":            j.DryRun,
			"previews":           previews,
			"protected_excluded": protectedExcluded,
			"candidate_facts":    totalFacts,
		},
	}, nil
}

// heatDampenTopN bounds how many of the hottest facts get their
// access_count halved per consolidation run.
const heatDampenTopN = 20

// filterMergeProtected drops facts that semantic.MergeProtected reports
// (pinned / protected / high-importance-and-used) from the merge-candidate
// list, returning the survivors and the count excluded. Fails open: on a
// signals-load error it logs and returns the input unchanged (today's
// behavior) rather than silently halting consolidation.
func (j *ConsolidationJob) filterMergeProtected(ctx context.Context, facts []*semantic.Fact, log *slog.Logger) ([]*semantic.Fact, int) {
	if len(facts) == 0 {
		return facts, 0
	}
	ids := make([]int64, len(facts))
	for i, f := range facts {
		ids[i] = f.ID
	}
	sigs, err := j.Semantic.SignalsBatch(ctx, ids)
	if err != nil {
		log.Warn("consolidate: signals load failed; not filtering protected", "agent", j.AgentID, "err", err)
		return facts, 0
	}
	kept := make([]*semantic.Fact, 0, len(facts))
	excluded := 0
	for _, f := range facts {
		if semantic.MergeProtected(sigs[f.ID]) {
			excluded++
			continue
		}
		kept = append(kept, f)
	}
	return kept, excluded
}

func (j *ConsolidationJob) summarizerRole() model.Role {
	if j.SummarizerRole != "" {
		return j.SummarizerRole
	}
	return model.RoleSummarizer
}

// summarizerRouter is the router for the merge SUMMARIZER LLM: the pinned
// proposer router when set (TEN-195), else the main Router. The embedder is
// resolved off Router directly and is unaffected.
func (j *ConsolidationJob) summarizerRouter() *model.Router {
	if j.SummarizerRouter != nil {
		return j.SummarizerRouter
	}
	return j.Router
}

func (j *ConsolidationJob) embedderRole() model.Role {
	if j.EmbedderRole != "" {
		return j.EmbedderRole
	}
	return model.RoleEmbedder
}

// --- holistic (meaning-based) grouping ---

const holisticSystemPrompt = `You are given a numbered list of memory facts about a user and their projects. Find GROUPS of facts that state the SAME underlying claim — even if worded differently, or one is more specific than another (e.g. "Tenant is a Go MCP framework" and "Tenant: an MCP framework written in Go" are the same claim). Facts that state different things stay out of groups.

For each group of 2 or more facts, produce ONE merged fact that preserves every specific detail from its members (versions, file names, paths, numbers) in a single sentence.

Respond with JSON only:
{"groups": [{"members": [1, 4, 9], "fact": "<merged fact>"}]}

Use the numbers from the list. Only include groups with 2+ members. Be precise: do NOT group facts that are merely about the same topic but state different things.`

const holisticJSONSchema = `{"type":"object","properties":{"groups":{"type":"array","items":{"type":"object","properties":{"members":{"type":"array","items":{"type":"integer"}},"fact":{"type":"string"}},"required":["members","fact"]}}},"required":["groups"]}`

// holisticGroups asks the summarizer to group the whole fact list by meaning.
// Indices in the response are 1-based into `facts`.
func (j *ConsolidationJob) holisticGroups(ctx context.Context, llm model.LLM, facts []*semantic.Fact) ([]mergeGroup, error) {
	if len(facts) > holisticMaxFacts {
		facts = facts[:holisticMaxFacts]
	}
	var b strings.Builder
	b.WriteString("Facts:\n")
	for i, f := range facts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, f.Fact)
	}
	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: holisticSystemPrompt},
			{Role: "user", Content: b.String()},
		},
		JSONSchema:  []byte(holisticJSONSchema),
		Temperature: 0.1,
	})
	if err != nil {
		return nil, fmt.Errorf("summarizer: %w", err)
	}
	if resp.Text == "" {
		return nil, fmt.Errorf("summarizer returned empty")
	}
	var parsed struct {
		Groups []struct {
			Members []int  `json:"members"`
			Fact    string `json:"fact"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(firstJSONObject(resp.Text)), &parsed); err != nil {
		return nil, fmt.Errorf("parse holistic JSON: %w (%q)", err, clip(resp.Text, 200))
	}
	var out []mergeGroup
	used := map[int]bool{} // a fact can only belong to one group
	for _, g := range parsed.Groups {
		if strings.TrimSpace(g.Fact) == "" {
			continue
		}
		seen := map[int]bool{}
		var members []*semantic.Fact
		for _, idx := range g.Members {
			if idx < 1 || idx > len(facts) || seen[idx] || used[idx] {
				continue
			}
			seen[idx] = true
			members = append(members, facts[idx-1])
		}
		if len(members) >= 2 {
			for idx := range seen {
				used[idx] = true
			}
			out = append(out, mergeGroup{members: members, text: strings.TrimSpace(g.Fact)})
		}
	}
	return out, nil
}

// --- cosine grouping ---

// clusterFacts greedily groups facts whose embedding is within `threshold`
// cosine of a cluster's seed (its first member). O(n*clusters); fine at
// personal scale. Caps cluster size so merge prompts stay bounded.
func clusterFacts(facts []*semantic.Fact, threshold float64, maxSize int) [][]*semantic.Fact {
	var clusters [][]*semantic.Fact
	for _, f := range facts {
		if len(f.Embedding) == 0 {
			clusters = append(clusters, []*semantic.Fact{f})
			continue
		}
		placed := false
		for ci := range clusters {
			seed := clusters[ci][0]
			if len(clusters[ci]) >= maxSize || len(seed.Embedding) != len(f.Embedding) {
				continue
			}
			if cosine(f.Embedding, seed.Embedding) >= threshold {
				clusters[ci] = append(clusters[ci], f)
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, []*semantic.Fact{f})
		}
	}
	return clusters
}

const consolidateSystemPrompt = `You consolidate overlapping memory facts about a user/project into one canonical fact.

You are given a small set of facts an embedding model flagged as similar. Decide:
- If they are restatements, or one subsumes the others (same underlying claim, just different wording or granularity), MERGE them into ONE atomic fact that keeps the most specific, complete information and loses no important detail.
- If they are genuinely DISTINCT claims that merely share vocabulary, do NOT merge.

Respond with JSON only:
{"merge": true, "fact": "<the single merged one-sentence fact>"}
{"merge": false}`

const consolidateJSONSchema = `{"type":"object","properties":{"merge":{"type":"boolean"},"fact":{"type":"string"}},"required":["merge"]}`

// mergeCluster asks the summarizer to merge a cosine cluster. Returns
// (mergedText, merged?, error). merged=false means "judged distinct".
func (j *ConsolidationJob) mergeCluster(ctx context.Context, llm model.LLM, cluster []*semantic.Fact) (string, bool, error) {
	var b strings.Builder
	b.WriteString("Facts flagged as similar:\n")
	for i, f := range cluster {
		fmt.Fprintf(&b, "%d. %s\n", i+1, f.Fact)
	}
	b.WriteString("\nMerge them if they are the same underlying claim; otherwise return {\"merge\": false}.")
	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: consolidateSystemPrompt},
			{Role: "user", Content: b.String()},
		},
		JSONSchema:  []byte(consolidateJSONSchema),
		Temperature: 0.1,
	})
	if err != nil {
		return "", false, fmt.Errorf("summarizer: %w", err)
	}
	if resp.Text == "" {
		return "", false, fmt.Errorf("summarizer returned empty")
	}
	var out struct {
		Merge bool   `json:"merge"`
		Fact  string `json:"fact"`
	}
	if err := json.Unmarshal([]byte(firstJSONObject(resp.Text)), &out); err != nil {
		return "", false, fmt.Errorf("parse merge JSON: %w (%q)", err, clip(resp.Text, 160))
	}
	if !out.Merge || strings.TrimSpace(out.Fact) == "" {
		return "", false, nil
	}
	return strings.TrimSpace(out.Fact), true, nil
}

// --- helpers ---

func unionSources(cluster []*semantic.Fact) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, f := range cluster {
		for _, s := range f.SourceEpisodes {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func maxConfidence(cluster []*semantic.Fact) float64 {
	m := 0.0
	for _, f := range cluster {
		if f.Confidence > m {
			m = f.Confidence
		}
	}
	if m == 0 {
		m = 0.7
	}
	return m
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// firstJSONObject extracts the first balanced top-level JSON object from noisy
// LLM output (```json fences, leading prose, trailing commentary).
func firstJSONObject(s string) string {
	i := strings.IndexByte(s, '{')
	if i < 0 {
		return s
	}
	depth := 0
	for k := i; k < len(s); k++ {
		switch s[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[i : k+1]
			}
		}
	}
	return s[i:]
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
