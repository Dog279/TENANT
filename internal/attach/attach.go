// Package attach turns image bytes into a model.Image (TEN-493): it sniffs
// the type, refuses decompression bombs, converts HEIC (and webp) with macOS
// sips, scales anything larger than vision encoders use, re-encodes, and
// saves one normalized copy under <data>/attachments. It decodes every image
// a message carries (the web plugin decodes only its own screenshot, to crop
// it); everything downstream carries the saved path.
package attach

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/model"
)

// Limits. The long edge and area match what Anthropic resizes to (so an image
// costs about 1600 tokens); 5 MB is its per-image limit; the pixel budget is
// checked from the header before anything is decoded.
const (
	MaxSourceBytes = 20 << 20
	MaxImageBytes  = 5 << 20
	MaxLongEdge    = 1568
	MaxArea        = 1_150_000
	MaxPixels      = 40_000_000
	keepPNGBytes   = 1 << 20
	jpegQuality    = 85
)

var (
	ErrNotImage    = errors.New("not an image (png, jpeg, gif, webp or HEIC)")
	ErrUnsupported = errors.New("this image format can't be shown on this host")
	ErrTooLarge    = errors.New("image too large")
)

// Store saves normalized images under dir.
type Store struct{ dir string }

// NewStore returns the store for a data directory.
func NewStore(dataDir string) *Store { return &Store{dir: filepath.Join(dataDir, "attachments")} }

// Dir is where the store saves.
func (s *Store) Dir() string { return s.dir }

// Seams for tests.
var (
	goos   = runtime.GOOS
	now    = time.Now
	runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
)

// Add saves a normalized copy of src and returns its reference. Callers pass
// bytes they already read safely (a vetted handle, a download, the
// clipboard), so the store never re-opens a caller's path. name is
// display-only.
func (s *Store) Add(ctx context.Context, src []byte, name string) (model.Image, error) {
	if len(src) > MaxSourceBytes {
		return model.Image{}, fmt.Errorf("%w: %d MB (the limit is %d MB)", ErrTooLarge, len(src)>>20, MaxSourceBytes>>20)
	}
	out, mime, w, h, err := normalize(ctx, src)
	if err != nil {
		return model.Image{}, err
	}
	path, err := s.save(out, mime)
	if err != nil {
		return model.Image{}, err
	}
	return model.Image{Path: path, MIME: mime, Width: w, Height: h, Name: name}, nil
}

// Sniff reports the image type of b ("image/png", "image/jpeg", "image/gif",
// "image/webp", "image/heic"), or "" when it isn't one Tenant takes.
func Sniff(b []byte) string {
	if isHEIC(b) {
		return "image/heic"
	}
	switch ct := http.DetectContentType(b); ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct
	}
	return ""
}

