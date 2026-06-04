package toolfmt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"tenant/internal/model"
)

// Llama implements the Llama 3.1+ tool-call format. Llama uses a
// special token prefix to mark tool calls in the output:
//
//	Model output:
//	  <|python_tag|>{"name": "search", "parameters": {"q": "go"}}<|eom_id|>
//
// Some Llama variants use "parameters" (per the Meta docs), others use
// "arguments" (OpenAI convention). We accept both.
//
// Tool descriptions in the system prompt follow a JSON-schema block:
//
//	Tools: You have access to the following functions:
//	[{"name":"search","description":"...","parameters":{...}}]
//	When you call a function, output:
//	<|python_tag|>{"name":"<fn>","parameters":{...}}<|eom_id|>
type Llama struct{}

func (Llama) Name() string { return "llama" }

func (Llama) FormatToolPrompt(tools []model.ToolSpec) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	arr, err := toolsAsJSONArray(tools)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("You have access to the following functions:\n")
	b.WriteString(arr)
	b.WriteString("\n")
	b.WriteString("When you decide to call a function, output exactly:\n")
	b.WriteString("<|python_tag|>{\"name\":\"<fn>\",\"parameters\":{...}}<|eom_id|>\n")
	return b.String(), nil
}

// llamaCallRE matches one <|python_tag|>{...}<|eom_id|> block. The body
// may contain {} pairs; we lazy-match up to the closing marker.
var llamaCallRE = regexp.MustCompile(`(?s)<\|python_tag\|>\s*(\{.*?\})\s*<\|eom_id\|>`)

type llamaCallPayload struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
}

// CleanText is identity: the Llama format has no reasoning/control artifacts
// to strip for display.
func (Llama) CleanText(s string) string { return s }

// StripToolCalls removes <|python_tag|>{...}<|eom_id|> blocks from visible text
// when the safety net extracts them as structured calls.
func (Llama) StripToolCalls(s string) string {
	return strings.TrimSpace(llamaCallRE.ReplaceAllString(s, ""))
}

func (Llama) ParseToolCalls(content string) ([]model.ToolCall, error) {
	matches := llamaCallRE.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	out := make([]model.ToolCall, 0, len(matches))
	for i, m := range matches {
		var p llamaCallPayload
		if err := json.Unmarshal([]byte(m[1]), &p); err != nil {
			return nil, fmt.Errorf("llama: parse python_tag call %d: %w", i, err)
		}
		if p.Name == "" {
			return nil, fmt.Errorf("llama: call %d missing name", i)
		}
		args := p.Arguments
		if len(args) == 0 {
			args = p.Parameters
		}
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
