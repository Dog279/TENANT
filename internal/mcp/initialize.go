package mcp

import "encoding/json"

// Lifecycle messages per MCP spec § Lifecycle.
//
// Sequence:
//   1. Client → Server: initialize (request)
//   2. Server → Client: initialize result with negotiated capabilities
//   3. Client → Server: notifications/initialized (notification)
//   4. Normal operation
//   5. Either side → other: notifications/cancelled, or transport close

// Method names. Keep these centralized — any drift between client
// and server here is a silent protocol bug.
const (
	MethodInitialize  = "initialize"
	MethodInitialized = "notifications/initialized"
	MethodShutdown    = "shutdown" // reserved for future graceful-shutdown extension
	MethodPing        = "ping"
	MethodCancelled   = "notifications/cancelled"
	MethodProgress    = "notifications/progress"
)

// Implementation identifies the peer software in the handshake.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities is what the client advertises it can do.
// Fields are pointers so omitempty distinguishes "not supported"
// from "supported with empty configuration".
type ClientCapabilities struct {
	Experimental map[string]any         `json:"experimental,omitempty"`
	Roots        *RootsCapability       `json:"roots,omitempty"`
	Sampling     *SamplingCapability    `json:"sampling,omitempty"`
	Elicitation  *ElicitationCapability `json:"elicitation,omitempty"`
}

// ServerCapabilities is what the server advertises it can do.
type ServerCapabilities struct {
	Experimental map[string]any         `json:"experimental,omitempty"`
	Logging      *LoggingCapability     `json:"logging,omitempty"`
	Prompts      *PromptsCapability     `json:"prompts,omitempty"`
	Resources    *ResourcesCapability   `json:"resources,omitempty"`
	Tools        *ToolsCapability       `json:"tools,omitempty"`
	Completions  *CompletionsCapability `json:"completions,omitempty"`
}

// Capability presence is what matters; most carry only listChanged.
type (
	RootsCapability struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}
	SamplingCapability    struct{}
	ElicitationCapability struct{}
	LoggingCapability     struct{}
	PromptsCapability     struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}
	ResourcesCapability struct {
		Subscribe   bool `json:"subscribe,omitempty"`
		ListChanged bool `json:"listChanged,omitempty"`
	}
	ToolsCapability struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}
	CompletionsCapability struct{}
)

// InitializeParams is the request body sent by the client.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the response sent by the server.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	// Instructions is an optional human/model-readable string the
	// server can use to bias agent behavior (e.g. "always cite sources").
	Instructions string `json:"instructions,omitempty"`
}

// CancelledParams is the body of a notifications/cancelled notification.
type CancelledParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

// ProgressParams is the body of a notifications/progress notification.
// ProgressToken echoes the token the requester put in the request's
// `_meta`. Message carries the streamed human/model-readable update.
type ProgressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         *float64        `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

// ProgressTokenFrom extracts `_meta.progressToken` from a request's
// params, so a handler can report progress against it. ok is false if
// the requester didn't ask for progress.
func ProgressTokenFrom(params json.RawMessage) (json.RawMessage, bool) {
	var p struct {
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.Meta.ProgressToken) == 0 {
		return nil, false
	}
	return p.Meta.ProgressToken, true
}
