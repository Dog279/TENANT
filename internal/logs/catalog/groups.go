package catalog

import "strings"

// Group is where a log sits in the viewer's tree (docs/design/logging-v2.md
// §9.2), outermost first: Tenant's own logs, the subsystems by kind, and
// External for logs other programs write. A log named by nobody here is a
// subsystem of its own, so a new logs.For("x") still gets a place.
func Group(log string) []string {
	switch {
	case strings.HasPrefix(log, "ext."):
		return []string{"External"}
	case log == "mcp" || strings.HasPrefix(log, "mcp."):
		return []string{"Subsystems", "Integrations", "MCP"}
	case strings.HasPrefix(log, "plugin."):
		return []string{"Subsystems", "Integrations", "Plugins"}
	}
	if g, ok := groups[log]; ok {
		return g
	}
	return []string{"Subsystems"}
}

var (
	tenant     = []string{"Tenant"}
	channels   = []string{"Subsystems", "Channels"}
	model      = []string{"Subsystems", "Model"}
	memory     = []string{"Subsystems", "Memory"}
	backupPriv = []string{"Subsystems", "Backup and Privacy"}

	groups = map[string][]string{
		"system": tenant, "security": tenant, "turns": tenant, "subagents": tenant, "alerts": tenant,
		"approvals": tenant, // v1's folder; its records move into security (LOG-062)
		"logbook":   tenant,
		"discord":   channels, "imessage": channels,
		"cron":  {"Subsystems", "Scheduler"},
		"model": model, "route": model,
		"memory": memory, "improve": memory, "skills": memory, "research": memory,
		"sandbox":   {"Subsystems", "Sandbox"},
		"dashboard": {"Subsystems", "Dashboard"},
		"peering":   {"Subsystems", "Peering"},
		"backup":    backupPriv, "privacy": backupPriv,
		"eval":    {"Subsystems", "Eval"},
		"browser": {"Subsystems", "Integrations", "Plugins"},
		"toolmux": {"Subsystems", "Integrations", "Plugins"},
	}
)
