package assemble_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/memory/assemble"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

func img(n int) model.Image {
	return model.Image{Path: fmt.Sprintf("/d/attachments/2026-10/%02d.png", n), MIME: "image/png", Width: 750, Height: 100, Name: fmt.Sprintf("shot%d.png", n)}
}

func seeing() model.Profile { p := mkProfile(); p.Vision = "on"; return p }

// TEN-493: images stay for the last 3 exchanges; older ones become a stable
// note with the saved path.
func TestAssemble_ImagesAgeOutAfterThreeExchanges(t *testing.T) {
	w := working.New()
	for i := 1; i <= 4; i++ {
		w.Append(working.Message{Role: "user", Content: fmt.Sprintf("q%d", i), Images: []model.Image{img(i)}})
		w.Append(working.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)})
	}
	r, err := assemble.New(fakeCounter()).Assemble(context.Background(), assemble.Request{Profile: seeing(), Working: w})
	if err != nil {
		t.Fatal(err)
	}
	first := r.Messages[0]
	if len(first.Images) != 0 || first.Content != "q1\n"+model.ImageNote(img(1), model.ReasonAgedOut) {
		t.Fatalf("exchange 1 should be a note now: %+v", first)
	}
	for _, m := range r.Messages[2:] {
		if m.Role == "user" && len(m.Images) != 1 {
			t.Fatalf("exchanges 2-4 keep their image: %+v", m)
		}
	}
	// The working set itself is untouched (notes are made per request).
	if got := w.Messages()[0]; len(got.Images) != 1 || got.Content != "q1" {
		t.Fatalf("assembling mutated the working set: %+v", got)
	}
}

// At most 8 images per request, newest first.
func TestAssemble_ImageCap(t *testing.T) {
	w := working.New()
	var imgs []model.Image
	for i := 1; i <= 10; i++ {
		imgs = append(imgs, img(i))
	}
	w.Append(working.Message{Role: "user", Content: "ten screenshots", Images: imgs})
	r, err := assemble.New(fakeCounter()).Assemble(context.Background(), assemble.Request{Profile: seeing(), Working: w})
	if err != nil {
		t.Fatal(err)
	}
	m := r.Messages[0]
	if len(m.Images) != 8 || m.Images[0].Path != img(3).Path {
		t.Fatalf("kept %d images starting at %v, want the newest 8", len(m.Images), m.Images[0].Path)
	}
	if strings.Count(m.Content, model.ReasonOverCap) != 2 || !strings.Contains(m.Content, img(1).Path) {
		t.Fatalf("the two oldest should be notes: %q", m.Content)
	}
}

// A model that can't see gets notes; a request without images is untouched.
func TestAssemble_NoVisionNotes(t *testing.T) {
	w := working.New()
	w.Append(working.Message{Role: "user", Content: "look", Images: []model.Image{img(1)}})
	blind := mkProfile() // vllm with no probe answer: can't see yet
	r, err := assemble.New(fakeCounter()).Assemble(context.Background(), assemble.Request{Profile: blind, Working: w})
	if err != nil {
		t.Fatal(err)
	}
	if m := r.Messages[0]; len(m.Images) != 0 || !strings.HasSuffix(m.Content, model.ReasonNoVision+"]") {
		t.Fatalf("blind model got %+v", m)
	}
}

// Images count toward the working tier: in-window at their size, aged-out
// ones at a note's size.
func TestAssemble_ImageTokensCount(t *testing.T) {
	plain, withImg := working.New(), working.New()
	plain.Append(working.Message{Role: "user", Content: "look"})
	withImg.Append(working.Message{Role: "user", Content: "look", Images: []model.Image{img(1)}})
	a := assemble.New(fakeCounter())
	r1, _ := a.Assemble(context.Background(), assemble.Request{Profile: seeing(), Working: plain})
	r2, _ := a.Assemble(context.Background(), assemble.Request{Profile: seeing(), Working: withImg})
	if d := r2.BudgetReport.WorkingTokens - r1.BudgetReport.WorkingTokens; d != model.ImageTokens(img(1)) {
		t.Fatalf("an in-window 750×100 image added %d tokens, want %d", d, model.ImageTokens(img(1)))
	}
}
