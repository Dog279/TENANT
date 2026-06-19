package toolfmt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"tenant/internal/model"
)

// Qwen implements the Hermes-Pro / Qwen ChatML tool-call format.
// This is what Qwen 2.5+ and Qwen 3.x families emit. Reference shape:
//
//	System prompt fragment:
//	  You have access to the following tools:
//	  <tools>
//	  [{"name":"...","description":"...","parameters":{...}}]
//	  </tools>
//	  For each function call, return a json object with function name
//	  and arguments within <tool_call></tool_call> XML tags.
//
//	Model output:
//	  <tool_call>
//	  {"name": "search", "arguments": {"q": "go"}}
//	  </tool_call>
//
// vLLM's `--tool-call-parser hermes` extracts these into structured
// tool_calls. Our ParseToolCalls is the safety net for when that flag
// is missing or the model formats slightly off-spec.
type Qwen struct{}

func (Qwen) Name() string { return "qwen" }

func (Qwen) FormatToolPrompt(tools []model.ToolSpec) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	arr, err := toolsAsJSONArray(tools)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("You have access to the following tools:\n<tools>\n")
	b.WriteString(arr)
	b.WriteString("\n</tools>\n")
	b.WriteString("For each function call, return a json object with the function name and arguments within <tool_call></tool_call> XML tags.")
	return b.String(), nil
}

// qwenToolCallRE matches one <tool_call>...</tool_call> block (non-greedy).
// The (?s) flag enables . to match newlines.
var qwenToolCallRE = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

// hermesXMLFunctionRE matches the Hermes XML tool-call shape that
// NousResearch Hermes models — and some Qwen-derived merges like
// aeon-ultimate — emit instead of the documented JSON shape:
//
//	<tool_call>
//	  <function=web_search>
//	    <parameter=query>"Hermes 4" vLLM</parameter>
//	    <parameter=k>5</parameter>
//	  </function>
//	</tool_call>
//
// vLLM's --tool-call-parser hermes parses this server-side, but merged-model
// drift means the parser doesn't always recognize the variant — when it
// misses, the entire XML blob lands in resp.Text and the agent treats it as
// a final answer. Parsing it ourselves recovers the call. We match
// <function=NAME>...</function> as the unit (the outer <tool_call> wrapper is
// optional context) — some emissions drop the </tool_call> closer.
var (
	hermesXMLFunctionRE = regexp.MustCompile(`(?s)<function=([^>\s]+)>(.*?)</function>`)
	hermesXMLParamRE    = regexp.MustCompile(`(?s)<parameter=([^>\s]+)>\s*(.*?)\s*</parameter>`)
)

// parseHermesXMLCalls extracts Hermes-shape tool calls from raw model text.
// Per-call malformations are tolerated: a parameter that doesn't close cleanly
// is skipped, not fatal. Values are JSON-decoded if they parse cleanly (so
// `<parameter=k>5</parameter>` becomes the number 5, not the string "5"),
// otherwise marshaled as strings.
func parseHermesXMLCalls(content string) []model.ToolCall {
	matches := hermesXMLFunctionRE.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]model.ToolCall, 0, len(matches))
	for i, m := range matches {
		name := strings.TrimSpace(m[1])
		if name == "" {
			continue
		}
		argsMap := map[string]json.RawMessage{}
		for _, pm := range hermesXMLParamRE.FindAllStringSubmatch(m[2], -1) {
			key := strings.TrimSpace(pm[1])
			val := strings.TrimSpace(pm[2])
			if key == "" {
				continue
			}
			// Prefer typed values: a bare number/bool/JSON-object value goes
			// in as-is; everything else gets JSON-string-encoded. This makes
			// `<parameter=k>5</parameter>` arrive as int rather than the
			// string "5" (the tool-arg validator type-checks).
			if val != "" && json.Valid([]byte(val)) {
				argsMap[key] = json.RawMessage(val)
			} else {
				b, _ := json.Marshal(val)
				argsMap[key] = json.RawMessage(b)
			}
		}
		argsJSON, err := json.Marshal(argsMap)
		if err != nil {
			argsJSON = []byte(`{}`)
		}
		out = append(out, model.ToolCall{
			ID:        fmt.Sprintf("call_%d", i),
			Name:      name,
			Arguments: argsJSON,
		})
	}
	return out
}

