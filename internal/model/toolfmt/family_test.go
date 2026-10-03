package toolfmt

import "testing"

func TestForModel(t *testing.T) {
	for id, want := range map[string]string{
		"qwen3.8-27b":                          "qwen",
		"Qwen3.8-27B-NVFP4":                    "qwen",
		"Qwen/QwQ-32B":                         "qwen",
		"NousResearch/Hermes-3-Llama-3.1-8B":   "qwen", // Hermes-trained: Hermes first
		"hf.co/unsloth/gemma-3-27b-it-GGUF:Q4": "gemma",
		"google/codegemma-7b":                  "gemma",
		"meta-llama/Llama-3.3-70B-Instruct":    "llama",
		"llama3.1:8b":                          "llama",
		"mistralai/Devstral-Small-2507":        "mistral",
		"mixtral-8x7b":                         "mistral",
		"Ministral-8B":                         "mistral",
		"gpt-oss-120b":                         "",
		"glm-4.6":                              "glm",
		"zai-org/GLM-4.5-Air":                  "glm",
		"deepseek-v3":                          "",
		"":                                     "",
	} {
		if got := ForModel(id); got != want {
			t.Errorf("ForModel(%q) = %q, want %q", id, got, want)
		}
	}
	// Every format ForModel names has an adapter of that name.
	for _, f := range modelFamilies {
		if AdapterFor(f.format).Name() != f.format {
			t.Errorf("family format %q has no adapter", f.format)
		}
	}
}
