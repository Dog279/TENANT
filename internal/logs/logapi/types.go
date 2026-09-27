package logapi

import (
	"time"

	"tenant/internal/lifecycle"
	"tenant/internal/logs/event"
	"tenant/internal/runtrace"
)

// The API's bodies. Events travel in their wire form (event.Event's JSON);
// sensitive values are «sensitive» unless the request says sensitive=1.

// EventsPage is GET /api/events: up to limit events and the cursor that
// continues after them. Restarted says the cursor given came from another
// store (a restored or rebuilt logbook.db), so the page starts over.
type EventsPage struct {
	Events    []event.Event `json:"events"`
	Next      string        `json:"next"`
	Restarted bool          `json:"restarted"`
	Count     *int64        `json:"count,omitempty"` // every event the filter matches, with count=1
}

// EventDetail is GET /api/events/{seq}: the event, and for a catalogued one
// its catalog entry, its Help and how often it has occurred.
type EventDetail struct {
	Event event.Event `json:"event"`
	Spec  *Spec       `json:"spec,omitempty"`
	Help  string      `json:"help,omitempty"` // Markdown
	Stats *IDStats    `json:"stats,omitempty"`
}

// IDStats is how often one event ID has occurred.
type IDStats struct {
	Count int64      `json:"count"`
	First *time.Time `json:"first,omitempty"`
	Last  *time.Time `json:"last,omitempty"`
	Trend [7]int64   `json:"trend"` // per day over the last 7, oldest first
}

// Spec is one catalog entry.
type Spec struct {
	Provider  string   `json:"provider"`
	Log       string   `json:"log"`
	ID        uint32   `json:"id"`
	Level     string   `json:"level"`
	Channel   string   `json:"channel"`
	Task      string   `json:"task,omitempty"`
	Template  string   `json:"template"`
	Fields    []string `json:"fields,omitempty"`
	Sensitive []string `json:"sensitive,omitempty"`
	Keywords  []string `json:"keywords,omitempty"`
	Meaning   string   `json:"meaning,omitempty"`
	Cause     string   `json:"cause,omitempty"`
	Action    string   `json:"action,omitempty"`
	Retired   bool     `json:"retired,omitempty"`
	Filter    string   `json:"filter"` // finds its events
}

// Stats is GET /api/events/stats: counts by span, log and level.
type Stats struct {
	Buckets []Bucket `json:"buckets"`
}

// Bucket counts the events of one log at one level in one span.
type Bucket struct {
	Start *time.Time `json:"start,omitempty"` // absent without bucket=
	Log   string     `json:"log"`
	Level string     `json:"level"`
	N     int64      `json:"n"`
}

// Top is GET /api/events/top: the most frequent event IDs.
type Top struct {
	Top []IDCount `json:"top"`
}

// IDCount is how many times one event ID occurred.
type IDCount struct {
	Provider string `json:"provider"`
	ID       uint32 `json:"id"`
	N        int64  `json:"n"`
}

// Timeline is GET /api/events/timeline/{id}: the pivot (§7), and the run
// record of a trace when this process can read runs.db. RunNote says why
// there is none.
type Timeline struct {
	ID      string        `json:"id"`
	Events  []event.Event `json:"events"`
	Run     *runtrace.Run `json:"run,omitempty"`
	RunNote string        `json:"run_note,omitempty"`
}

// Policy is how a log is kept and written.
type Policy struct {
	MaxBytes int64  `json:"max_bytes"` // 0: no limit
	MaxAgeS  int64  `json:"max_age_s"` // 0: kept forever
	OnFull   string `json:"on_full"`   // overwrite, archive or keep
	MinLevel string `json:"min_level"` // "": every level
	Enabled  bool   `json:"enabled"`
}

