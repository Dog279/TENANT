package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TEN-493: a turn may carry images and no text; the message the model gets
// names them (the test profile has no vision answer, so they are notes), and
// the working set keeps the images themselves.
func TestTurn_ImageOnly(t *testing.T) {
	a, fake, w, _ := buildAgent(t, model.Profile{}, nil, agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil }))
	var sent []model.Message
	fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
		sent = req.Messages
		return &model.GenerateResponse{Text: "a red square"}, nil
	}
	pic := model.Image{Path: "/d/attachments/2026-10/aa.png", MIME: "image/png", Width: 8, Height: 8, Name: "shot.png"}
	if _, err := a.Turn(context.Background(), agent.TurnRequest{Images: []model.Image{pic}}); err != nil {
		t.Fatal(err)
	}
	var user *model.Message
	for i := range sent {
		if sent[i].Role == "user" && strings.Contains(sent[i].Content, "(image)") {
			user = &sent[i]
		}
	}
	if user == nil || !strings.Contains(user.Content, pic.Path) {
		t.Fatalf("the request lacks the image or its note: %+v", sent)
	}
	if got := w.Messages()[0]; got.Content != "(image)" || len(got.Images) != 1 {
		t.Fatalf("working set user message = %+v", got)
	}
}

// A tool's image is saved, labelled like its text result, and rides the
// tool message.
func TestTurn_ToolImage(t *testing.T) {
	pic := tinyPNG(t)
	look := model.ToolSpec{Name: "look", Parameters: json.RawMessage(`{"type":"object"}`),
		ResultLabels: []model.InfoFlowLabel{{Sensitivity: model.SensitivitySensitive, Provenance: model.ProvenanceLocalFile}}}
	disp := agent.DispatcherFunc(func(ctx context.Context, _ model.ToolCall) (string, bool, error) {
		model.AttachImage(ctx, pic, "photo.png")
		return "image photo.png: format=png width=8 height=8", false, nil
	})
	a, fake, w, _ := buildAgent(t, model.Profile{}, []model.ToolSpec{look}, disp)
	n := 0
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		if n++; n == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{{ID: "c1", Name: "look", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		return &model.GenerateResponse{Text: "done"}, nil
	}
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "look at it"}); err != nil {
		t.Fatal(err)
	}
	var tool *model.Image
	for _, m := range w.Messages() {
		if m.Role == "tool" && len(m.Images) == 1 {
			tool = &m.Images[0]
		}
	}
	if tool == nil {
		t.Fatalf("no image on the tool message: %+v", w.Messages())
	}
	if tool.Name != "photo.png" || len(tool.Labels) != 1 || tool.Labels[0].Sensitivity != model.SensitivitySensitive {
		t.Errorf("tool image = %+v, want the tool's sensitive label", tool)
	}
	if b, err := os.ReadFile(tool.Path); err != nil || !bytes.Equal(b, pic) {
		t.Errorf("saved image unreadable or changed: %v", err)
	}
}

// An interjection can carry images, which reach the working set.
func TestInterjectWithImages(t *testing.T) {
	a, fake, w, _ := buildAgent(t, model.Profile{}, nil, agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil }))
	pic := model.Image{Path: "/d/attachments/2026-10/bb.png", MIME: "image/png", Width: 4, Height: 4}
	fake.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: "ok"}, nil
	}
	a.InterjectWithImages("", []model.Image{pic}) // queued for the next turn's first iteration
	if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "hi"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range w.Messages() {
		if m.Role == "user" && strings.Contains(m.Content, "(image)") && len(m.Images) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("interjected image missing from the working set: %+v", w.Messages())
	}
}
