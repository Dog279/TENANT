package toolfmt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"tenant/internal/model"
)

// Gemma implements the Gemma function-call format. Gemma 3+ uses
// fenced code blocks tagged `tool_code` for tool invocations:
//
//	System prompt fragment:
//	  You have access to functions. To call one, output a fenced
//	  ```tool_code
//	  function_name(arg1=value1, arg2=value2)
//	  ```
//	  block. The available functions are:
//	  - search(q: string)
//	  - read_file(path: string)
//
//	Model output:
//	  ```tool_code
//	  search(q="golang")
//	  ```
//
// Gemma's function-call grammar is Python-call-like, not JSON. We
// translate kwargs into a JSON arguments object for the agent runtime
// (which always sees JSON regardless of model family).
//
// NOTE: Gemma 4's exact tool-call format spec may differ; operators
// running newer Gemma releases should verify and update this adapter.
// Worst case at runtime: parse failure surfaces as text content, the
// agent runtime sees no tool calls, falls back to asking the user.
type Gemma struct{}

func (Gemma) Name() string { return "gemma" }

func (Gemma) FormatToolPrompt(tools []model.ToolSpec) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("You have access to functions. To CALL one, output a fenced\n")
	b.WriteString("```tool_code\nfunction_name(arg1=value1, arg2=value2)\n```\nblock — ONE call per line. ")
	b.WriteString("Actually emit the fenced block to invoke a function; do not just describe the call in prose. ")
	b.WriteString("String values may be written plainly.\n\n")
	b.WriteString("The available functions are:\n")
	for _, t := range tools {
		sig, err := gemmaSignature(t)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "- %s: %s\n", sig, t.Description)
	}
	return b.String(), nil
}

// gemmaSignature renders a function signature like `search(q: string)`
// from a tool's JSON-schema parameters block. Best-effort: deeply nested
// schemas degrade to `name(...)`.
func gemmaSignature(t model.ToolSpec) (string, error) {
	if len(t.Parameters) == 0 {
		return fmt.Sprintf("%s()", t.Name), nil
	}
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(t.Parameters, &schema); err != nil {
		return fmt.Sprintf("%s(...)", t.Name), nil
	}
	parts := make([]string, 0, len(schema.Properties))
	for name, p := range schema.Properties {
		ty := p.Type
		if ty == "" {
			ty = "any"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", name, ty))
	}
	return fmt.Sprintf("%s(%s)", t.Name, strings.Join(parts, ", ")), nil
}

// gemmaCallRE matches one ```tool_code\n...\n``` block (non-greedy).
var gemmaCallRE = regexp.MustCompile("(?s)```tool_code\\s*\\n(.*?)\\n```")

// gemmaCallSyntaxRE matches a Python-call-like syntax: name(args). Greedy
// `.*` so the closing ')' is the LAST one on the line (values may contain
// their own parentheses).
var gemmaCallSyntaxRE = regexp.MustCompile(`(?s)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\((.*)\)\s*$`)

// gemmaNameParenRE / gemmaBareNameRE detect call expressions on a line in
// the UNFENCED fallback. Gemma 4 frequently drops the ```tool_code fence
// and writes calls (or a bare zero-arg name like `team_await`) as prose.
// Bare names are restricted to lowercase-with-underscore (our tool naming)
// to avoid matching ordinary words; the agent runtime validates names
// against the registry anyway, so a stray match is rejected, not run.
var (
	gemmaNameParenRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*\(.*\)\s*$`)
	gemmaBareNameRE  = regexp.MustCompile(`^[a-z][a-z0-9]*_[a-z0-9_]*$`)
	// gemmaSpaceCallRE catches the paren-LESS form Gemma 4 also emits:
	// `tool_name key=value key2: value2` (e.g. `os_list_dir path=C:\x`).
	// The name must be lowercase-with-underscore (our tool naming) and the
	// args must start with a `key=`/`key:`, so ordinary prose doesn't match.
	gemmaSpaceCallRE = regexp.MustCompile(`(?s)^([a-z][a-z0-9]*_[a-z0-9_]*)\s+([A-Za-z_][A-Za-z0-9_]*\s*[:=].*)$`)
	// gemmaKVRE splits one arg on its FIRST `=` or `:` (Gemma 4 uses `:`).
	gemmaKVRE = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*[:=]\s*(.*)$`)
	// gemmaNewKeyRE: text after a comma that begins a NEW key (so commas
	// inside a value don't split it).
	gemmaNewKeyRE = regexp.MustCompile(`^\s*[A-Za-z_][A-Za-z0-9_]*\s*[:=]`)
	// gemmaWrapperRE strips chat-template tool-call markers Gemma 4 sometimes
	// wraps a call in: <|tool_call>, <tool_call|>, <tool_call>, </tool_call>.
	gemmaWrapperRE = regexp.MustCompile(`(?i)<\|?/?tool_call\|?>`)
	// gemmaCallKwRE strips a leading `call:` / `tool_call:` prefix Gemma 4
	// sometimes prepends to the actual call.
	gemmaCallKwRE = regexp.MustCompile(`(?i)^(?:tool_)?call\s*:\s*`)
)

