package main

// modeldiscover.go checks an OpenAI-compatible server before the Model page
// adds it ("Add a model from a server on your network"). The operator types
// an address and, if they like, a model; this finds the rest:
//
//   - the models it serves (/v1/models), and which one to use;
//   - what kind of server it is: vLLM, llama.cpp or Ollama, else a plain
//     OpenAI-compatible one. The kind isn't a label: only vLLM takes
//     guided_json and has vLLM's /tokenize, and llama.cpp tokenizes in its
//     own dialect, so a wrong kind means refused requests or estimated
//     budgets;
//   - how the model writes tool calls: a test call offers one tool and reads
//     the answer. Structured tool_calls mean the server parses them itself;
//     a call written as text names the toolfmt adapter that reads it.
//
// Every request goes only to the address typed, follows no redirect, and
// reads a bounded answer. Nothing the server says reaches the page except
// model ids and a short, cleaned refusal reason.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"tenant/internal/model/toolfmt"
)

// discoverBudget bounds the whole check. The dashboard answers a form post
// within its 30s write deadline (httplimits.ResponseWriteTimeout), so the
// check leaves time to save and render. A variable only for tests.
var discoverBudget = 22 * time.Second

// discoverReadLimit caps any one answer read during the check.
const discoverReadLimit = 4 << 20

// discoverClient never follows a redirect: the check talks to the address
// the operator typed and nowhere else.
var discoverClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// toolCheck is what the test call showed about the model's tool calls.
type toolCheck string

const (
	toolsNative    toolCheck = "native"    // the server returned structured tool_calls
	toolsText      toolCheck = "text"      // the model wrote the call as text that ToolFmt reads
	toolsNone      toolCheck = "none"      // it answered without calling the tool
	toolsUnchecked toolCheck = "unchecked" // the test call didn't finish (time, 5xx, network)
)

// serverFound is a checked server: what the Model page saves and says.
type serverFound struct {
	Endpoint string    // the base to save: no trailing slash, no /v1
	Kind     string    // vllm | llamacpp | ollama | openai-compat
	Served   []string  // the model ids it lists
	Model    string    // the model the check used
	Auto     bool      // no model was typed: the provider follows what the server serves
	ToolFmt  string    // the format its tool calls are read in (never "auto")
	Tools    toolCheck //
	Why      string    // why Tools is unchecked or none, for the log only
}

// plainError is a failure the operator can act on, in the page's words
// (plain). Error() keeps the technical detail for the log.
type plainError struct {
	plain string
	err   error
}

func (e *plainError) Error() string {
	if e.err == nil {
		return e.plain
	}
	return e.plain + " (" + e.err.Error() + ")"
}

func (e *plainError) Unwrap() error { return e.err }

func plainf(err error, format string, a ...any) error {
	return &plainError{plain: fmt.Sprintf(format, a...), err: err}
}

// normalizeServerAddress turns what the operator typed into the base the
// providers map keeps: http(s), a host, no credentials, query or fragment,
// and without the /v1 (or a pasted /v1/chat/completions) that Tenant adds
// itself. A value that isn't such an address is never echoed back: an API
// key pasted into the wrong box must not be shown or looked up.
func normalizeServerAddress(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", plainf(nil, "Enter the server's address, like http://my-server:8080.")
	}
	u, err := neturl.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return "", plainf(nil, "That isn't a server address. Start it with http:// or https://, like http://my-server:8080.")
	}
	if u.User != nil {
		return "", plainf(nil, "Put a key in the API key box under More options, not in the address.")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", plainf(nil, "Leave out anything after ? or # in the address.")
	}
	p := strings.TrimRight(u.Path, "/")
	if i := strings.LastIndex(strings.ToLower(p), "/v1"); i >= 0 && (i+3 == len(p) || p[i+3] == '/') {
		p = p[:i]
	}
	u.Path, u.RawPath = strings.TrimRight(p, "/"), ""
	return strings.TrimRight(u.String(), "/"), nil
}

