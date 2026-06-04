package toolfmt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"tenant/internal/model"
)

// Mistral implements the Mistral function-call format. Mistral models
// emit a [TOOL_CALLS] tag followed by a JSON array:
//
//	Model output:
//	  [TOOL_CALLS] [{"name": "search", "arguments": {"q": "go"}}]
//
// Tool descriptions in the system prompt use a JSON-schema array.
type Mistral struct{}

func (Mistral) Name() string { return "mistral" }

func (Mistral) FormatToolPrompt(tools []model.ToolSpec) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	arr, err := toolsAsJSONArray(tools)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("You have access to the following tools:\n")
	b.WriteString(arr)
	b.WriteString("\n")
	b.WriteString("To call a tool, output exactly:\n")
	b.WriteString(`[TOOL_CALLS] [{"name":"<fn>","arguments":{...}}]`)
	return b.String(), nil
}

// mistralCallRE matches `[TOOL_CALLS]` followed by a JSON array.
// Greedy on the array body to allow nested {}.
var mistralCallRE = regexp.MustCompile(`(?s)\[TOOL_CALLS\]\s*(\[.*?\])`)

type mistralCallPayload struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// CleanText is identity: the Mistral format has no reasoning/control
// artifacts to strip for display.
func (Mistral) CleanText(s string) string { return s }

// StripToolCalls removes the [TOOL_CALLS][...] block when the safety net
// extracts it as structured calls.
func (Mistral) StripToolCalls(s string) string {
	return strings.TrimSpace(mistralCallRE.ReplaceAllString(s, ""))
}

func (Mistral) ParseToolCalls(content string) ([]model.ToolCall, error) {
	m := mistralCallRE.FindStringSubmatch(content)
	if m == nil {
		return nil, nil
	}
	var arr []mistralCallPayload
	if err := json.Unmarshal([]byte(m[1]), &arr); err != nil {
		return nil, fmt.Errorf("mistral: parse [TOOL_CALLS] array: %w", err)
	}
	out := make([]model.ToolCall, 0, len(arr))
	for i, p := range arr {
		if p.Name == "" {
			return nil, fmt.Errorf("mistral: call %d missing name", i)
		}
		args := p.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		out = append(out, model.ToolCall{
			ID:        fmt.Sprintf("call_%d", i),
			Name:      p.Name,
			Arguments: args,
		})
	}
	return out, nil
}
