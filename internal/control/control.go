// Package control holds the plain data types the control plane passes
// between the runtime, the terminal UI and the dashboard (TEN-314): a leaf, so
// every side can share them without importing each other.
package control

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
}

// PermissionInfo is one safety category's mode, as /permissions lists it.
type PermissionInfo struct {
	Category string
	Mode     string // ask | allow | deny
	Desc     string
}

// ToolRuleInfo is one per-tool permission rule (pattern → mode): the layer the
// operator fills in for tools the categories cannot know in advance.
type ToolRuleInfo struct {
	Pattern string // exact action id, bare tool name, or glob
	Mode    string // ask | allow | deny
	Kind    string // exact | name | glob
}
