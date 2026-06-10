package main

import (
	"net/http"
	"testing"
)

// TestSetProviderAuth_AnthropicUsesXAPIKey is the TEN-171 regression: Anthropic
// rejects Bearer ("Invalid bearer token", 401), so the model-list call must use
// x-api-key + anthropic-version — never Authorization: Bearer.
func TestSetProviderAuth_AnthropicUsesXAPIKey(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	setProviderAuth(req, "https://api.anthropic.com/v1/models", "sk-ant-secret")
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Anthropic must NOT send Bearer; got Authorization=%q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-ant-secret" {
		t.Errorf("x-api-key = %q, want the key", got)
	}
	if got := req.Header.Get("anthropic-version"); got == "" {
		t.Error("anthropic-version header is required and was not set")
	}
}

func TestSetProviderAuth_OpenAICompatUsesBearer(t *testing.T) {
	for _, url := range []string{
		"https://api.openai.com/v1/models",
		"https://api.x.ai/v1/models",
		"https://api.z.ai/api/paas/v4/models",
		"http://localhost:8000/v1/models",
	} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		setProviderAuth(req, url, "sk-key")
		if got := req.Header.Get("Authorization"); got != "Bearer sk-key" {
			t.Errorf("%s: Authorization = %q, want Bearer", url, got)
		}
		if req.Header.Get("x-api-key") != "" {
			t.Errorf("%s: must not set x-api-key", url)
		}
	}
}

func TestSetProviderAuth_EmptyKeyNoHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	setProviderAuth(req, "https://api.anthropic.com/v1/models", "")
	if req.Header.Get("Authorization") != "" || req.Header.Get("x-api-key") != "" {
		t.Error("no auth headers should be set when the key is empty")
	}
}

func TestIsAnthropicEndpoint(t *testing.T) {
	yes := []string{"https://api.anthropic.com/v1/models", "https://foo.anthropic.com/v1/models"}
	no := []string{
		"https://api.openai.com/v1/models",
		"http://localhost:8000/v1/models",
		"https://evil.com/api.anthropic.com/models", // host is evil.com, not anthropic
	}
	for _, u := range yes {
		if !isAnthropicEndpoint(u) {
			t.Errorf("isAnthropicEndpoint(%q) = false, want true", u)
		}
	}
	for _, u := range no {
		if isAnthropicEndpoint(u) {
			t.Errorf("isAnthropicEndpoint(%q) = true, want false", u)
		}
	}
}
