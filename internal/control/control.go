// Package control holds the plain data types the control plane passes
// between the runtime, the terminal UI and the dashboard (TEN-314): a leaf, so
// every side can share them without importing each other.
package control

import (
	"context"

	"tenant/internal/logs/event"
)

// ToolInfo is one tool's state for listings: its name, the plugin that owns
// it, whether it is enabled, and whether calling it asks for approval.
type ToolInfo struct {
	Name    string
	Plugin  string
	Enabled bool
	Gated   bool
}

// ApprovalDecision is the operator's answer to an approval request.
type ApprovalDecision int

const (
	DenyOnce ApprovalDecision = iota
	ApproveOnce
	ApproveSession
	ApproveAlways
	// Expired and DenyShutdown refuse exactly like DenyOnce; they record why
	// no operator decided: the prompt timed out, or the process shut down.
	Expired
	DenyShutdown
)

// ApprovalRequest is a dangerous action paused awaiting the operator. The
// broker that raised it blocks on Reply until a surface sends a decision (or
// the turn's context is cancelled).
type ApprovalRequest struct {
	ID       string // broker-assigned id (TEN-203), the key a surface resolves by
	Category string // safety category (exec, destructive, web, send, …)
	Action   string // specific action id (os_exec, web_transact, …)
	Detail   string // human-readable description of exactly what will run
	Reply    chan ApprovalDecision
}

// PendingApproval is one still-pending approval, as the dashboard lists it.
type PendingApproval struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Action   string `json:"action"`
	Detail   string `json:"detail"`
	AgeSecs  int    `json:"age_secs"`
	Origin   string `json:"origin,omitempty"` // the broker that asked: local | imessage
	// SessionOK: approving "for this session" grants the category until
	// restart. False where it only covers this request (a tool rule forced
	// the prompt, or a pairing or an alert asked).
	SessionOK bool `json:"session_ok,omitempty"`
}

// PermissionInfo is one safety category's mode, as /permissions lists it.
type PermissionInfo struct {
	Category string
	Mode     string // ask | allow | deny
	Desc     string
	Title    string // plain name: "Run commands"
	Can      string // what Allow lets the agent do: "run commands and programs on this computer"
	New      bool   // an update added it and the operator never chose it, so it asks (TEN-343)
	Session  bool   // an "approve for this session" grant skips its prompts until restart
}

// ToolRuleInfo is one per-tool permission rule (pattern → mode): the layer the
// operator fills in for tools the categories cannot know in advance.
type ToolRuleInfo struct {
	Pattern string // exact action id, bare tool name, or glob
	Mode    string // ask | allow | deny
	Kind    string // exact | name | glob
}

// LogsControl is the log book as the terminal UI's /logs reads it (TEN-381).
// The composition root answers through the log book's API, so a filter is
// the language every viewer takes and sensitive values arrive masked.
type LogsControl interface {
	// Query is the newest limit events the filter matches, oldest first, and
	// the cursor a live tail resumes from after them. A filter that doesn't
	// parse is an error that says why.
	Query(ctx context.Context, filter string, limit int) (evs []event.Event, cursor string, err error)
	// Follow sends each event the filter matches that is written after
	// cursor, until ctx is done; then it closes the channel.
	Follow(ctx context.Context, filter, cursor string) <-chan event.Event
	// Event is one event in full, as text: its fields, how often it has
	// occurred and its catalog Help.
	Event(ctx context.Context, seq uint64) (string, error)
	// Timeline is the pivot on a trace or activity ID: its events across
	// every log, oldest first, and a line about its run.
	Timeline(ctx context.Context, id string) (head string, evs []event.Event, err error)
}