// Log is one log: GET /api/logbook/logs[/{log}].
type Log struct {
	Name       string     `json:"name"`
	Group      []string   `json:"group"` // its place in the viewer's tree, outermost first
	Events     int64      `json:"events"`
	Bytes      int64      `json:"bytes"`
	Policy     Policy     `json:"policy"`
	LastWrite  *time.Time `json:"last_write,omitempty"`
	Dropped    int64      `json:"dropped"`
	Suppressed int64      `json:"suppressed"`
	DebugUntil *time.Time `json:"debug_until,omitempty"` // its debug switch is on until then
	Cleared    *time.Time `json:"cleared,omitempty"`
}

// Logs is GET /api/logbook/logs.
type Logs struct {
	Logs []Log `json:"logs"`
}

// PolicyChange is PATCH /api/logbook/logs/{log}. Each field given changes
// the log's own policy, which wins over config.json's; Reset returns it to
// config.json's. DebugFor turns the timed debug switch on for that long
// (at most a day), or off with "0".
type PolicyChange struct {
	MaxBytes *int64  `json:"max_bytes,omitempty"`
	MaxAgeS  *int64  `json:"max_age_s,omitempty"`
	OnFull   *string `json:"on_full,omitempty"`
	MinLevel *string `json:"min_level,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
	DebugFor *string `json:"debug_for,omitempty"`
	Reset    bool    `json:"reset,omitempty"`
}

// Cleared is POST /api/logbook/logs/{log}/clear.
type Cleared struct {
	Log     string `json:"log"`
	Cleared int64  `json:"cleared"`
}

// View is a saved filter.
type View struct {
	Name    string `json:"name"`
	Filter  string `json:"filter"`
	Builtin bool   `json:"builtin,omitempty"`
}

// Views is GET /api/logbook/views: the built-in views, then the saved ones.
type Views struct {
	Views []View `json:"views"`
}

// Catalog is GET /api/logbook/catalog[/{provider}][?id=N]: the entries and
// their reference in Markdown. Version changes whenever any entry does.
type Catalog struct {
	Version  string `json:"version"`
	Specs    []Spec `json:"specs"`
	Markdown string `json:"markdown"`
}

// Component is one thing running now (docs/design/logging-v2.md §8.7):
// the component registry's.
type Component = lifecycle.Component

// Components is GET /api/logbook/components.
type Components struct {
	Components []Component `json:"components"`
}

// Verify is GET /api/logbook/verify[?log=security]: SQLite's integrity
// check, and with log=security the Security log's chain.
type Verify struct {
	Log      string   `json:"log,omitempty"`
	OK       bool     `json:"ok"`
	Problems []string `json:"problems,omitempty"` // what the integrity check found
	Chain    *Chain   `json:"chain,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// Chain is what a walk of the Security log's chain found (store.Verify).
type Chain struct {
	Records    int64    `json:"records"`
	Head       int64    `json:"head"` // the last record verified
	Archives   int      `json:"archives"`
	Tombstones int64    `json:"tombstones"`
	Restores   int      `json:"restores"`
	Notes      []string `json:"notes,omitempty"`
	Break      string   `json:"break,omitempty"` // the first break, by record number; empty when it verifies
}

// Recovered is POST /api/logbook/recover: what could still be read of a
// quarantined logbook.db, copied into the store.
type Recovered struct {
	File      string `json:"file"`
	Recovered int    `json:"recovered"`
	Problems  string `json:"problems,omitempty"` // the stretches it could not read
}

// Accepted is POST /api/events.
type Accepted struct {
	Accepted int `json:"accepted"`
}

// ExportHeader starts every export, so a file still makes sense on another
// machine a year later. In NDJSON it is the first line, {"export": {…}}.
type ExportHeader struct {
	Schema    int       `json:"schema"`  // logbook.db's schema version
	Catalog   string    `json:"catalog"` // the catalog's version
	Tenant    string    `json:"tenant"`  // Tenant's version
	Filter    string    `json:"filter"`
	Exported  time.Time `json:"exported"`
	Host      string    `json:"host"`
	Sensitive bool      `json:"sensitive"` // sensitive values are in it
}