// qwenThinkRE matches a <think>…</think> / <thinking>…</thinking> reasoning
// block (Qwen3 / DeepSeek-R1 thinking mode). The real answer follows the close.
var qwenThinkRE = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)

// StripToolCalls removes both the JSON-shape `<tool_call>{...}</tool_call>`
// and the Hermes XML `<tool_call><function=...>...</function></tool_call>`
// blocks. Called by the vLLM safety-net path so XML tool calls don't end up
// in resp.Text after we've already extracted them as structured calls — a
// LIVE bug had the XML survive into synthesizeFinal and become the agent's
// "final answer" (which then became the research finding, which then made
// the synthesizer report "no usable findings"). Also strips bare
// <function=...>...</function> blocks (for emissions missing the wrapper).
func (Qwen) StripToolCalls(s string) string {
	s = qwenToolCallRE.ReplaceAllString(s, "")
	s = hermesXMLFunctionRE.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// CleanText strips reasoning blocks from the visible answer. vLLM's
// --reasoning-parser handles this server-side when enabled; this is the
// fallback when it isn't. Tool-call extraction runs on RAW text separately,
// so cleaning here is display-only.
//
// CRITICAL: never return empty when the raw input had content. Merged
// "thinking" models (aeon-ultimate, deep-r1 derivatives) sometimes emit
// ONLY a reasoning block when forced into a tools-off synthesis — stripping
// it to "" cascaded into "(no result)" reports because the agent treated
// the blank as "the model said nothing." Better: surface the raw reasoning
// than lose the answer entirely. The operator can still see the leak; an
// empty report is unrecoverable.
func (Qwen) CleanText(s string) string {
	original := s
	s = qwenThinkRE.ReplaceAllString(s, "")
	// A lone close tag (model emitted the answer after </think> but omitted
	// the open, as DeepSeek-R1 sometimes does): drop everything up to it.
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	cleaned := strings.TrimSpace(s)
	if cleaned == "" && strings.TrimSpace(original) != "" {
		// Whole response was a reasoning block. Salvage by stripping ONLY
		// the tags — keep the inner text so the agent has something to use.
		salvaged := strings.NewReplacer(
			"<think>", "", "</think>", "",
			"<thinking>", "", "</thinking>", "",
		).Replace(original)
		return strings.TrimSpace(salvaged)
	}
	return cleaned
}

type qwenCallPayload struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (Qwen) ParseToolCalls(content string) ([]model.ToolCall, error) {
	matches := qwenToolCallRE.FindAllStringSubmatch(content, -1)
	// Decide JSON vs Hermes XML by peeking at the FIRST <tool_call> block's
	// first non-whitespace char. JSON starts with { or [; XML starts with <.
	// If no <tool_call> wrapper at all, try bare Hermes <function=...>.
	if len(matches) == 0 {
		if xml := parseHermesXMLCalls(content); len(xml) > 0 {
			return xml, nil
		}
		return nil, nil
	}
	first := strings.TrimSpace(matches[0][1])
	if len(first) > 0 && first[0] == '<' {
		// Hermes XML wrapped inside <tool_call> — re-parse the FULL content
		// (not per-wrapper inner) so multiple <function=> calls are found.
		if xml := parseHermesXMLCalls(content); len(xml) > 0 {
			return xml, nil
		}
		return nil, fmt.Errorf("qwen: tool_call block is XML-shape but no <function=> found")
	}
	out := make([]model.ToolCall, 0, len(matches))
	for i, m := range matches {
		var p qwenCallPayload
		if err := json.Unmarshal([]byte(m[1]), &p); err != nil {
			// Weak models truncate or trail-comma their tool JSON; try a
			// best-effort repair before dropping the whole call (TEN-260).
			repaired := RepairJSON(m[1])
			p = qwenCallPayload{}
			if repaired == m[1] || json.Unmarshal([]byte(repaired), &p) != nil {
				return nil, fmt.Errorf("qwen: parse tool_call %d: %w", i, err)
			}
		}
		if p.Name == "" {
			return nil, fmt.Errorf("qwen: tool_call %d missing name", i)
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
