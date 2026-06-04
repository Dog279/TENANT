package dashboard

// memory.go is TEN-88: the dashboard's Memory Curator control surface —
// the interface + JSON-friendly view types the REST layer (TEN-89) will
// build against. NO routes are mounted here; TEN-89 adds mountMemoryREST
// in its own file, exactly as rest.go/ws.go did. The interface is
// implemented in cmd/tenant by an adapter over memControl (mirroring the
// dashTools adapter for ToolControl).

import "time"

// MemoryControl is the runtime memory-curation surface: view + curate the
// agent's soul (T0) and distilled facts (T3), with provenance, deletion,
// contradiction resolution, and restore. All return types are defined in
// this package so the REST layer stays decoupled from the memory stores.
// A nil MemoryControl is valid — TEN-89 simply won't mount memory routes.
type MemoryControl interface {
	// Soul returns the editable soul: persona prose plus the user-fact and
	// instruction lists, each item carrying a stable derived ID.
	Soul() (SoulView, error)
	// SoulEdit applies one add/edit/remove to a soul list by derived ID.
	SoulEdit(op SoulEditOp) error

	// Facts returns live (non-tombstoned) facts. q filters by relevance
	// when non-empty; limit caps the page; cursor is an opaque keyset token
	// ("" = first page). The returned nextCursor is "" when no more remain.
	Facts(q string, limit int, cursor string) (facts []FactView, nextCursor string, err error)
	// FactProvenance resolves a fact's SourceEpisodes to the originating
	// turns. A missing/forgotten source episode is returned as a marked
	// EpisodeView (Missing=true), never an error for the whole call.
	FactProvenance(id int64) ([]EpisodeView, error)
	// ResolveFacts records that keepID supersedes discardID (Supersede).
	ResolveFacts(keepID, discardID int64) error
	// DeleteFact tombstones a fact (recoverable via RestoreFact).
	DeleteFact(id int64) error
	// RestoreFact un-tombstones a previously deleted fact (Reaffirm).
	RestoreFact(id int64) error
	// RemovedFacts lists tombstoned facts for the restore view.
	RemovedFacts(limit int) ([]FactView, error)

	// TemporalFacts returns every fact — live, superseded, and tombstoned —
	// projected onto the knowledge-time axis: the transaction-time columns
	// (first_seen / last_confirmed), the supersession pointer, a server-
	// computed effective confidence (decay applied), and a derived status,
	// plus a count summary. It backs the read-only Overview; unlike Facts (a
	// live page) it is a full enumeration and does not paginate.
	TemporalFacts() ([]TemporalFactView, MemStats, error)

	// WorkingCount reports the T1 working-set size (status page; no list).
	WorkingCount() int

	// UserProfile returns the synthesized always-on user model (read-only).
	UserProfile() (string, error)
	// ResyncUserProfile regenerates the profile from current T3 facts.
	ResyncUserProfile() error

	// CompactionProvenance returns the latest compaction summary's source range
	// and the original archived turns it replaced (read-only audit — TEN-104).
	// A nil result (with nil error) is valid: nothing compacted yet, or no
	// expansion source wired.
	CompactionProvenance() (*CompactionProvenanceView, error)
}

// SoulView is the curatable soul. Persona is human-readable identity prose
// (agent identity + values); the two lists are individually editable.
type SoulView struct {
	Persona      string     `json:"persona"`
	UserFacts    []SoulItem `json:"user_facts"`
	Instructions []SoulItem `json:"instructions"`
}

// SoulItem is one soul list element with its stable, content-derived ID.
type SoulItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Soul edit sections — which list an edit targets.
const (
	SoulSectionUserFact    = "user_fact"
	SoulSectionInstruction = "instruction"
)

// Soul edit actions.
const (
	SoulActionAdd    = "add"
	SoulActionEdit   = "edit"
	SoulActionRemove = "remove"
)

// SoulEditOp is one field-level soul mutation. Section is user_fact or
// instruction; Action is add/edit/remove. ID identifies the target item
// for edit/remove (ignored for add); Text is the new content for add/edit
// (ignored for remove).
type SoulEditOp struct {
	Section string `json:"section"`
	Action  string `json:"action"`
	ID      string `json:"id,omitempty"`
	Text    string `json:"text,omitempty"`
}

// FactView is one semantic fact as the curator renders it.
type FactView struct {
	ID             int64   `json:"id"`
	Text           string  `json:"text"`
	Confidence     float64 `json:"confidence"`
	SourceEpisodes []int64 `json:"source_episodes,omitempty"`
}

// TemporalFactView is one fact projected onto the knowledge-time axis: the
// curator's id/text/confidence plus the transaction-time columns the store
// actually records, a server-computed EffectiveConfidence (decay applied as
// of the request), and a derived lifecycle status. Timestamps are unix
// seconds. There is deliberately NO valid-time here: the store records when a
// fact was learned and last confirmed, not when it became true in the world,
// so the Overview must not draw validity intervals.
type TemporalFactView struct {
	ID                  int64   `json:"id"`
	Text                string  `json:"text"`
	Confidence          float64 `json:"confidence"`
	EffectiveConfidence float64 `json:"effective_confidence"`
	FirstSeen           int64   `json:"first_seen"`
	LastConfirmed       int64   `json:"last_confirmed"`
	SupersededBy        int64   `json:"superseded_by,omitempty"`
	Tombstoned          bool    `json:"tombstoned"`
	Status              string  `json:"status"`
}

// Fact lifecycle statuses for TemporalFactView.Status. A fact is live when it
// is neither tombstoned nor superseded — the same predicate the store uses to
// enumerate live facts. Tombstoned takes precedence over superseded so the
// buckets stay mutually exclusive and sum to the total.
const (
	FactStatusLive       = "live"
	FactStatusSuperseded = "superseded"
	FactStatusTombstoned = "tombstoned"
)

// MemStats is the Overview's count summary. Only fact counts are reported —
// the entity/edge tables the proposal's graph needs don't exist yet, so no
// graph counts are surfaced.
type MemStats struct {
	Total      int `json:"total"`
	Live       int `json:"live"`
	Superseded int `json:"superseded"`
	Tombstoned int `json:"tombstoned"`
}

// CompactionProvenanceView is the audit view of the latest compaction summary
// (TEN-104): what archive range it covered and the original turns within it.
// HasSummary is false when nothing has been compacted yet.
type CompactionProvenanceView struct {
	HasSummary bool
	Summary    string
	SessionID  string
	After      time.Time
	Before     time.Time
	MsgCount   int
	Origin     string
	Events     []ProvenanceEventView
}

// ProvenanceEventView is one rehydrated archived turn behind a compaction summary.
type ProvenanceEventView struct {
	When    time.Time
	Role    string
	Content string
}

// EpisodeView is one provenance turn behind a fact. Missing marks a source
// episode that no longer exists (forgotten / predates provenance) so the
// UI can render "source unavailable" instead of breaking — ID is still set
// so the operator sees which episode is gone.
type EpisodeView struct {
	ID        int64     `json:"id"`
	Prompt    string    `json:"prompt,omitempty"`
	Response  string    `json:"response,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty"`
	Missing   bool      `json:"missing,omitempty"`
}
