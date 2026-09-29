package toolfmt

import "strings"

// Auto is the tool_format that follows the served model: a provider saved
// with it gets ForModel of whatever model it serves each time its profiles
// are built, so a server that swaps model families keeps a matching parser.
const Auto = "auto"

// ForModel names the tool-call format a model's family writes, read from the
// id a server lists it under ("Qwen3.8-27B-NVFP4",
// "hf.co/unsloth/gemma-3-27b-it-GGUF:Q4_K_M", "meta-llama/Llama-3.3-70B",
// "mistralai/Devstral-Small-2507"), or "" when the id names no family this
// package has an adapter for. Those usually return native tool_calls, which
// the OpenAI pass-through handles.
//
// A name is only a guess: a live test call is the proof (the dashboard's
// model check makes one). Families are tried in order, so a Hermes
// fine-tune of Llama reads as Hermes, the format it was trained on.
func ForModel(id string) string {
	s := strings.ToLower(id)
	for _, f := range modelFamilies {
		for _, w := range f.words {
			if strings.Contains(s, w) {
				return f.format
			}
		}
	}
	return ""
}

var modelFamilies = []struct {
	format string
	words  []string
}{
	{"qwen", []string{"qwen", "qwq", "hermes"}},
	{"gemma", []string{"gemma"}},
	{"mistral", []string{"mistral", "mixtral", "devstral", "magistral", "codestral", "ministral", "pixtral"}},
	{"llama", []string{"llama"}},
}
