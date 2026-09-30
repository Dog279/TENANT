// Package route classifies prompts and records shadow routing telemetry.
package route

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultModel is the Q4_K_M Ollama model shipped for the phase-one
	// classifier. The checkpoint remains a constructor argument so replacing it
	// does not affect callers or policy code.
	DefaultModel = "hf.co/SupraLabs/Supra-Router-51M-gguf:Q4_K_M"

	DefaultTimeout = 250 * time.Millisecond
	// defaultKeepAlive is negative so Ollama keeps the router loaded
	// indefinitely. "5m" evicted it between quiet spells (and overrode a
	// server-wide OLLAMA_KEEP_ALIVE); every cold load (~2.7s) then blew the
	// per-turn classify budget and routed turns fell back to the planner.
	defaultKeepAlive = "-1s"
	maxResponseBytes = 1 << 20
)

// Signal is Supra's parsed assessment. Label is normalized to "small" or
// "big"; Raw preserves the completion for audit and parser-failure diagnosis.
type Signal struct {
	Domain        string
	Complexity    int
	Math          bool
	Code          bool
	Label         string
	Justification string
	Raw           string
	Latency       time.Duration
	// Endpoint and Model say where it was classified (TEN-340): the
	// classifier's host, never its credentials, path or query, and its model.
	Endpoint, Model string
}

// Classifier is the backend-neutral prompt classifier seam. A future
// checkpoint or local runtime only needs to implement this method.
type Classifier interface {
	Classify(ctx context.Context, query string) (Signal, error)
}

// Supra calls an Ollama native generation endpoint. It intentionally does not
// use model.LLM: Supra is a raw completion model with its own training frame,
// not a Tenant chat model.
type Supra struct {
	endpoint  string
	model     string
	timeout   time.Duration
	keepAlive string
	client    *http.Client
}

