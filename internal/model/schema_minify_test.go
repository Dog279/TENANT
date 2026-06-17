package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func parseObj(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("minified output is not valid JSON: %v\n%s", err, raw)
	}
	return m
}

func TestMinifySchema_PreservesValiditySemantics(t *testing.T) {
	in := json.RawMessage(`{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"title": "MyTool input",
		"description": "the tool input",
		"properties": {
			"q": {"type": "string", "description": "the query", "examples": ["foo","bar"]},
			"k": {"type": "integer", "default": 8, "minimum": 1, "maximum": 25},
			"mode": {"type": "string", "enum": ["fast","slow"]}
		},
		"required": ["q"],
		"additionalProperties": false
	}`)
	m := parseObj(t, MinifySchema(in))

	if m["type"] != "object" {
		t.Errorf("type lost: %v", m["type"])
	}
	if _, ok := m["$schema"]; ok {
		t.Error("$schema should be dropped")
	}
	if _, ok := m["title"]; ok {
		t.Error("top-level title should be dropped")
	}
	if req, _ := m["required"].([]any); len(req) != 1 || req[0] != "q" {
		t.Errorf("required lost: %v", m["required"])
	}
	if m["additionalProperties"] != false {
		t.Errorf("additionalProperties lost: %v", m["additionalProperties"])
	}
	props := m["properties"].(map[string]any)
	q := props["q"].(map[string]any)
	if q["type"] != "string" || q["description"] != "the query" {
		t.Errorf("q type/description lost: %v", q)
	}
	if _, ok := q["examples"]; ok {
		t.Error("examples should be dropped from a property schema")
	}
	k := props["k"].(map[string]any)
	if k["minimum"] != float64(1) || k["maximum"] != float64(25) {
		t.Errorf("k numeric constraints lost: %v", k)
	}
	if k["default"] != float64(8) {
		t.Errorf("default should be preserved (kept for model guidance): %v", k)
	}
	if en := props["mode"].(map[string]any)["enum"].([]any); len(en) != 2 || en[0] != "fast" {
		t.Errorf("enum lost: %v", props["mode"])
	}
}

func TestMinifySchema_PropertyNamedLikeKeywordSurvives(t *testing.T) {
	// CRITICAL: drop-keys apply only in KEYWORD position. A property literally
	// named "title"/"examples"/"description" is a user field and must survive.
	in := json.RawMessage(`{"type":"object","properties":{
		"title":{"type":"string"},
		"examples":{"type":"array"},
		"description":{"type":"string"}
	},"required":["title"]}`)
	props := parseObj(t, MinifySchema(in))["properties"].(map[string]any)
	for _, name := range []string{"title", "examples", "description"} {
		if _, ok := props[name]; !ok {
			t.Errorf("user property %q was wrongly dropped as a keyword", name)
		}
	}
}

func TestMinifySchema_DataInEnumNotMangled(t *testing.T) {
	// enum/const/default values are DATA, never schemas — their object keys
	// (even ones named "title"/"examples") must be preserved verbatim.
	in := json.RawMessage(`{"type":"object","properties":{
		"choice":{"enum":[{"title":"A","examples":1},{"title":"B"}]},
		"d":{"type":"object","default":{"title":"keepme"}}
	}}`)
	props := parseObj(t, MinifySchema(in))["properties"].(map[string]any)
	first := props["choice"].(map[string]any)["enum"].([]any)[0].(map[string]any)
	if first["title"] != "A" {
		t.Errorf("enum data value key 'title' mangled: %v", first)
	}
	if first["examples"] != float64(1) {
		t.Errorf("enum data value key 'examples' mangled: %v", first)
	}
	if dv := props["d"].(map[string]any)["default"].(map[string]any); dv["title"] != "keepme" {
		t.Errorf("default data value mangled: %v", dv)
	}
}

