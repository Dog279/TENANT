package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
)

// dashMemory adapts memControl to dashboard.MemoryControl — the Memory
// Curator backend (TEN-88). It mirrors the dashTools adapter: the dashboard
// package stays decoupled from the stores, and this layer does the view-type
// translation + keyset pagination. No HTTP routes here (that's TEN-89).
type dashMemory struct{ c memControl }

// soulFactsLimit caps a Facts page when the caller passes a non-positive limit.
const soulFactsLimit = 50

// Soul renders the curatable soul: persona prose (identity + values) plus the
// user-fact and instruction lists, each item carrying a stable derived ID.
func (d dashMemory) Soul() (dashboard.SoulView, error) {
	sl := d.c.soulLive.Load()
	if sl == nil {
		return dashboard.SoulView{}, nil
	}
	facts, err := sl.Items(soul.SectionUserFact)
	if err != nil {
		return dashboard.SoulView{}, err
	}
	instrs, err := sl.Items(soul.SectionInstruction)
	if err != nil {
		return dashboard.SoulView{}, err
	}
	return dashboard.SoulView{
		Persona:      personaProse(sl),
		UserFacts:    toSoulItems(facts),
		Instructions: toSoulItems(instrs),
	}, nil
}

// SoulEdit applies one add/edit/remove to a soul list by derived ID, persists
// it, then swaps it into the live holder. Save-before-Store: a failed disk
// write never publishes an unsaved edit to the running agent.
func (d dashMemory) SoulEdit(op dashboard.SoulEditOp) error {
	if d.c.soulLive == nil {
		return errors.New("memory: soul editing unavailable")
	}
	cur := d.c.soulLive.Load()
	if cur == nil {
		cur = soul.NewDefault(d.c.agentID)
	}
	next := cur.Clone()
	var err error
	switch op.Action {
	case dashboard.SoulActionAdd:
		_, err = next.AddItem(op.Section, op.Text)
	case dashboard.SoulActionEdit:
		_, err = next.EditItem(op.Section, op.ID, op.Text)
	case dashboard.SoulActionRemove:
		err = next.RemoveItem(op.Section, op.ID)
	default:
		return fmt.Errorf("memory: unknown soul edit action %q", op.Action)
	}
	if err != nil {
		return err
	}
	if err := next.Save(d.c.cfgDir); err != nil {
		return fmt.Errorf("memory: save soul: %w", err)
	}
	d.c.soulLive.Store(next)
	return nil
}

// Facts returns one page of live facts. A non-empty q ranks by relevance
// (top matches, no cursor — search is not a full scan); an empty q paginates
// the whole store by keyset (cursor = last fact id).
func (d dashMemory) Facts(q string, limit int, cursor string) ([]dashboard.FactView, string, error) {
	ctx := context.Background()
	if limit <= 0 {
		limit = soulFactsLimit
	}
	if strings.TrimSpace(q) != "" {
		hits, err := d.c.semantic.Search(ctx, semantic.Query{
			AgentIDs: []string{d.c.agentID}, Embedding: d.c.embed(q),
			Keywords: ftsTokens(q), K: limit,
		})
		if err != nil {
			return nil, "", err
		}
		out := make([]dashboard.FactView, 0, len(hits))
		for _, h := range hits {
			out = append(out, toFactView(h.Fact))
		}
		return out, "", nil // ranked results aren't keyset-paginated
	}

	afterID, err := parseCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := d.c.semantic.ListPage(ctx, d.c.agentID, afterID, limit)
	if err != nil {
		return nil, "", err
	}
	out := make([]dashboard.FactView, 0, len(rows))
	for _, f := range rows {
		out = append(out, toFactView(f))
	}
	// A full page implies there may be more; cursor on the last id.
	next := ""
	if len(rows) == limit {
		next = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	return out, next, nil
}

// FactProvenance resolves a fact's SourceEpisodes to the originating turns.
// A missing source episode becomes a marked EpisodeView (Missing=true) — the
// whole call never fails because one episode was forgotten.
func (d dashMemory) FactProvenance(id int64) ([]dashboard.EpisodeView, error) {
	ctx := context.Background()
	f, err := d.c.semantic.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]dashboard.EpisodeView, 0, len(f.SourceEpisodes))
	for _, epID := range f.SourceEpisodes {
		ep, gerr := d.c.episodic.Get(ctx, epID)
		if gerr != nil {
			if errors.Is(gerr, episodic.ErrNotFound) {
				out = append(out, dashboard.EpisodeView{ID: epID, Missing: true})
				continue
			}
			return nil, gerr
		}
		out = append(out, dashboard.EpisodeView{
			ID: ep.ID, Prompt: ep.Prompt, Response: ep.Response, Timestamp: ep.Timestamp,
		})
	}
	return out, nil
}