// Warm loads the classifier model into the backend so the FIRST real Classify —
// which is bounded by the tight per-turn timeout — does not pay the cold
// model-load cost and time out ("context deadline exceeded"). Unlike Classify,
// Warm honors the caller's context deadline instead of the per-turn timeout, so
// callers should pass a generous one (cold GGUF load can take tens of seconds).
// keep_alive then holds the model resident. Errors are advisory: a failed warmup
// only means the first real classify may still be slow.
func (s *Supra) Warm(ctx context.Context) error {
	if s == nil || s.endpoint == "" {
		return errors.New("route: supra: endpoint required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := json.Marshal(generateRequest{
		Model: s.model, Prompt: supraPrompt("ping"), Raw: true,
		Stream: false, KeepAlive: s.keepAlive,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.endpoint+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("route: supra: warm: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes+1))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("route: supra: warm returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// NewSupra builds a classifier. Empty model and non-positive timeout values
// select the phase-one defaults.
func NewSupra(endpoint, model string, timeout time.Duration) *Supra {
	if model == "" {
		model = DefaultModel
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Supra{
		endpoint:  strings.TrimRight(endpoint, "/"),
		model:     model,
		timeout:   timeout,
		keepAlive: defaultKeepAlive,
		client:    &http.Client{},
	}
}

type generateRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	Raw       bool   `json:"raw"`
	Stream    bool   `json:"stream"`
	KeepAlive string `json:"keep_alive"`
}

type generateResponse struct {
	Response string `json:"response"`
	Error    string `json:"error"`
}

// Classify runs one bounded, non-streaming Ollama completion and strictly
// parses it. Every error still returns Raw and Latency when they are available.
func (s *Supra) Classify(ctx context.Context, query string) (Signal, error) {
	start := time.Now()
	done := func(sig Signal) Signal {
		sig.Latency = time.Since(start)
		if s != nil {
			sig.Endpoint, sig.Model = hostOf(s.endpoint), s.model
		}
		return sig
	}
	fail := func(sig Signal, err error) (Signal, error) { return done(sig), err }
	if s == nil {
		return fail(Signal{}, errors.New("route: supra: nil classifier"))
	}
	if s.endpoint == "" {
		return fail(Signal{}, errors.New("route: supra: endpoint required"))
	}
	if ctx == nil {
		ctx = context.Background()
	}

	body, err := json.Marshal(generateRequest{
		Model: s.model, Prompt: supraPrompt(query), Raw: true,
		Stream: false, KeepAlive: s.keepAlive,
	})
	if err != nil {
		return fail(Signal{}, fmt.Errorf("route: supra: marshal request: %w", err))
	}
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		s.endpoint+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return fail(Signal{}, fmt.Errorf("route: supra: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fail(Signal{}, fmt.Errorf("route: supra: generate: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fail(Signal{}, fmt.Errorf("route: supra: generate returned HTTP %d", resp.StatusCode))
	}
	rawJSON, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fail(Signal{}, fmt.Errorf("route: supra: read response: %w", err))
	}
	if len(rawJSON) > maxResponseBytes {
		return fail(Signal{}, errors.New("route: supra: response exceeds 1 MiB"))
	}
	var out generateResponse
	if err := json.Unmarshal(rawJSON, &out); err != nil {
		return fail(Signal{}, fmt.Errorf("route: supra: decode response: %w", err))
	}
	if out.Error != "" {
		return fail(Signal{}, errors.New("route: supra: backend reported an error"))
	}
	sig, err := ParseSignal(out.Response)
	if err != nil {
		return fail(sig, err)
	}
	return done(sig), nil
}

// hostOf is an endpoint as the feed and the log show it: its host and port
// ("" when it isn't a URL).
func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}

// ParseSignal accepts exactly the sequence used to train Supra. Outer
// whitespace is harmless completion framing; field order, separators, names,
// values, and bounds are otherwise exact. It never infers a missing value.
func ParseSignal(raw string) (Signal, error) {
	sig := Signal{Raw: raw}
	text := strings.TrimSpace(raw)
	if text == "" {
		return sig, errors.New("route: parse: empty output")
	}
	if strings.ContainsAny(text, "\r\n") {
		return sig, errors.New("route: parse: output must be one line")
	}
	parts := strings.Split(text, " | ")
	if len(parts) != 6 {
		return sig, fmt.Errorf("route: parse: expected 6 pipe-delimited fields, got %d", len(parts))
	}

	domain, err := field(parts[0], "Domain: ")
	if err != nil {
		return sig, err
	}
	complexity, err := field(parts[1], "Complexity: ")
	if err != nil {
		return sig, err
	}
	mathValue, err := field(parts[2], "Math: ")
	if err != nil {
		return sig, err
	}
	codeValue, err := field(parts[3], "Code: ")
	if err != nil {
		return sig, err
	}
	routeValue, err := field(parts[4], "Route: ")
	if err != nil {
		return sig, err
	}
	justification, err := field(parts[5], "Justification: ")
	if err != nil {
		return sig, err
	}

	n, err := strconv.Atoi(complexity)
	if err != nil || n < 1 || n > 5 || complexity != strconv.Itoa(n) {
		return sig, fmt.Errorf("route: parse: complexity %q outside integer range 1-5", complexity)
	}
	mathFlag, err := parseFlag("Math", mathValue)
	if err != nil {
		return sig, err
	}
	codeFlag, err := parseFlag("Code", codeValue)
	if err != nil {
		return sig, err
	}
	var label string
	switch routeValue {
	case "small model":
		label = "small"
	case "big model":
		label = "big"
	default:
		return sig, fmt.Errorf("route: parse: invalid route %q", routeValue)
	}

	sig.Domain = domain
	sig.Complexity = n
	sig.Math = mathFlag
	sig.Code = codeFlag
	sig.Label = label
	sig.Justification = justification
	return sig, nil
}

func field(part, prefix string) (string, error) {
	if !strings.HasPrefix(part, prefix) {
		return "", fmt.Errorf("route: parse: expected field %q", strings.TrimSuffix(prefix, ": "))
	}
	value := strings.TrimPrefix(part, prefix)
	if value == "" || value != strings.TrimSpace(value) {
		return "", fmt.Errorf("route: parse: field %q is empty or padded", strings.TrimSuffix(prefix, ": "))
	}
	return value, nil
}

func parseFlag(name, value string) (bool, error) {
	switch value {
	case "True", "yes":
		return true, nil
	case "False", "no":
		return false, nil
	default:
		return false, fmt.Errorf("route: parse: %s flag %q is not True/False or yes/no", name, value)
	}
}

// supraPrompt is deliberately private: this file is the sole owner of the
// classifier's training-time completion frame.
func supraPrompt(query string) string {
	return "Task: " + query + "\nAnalysis: "
}
