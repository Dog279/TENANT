// Package mcp implements the Model Context Protocol wire layer:
// JSON-RPC 2.0 message types, framing, lifecycle, and session
// routing. Higher-level concerns (tool registration, planner loop)
// live in sibling packages.
package mcp

import "encoding/json"

const jsonrpcVersion = "2.0"

// ID is a JSON-RPC request identifier. The spec allows string,
// number, or null; we preserve the raw bytes so responses echo the
// peer's chosen representation exactly. Outbound IDs minted here
// are always integers.
type ID struct {
	raw json.RawMessage
}

// NewIntID mints a numeric ID. Used by the session's outbound
// request counter.
func NewIntID(n int64) ID {
	b, _ := json.Marshal(n)
	return ID{raw: b}
}

// Raw returns the underlying bytes; callers MUST NOT mutate.
func (i ID) Raw() json.RawMessage { return i.raw }

// IsZero reports whether the ID was never set (notifications).
func (i ID) IsZero() bool { return len(i.raw) == 0 }

func (i ID) MarshalJSON() ([]byte, error) {
	if len(i.raw) == 0 {
		return []byte("null"), nil
	}
	return i.raw, nil
}

func (i *ID) UnmarshalJSON(b []byte) error {
	i.raw = append(i.raw[:0], b...)
	return nil
}

// Message is the discriminated union of all JSON-RPC frames.
// Use IsRequest / IsResponse / IsNotification to classify.
//
// params and result are RawMessage so handlers decode into their
// own typed structs — avoids interface{} traffic in the hot path.
type Message struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      *ID             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func (m *Message) IsRequest() bool      { return m.Method != "" && m.ID != nil }
func (m *Message) IsNotification() bool { return m.Method != "" && m.ID == nil }
func (m *Message) IsResponse() bool     { return m.Method == "" && m.ID != nil }

// Error is the JSON-RPC 2.0 error object. It satisfies the error
// interface so handlers can return it directly.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Standard JSON-RPC 2.0 error codes. MCP-specific codes live in
// the -32000..-32099 server-defined range and are added per
// primitive (tools.go, resources.go, etc.) when needed.
const (
	ErrParseError     = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternalError  = -32603
)

// NewError builds an Error with no data payload.
func NewError(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}