// ResolveFacts records keepID superseding discardID.
func (d dashMemory) ResolveFacts(keepID, discardID int64) error {
	return d.c.semantic.Supersede(context.Background(), discardID, keepID)
}

// DeleteFact tombstones a fact (recoverable via RestoreFact).
func (d dashMemory) DeleteFact(id int64) error {
	return d.c.semantic.Tombstone(context.Background(), id)
}

// RestoreFact un-tombstones a fact. Tombstone only flips the flag, so
// Reaffirm (which targets the row by id regardless of tombstone state)
// resets last_confirmed; we clear the tombstone alongside it.
func (d dashMemory) RestoreFact(id int64) error {
	return d.c.semantic.Restore(context.Background(), id)
}

// RemovedFacts lists tombstoned facts (newest first) for the restore view.
func (d dashMemory) RemovedFacts(limit int) ([]dashboard.FactView, error) {
	if limit <= 0 {
		limit = soulFactsLimit
	}
	rows, err := d.c.semantic.List(context.Background(), semantic.ListFilter{
		AgentIDs: []string{d.c.agentID}, IncludeTombstoned: true,
	})
	if err != nil {
		return nil, err
	}
	out := make([]dashboard.FactView, 0)
	for _, f := range rows {
		if !f.Tombstoned {
			continue
		}
		out = append(out, toFactView(f))
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// TemporalFacts enumerates every fact — live, superseded, and tombstoned —
// for the knowledge-time Overview. A single full List (both Include flags on)
// backs both the rows and the count summary, so the rendered timeline and the
// stat pills can never disagree. Status precedence is tombstoned > superseded
// > live, matching the mutually-exclusive buckets in MemStats.
func (d dashMemory) TemporalFacts() ([]dashboard.TemporalFactView, dashboard.MemStats, error) {
	rows, err := d.c.semantic.List(context.Background(), semantic.ListFilter{
		AgentIDs:          []string{d.c.agentID},
		IncludeTombstoned: true,
		IncludeSuperseded: true,
	})
	if err != nil {
		return nil, dashboard.MemStats{}, err
	}
	now := time.Now()
	out := make([]dashboard.TemporalFactView, 0, len(rows))
	var stats dashboard.MemStats
	for _, f := range rows {
		v := toTemporalFactView(f, now)
		out = append(out, v)
		stats.Total++
		switch v.Status {
		case dashboard.FactStatusTombstoned:
			stats.Tombstoned++
		case dashboard.FactStatusSuperseded:
			stats.Superseded++
		default:
			stats.Live++
		}
	}
	return out, stats, nil
}

// WorkingCount reports the live T1 working-set size.
func (d dashMemory) WorkingCount() int {
	if d.c.working == nil {
		return 0
	}
	return d.c.working.Len()
}

// UserProfile returns the synthesized always-on user model body (read-only).
func (d dashMemory) UserProfile() (string, error) {
	if d.c.profile == nil {
		return "", nil
	}
	return d.c.profile.Body(), nil
}

// ResyncUserProfile regenerates the profile from current T3 facts.
func (d dashMemory) ResyncUserProfile() error {
	if d.c.profileRefresh == nil {
		return errors.New("memory: user-profile synthesis unavailable")
	}
	_, err := d.c.profileRefresh(context.Background())
	return err
}

// CompactionProvenance maps the agent's latest compaction expansion (source
// range + rehydrated turns) into the dashboard view (TEN-104). nil expand hook
// or no summary yet → (nil, nil) so the page renders "nothing compacted yet."
func (d dashMemory) CompactionProvenance() (*dashboard.CompactionProvenanceView, error) {
	if d.c.expand == nil {
		return nil, nil
	}
	exp, err := d.c.expand(context.Background())
	if err != nil || exp == nil {
		return nil, err
	}
	v := &dashboard.CompactionProvenanceView{
		HasSummary: true,
		Summary:    exp.Summary,
		SessionID:  exp.Source.SessionID,
		After:      exp.Source.After,
		Before:     exp.Source.Before,
		MsgCount:   exp.Source.MsgCount,
		Origin:     exp.Source.Origin,
	}
	for _, ev := range exp.Events {
		v.Events = append(v.Events, dashboard.ProvenanceEventView{When: ev.When, Role: ev.Role, Content: ev.Content})
	}
	return v, nil
}

// --- helpers ---

// personaProse renders the identity + values blocks (the non-list part of
// the soul) as the editor's read-only persona summary. The user-fact and
// instruction lists are exposed separately as editable items.
func personaProse(s *soul.Soul) string {
	var b strings.Builder
	if s.Agent.Name != "" {
		fmt.Fprintf(&b, "You are %s", s.Agent.Name)
		if s.Agent.Role != "" {
			fmt.Fprintf(&b, ", %s", s.Agent.Role)
		}
		b.WriteString(".\n")
	} else if s.Agent.Role != "" {
		fmt.Fprintf(&b, "You are a %s.\n", s.Agent.Role)
	}
	if s.Agent.Voice != "" {
		fmt.Fprintf(&b, "Voice: %s.\n", s.Agent.Voice)
	}
	for _, v := range s.Values.Items {
		fmt.Fprintf(&b, "- %s\n", v)
	}
	return strings.TrimRight(b.String(), "\n")
}

func toSoulItems(in []soul.Item) []dashboard.SoulItem {
	out := make([]dashboard.SoulItem, len(in))
	for i, it := range in {
		out[i] = dashboard.SoulItem{ID: it.ID, Text: it.Text}
	}
	return out
}

func toFactView(f *semantic.Fact) dashboard.FactView {
	return dashboard.FactView{
		ID: f.ID, Text: f.Fact, Confidence: f.Confidence, SourceEpisodes: f.SourceEpisodes,
	}
}

// toTemporalFactView projects a fact onto the knowledge-time axis. The store
// records only transaction time (first_seen / last_confirmed), so end-of-life
// (supersede / tombstone) carries no timestamp — it is surfaced as state via
// Status, never as a dated event. EffectiveConfidence is decay applied as of
// now. Status precedence: tombstoned > superseded > live.
func toTemporalFactView(f *semantic.Fact, now time.Time) dashboard.TemporalFactView {
	status := dashboard.FactStatusLive
	switch {
	case f.Tombstoned:
		status = dashboard.FactStatusTombstoned
	case f.SupersededBy != 0:
		status = dashboard.FactStatusSuperseded
	}
	return dashboard.TemporalFactView{
		ID:                  f.ID,
		Text:                f.Fact,
		Confidence:          f.Confidence,
		EffectiveConfidence: f.EffectiveConfidence(now),
		FirstSeen:           f.FirstSeen.Unix(),
		LastConfirmed:       f.LastConfirmed.Unix(),
		SupersededBy:        f.SupersededBy,
		Tombstoned:          f.Tombstoned,
		Status:              status,
	}
}

// parseCursor decodes the opaque keyset cursor (a fact id). "" = first page.
func parseCursor(cursor string) (int64, error) {
	if strings.TrimSpace(cursor) == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory: bad cursor %q", cursor)
	}
	return id, nil
}
