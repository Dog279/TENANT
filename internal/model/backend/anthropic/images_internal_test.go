package anthropic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/model"
)

// TEN-493: user images become image blocks (before the text); a tool result
// with images becomes a block array; a text-only tool result stays a string.
func TestWire_Images(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(p, []byte{1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	img := model.Image{Path: p, MIME: "image/png", Width: 10, Height: 10}
	msgs := []model.Message{
		{Role: "user", Content: "what is this?", Images: []model.Image{img}},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c1", Name: "shot"}, {ID: "c2", Name: "read"}}},
		{Role: "tool", ToolCallID: "c1", Content: "saved", Images: []model.Image{img}},
		{Role: "tool", ToolCallID: "c2", Content: "plain"},
	}
	raw, _ := json.Marshal(toWireMessages(msgs))
	s := string(raw)
	for _, want := range []string{
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}},{"type":"text","text":"what is this?"}]}`,
		`{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"saved"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}}]}`,
		`{"type":"tool_result","tool_use_id":"c2","content":"plain"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wire lacks %s\n in %s", want, s)
		}
	}
	b := &Backend{profile: model.Profile{Backend: "anthropic", Vision: "off"}}
	if got := b.visibleMessages(msgs[:1]); len(got[0].Images) != 0 || !strings.Contains(got[0].Content, model.ReasonNoVision) {
		t.Errorf("vision off kept images: %+v", got[0])
	}
}
