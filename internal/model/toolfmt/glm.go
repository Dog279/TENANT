package toolfmt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"tenant/internal/model"
)

// GLM implements the tool-call format of Z.ai's GLM-4.5 family and later
// (GLM-4.5/4.6/4.7, GLM-5), as the family's chat template writes it:
//
//	System prompt fragment:
//	  # Tools
//	  You may call one or more functions to assist with the user query.
//	  You are provided with function signatures within <tools></tools> XML tags:
//	  <tools>
//	  {"type":"function","function":{"name":"...","description":"...","parameters":{...}}}
//	  </tools>
//	  For each function call, output the function name and arguments within the following XML format:
//	  <tool_call>{function-name}
//	  <arg_key>{arg-key-1}</arg_key>
//	  <arg_value>{arg-value-1}</arg_value>
//	  ...
//	  </tool_call>
//
//	Model output:
//	  <tool_call>get_weather
//	  <arg_key>city</arg_key>
//	  <arg_value>Beijing</arg_value>
//	  <arg_key>days</arg_key>
//	  <arg_value>3</arg_value>
//	  </tool_call>
//
// The template writes a string value bare and any other value as JSON, so a
// value that reads as JSON is kept typed and the rest is a string (as the
// Hermes XML parser does). llama.cpp (--jinja) and vLLM (--tool-call-parser
// glm45) parse this server-side; ParseToolCalls is the safety net when they
// don't, and FormatToolPrompt the prompt for a backend without native tools.
// The JSON `{"name":…}` and Hermes XML shapes some GLM serving stacks and
// fine-tunes write instead are Qwen's to read. Thinking goes in <think>
// blocks, as Qwen's.
type GLM struct{}

func (GLM) Name() string { return "glm" }

func (GLM) FormatToolPrompt(tools []model.ToolSpec) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("# Tools\n\nYou may call one or more functions to assist with the user query.\n\n" +
		"You are provided with function signatures within <tools></tools> XML tags:\n<tools>\n")
	for _, t := range tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		line, err := json.Marshal(map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": params,
		}})
		if err != nil {
			return "", fmt.Errorf("glm: tool %s: %w", t.Name, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteString("</tools>\n\nFor each function call, output the function name and arguments within the following XML format:\n" +
		"<tool_call>{function-name}\n<arg_key>{arg-key-1}</arg_key>\n<arg_value>{arg-value-1}</arg_value>\n" +
		"<arg_key>{arg-key-2}</arg_key>\n<arg_value>{arg-value-2}</arg_value>\n...\n</tool_call>")
	return b.String(), nil
}

var (
	// glmToolCallRE matches one block: the name after the open tag, then the
	// argument pairs up to the close tag — or the end of the text, so a call
	// cut off by max_tokens still yields its complete pairs. Group 3 is empty
	// for a cut-off block.
	glmToolCallRE = regexp.MustCompile(`(?s)<tool_call>\s*([^\s<]+)\s*(.*?)(</tool_call>|\z)`)
	glmArgRE      = regexp.MustCompile(`(?s)<arg_key>\s*(.*?)\s*</arg_key>\s*<arg_value>(.*?)</arg_value>`)
)

// glmBlock says whether a match is a tool call rather than the words
// "<tool_call>" in prose: it closed, or it carries a whole argument pair.
func glmBlock(m []string) bool {
	return m[3] != "" || glmArgRE.MatchString(m[2])
}

func (GLM) ParseToolCalls(content string) ([]model.ToolCall, error) {
	matches := glmToolCallRE.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 || strings.HasPrefix(matches[0][1], "{") {
		return Qwen{}.ParseToolCalls(content)
	}
	out := make([]model.ToolCall, 0, len(matches))
	for i, m := range matches {
		if !glmBlock(m) {
			continue
		}
		body := strings.TrimSpace(m[2])
		var args json.RawMessage
		if strings.HasPrefix(body, "{") {
			// A JSON object after the name: some servers' GLM parsers were
			// trained on it, and a weak model writes it. Repaired as Qwen's.
			if !json.Valid([]byte(body)) {
				body = RepairJSON(body)
			}
			if !json.Valid([]byte(body)) {
				return nil, fmt.Errorf("glm: tool_call %d: arguments are not JSON", i)
			}
			args = json.RawMessage(body)
		} else {
			argsMap := map[string]json.RawMessage{}
			for _, am := range glmArgRE.FindAllStringSubmatch(body, -1) {
				key, val := strings.TrimSpace(am[1]), strings.TrimSpace(am[2])
				if key == "" {
					continue
				}
				if val != "" && json.Valid([]byte(val)) {
					argsMap[key] = json.RawMessage(val)
				} else {
					b, _ := json.Marshal(val)
					argsMap[key] = json.RawMessage(b)
				}
			}
			args, _ = json.Marshal(argsMap)
		}
		out = append(out, model.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: m[1], Arguments: args})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// CleanText strips <think> blocks, which GLM's thinking mode writes as Qwen does.
func (GLM) CleanText(s string) string { return Qwen{}.CleanText(s) }

// StripToolCalls removes the blocks ParseToolCalls reads (a cut-off one
// included), then the JSON and Hermes shapes.
func (GLM) StripToolCalls(s string) string {
	s = glmToolCallRE.ReplaceAllStringFunc(s, func(block string) string {
		if m := glmToolCallRE.FindStringSubmatch(block); m != nil && !glmBlock(m) {
			return block
		}
		return ""
	})
	return Qwen{}.StripToolCalls(s)
}
