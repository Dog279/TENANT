package web

import (
	"bytes"
	"image"
	"image/jpeg"
	_ "image/png" // decode PNG captures
	"net/http"
	"net/url"
	"os"
	"strings"
)

// maxViewPixels bounds the capture decoded for the model's view (TEN-493):
// a very long page is left as the saved file only.
const maxViewPixels = 40_000_000

// nameByContent renames a capture to the extension its bytes call for:
// chromedp's FullScreenshot at quality 80 writes JPEG, which the old .png
// name hid. It returns the (possibly new) path.
func nameByContent(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return path
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	f.Close()
	if http.DetectContentType(head[:n]) != "image/jpeg" || !strings.HasSuffix(path, ".png") {
		return path
	}
	jpg := strings.TrimSuffix(path, ".png") + ".jpg"
	if err := os.Rename(path, jpg); err != nil {
		return path
	}
	return jpg
}

// screenshotView is what the model is shown of a capture: the top of the
// page, cropped to at most twice as tall as it is wide, as a JPEG. A
// full-page capture of a long page would otherwise scale to an unreadable
// sliver. ok is false when the file can't be read or is too large to decode.
func screenshotView(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || int64(cfg.Width)*int64(cfg.Height) > maxViewPixels {
		return nil, false
	}
	if cfg.Height <= 2*cfg.Width {
		return data, true // already a viewable shape
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	b := img.Bounds()
	top := image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+2*b.Dx())
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return nil, false
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, sub.SubImage(top), &jpeg.Options{Quality: 85}); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}

// hostOf is the host of a page URL, or the URL itself.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