// stripCallMarkers removes the leading `call:`/`tool_call:` keyword from a
// candidate line. (Wrapper tags like <|tool_call> are stripped earlier, at
// the whole-content level.)
func stripCallMarkers(s string) string {
	return gemmaCallKwRE.ReplaceAllString(strings.TrimSpace(s), "")
}

func (Gemma) ParseToolCalls(content string) ([]model.ToolCall, error) {
	// Some "gemma" deployments actually emit OpenAI-harmony reasoning
	// channels (<|channel|>commentary to=functions.NAME <|message|>{json}).
	// Detect those FIRST — the call lives inside the channel envelope, which
	// the plain gemma scanners below would never find. Run on raw content
	// (before token stripping) so the to=/JSON structure is intact.
	if hc := harmonyToolCalls(content); len(hc) > 0 {
		return hc, nil
	}
	// Otherwise scan native gemma syntax on harmony-stripped text, so a
	// fenced/prose call wrapped in a channel still parses.
	exprs := gemmaCallExprs(stripHarmony(content))
	out := make([]model.ToolCall, 0, len(exprs))
	idx := 0
	for _, expr := range exprs {
		name, argStr, ok := splitGemmaCall(expr)
		if !ok {
			continue // Gemma's prose is noisy — skip non-calls, don't fail the turn
		}
		args, err := parseGemmaArgs(argStr)
		if err != nil {
			continue
		}
		out = append(out, model.ToolCall{ID: fmt.Sprintf("call_%d", idx), Name: name, Arguments: args})
		idx++
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// --- harmony / channel handling -------------------------------------------
//
// A subset of "gemma" deployments (reasoning variants, mislabeled servers)
// emit OpenAI-harmony control tokens instead of plain gemma output, e.g.:
//
//	<|start|>assistant<|channel|>analysis<|message|>…thinking…<|end|>
//	<|channel|>commentary to=functions.spawn_agent <|constrain|>json
//	<|message|>{"role":"researcher","task":"…"}<|call|>
//	<|channel|>final<|message|>here is the answer<|return|>
//
// Some emit a malformed/truncated form (`<|channel>thought<channel|>`). Left
// unparsed, the channel envelope hides tool calls (so nothing dispatches) and
// the control tokens leak into the visible answer.
var (
	// A full channel header up to its message marker:
	// <|channel|>NAME [to=recipient] [<|constrain|>fmt] <|message|>
	harmonyHeaderRE = regexp.MustCompile(`(?is)<\|?channel\|?>\s*[a-z]+(?:\s+to=[^\s<]+)?(?:\s*<\|?constrain\|?>\s*[a-z0-9_]+)?\s*<\|?message\|?>`)
	// A malformed bare channel: <|channel>NAME<channel|> with no message.
	harmonyBareChannelRE = regexp.MustCompile(`(?i)<\|?channel\|?>\s*[a-z]+\s*<\|?channel\|?>`)
	// A <|start|>role prefix.
	harmonyStartRoleRE = regexp.MustCompile(`(?i)<\|?start\|?>\s*[a-z]+`)
	// Any leftover standalone harmony control token (well-formed or malformed).
	harmonyTokenRE = regexp.MustCompile(`(?i)<\|?(?:start|end|return|call|message|channel|constrain)\|?>`)
	// A harmony tool-call recipient: `to=functions.NAME` or `to=NAME`.
	harmonyRecipientRE = regexp.MustCompile(`to=(?:functions\.)?([A-Za-z_][A-Za-z0-9_]*)`)
)

// CleanText strips OpenAI-harmony channel control tokens that leak into the
// visible answer (some "gemma" deployments are actually reasoning-channel
// models). Tool-call extraction runs on the RAW text separately, so this is
// display-only. A no-op for normal gemma output.
func (Gemma) CleanText(s string) string { return stripHarmony(s) }

// StripToolCalls removes the ```tool_code``` fenced blocks AND the unfenced
// `tool_name(args)` lines Gemma 4 sometimes emits. Used by the vLLM
// safety-net path after the call is extracted as structured.
func (Gemma) StripToolCalls(s string) string {
	return strings.TrimSpace(gemmaCallRE.ReplaceAllString(s, ""))
}

// stripHarmony removes the harmony channel envelope. Order matters: strip the
// <|start|>role prefix first so removing the channel header doesn't glue the
// role onto the following message text.
func stripHarmony(s string) string {
	s = harmonyStartRoleRE.ReplaceAllString(s, "")
	s = harmonyHeaderRE.ReplaceAllString(s, "")
	s = harmonyBareChannelRE.ReplaceAllString(s, "")
	s = harmonyTokenRE.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// harmonyToolCalls extracts harmony function calls (`to=functions.NAME` with a
// following JSON object). Names are validated against the registry by the
// agent runtime, so a stray recipient match is rejected, not run.
func harmonyToolCalls(content string) []model.ToolCall {
	locs := harmonyRecipientRE.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return nil
	}
	var out []model.ToolCall
	for i, m := range locs {
		name := content[m[2]:m[3]]
		// Search only up to the next recipient so we don't steal its args.
		end := len(content)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		args := json.RawMessage(`{}`)
		if obj, ok := firstJSONObject(content[m[1]:end]); ok {
			args = json.RawMessage(obj)
		}
		out = append(out, model.ToolCall{ID: fmt.Sprintf("call_%d", len(out)), Name: name, Arguments: args})
	}
	return out
}

// firstJSONObject returns the first balanced {...} object in s (respecting
// quoted strings) if it is valid JSON.
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	var quote byte
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote && s[i-1] != '\\' {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				cand := s[start : i+1]
				if json.Valid([]byte(cand)) {
					return cand, true
				}
				return "", false
			}
		}
	}
	return "", false
}

