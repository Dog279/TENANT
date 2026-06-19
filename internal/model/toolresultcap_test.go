package model

import "testing"

// TEN-259 wiring: the tool-result cap must engage by default for self-hosted
// vLLM (which drives weak/local models) while leaving hosted frontier providers
// — even those on the OpenAI-compatible "vllm" backend (Z.ai/GLM, OpenAI, Grok)
// — uncapped. An explicit value always wins (negative force-disables).
func TestProfile_ToolResultCap(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		grammar bool
		set     int
		want    int
	}{
		{"self-hosted vLLM default-on", "vllm", true, 0, defaultVLLMToolResultTokens},
		{"self-hosted vLLM explicit cap wins", "vllm", true, 2048, 2048},
		{"self-hosted vLLM explicit disable", "vllm", true, -1, -1},
		{"hosted on vllm backend (zai/openai/grok) uncapped", "vllm", false, 0, 0},
		{"hosted explicit cap honored", "vllm", false, 9000, 9000},
		{"anthropic uncapped", "anthropic", false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Profile{Backend: c.backend, SupportsGrammar: c.grammar, MaxToolResultTokens: c.set}
			if got := p.ToolResultCap(); got != c.want {
				t.Fatalf("ToolResultCap() = %d, want %d", got, c.want)
			}
		})
	}
}