// discoverServer checks the server at endpoint (already normalized): the
// model to use (want, else the only one served), its kind, and its tool-call
// format. An error is a *plainError, except a cancelled request.
func discoverServer(ctx context.Context, endpoint, apiKey, want string) (*serverFound, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverBudget)
	defer cancel()
	listCtx, listCancel := context.WithTimeout(ctx, 6*time.Second)
	listed, err := listServerModels(listCtx, endpoint, apiKey)
	listCancel()
	if err != nil {
		return nil, err
	}
	f := &serverFound{Endpoint: endpoint}
	for _, m := range listed {
		f.Served = append(f.Served, m.ID)
	}
	want = strings.TrimSpace(want)
	switch {
	case want != "":
		id, ok := matchServedModel(f.Served, want)
		if !ok {
			return nil, plainf(nil, "The server doesn't offer %s. It offers %s.", displayText(want, 80), listForPage(f.Served))
		}
		f.Model = id
	case len(f.Served) == 1:
		f.Model, f.Auto = f.Served[0], true
	default:
		return nil, plainf(nil, "The server offers several models: %s. Type the one you want in Model.", listForPage(f.Served))
	}
	f.Kind = detectServerKind(ctx, endpoint, apiKey, listed)
	if err := probeToolCalls(ctx, f, apiKey); err != nil {
		return nil, err
	}
	return f, nil
}

// listedModel is one /v1/models entry, with the fields that say which server
// it is: vLLM and llama.cpp name themselves in owned_by.
type listedModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
}

// listServerModels reads <endpoint>/v1/models. Only that path: Tenant sends
// chat to <endpoint>/v1/chat/completions, so a server that lists its models
// elsewhere wouldn't answer it either.
func listServerModels(ctx context.Context, endpoint, apiKey string) ([]listedModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/models", nil)
	if err != nil {
		return nil, plainf(err, "That isn't a server address. Start it with http:// or https://, like http://my-server:8080.")
	}
	setProviderAuth(req, endpoint, apiKey)
	resp, err := discoverClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, plainf(err, "Tenant couldn't reach %s. Check the address, and that the server is running and reachable from the computer running Tenant.", endpoint)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		if apiKey == "" {
			return nil, plainf(statusErr(resp), "The server at %s wants an API key. Add it under More options.", endpoint)
		}
		return nil, plainf(statusErr(resp), "The server at %s turned down that API key.", endpoint)
	case resp.StatusCode >= 300:
		return nil, plainf(statusErr(resp), "The server at %s doesn't answer like an OpenAI-compatible server (it has no model list at /v1/models).", endpoint)
	}
	var out struct {
		Data []listedModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, discoverReadLimit)).Decode(&out); err != nil {
		return nil, plainf(err, "The server at %s doesn't answer like an OpenAI-compatible server (its model list isn't in the usual form).", endpoint)
	}
	models := out.Data[:0]
	for _, m := range out.Data {
		if strings.TrimSpace(m.ID) != "" {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		return nil, plainf(nil, "The server at %s isn't offering a model yet. Load one on the server, then add it again.", endpoint)
	}
	return models, nil
}

func statusErr(resp *http.Response) error {
	return fmt.Errorf("HTTP %d from %s", resp.StatusCode, resp.Request.URL.Redacted())
}

// matchServedModel finds want among the served ids: exactly, else ignoring
// case (a server's model lookup is case-sensitive, so the saved id takes the
// server's spelling).
func matchServedModel(served []string, want string) (string, bool) {
	for _, id := range served {
		if id == want {
			return id, true
		}
	}
	for _, id := range served {
		if strings.EqualFold(id, want) {
			return id, true
		}
	}
	return "", false
}