// gemmaCallExprs pulls candidate call expressions out of model output.
// Prefers fenced ```tool_code blocks (the documented format); if there are
// none, falls back to scanning each line for a bare call.
func gemmaCallExprs(content string) []string {
	// Strip chat-template tool-call wrapper tags first (Gemma 4 sometimes
	// emits <|tool_call>call:foo(...)<tool_call|>); turn them into line
	// breaks so the call lands on its own line for scanning.
	content = gemmaWrapperRE.ReplaceAllString(content, "\n")
	var exprs []string
	if blocks := gemmaCallRE.FindAllStringSubmatch(content, -1); len(blocks) > 0 {
		for _, b := range blocks {
			for _, line := range strings.Split(b[1], "\n") {
				if s := stripCallMarkers(line); s != "" {
					exprs = append(exprs, s)
				}
			}
		}
		return exprs
	}
	for _, raw := range strings.Split(content, "\n") {
		line := stripCallMarkers(raw)
		if line != "" && (gemmaNameParenRE.MatchString(line) || gemmaSpaceCallRE.MatchString(line) || gemmaBareNameRE.MatchString(line)) {
			exprs = append(exprs, line)
		}
	}
	return exprs
}

// splitGemmaCall splits a call expression into (name, args, ok). Accepts
// `name(args)`, the paren-less `name key=value` form, and a bare `name`.
func splitGemmaCall(expr string) (string, string, bool) {
	expr = strings.TrimSpace(expr)
	if m := gemmaCallSyntaxRE.FindStringSubmatch(expr); m != nil {
		return m[1], m[2], true
	}
	if m := gemmaSpaceCallRE.FindStringSubmatch(expr); m != nil {
		return m[1], m[2], true
	}
	if gemmaBareNameRE.MatchString(expr) {
		return expr, "", true // zero-arg call, e.g. team_await
	}
	return "", "", false
}

