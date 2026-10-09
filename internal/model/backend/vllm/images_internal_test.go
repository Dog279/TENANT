package vllm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/model"
)

func savedImage(t *testing.T, name string) model.Image {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte{1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	return model.Image{Path: p, MIME: "image/png", Width: 10, Height: 10, Name: name}
}

// TEN-493 acceptance 10: a request without images encodes exactly as it did
// before images existed. The golden is main's toWireMessages output (3794a03)
// for these messages, including an assistant tool-call replay with empty
// content (omitted, not "content":"").
func TestWire_TextOnlyIsByteIdentical(t *testing.T) {
	const golden = `[{"role":"system","content":"be brief"},{"role":"user","content":"hi"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"look","arguments":"{\"a\":1}"}}]},` +
		`{"role":"tool","content":"result","tool_call_id":"c1"},{"role":"assistant","content":"done"}]`
	msgs := []model.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c1", Name: "look", Arguments: json.RawMessage(`{"a":1}`)}}},
		{Role: "tool", ToolCallID: "c1", Content: "result"},
		{Role: "assistant", Content: "done"},
	}
	got, _ := json.Marshal(toWireMessages(msgs))
	if string(got) != golden {
		t.Fatalf("text-only wire changed:\n got %s\nwant %s", got, golden)
	}
}

func TestWire_UserImagesAndToolImages(t *testing.T) {
	a, b := savedImage(t, "a.png"), savedImage(t, "b.png")
	gone := model.Image{Path: filepath.Join(t.TempDir(), "gone.png"), MIME: "image/png", Name: "gone.png"}
	msgs := []model.Message{
		{Role: "user", Content: "what is this?", Images: []model.Image{a, gone}},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c1", Name: "web_screenshot"}, {ID: "c2", Name: "os_read_image"}}},
		{Role: "tool", ToolCallID: "c1", Content: "saved", Images: []model.Image{b}},
		{Role: "tool", ToolCallID: "c2", Content: "png 10×10", Images: []model.Image{a}},
		{Role: "assistant", Content: "ok"},
	}
	out := toWireMessages(msgs)
	raw, _ := json.Marshal(out)
	var wire []map[string]any
	_ = json.Unmarshal(raw, &wire)
	if len(wire) != 6 {
		t.Fatalf("got %d wire messages, want 6 (one image message after the tool run): %s", len(wire), raw)
	}
	user := wire[0]["content"].([]any)
	if user[0].(map[string]any)["type"] != "text" || !strings.Contains(user[0].(map[string]any)["text"].(string), model.ReasonFileGone) {
		t.Fatalf("first part should be the text with the missing file's note: %v", user[0])
	}
	if url := user[1].(map[string]any)["image_url"].(map[string]any)["url"].(string); url != "data:image/png;base64,AQID" {
		t.Fatalf("image part = %q", url)
	}
	if wire[2]["role"] != "tool" || wire[3]["role"] != "tool" || wire[2]["content"] != "saved" {
		t.Fatalf("tool results must stay adjacent and text-only: %s", raw)
	}
	follow := wire[4]
	parts := follow["content"].([]any)
	if follow["role"] != "user" || len(parts) != 3 || !strings.Contains(parts[0].(map[string]any)["text"].(string), "web_screenshot, os_read_image") {
		t.Fatalf("tool images message = %v", follow)
	}
}

func TestWire_VisionOffStripsToNotes(t *testing.T) {
	b := &Backend{profile: model.Profile{Backend: "vllm", Vision: "off"}}
	img := savedImage(t, "a.png")
	out := b.wireMessages([]model.Message{{Role: "user", Content: "look", Images: []model.Image{img}}})
	if len(out[0].Parts) != 0 || !strings.Contains(out[0].Content, model.ReasonNoVision) {
		t.Fatalf("vision off sent %+v", out[0])
	}
}
