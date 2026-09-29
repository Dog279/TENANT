package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// --- helpers --------------------------------------------------------------

// servedModel is one entry from /v1/models. MaxLen is vLLM's reported
// max_model_len (0 when the server doesn't report it).
type servedModel struct {
	ID     string
	MaxLen int
}

// fetchModels queries an OpenAI-compatible models-list endpoint. vLLM,
// Ollama, OpenAI, etc. expose this, which is why the model (and, for
// vLLM, its context length) can be auto-detected instead of typed every
// launch.
//
// Two paths attempted in order:
//  1. <base>/v1/models   — standard OpenAI convention (works when base
//     does NOT already carry a version segment:
//     vllm/ollama/openai/grok/anthropic-vertex)
//  2. <base>/models      — works when base ALREADY carries the version:
//     Z.ai's /api/[coding/]paas/v4 routes; some
//     Azure deployments; bigmodel.cn
//
// Only one network call is paid on the happy path; the fallback fires
// only on 404. Either success short-circuits. Discovered 2026-05-26
// when Z.ai's `/api/coding/paas/v4/v1/models` 404'd while the chat
// completions endpoint at the same base worked fine — the probe lied
// about reachability and operators (correctly) didn't trust the swap.
func fetchModels(ctx context.Context, base, apiKey string) ([]servedModel, error) {
	trimmed := strings.TrimRight(base, "/")
	// Primary attempt — standard OpenAI convention.
	models, err := fetchModelsAt(ctx, trimmed+"/v1/models", apiKey)
	if err == nil {
		return models, nil
	}
	// Only fall back to /models on a 404 — that's the "you have the
	// wrong path" signal. For 401/connection-refused/500 the alternate
	// path won't help and we'd just double the operator's wait.
	if !isHTTP404(err) {
		return nil, err
	}
	models, err2 := fetchModelsAt(ctx, trimmed+"/models", apiKey)
	if err2 == nil {
		return models, nil
	}
	if isHTTP404(err2) {
		// Both 404 — operator needs to know we exhausted the conventions.
		return nil, fmt.Errorf("models list 404 at both %s/v1/models and %s/models", trimmed, trimmed)
	}
	return nil, err2
}

// isHTTP404 detects the "HTTP 404 ..." string we emit in fetchModelsAt.
// Kept narrow so we don't mis-retry on a 4xx that genuinely shouldn't
// fall through (e.g. 401 auth-missing — alternate path won't fix that).
func isHTTP404(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}

// anthropicModelsAPIVersion mirrors internal/model/backend/anthropic's
// anthropicVersion — the value Anthropic requires in the anthropic-version
// header. Kept here (not imported) to avoid a cmd→backend dependency.
const anthropicModelsAPIVersion = "2023-06-01"

// setProviderAuth applies the correct auth scheme for a model-list request.
// Anthropic REJECTS Bearer auth ("Invalid bearer token", HTTP 401) and requires
// `x-api-key` + `anthropic-version` — the same headers the anthropic generate
// backend already uses. OpenAI-compatible providers (vLLM, OpenAI, Grok, Z.ai)
// use Bearer. Without this, `/model add-cloud anthropic` 401'd while listing
// models for the picker even though the stored key was valid (TEN-171).
func setProviderAuth(req *http.Request, rawURL, apiKey string) {
	if apiKey == "" {
		return
	}
	if isAnthropicEndpoint(rawURL) {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicModelsAPIVersion)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

// isAnthropicEndpoint reports whether rawURL targets Anthropic's API host.
func isAnthropicEndpoint(rawURL string) bool {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "api.anthropic.com" || strings.HasSuffix(h, ".anthropic.com")
}

func fetchModelsAt(ctx context.Context, url, apiKey string) ([]servedModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setProviderAuth(req, url, apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	var out struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]servedModel, 0, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			models = append(models, servedModel{ID: d.ID, MaxLen: d.MaxModelLen})
		}
	}
	return models, nil
}

// servedModelFor is the model a self-hosted server serves when its provider
// leaves the model blank (found automatically): the first one it lists, with
// the context window it reports. The active provider gets it at launch
// (autodetectModel); helpers, fallbacks and planner-big build their profiles
// through here, so such a server works in those places too. ok is false for
// a hosted kind, or when the server can't be asked: the caller's own "no
// model" error stands.
func servedModelFor(pk providerKind, endpoint, apiKey string) (servedModel, bool) {
	if !pk.Local || pk.Backend != "vllm" || strings.TrimSpace(endpoint) == "" {
		return servedModel{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ms, err := fetchModels(ctx, endpoint, apiKey)
	if err != nil || len(ms) == 0 {
		return servedModel{}, false
	}
	return ms[0], true
}

// fetchModelIDs returns just the served model ids (used by the setup wizard).
func fetchModelIDs(ctx context.Context, base, apiKey string) ([]string, error) {
	models, err := fetchModels(ctx, base, apiKey)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// embedSetupHint is the stern, actionable warning shown when the embedding
// server is unreachable. It does NOT abort (the agent still runs), but it
// makes the degradation impossible to miss — semantic memory is a core
// pillar, not an optional extra.
const embedSetupHint = `╔══════════════════════════════════════════════════════════════════════╗
║  EMBEDDINGS DOWN — SEMANTIC MEMORY IS DISABLED                         ║
╚══════════════════════════════════════════════════════════════════════╝
Embedding server unreachable at %s.

This is NOT optional plumbing. Without it, Tenant falls back to a hash
stand-in: real vector recall is OFF, so the agent CANNOT meaningfully
search past episodes, recall stored facts, or surface relevant memories.
It will run, but it is effectively amnesiac. Fix this before relying on it.

Fix (Ollama — recommended):
  1. install Ollama       → https://ollama.com/download
  2. pull the model       → ollama pull %s
  3. ensure it's running  → ollama serve   (listens on :11434)
Or point elsewhere:       --embed-endpoint http://HOST:PORT --embed-model NAME
Or accept it explicitly:  --backend echo   (offline dev; no real memory)`
