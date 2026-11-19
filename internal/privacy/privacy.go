// Package privacy removes what Tenant keeps, on request or by age, and says
// what it will remove before it does (OPS-003 / TEN-309).
//
// One planner serves both callers. A forget request (a session, an episode,
// a fact, a skill, some text, everything before a date) and a retention
// prune (each data class older than its configured age) both become a Plan:
// a preview of every row, file and archive event that goes, and of the links
// that get repaired. Apply then deletes for real:
//   - rows are deleted, not tombstoned, with SQLite's secure_delete on, so
//     freed pages are zeroed;
//   - the full-text indexes are optimized so no deleted words stay behind;
//   - archive session files are rewritten without the removed events.
//
// Provenance stays consistent. A fact that loses an episode or run it was
// distilled from keeps the rest of its sources and records the removed one
// in fact_source_tombstones. A forget also deletes facts every source of
// which is gone. A retention prune keeps them, because distilled memory
// outlives the raw turns it came from.
package privacy

import (
	"time"
)

// Class is a kind of data Tenant keeps.
type Class string

// The classes a retention period can be set for.
const (
	Episodes      Class = "episodes"       // conversation turns (episodes.db)
	Facts         Class = "facts"          // distilled facts not confirmed within the period (facts.db)
	Archive       Class = "archive"        // raw session events (archive/)
	Usage         Class = "usage"          // token usage ledger (usage.db)
	Research      Class = "research"       // research runs and reports (research/)
	Screenshots   Class = "screenshots"    // browser screenshots (screenshots/)
	RelayInbox    Class = "relay_inbox"    // files received over the Discord relay (relay-inbox/)
	EvalArtifacts Class = "eval_artifacts" // eval reports (eval-artifacts/eval-*.json)
)

// Classes lists the retention classes in display order.
var Classes = []Class{Episodes, Facts, Archive, Usage, Research, Screenshots, RelayInbox, EvalArtifacts}

// Retention is how many days each class is kept. A class that is absent or
// set to 0 or less is kept forever.
type Retention map[Class]int

// Reason says why data was removed. It is recorded on the provenance of the
// facts that outlive their sources.
type Reason string

const (
	// ReasonForget is an operator's request to remove specific data.
	ReasonForget Reason = "forget"
	// ReasonRetention is a data class ageing out.
	ReasonRetention Reason = "retention"
)

// Selector picks what a forget removes. Fields combine: anything any of them
// selects goes, along with facts, skills and SME sections derived only from
// it.
type Selector struct {
	Sessions []string
	Episodes []int64
	// Runs are run trace ids: the episodes recorded for those turns go. A
	// fleet machine's memory finds the turns a forget of the master's
	// episodes named by them (TEN-532).
	Runs  []string
	Facts []int64
	// Skills are skill names.
	Skills []string
	// Match is text to forget wherever it appears: episodes, facts, skills
	// and their history, SME sections, archive events, and lines of the
	// synthesized user profile. Letters match regardless of ASCII case.
	Match string
	// Before removes every episode, run, archive event, usage row, research
	// run, screenshot, relay file and eval report recorded before it.
	Before time.Time
	// ArchiveMonths are archive month directories ("2026-05") to remove.
	ArchiveMonths []string
	// Agent limits the forget to one agent's data; empty means every agent.
	// Files that belong to no agent (research, screenshots, relay files,
	// eval reports) are left alone when it is set.
	Agent string
}

// Empty reports whether the selector selects nothing.
func (s Selector) Empty() bool {
	return len(s.Sessions) == 0 && len(s.Episodes) == 0 && len(s.Runs) == 0 && len(s.Facts) == 0 && len(s.Skills) == 0 &&
		s.Match == "" && s.Before.IsZero() && len(s.ArchiveMonths) == 0
}
