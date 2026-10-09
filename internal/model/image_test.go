package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageTokensAndNotes(t *testing.T) {
	if got := ImageTokens(Image{Width: 750, Height: 100}); got != 100 {
		t.Errorf("750×100 = %d tokens, want 100", got)
	}
	if got := ImageTokens(Image{Width: 4000, Height: 4000}); got != maxImageTokens {
		t.Errorf("huge = %d, want the cap %d", got, maxImageTokens)
	}
	if got := ImageTokens(Image{}); got != maxImageTokens {
		t.Errorf("unknown size = %d, want the cap", got)
	}
	img := Image{Path: "/d/attachments/2026-10/ab.png", Name: "shot.png", Width: 1280, Height: 720}
	if got := ImageNote(img, ReasonAgedOut); got != "[image: shot.png 1280×720, saved at /d/attachments/2026-10/ab.png; no longer shown]" {
		t.Errorf("note = %q", got)
	}
	if got := WithNotes("look", []Image{img, {Path: "/x/y.jpg"}}, ReasonNoVision); !strings.HasPrefix(got, "look\n[image: shot.png") || !strings.Contains(got, "\n[image: y.jpg, saved at /x/y.jpg; the current model can't see images]") {
		t.Errorf("WithNotes = %q", got)
	}
}

func TestImageDataURIAndSink(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.png")
	if err := os.WriteFile(p, []byte{1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	if uri, err := ImageDataURI(Image{Path: p, MIME: "image/png"}); err != nil || uri != "data:image/png;base64,AQID" {
		t.Errorf("uri = %q, %v", uri, err)
	}
	AttachImage(context.Background(), []byte{1}, "no sink") // no-op, no panic
	var got []string
	ctx := WithImageSink(context.Background(), func(data []byte, name string) { got = append(got, name) })
	AttachImage(ctx, []byte{1}, "one")
	AttachImage(ctx, nil, "empty is ignored")
	if len(got) != 1 || got[0] != "one" {
		t.Errorf("sink got %v", got)
	}
}
