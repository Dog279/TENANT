package config

// MCPServer is one local MCP server Tenant starts and talks to over stdio
// (TEN-318): the command that starts it and the boundary it runs in. The
// boundary fields mean what they mean in exec_profiles; unset ones take the
// sandbox's defaults for a confined process.
type MCPServer struct {
	Name    string   `json:"name"`    // connector mcp:<name>: letters, digits, '-' and '_'
	Command []string `json:"command"` // the program and its arguments (under restricted on Windows: a Linux program in WSL)
	// Sandbox is the backend: restricted (the default: filesystem and network
	// confinement), jobobject (Windows: resource caps and the environment
	// allowlist only) or host (no boundary: the operator's explicit choice).
	Sandbox    string            `json:"sandbox,omitempty"`
	WorkDir    string            `json:"work_dir,omitempty"` // default <data>/mcp/<name>
	Network    string            `json:"network,omitempty"`  // none (default) | full | inherit
	ReadRoots  []string          `json:"read_roots,omitempty"`
	WriteRoots []string          `json:"write_roots,omitempty"`
	DenyRoots  []string          `json:"deny_roots,omitempty"`
	EnvPass    []string          `json:"env_pass,omitempty"` // parent variables it may see
	EnvSet     map[string]string `json:"env_set,omitempty"`
	CPUSeconds int               `json:"cpu_seconds,omitempty"`
	MemoryMB   int               `json:"memory_mb,omitempty"`
	MaxPIDs    int               `json:"max_pids,omitempty"`
}
