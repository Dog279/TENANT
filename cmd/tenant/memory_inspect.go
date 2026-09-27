package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/sme"
)

// The memory adapter's side of the provenance inspector (PRD-001 / TEN-297):
// dashMemory answers dashboard.FactInspector from the stores and the archive.

var _ dashboard.FactInspector = dashMemory{}

// FactDetail explains one fact, whatever its status.
func (d dashMemory) FactDetail(id int64) (*dashboard.FactDetailView, error) {
	ctx := context.Background()
	now := time.Now()
	f, err := d.c.semantic.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v := &dashboard.FactDetailView{Fact: toTemporalFactView(f, now), SourceRuns: f.SourceRunIDs}
	origin, ok, err := d.c.semantic.Origin(ctx, id)
	if err != nil {
		return nil, err
	}
	if ok {
		v.Origin = &dashboard.OriginView{Job: origin.Job, Model: origin.Model, RecordedAt: origin.RecordedAt}
	}
	sig, err := d.c.semantic.GetSignals(ctx, id)
	if err != nil {
		return nil, err
	}
	v.Signals = dashboard.SignalsView{
		Importance: sig.Importance, Pinned: sig.Pinned, Protected: sig.Protected, AccessCount: sig.AccessCount,
		LastAccessed: timePtr(sig.LastAccessed), ValidFrom: timePtr(sig.ValidFrom), ValidTo: timePtr(sig.ValidTo),
	}
	if v.Sources, err = d.FactProvenance(id); err != nil {
		return nil, err
	}
	removed, err := d.c.semantic.RemovedSources(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, r := range removed {
		v.RemovedSources = append(v.RemovedSources, dashboard.RemovedSourceView{Kind: r.Kind, ID: r.ID, RemovedAt: r.RemovedAt, Reason: r.Reason})
	}
	versions, err := d.c.semantic.Versions(ctx, id)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]dashboard.VersionView, len(versions))
	for _, ver := range versions {
		vv := dashboard.VersionView{ID: ver.ID, Text: ver.Fact, FirstSeen: ver.FirstSeen, Status: toTemporalFactView(ver, now).Status}
		o, ok, err := d.c.semantic.Origin(ctx, ver.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			vv.Job = o.Job
		}
		byID[ver.ID] = vv
		v.Versions = append(v.Versions, vv)
	}
	// A replacement carries the job that made the newer fact of the pair.
	for _, ver := range versions {
		if ver.SupersededBy == id {
			older := byID[ver.ID]
			older.Job = byID[id].Job
			v.Replaced = append(v.Replaced, older)
		}
	}
	if newer, ok := byID[f.SupersededBy]; ok && f.SupersededBy != 0 {
		v.ReplacedBy = &newer
	}
	v.Downstream.Retrievals = sig.AccessCount
	if st, err := sme.New(d.c.semantic.DB()); err == nil {
		secs, err := st.CitingFact(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, s := range secs {
			v.Downstream.SMESections = append(v.Downstream.SMESections, s.Section)
		}
	}
	return v, nil
}

// EditFact saves new text as a new version of a current fact.
func (d dashMemory) EditFact(id int64, text string) (int64, error) {
	emb, embedderID, err := d.embedVersion(id, text)
	if err != nil {
		return 0, err
	}
	return d.c.semantic.Edit(context.Background(), id, text, "edit", embedderID, emb)
}

// RestoreFactVersion makes an earlier version current again, as a new one.
func (d dashMemory) RestoreFactVersion(versionID int64) (int64, error) {
	ctx := context.Background()
	old, err := d.c.semantic.Get(ctx, versionID)
	if err != nil {
		return 0, err
	}
	emb, embedderID, err := d.embedVersion(versionID, old.Fact)
	if err != nil {
		return 0, err
	}
	return d.c.semantic.RestoreVersion(ctx, versionID, embedderID, emb)
}

// embedVersion embeds a new version's text with the live embedder, so it is
// found by recall like any other fact. Like the remember tool, it refuses
// rather than save a fact recall could not find. Without a configured
// embedder id it uses the one the fact was embedded with.
func (d dashMemory) embedVersion(id int64, text string) ([]float32, string, error) {
	emb := d.c.embed(text)
	if len(emb) == 0 {
		return nil, "", errors.New("the embedder is unavailable, so the new version could not be made findable; try again once embeddings are back")
	}
	if d.c.embedderID != "" {
		return emb, d.c.embedderID, nil
	}
	f, err := d.c.semantic.Get(context.Background(), id)
	if err != nil {
		return nil, "", err
	}
	return emb, f.EmbedderID, nil
}

// InvalidateFact records that the fact stopped being true now. It stays in
// memory for as-of recall.
func (d dashMemory) InvalidateFact(id int64) error {
	return d.c.semantic.SetValidTo(context.Background(), id, time.Now().UTC())
}

// RevalidateFact records that the fact is true again.
func (d dashMemory) RevalidateFact(id int64) error {
	return d.c.semantic.SetValidTo(context.Background(), id, time.Time{})
}

// FactTurns rebuilds each source turn of the fact from the archive.
func (d dashMemory) FactTurns(id int64) ([]dashboard.TurnView, error) {
	ctx := context.Background()
	f, err := d.c.semantic.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	reader := archive.NewReader(d.c.dataDir)
	out := make([]dashboard.TurnView, 0, len(f.SourceEpisodes))
	for _, epID := range f.SourceEpisodes {
		tv := dashboard.TurnView{EpisodeID: epID}
		ep, err := d.c.episodic.Get(ctx, epID)
		switch {
		case errors.Is(err, episodic.ErrNotFound):
			tv.Missing, tv.Why = true, "the conversation is no longer in memory"
		case err != nil:
			return nil, fmt.Errorf("episode %d: %w", epID, err)
		case d.c.dataDir == "":
			tv.SessionID = ep.SessionID
			tv.Missing, tv.Why = true, "the archive isn't available here"
		default:
			tv.SessionID = ep.SessionID
			events, err := reader.Turn(ep.SessionID, ep.Prompt, ep.Timestamp)
			if err != nil {
				return nil, fmt.Errorf("episode %d: %w", epID, err)
			}
			if len(events) == 0 {
				tv.Missing, tv.Why = true, "the archive has no record of it (pruned, or older than the archive)"
			}
			for _, ev := range events {
				tv.Events = append(tv.Events, dashboard.ProvenanceEventView{When: ev.Timestamp, Role: ev.Role, Content: turnEventText(ev)})
			}
		}
		out = append(out, tv)
	}
	return out, nil
}

// turnEventText is what the inspector shows for one archived event: its text,
// or for a tool call or result, what the tool was asked and answered.
func turnEventText(ev archive.Event) string {
	switch {
	case len(ev.ToolCalls) > 0:
		s := ev.Content
		for _, tc := range ev.ToolCalls {
			if s != "" {
				s += "\n"
			}
			s += "→ " + tc.Name + " " + string(tc.Arguments)
		}
		return s
	case ev.ToolResult != nil && ev.Content == "":
		if ev.ToolResult.IsError {
			return "error: " + ev.ToolResult.Content
		}
		return ev.ToolResult.Content
	}
	return ev.Content
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
