package toolfmt

import "tenant/internal/model"

// OpenAI is the pass-through adapter. vLLM, llama.cpp's server, and
// LM Studio all speak OpenAI-compatible tool calls natively when
// configured correctly. No prompt-side formatting, no text-side
// parsing — if the backend didn't return structured tool_calls, this
// adapter sees nothing to extract.
type OpenAI struct{}

func (OpenAI) Name() string { return "openai" }

func (OpenAI) FormatToolPrompt(_ []model.ToolSpec) (string, error) {
	return "", nil
}

func (OpenAI) ParseToolCalls(_ string) ([]model.ToolCall, error) {
	return nil, nil
}

// CleanText is identity: OpenAI-style content carries no reasoning/control
// artifacts to strip (structured tool_calls come back separately).
func (OpenAI) CleanText(s string) string      { return s }
func (OpenAI) StripToolCalls(s string) string { return s }
