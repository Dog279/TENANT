package dashboard

// fact_inspector.go is PRD-001 / TEN-297: the memory provenance inspector.
// One fact, fully explained: where it came from (the turns, as the archive
// recorded them, and the job and model that extracted it), how sure Tenant is
// and why (confidence, decay, signals), when it held (its temporal state),
// what it contradicted or replaced, and what uses it. The operator can edit
// it (a new version), mark it no longer true, restore an earlier version, and
// compare versions. It is an optional interface next to MemoryControl, so a
// MemoryControl without it (and the existing test fakes) keeps working.

import "time"

// FactInspector is implemented by a MemoryControl that can explain and
// version facts. The fact page and its routes mount only when it is present.
type FactInspector interface {
	// FactDetail explains one fact, whatever its status.
	FactDetail(id int64) (*FactDetailView, error)
	// EditFact replaces the fact's text with a new version and returns the
	// new version's id. The old version stays, superseded.
	EditFact(id int64, text string) (int64, error)
	// InvalidateFact records that the fact stopped being true now; it stays
	// in memory for as-of recall. RevalidateFact undoes that.
	InvalidateFact(id int64) error
	RevalidateFact(id int64) error
	// RestoreFactVersion makes an earlier version current again, as a new
	// version, and returns its id.
	RestoreFactVersion(versionID int64) (int64, error)
	// FactTurns rebuilds each source turn from the archive: the user's
	// message, the tool calls and results, and the reply.
	FactTurns(id int64) ([]TurnView, error)
}

// FactDetailView is everything the inspector shows about one fact.
type FactDetailView struct {
	Fact TemporalFactView `json:"fact"`
	// Origin is the job and model that made the fact; nil when the fact
	// predates origin records.
	Origin  *OriginView `json:"origin,omitempty"`
	Signals SignalsView `json:"signals"`
	// Sources are the turns the fact was distilled from; a forgotten one is
	// marked Missing.
	Sources []EpisodeView `json:"sources"`
	// SourceRuns are the durable run ids behind the fact.
	SourceRuns []string `json:"source_runs,omitempty"`
	// RemovedSources were deleted by a forget or retention (TEN-309).
	RemovedSources []RemovedSourceView `json:"removed_sources,omitempty"`
	// Versions are every version of this fact, oldest first, this one
	// included.
	Versions []VersionView `json:"versions"`
	// Replaced are the facts this one superseded; ReplacedBy is the fact
	// that superseded it. Each carries the job that made the newer fact of
	// the pair: "distill" for a contradiction found in conversation,
	// "consolidate" for a merge, "edit"/"restore" for the operator's own
	// change. A conflict the operator resolved keeps the kept fact's job.
	Replaced   []VersionView `json:"replaced,omitempty"`
	ReplacedBy *VersionView  `json:"replaced_by,omitempty"`
	// Downstream is where the fact is used.
	Downstream DownstreamView `json:"downstream"`
}

// OriginView is what created a fact.
type OriginView struct {
	Job        string    `json:"job"`
	Model      string    `json:"model,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// SignalsView is a fact's curation and use signals.
type SignalsView struct {
	Importance   float64    `json:"importance"`
	Pinned       bool       `json:"pinned"`
	Protected    bool       `json:"protected"`
	AccessCount  int64      `json:"access_count"`
	LastAccessed *time.Time `json:"last_accessed,omitempty"`
	ValidFrom    *time.Time `json:"valid_from,omitempty"`
	// ValidTo is when the fact stopped being true; nil means it still is.
	ValidTo *time.Time `json:"valid_to,omitempty"`
}

// RemovedSourceView is a deleted source of a fact.
type RemovedSourceView struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	RemovedAt time.Time `json:"removed_at"`
	Reason    string    `json:"reason"`
}

// VersionView is one version of a fact.
type VersionView struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	FirstSeen time.Time `json:"first_seen"`
	Status    string    `json:"status"`
	// Job is what made this version, when recorded.
	Job string `json:"job,omitempty"`
}

// DownstreamView is where a fact is used.
type DownstreamView struct {
	// SMESections are the active subject-matter-expert sections citing it.
	SMESections []string `json:"sme_sections,omitempty"`
	// Retrievals is how many times recall pulled it into a turn.
	Retrievals int64 `json:"retrievals"`
}

// TurnView is one source turn rebuilt from the archive.
type TurnView struct {
	EpisodeID int64                 `json:"episode_id"`
	SessionID string                `json:"session_id,omitempty"`
	Events    []ProvenanceEventView `json:"events"`
	// Missing is set when the episode or its archived turn is gone.
	Missing bool   `json:"missing,omitempty"`
	Why     string `json:"why,omitempty"`
}
