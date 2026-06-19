package toolfmt

import (
	"encoding/json"
	"testing"
)

// TEN-260: RepairJSON must recover the common weak-model malformations so a
// nearly-valid tool call isn't dropped wholesale.
func TestRepairJSON_RecoversMalformed(t *testing.T) {
	cases := []struct{ name, in string }{
		{"trailing comma object", `{"q":"foo",}`},
		{"trailing comma array", `[1,2,]`},
		{"unterminated string", `{"q":"foo`},
		{"truncated nested object", `{"name":"search","arguments":{"q":"foo"`},
		{"truncated array", `{"items":[1,2`},
		{"truncated mid-string path", `{"name":"x","arguments":{"path":"/a/b`},
		{"trailing comma then truncation", `{"a":1, `},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RepairJSON(c.in)
			if !json.Valid([]byte(got)) {
				t.Fatalf("RepairJSON(%q) = %q — still invalid JSON", c.in, got)
			}
		})
	}
}

// Valid JSON — including commas/braces INSIDE strings — must be returned byte
// for byte (the repair must never corrupt a well-formed payload).
func TestRepairJSON_ValidUnchanged(t *testing.T) {
	for _, in := range []string{
		`{"q":"foo"}`,
		`{"a":{"b":[1,2,3]},"c":"x"}`,
		`{"text":"has a comma-brace ,} inside a string"}`,
		`[]`,
		`{}`,
	} {
		if got := RepairJSON(in); got != in {
			t.Errorf("RepairJSON(%q) = %q — valid input must be unchanged", in, got)
		}
	}
}

// The qwen parser must repair + recover a truncated tool call rather than
// dropping it (the silent-drop this ticket fixes).
func TestQwenParseToolCalls_RepairsTruncated(t *testing.T) {
	content := `<tool_call>{"name":"search","arguments":{"q":"foo"</tool_call>`
	calls, err := Qwen{}.ParseToolCalls(content)
	if err != nil {
		t.Fatalf("ParseToolCalls errored on repairable input: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("expected 1 call to search, got %+v", calls)
	}
	if !json.Valid(calls[0].Arguments) {
		t.Fatalf("recovered arguments not valid JSON: %s", calls[0].Arguments)
	}
}

func TestQwenParseToolCalls_ValidUnchanged(t *testing.T) {
	content := `<tool_call>{"name":"search","arguments":{"q":"foo"}}</tool_call>`
	calls, err := Qwen{}.ParseToolCalls(content)
	if err != nil || len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("valid tool call must parse cleanly: calls=%+v err=%v", calls, err)
	}
}