// parseGemmaArgs converts an arg list into a JSON object. Accepts both
// `k=v` and `k: v`, and unquoted values that contain commas/parentheses
// (Gemma 4 writes long bare-string args).
func parseGemmaArgs(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`), nil
	}
	out := map[string]json.RawMessage{}
	for _, p := range splitGemmaArgs(s) {
		m := gemmaKVRE.FindStringSubmatch(p)
		if m == nil {
			return nil, fmt.Errorf("malformed arg %q", strings.TrimSpace(p))
		}
		jv, err := pythonLitToJSON(strings.TrimSpace(m[2]))
		if err != nil {
			return nil, fmt.Errorf("arg %s: %w", m[1], err)
		}
		out[m[1]] = jv
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// splitGemmaArgs splits an arg list at commas that are at top level (not in
// quotes or parens/brackets) AND begin a new `key:`/`key=` pair. That lets
// unquoted values carry their own commas and parentheses.
func splitGemmaArgs(s string) []string {
	var parts []string
	var cur strings.Builder
	depth := 0
	var quote byte // 0 = not in a string; else the opening quote char (" or ')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote && s[i-1] != '\\' {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == '(' || c == '[' || c == '{':
			depth++
			cur.WriteByte(c)
		case (c == ')' || c == ']' || c == '}') && depth > 0:
			depth--
			cur.WriteByte(c)
		case c == ',' && depth == 0 && gemmaNewKeyRE.MatchString(s[i+1:]):
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		parts = append(parts, cur.String())
	}
	return parts
}

// pythonLitToJSON converts a Python-style literal to JSON: True/False/None
// become true/false/null; double-quoted strings pass through; numbers pass
// through; anything else is quoted as a string.
func pythonLitToJSON(v string) (json.RawMessage, error) {
	v = strings.TrimSpace(v)
	switch v {
	case "True", "true":
		return json.RawMessage(`true`), nil
	case "False", "false":
		return json.RawMessage(`false`), nil
	case "None", "null":
		return json.RawMessage(`null`), nil
	}
	// Quoted value (double or single). A double-quoted value MIGHT be valid
	// JSON — but Gemma writes Python-ish literals, so e.g. a Windows path
	// "C:\Users\x" has \U/\D which are NOT legal JSON escapes. Try it as
	// real JSON first; if that fails (or it's single-quoted), treat the
	// inner text as a raw literal and re-encode it as a proper JSON string
	// (escaping backslashes etc.). This is what made os_list_dir(path=
	// "C:\Users\Ada") silently fail to parse before.
	if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
		if v[0] == '"' {
			var s string
			if json.Unmarshal([]byte(v), &s) == nil {
				return json.RawMessage(v), nil // already valid JSON
			}
		}
		b, err := json.Marshal(v[1 : len(v)-1])
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	}
	// Try as a number.
	var n json.Number
	if err := json.Unmarshal([]byte(v), &n); err == nil {
		return json.RawMessage(v), nil
	}
	// Fallback: treat as bare string, quote it.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
