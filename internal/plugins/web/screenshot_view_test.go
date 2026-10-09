package web

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TEN-493: a JPEG capture saved as .png is renamed .jpg; a tall page is
// cropped to its top (at most 2:1) for the model; a short one is shown as is.
func TestScreenshotView(t *testing.T) {
	dir := t.TempDir()
	var jpg bytes.Buffer
	_ = jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 100, 1000)), nil)
	p := filepath.Join(dir, "web-1.png")
	_ = os.WriteFile(p, jpg.Bytes(), 0o600)
	p = nameByContent(p)
	if !strings.HasSuffix(p, "web-1.jpg") {
		t.Fatalf("JPEG capture kept the name %s", p)
	}
	view, ok := screenshotView(p)
	if !ok {
		t.Fatal("no view")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(view))
	if err != nil || cfg.Width != 100 || cfg.Height != 200 {
		t.Fatalf("tall page view = %+v, %v; want the top 100×200", cfg, err)
	}

	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 800, 600)))
	q := filepath.Join(dir, "web-2.png")
	_ = os.WriteFile(q, pngBuf.Bytes(), 0o600)
	if nameByContent(q) != q {
		t.Fatal("a real PNG was renamed")
	}
	if view, ok := screenshotView(q); !ok || !bytes.Equal(view, pngBuf.Bytes()) {
		t.Fatal("a short page should be shown unchanged")
	}
	if hostOf("https://example.com/a?b=1") != "example.com" {
		t.Fatal("hostOf")
	}
}