// detectServerKind names the server: vLLM and llama.cpp say so in owned_by;
// otherwise Ollama answers /api/version and an older llama.cpp /props. A
// server that is none of them is "openai-compat", which asks for nothing
// beyond the OpenAI API.
func detectServerKind(ctx context.Context, endpoint, apiKey string, listed []listedModel) string {
	for _, m := range listed {
		switch strings.ToLower(m.OwnedBy) {
		case "vllm":
			return "vllm"
		case "llamacpp":
			return "llamacpp"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ollama, llamacpp := make(chan bool, 1), make(chan bool, 1)
	go func() { ollama <- answersWith(ctx, endpoint+"/api/version", apiKey, "version") }()
	go func() { llamacpp <- answersWith(ctx, endpoint+"/props", apiKey, "default_generation_settings") }()
	isOllama, isLlamaCpp := <-ollama, <-llamacpp
	switch {
	case isLlamaCpp:
		return "llamacpp"
	case isOllama:
		return "ollama"
	}
	return "openai-compat"
}

// answersWith reports whether a GET of url answers 200 with a JSON object
// that has field.
func answersWith(ctx context.Context, url, apiKey, field string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	setProviderAuth(req, url, apiKey)
	resp, err := discoverClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var obj map[string]json.RawMessage
	if json.NewDecoder(io.LimitReader(resp.Body, discoverReadLimit)).Decode(&obj) != nil {
		return false
	}
	v, ok := obj[field]
	return ok && string(v) != "null"
}

// toolProbeRequest offers the model one tool and asks it to use it. "auto"
// rather than "required": servers that can't force a call reject the latter,
// and a model that calls tools calls this one when asked plainly.
const toolProbeTool = `[{"type":"function","function":{"name":"add","description":"Add two whole numbers and return the sum.","parameters":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}}}]`

// probeToolCalls makes the test call and fills f.Tools and f.ToolFmt. A
// server that refuses it outright (a 4xx) can't serve Tenant, which offers
// tools with every message: that is the error. A test call that doesn't
// finish in time leaves the format to the model's name.
func probeToolCalls(ctx context.Context, f *serverFound, apiKey string) error {
	family := toolfmt.ForModel(f.Model)
	f.ToolFmt = firstNonEmpty(family, "openai")
	body, err := json.Marshal(map[string]any{
		"model":       f.Model,
		"messages":    []map[string]string{{"role": "user", "content": "Use the add tool to add 2 and 3."}},
		"tools":       json.RawMessage(toolProbeTool),
		"tool_choice": "auto",
		"max_tokens":  600,
		"temperature": 0,
		"stream":      false,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.Endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setProviderAuth(req, f.Endpoint, apiKey)
	resp, err := discoverClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() == context.Canceled {
			return err
		}
		f.Tools, f.Why = toolsUnchecked, err.Error()
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, discoverReadLimit))
	if apiKey != "" { // a server that echoes the request must not put the key in the log or the page
		raw = bytes.ReplaceAll(raw, []byte(apiKey), []byte("[the API key]"))
	}
	switch {
	case resp.StatusCode >= 500:
		f.Tools, f.Why = toolsUnchecked, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, clip(string(raw), 300))
		return nil
	case resp.StatusCode >= 300:
		reason := refusalReason(raw)
		if reason == "" {
			reason = fmt.Sprintf("it answered %d", resp.StatusCode)
		}
		return plainf(fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(string(raw), 300)),
			"%s refused a test message (it said: %s), so your agent can't use it as the server is set up.",
			displayText(f.Model, 80), reason)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content          string            `json:"content"`
				Reasoning        string            `json:"reasoning"`
				ReasoningContent string            `json:"reasoning_content"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		f.Tools, f.Why = toolsUnchecked, "the test call's answer isn't a chat completion: "+clip(string(raw), 300)
		return nil
	}
	msg := out.Choices[0].Message
	if len(msg.ToolCalls) > 0 {
		f.Tools = toolsNative
		return nil
	}
	text := msg.Content
	if strings.TrimSpace(text) == "" { // as the backend does: an answer that is all reasoning
		text = firstNonEmpty(msg.ReasoningContent, msg.Reasoning)
	}
	for _, format := range textFormatsToTry(family) {
		calls, err := toolfmt.AdapterFor(format).ParseToolCalls(text)
		if err != nil {
			continue
		}
		for _, c := range calls {
			if c.Name == "add" {
				f.Tools, f.ToolFmt = toolsText, format
				return nil
			}
		}
	}
	f.Tools, f.Why = toolsNone, "no tool call in: "+clip(text, 300)
	return nil
}

// textFormatsToTry is the order a text tool call is read in: the model's own
// family first, then the others.
func textFormatsToTry(family string) []string {
	out := []string{}
	if family != "" {
		out = append(out, family)
	}
	for _, f := range []string{"qwen", "llama", "mistral", "gemma", "glm"} {
		if f != family {
			out = append(out, f)
		}
	}
	return out
}

// refusalReason is a server's own error message from a refusal body
// ({"error":{"message":…}} or {"error":"…"}), cleaned for the page.
func refusalReason(raw []byte) string {
	var out struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Error) == 0 {
		return ""
	}
	var msg string
	if json.Unmarshal(out.Error, &msg) != nil {
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(out.Error, &obj) != nil {
			return ""
		}
		msg = obj.Message
	}
	return strings.TrimRight(displayText(msg, 160), ".!? ")
}

// displayText is text a server sent, fit for the page: one line, no control
// or invisible formatting characters (bidi overrides, zero-width marks), at
// most n characters.
func displayText(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			// dropped: it draws nothing, or reorders what follows
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) > n {
		out = string([]rune(out)[:n]) + "…"
	}
	return out
}

// listForPage names served models for the page: the first six, and how
// many more.
func listForPage(ids []string) string {
	const show = 6
	parts := make([]string, 0, show)
	for i, id := range ids {
		if i == show {
			break
		}
		parts = append(parts, displayText(id, 80))
	}
	s := strings.Join(parts, ", ")
	if len(ids) > show {
		s += fmt.Sprintf(" and %d more", len(ids)-show)
	}
	return s
}
