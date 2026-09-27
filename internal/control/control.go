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
