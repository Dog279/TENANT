// Package toolfmt holds per-model-family tool-calling adapters.
//
// Why this exists: vLLM ships server-side tool-call parsers (`--tool-call-parser
// hermes|mistral|llama3_json|...`) that turn model output into structured
// tool_calls in the chat response. When that works, our backend gets parsed
// calls for free. When it doesn't — model emits a tool call in text content
// because the operator misconfigured the parser flag, or the model used a
// slightly off variant of its expected format — we need a Go-side parser
// safety net so the agent runtime doesn't see text that "looks like" a tool
// call and fail to dispatch it.
//
// Adapters expose two methods:
//
//   - FormatToolPrompt: emit a system-prompt fragment describing the tools
//     in the format this model family expects. Used by backends WITHOUT
//     native tool-call support (future Ollama / llama.cpp backends).
//
//   - ParseToolCalls: scan raw model output for tool-call syntax of this
//     model family and extract structured calls. Used as a safety net by
//     all backends after they've checked for native tool_calls.
//
// Each adapter is keyed by Profile.ToolFormat. AdapterFor returns the right
// one; OpenAI is the pass-through default.
package toolfmt

import (
	"fmt"

	"tenant/internal/model"
)

// Adapter is the model-family-specific tool-call format contract.
type Adapter interface {
	// Name returns the format key (matches Profile.ToolFormat).
	Name() string

	// FormatToolPrompt returns a system-prompt fragment describing the
	// tools. Empty string + nil error if the model family relies on the
	// backend's native tool-call mechanism (OpenAI / vLLM defaults).
	FormatToolPrompt(tools []model.ToolSpec) (string, error)

	// ParseToolCalls scans content for this format's tool-call syntax
	// and returns any calls found. Returns nil, nil when no calls are
	// present — that's the normal path, not an error.
	ParseToolCalls(content string) ([]model.ToolCall, error)

	// CleanText removes this family's display artifacts (reasoning blocks,
	// channel control tokens) from the visible answer. Identity for families
	// with none. Tool-call parsing runs on RAW text, so this is display-only.
	CleanText(content string) string

	// StripToolCalls removes tool-call markup from text. Used by the vLLM
	// backend's safety-net path: when ParseToolCalls extracts structured
	// calls from raw content, we must ALSO strip the same markup from the
	// visible Text — otherwise the XML/JSON blob ends up in synthesizeFinal
	// or the team result as "the model's answer." Identity for families with
	// none. Strips ONLY the call markup, NOT the surrounding prose.
	StripToolCalls(content string) string
}

// AdapterFor returns the adapter for the named format. Falls back to
// OpenAI pass-through for unknown formats so we never block on a typo
// in a user profile, only log + degrade.
func AdapterFor(format string) Adapter {
	switch format {
	case "qwen", "hermes":
		return Qwen{}
	case "gemma":
		return Gemma{}
	case "llama":
		return Llama{}
	case "mistral":
		return Mistral{}
	case "", "openai":
		return OpenAI{}
	default:
		return OpenAI{}
	}
}

// ListFormats returns all registered format keys, for diagnostics.
func ListFormats() []string {
	return []string{"qwen", "gemma", "llama", "mistral", "openai"}
}

// helper: pretty-print tool descriptions as a JSON-schema-friendly array.
// Used by adapters that inject tools into the system prompt.
func toolsAsJSONArray(tools []model.ToolSpec) (string, error) {
	if len(tools) == 0 {
		return "[]", nil
	}
	out := "["
	for i, t := range tools {
		if i > 0 {
			out += ","
		}
		params := string(t.Parameters)
		if params == "" {
			params = "{}"
		}
		out += fmt.Sprintf(`{"name":%q,"description":%q,"parameters":%s}`, t.Name, t.Description, params)
	}
	out += "]"
	return out, nil
}