func TestMinifySchema_CapsLongDescription(t *testing.T) {
	long := strings.Repeat("x", maxSchemaDescLen+50)
	in := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","description":"` + long + `"}}}`)
	d := parseObj(t, MinifySchema(in))["properties"].(map[string]any)["q"].(map[string]any)["description"].(string)
	if rc := len([]rune(d)); rc > maxSchemaDescLen+1 {
		t.Errorf("long description not capped: %d runes", rc)
	}
	if !strings.HasSuffix(d, "…") {
		t.Errorf("capped description should end with an ellipsis: %q", d)
	}
	short := json.RawMessage(`{"type":"string","description":"short and fine"}`)
	if parseObj(t, MinifySchema(short))["description"] != "short and fine" {
		t.Error("short description should be untouched")
	}
}

func TestMinifySchema_CompactsAndShrinks(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"q": { "type": "string", "description": "the query", "examples": ["a","b","c"], "title": "Query" }
		}
	}`)
	out := MinifySchema(in)
	if len(out) >= len(in) {
		t.Errorf("minified should be smaller: %d -> %d bytes", len(in), len(out))
	}
	if strings.Contains(string(out), "  ") {
		t.Errorf("whitespace not compacted: %s", out)
	}
}

func TestMinifySchema_MalformedAndEmptyPassthrough(t *testing.T) {
	bad := json.RawMessage(`{not valid json`)
	if string(MinifySchema(bad)) != string(bad) {
		t.Error("malformed schema must pass through unchanged")
	}
	if len(MinifySchema(nil)) != 0 {
		t.Error("nil/empty schema → empty")
	}
}

func TestMinifySchema_Idempotent(t *testing.T) {
	in := json.RawMessage(`{"type":"object","title":"x","properties":{"q":{"type":"string","examples":["a"]}},"required":["q"]}`)
	once := MinifySchema(in)
	twice := MinifySchema(once)
	if string(once) != string(twice) {
		t.Errorf("not idempotent:\n  %s\n  %s", once, twice)
	}
}

func TestMinifyToolSchemas_DoesNotMutateInput(t *testing.T) {
	orig := json.RawMessage(`{"type":"object","title":"x","properties":{"q":{"type":"string"}}}`)
	tools := []ToolSpec{{Name: "t", Description: "d", Parameters: orig}}
	out := MinifyToolSchemas(tools)
	if string(tools[0].Parameters) != string(orig) {
		t.Error("input spec Parameters was mutated (must operate on copies)")
	}
	if string(out[0].Parameters) == string(orig) {
		t.Error("output should be minified (title dropped), not the original")
	}
}

// TestMinifySchema_RealisticReduction measures the cut on a fat MCP-style schema
// (verbose descriptions + examples + metadata + pretty whitespace).
func TestMinifySchema_RealisticReduction(t *testing.T) {
	in := json.RawMessage(`{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id": "https://example.com/jira-create",
		"title": "createJiraIssue",
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"cloudId": {"type": "string", "title": "Cloud ID", "description": "Cloud ID (UUID or site URL). This is a very long property description that some MCP servers pad out with paragraphs of guidance about exactly how to obtain the cloud id, where to find it in the admin console, and what to do if you have multiple sites configured for your organization, which adds up across dozens of tools.", "examples": ["aefb707f-e555", "https://x.atlassian.net"]},
			"projectKey": {"type": "string", "description": "Project key", "examples": ["TEN","PROJ"], "$comment": "internal note"},
			"priority": {"type": "object", "title": "Priority", "properties": {"name": {"type": "string", "enum": ["High","Medium","Low"], "description": "priority name"}}},
			"labels": {"type": "array", "items": {"type": "string"}, "examples": [["bug"],["feature"]], "default": []}
		},
		"required": ["cloudId","projectKey"]
	}`)
	out := MinifySchema(in)
	m := parseObj(t, out)
	// semantics intact
	if _, ok := m["properties"].(map[string]any)["cloudId"]; !ok {
		t.Fatal("cloudId property lost")
	}
	if req := m["required"].([]any); len(req) != 2 {
		t.Fatalf("required lost: %v", req)
	}
	pct := 100 * (len(in) - len(out)) / len(in)
	t.Logf("realistic schema: %d -> %d bytes (%d%% smaller)", len(in), len(out), pct)
	if pct < 20 {
		t.Errorf("expected >=20%% reduction on a fat schema; got %d%%", pct)
	}
}
