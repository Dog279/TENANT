package model

import (
	"encoding/json"
	"sync"
)

// Tool-schema minification (TEN-227). After ranking trims the tool COUNT
// (TEN-226), the surviving tools' JSON-schema Parameters are the dominant
// on-wire cost — remote MCP schemas (Atlassian's 31 tools) especially. This
// strips pure-documentation keywords and compacts whitespace WITHOUT changing
// what arguments are VALID: types, enums, required, and the property structure
// are preserved verbatim, so constrained decoding and the model's arg-filling
// are unaffected. Applied ONCE to the surfaced tool set before it reaches any
// backend, so every model family — native vLLM/Anthropic `tools` arrays AND the
// toolfmt text formats — benefits uniformly.
//
// Cardinal rule (shared with TEN-226): never break a tool the model needs. The
// transform only removes documentation/metadata and trims paragraph-length
// PROPERTY prose; it never removes a property, type, enum, or required marker.

// maxSchemaDescLen caps a per-PROPERTY description inside a schema. The
// tool-level Description (the primary tool-selection signal, rendered
// separately) is never touched — only long per-property prose, which some MCP
// servers pad to paragraphs. Generous, so ordinary descriptions pass untouched.
const maxSchemaDescLen = 280

// schemaDropKeys are JSON-Schema keywords that are pure documentation/metadata:
// dropping them changes neither validity nor the model's ability to fill args.
// Applied ONLY in schema-keyword position — never to user property names.
var schemaDropKeys = map[string]bool{
	"examples":   true,
	"example":    true,
	"$schema":    true,
	"$comment":   true,
	"$id":        true,
	"title":      true,
	"readOnly":   true,
	"writeOnly":  true,
	"deprecated": true,
}

// subschemaMapKeys: keywords whose value is a {name -> subschema} object.
// Recurse the VALUES only; the names are arbitrary (a property literally named
// "title" or "examples" must survive untouched).
var subschemaMapKeys = map[string]bool{
	"properties":        true,
	"patternProperties": true,
	"$defs":             true,
	"definitions":       true,
	"dependentSchemas":  true,
}

// subschemaKeys: keywords whose value is a single subschema, or (for the *Of /
// items / prefixItems array forms) an array of subschemas. Recurse as schema(s).
var subschemaKeys = map[string]bool{
	"items": true, "additionalProperties": true, "propertyNames": true,
	"contains": true, "not": true, "if": true, "then": true, "else": true,
	"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true,
}

// minifyCache maps a raw schema (by bytes) to its minified form. The surfaced
// tool set is stable across turns, so this is O(1) after the first turn.
var minifyCache sync.Map // string(raw) -> json.RawMessage

// MinifyToolSchemas returns a copy of tools with each Parameters schema
// minified. The input slice and the canonical registry specs are NOT mutated
// (the struct is copied by value; only the copy's Parameters is reassigned).
func MinifyToolSchemas(tools []ToolSpec) []ToolSpec {
	if len(tools) == 0 {
		return tools
	}
	out := make([]ToolSpec, len(tools))
	for i, t := range tools {
		t.Parameters = MinifySchema(t.Parameters)
		out[i] = t
	}
	return out
}

// MinifySchema minifies one JSON-schema blob. Fail-safe: an empty or
// unparseable schema is returned unchanged (never corrupt what we can't parse).
func MinifySchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	key := string(raw)
	if v, ok := minifyCache.Load(key); ok {
		return v.(json.RawMessage)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		minifyCache.Store(key, raw) // un-parseable → never alter
		return raw
	}
	out, err := json.Marshal(minifyNode(tree))
	if err != nil {
		minifyCache.Store(key, raw)
		return raw
	}
	res := json.RawMessage(out)
	minifyCache.Store(key, res)
	return res
}

// minifyNode walks a decoded JSON-Schema value. It recurses ONLY into positions
// JSON Schema defines as subschemas; every other value (type, enum, const,
// default, required, format, numeric/string constraints, vendor x-* keywords)
// is kept VERBATIM — so a DATA value (e.g. an object inside an enum or default)
// can never be mistaken for a schema and have its keys stripped. json.Marshal
// re-emits compact JSON (the whitespace win) with deterministic key order.
func minifyNode(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, val := range n {
			switch {
			case schemaDropKeys[k]:
				// drop pure-doc/metadata keyword
			case k == "description":
				if s, ok := val.(string); ok {
					out[k] = capDesc(s)
				} else {
					out[k] = val
				}
			case subschemaMapKeys[k]:
				if m, ok := val.(map[string]any); ok {
					sub := make(map[string]any, len(m))
					for name, schema := range m {
						sub[name] = minifyNode(schema)
					}
					out[k] = sub
				} else {
					out[k] = val
				}
			case subschemaKeys[k]:
				out[k] = minifyNode(val) // single subschema OR array of subschemas
			default:
				out[k] = val // keep verbatim — NOT recursed (may be data)
			}
		}
		return out
	case []any:
		// Reached only via a subschemaKeys recursion (allOf/anyOf/oneOf/
		// prefixItems, or tuple-form items): every element is a subschema.
		out := make([]any, len(n))
		for i, e := range n {
			out[i] = minifyNode(e)
		}
		return out
	default:
		return v
	}
}

func capDesc(s string) string {
	r := []rune(s)
	if len(r) <= maxSchemaDescLen {
		return s
	}
	return string(r[:maxSchemaDescLen]) + "…"
}