// normalize returns the bytes to save, their type and size.
func normalize(ctx context.Context, src []byte) (out []byte, mime string, w, h int, err error) {
	mime = Sniff(src)
	switch mime {
	case "":
		return nil, "", 0, 0, ErrNotImage
	case "image/heic", "image/webp":
		if goos == "darwin" {
			if jpg, cerr := sipsJPEG(ctx, src, mime); cerr == nil {
				return normalize(ctx, jpg)
			} else if mime == "image/heic" {
				return nil, "", 0, 0, fmt.Errorf("%w: HEIC conversion failed: %v", ErrUnsupported, cerr)
			}
		}
		if mime == "image/heic" {
			return nil, "", 0, 0, fmt.Errorf("%w: HEIC needs macOS to convert", ErrUnsupported)
		}
		// webp: the stdlib can't decode it; pass it through when it fits.
		if len(src) > MaxImageBytes {
			return nil, "", 0, 0, fmt.Errorf("%w: webp over %d MB", ErrTooLarge, MaxImageBytes>>20)
		}
		return src, mime, 0, 0, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("%w: %v", ErrNotImage, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, "", 0, 0, fmt.Errorf("%w: %d×%d pixels (the limit is %d megapixels)", ErrTooLarge, cfg.Width, cfg.Height, MaxPixels/1_000_000)
	}
	tw, th := fit(cfg.Width, cfg.Height)
	if tw == cfg.Width && th == cfg.Height && len(src) <= MaxImageBytes {
		return src, mime, cfg.Width, cfg.Height, nil // already small enough: keep it byte for byte
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("%w: %v", ErrNotImage, err)
	}
	scaled := scale(img, tw, th)
	out, mime, err = encode(scaled, mime)
	if err != nil {
		return nil, "", 0, 0, err
	}
	if len(out) > MaxImageBytes {
		return nil, "", 0, 0, fmt.Errorf("%w: %d MB after scaling", ErrTooLarge, len(out)>>20)
	}
	return out, mime, tw, th, nil
}

// fit returns the size w×h scales to: within MaxLongEdge and MaxArea, never
// larger than it is.
func fit(w, h int) (int, int) {
	f := 1.0
	if long := max(w, h); long > MaxLongEdge {
		f = float64(MaxLongEdge) / float64(long)
	}
	if area := float64(w) * float64(h); area*f*f > MaxArea {
		f = math.Sqrt(MaxArea / area)
	}
	if f >= 1 {
		return w, h
	}
	return max(1, int(float64(w)*f)), max(1, int(float64(h)*f))
}

// scale resizes img to tw×th by averaging each destination pixel's source
// box (area averaging: no aliasing on the large reductions screenshots need,
// and no dependency beyond the stdlib).
func scale(img image.Image, tw, th int) *image.RGBA {
	b := img.Bounds()
	src, ok := img.(*image.RGBA)
	if !ok || b.Min != (image.Point{}) { // a PNG decodes to RGBA already: no second full copy
		src = image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	}
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	if tw == sw && th == sh {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		y0, y1 := y*sh/th, max((y+1)*sh/th, y*sh/th+1)
		for x := 0; x < tw; x++ {
			x0, x1 := x*sw/tw, max((x+1)*sw/tw, x*sw/tw+1)
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4]
					r += uint64(p[0])
					g += uint64(p[1])
					bl += uint64(p[2])
					a += uint64(p[3])
					n++
				}
			}
			d := dst.Pix[y*dst.Stride+x*4:]
			d[0], d[1], d[2], d[3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

// encode writes a scaled image: PNG stays PNG while it's small, otherwise
// JPEG; a GIF becomes a single-frame GIF.
func encode(img *image.RGBA, srcMIME string) ([]byte, string, error) {
	var buf bytes.Buffer
	switch srcMIME {
	case "image/gif":
		if err := gif.Encode(&buf, img, nil); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/gif", nil
	case "image/png":
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", err
		}
		if buf.Len() <= keepPNGBytes {
			return buf.Bytes(), "image/png", nil
		}
		buf.Reset()
	}
	flattenOnWhite(img)
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/jpeg", nil
}

// flattenOnWhite composites img over white in place: JPEG has no alpha, and
// a transparent pixel would otherwise come out black. RGBA is premultiplied,
// so each channel gains the white the alpha didn't cover.
func flattenOnWhite(img *image.RGBA) {
	for i := 0; i+3 < len(img.Pix); i += 4 {
		pad := 255 - img.Pix[i+3]
		img.Pix[i] += pad
		img.Pix[i+1] += pad
		img.Pix[i+2] += pad
		img.Pix[i+3] = 255
	}
}

// isHEIC reports an ISO-BMFF file whose major or compatible brands name HEIC.
// The generic HEIF brand (mif1) alone could be any HEIF container, so it
// doesn't count.
func isHEIC(b []byte) bool {
	if len(b) < 16 || string(b[4:8]) != "ftyp" {
		return false
	}
	size := int(binary.BigEndian.Uint32(b[0:4]))
	if size < 16 || size > len(b) {
		size = min(len(b), 64)
	}
	brand := func(s string) bool { return s == "heic" || s == "heix" }
	if brand(string(b[8:12])) {
		return true
	}
	for i := 16; i+4 <= size; i += 4 {
		if brand(string(b[i : i+4])) {
			return true
		}
	}
	return false
}

// sipsJPEG converts HEIC or webp to JPEG with macOS's built-in sips.
func sipsJPEG(ctx context.Context, src []byte, mime string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "tenant-attach-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ext := ".heic"
	if mime == "image/webp" {
		ext = ".webp"
	}
	in, out := filepath.Join(dir, "in"+ext), filepath.Join(dir, "out.jpg")
	if err := os.WriteFile(in, src, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if msg, err := runCmd(ctx, "sips", "-s", "format", "jpeg", in, "--out", out); err != nil {
		return nil, fmt.Errorf("sips: %v: %s", err, bytes.TrimSpace(msg))
	}
	jpg, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	if Sniff(jpg) != "image/jpeg" {
		return nil, errors.New("sips produced no JPEG")
	}
	return jpg, nil
}

// save writes out under <dir>/YYYY-MM/<sha256[:16]>.<ext>; the same bytes
// saved twice reuse one file.
func (s *Store) save(out []byte, mime string) (string, error) {
	sum := sha256.Sum256(out)
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[mime]
	dir := filepath.Join(s.dir, now().UTC().Format("2006-01"))
	path := filepath.Join(dir, hex.EncodeToString(sum[:8])+ext)
	if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(out)) {
		return path, nil
	}
	if err := fsperm.MkdirAll(dir); err != nil {
		return "", err
	}
	if err := fsperm.WriteFile(path, out); err != nil {
		return "", err
	}
	return path, nil
}
