package attach

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func pngOf(t *testing.T, w, h int, fill func(x, y int) color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fill(x, y))
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func solid(c color.Color) func(int, int) color.Color { return func(int, int) color.Color { return c } }

func newStore(t *testing.T) *Store {
	t.Helper()
	old := now
	now = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = old })
	return NewStore(t.TempDir())
}

func TestSniff(t *testing.T) {
	var jpg, gf bytes.Buffer
	_ = jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil)
	_ = gif.Encode(&gf, image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White}), nil)
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 20)...)
	heic := ftyp("heic", "mif1", "heic")
	heifOnly := ftyp("mif1", "mif1", "miaf")
	for name, c := range map[string]struct {
		b    []byte
		want string
	}{
		"png":       {pngOf(t, 4, 4, solid(color.White)), "image/png"},
		"jpeg":      {jpg.Bytes(), "image/jpeg"},
		"gif":       {gf.Bytes(), "image/gif"},
		"webp":      {webp, "image/webp"},
		"heic":      {heic, "image/heic"},
		"mif1 only": {heifOnly, ""},
		"text":      {[]byte("hello, not an image"), ""},
	} {
		if got := Sniff(c.b); got != c.want {
			t.Errorf("%s: Sniff = %q, want %q", name, got, c.want)
		}
	}
}

// ftyp builds an ISO-BMFF ftyp box with a major brand and compatible brands.
func ftyp(major string, compat ...string) []byte {
	box := []byte("\x00\x00\x00\x00ftyp" + major + "\x00\x00\x00\x00")
	for _, c := range compat {
		box = append(box, c...)
	}
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	return append(box, make([]byte, 16)...)
}

// A PNG that declares 100 000 × 100 000 pixels is refused from its header,
// before anything is decoded (a decompression bomb would need ~40 GB).
func TestAdd_RefusesPixelBomb(t *testing.T) {
	header := pngOf(t, 1, 1, solid(color.White))
	// Rewrite IHDR's width and height (bytes 16..24) and its CRC.
	binary.BigEndian.PutUint32(header[16:], 100_000)
	binary.BigEndian.PutUint32(header[20:], 100_000)
	binary.BigEndian.PutUint32(header[29:], crc32.ChecksumIEEE(header[12:29]))
	_, err := newStore(t).Add(context.Background(), header, "bomb.png")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestAdd_ScalesLargeImages(t *testing.T) {
	src := pngOf(t, 4000, 3000, func(x, y int) color.Color { return color.RGBA{uint8(x), uint8(y), 80, 255} })
	img, err := newStore(t).Add(context.Background(), src, "big.png")
	if err != nil {
		t.Fatal(err)
	}
	if max(img.Width, img.Height) > MaxLongEdge || img.Width*img.Height > MaxArea {
		t.Fatalf("scaled to %d×%d, over %d px or %d px²", img.Width, img.Height, MaxLongEdge, MaxArea)
	}
	if img.Width*3 != img.Height*4 && abs(img.Width*3-img.Height*4) > 4 {
		t.Errorf("aspect not kept: %d×%d", img.Width, img.Height)
	}
	cfg, _, err := image.DecodeConfig(mustRead(t, img.Path))
	if err != nil || cfg.Width != img.Width || cfg.Height != img.Height {
		t.Fatalf("saved file is %v (%v), reference says %d×%d", cfg, err, img.Width, img.Height)
	}
	if !strings.HasPrefix(img.Path, filepath.Join(filepath.Dir(filepath.Dir(img.Path)), "2026-10")) {
		t.Errorf("path %s not under YYYY-MM", img.Path)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func mustRead(t *testing.T, p string) *bytes.Reader {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

// Small images are kept byte for byte (a small animated GIF stays animated).
func TestAdd_KeepsSmallImagesAsIs(t *testing.T) {
	var gf bytes.Buffer
	pal := color.Palette{color.Black, color.White}
	anim := &gif.GIF{Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 8, 8), pal), image.NewPaletted(image.Rect(0, 0, 8, 8), pal)}, Delay: []int{10, 10}}
	if err := gif.EncodeAll(&gf, anim); err != nil {
		t.Fatal(err)
	}
	img, err := newStore(t).Add(context.Background(), gf.Bytes(), "anim.gif")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(img.Path)
	if !bytes.Equal(got, gf.Bytes()) || img.MIME != "image/gif" || filepath.Ext(img.Path) != ".gif" {
		t.Fatalf("small gif changed: %d bytes → %d, %s %s", gf.Len(), len(got), img.MIME, img.Path)
	}
}

// A large noisy PNG that's still big after scaling becomes a JPEG, with any
// transparency flattened onto white rather than black.
func TestAdd_BigPNGBecomesJPEGOnWhite(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	src := pngOf(t, 2000, 1500, func(x, y int) color.Color {
		if x < 200 {
			return color.RGBA{} // fully transparent strip on the left
		}
		return color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255}
	})
	img, err := newStore(t).Add(context.Background(), src, "noise.png")
	if err != nil {
		t.Fatal(err)
	}
	if img.MIME != "image/jpeg" {
		t.Fatalf("mime = %s, want image/jpeg", img.MIME)
	}
	dec, err := jpeg.Decode(mustRead(t, img.Path))
	if err != nil {
		t.Fatal(err)
	}
	r8, g8, b8, _ := dec.At(5, 5).RGBA()
	if r8>>8 < 240 || g8>>8 < 240 || b8>>8 < 240 {
		t.Errorf("transparent area came out %d,%d,%d, want white", r8>>8, g8>>8, b8>>8)
	}
}

func TestAdd_DedupAndNotImage(t *testing.T) {
	s := newStore(t)
	src := pngOf(t, 10, 10, solid(color.RGBA{200, 10, 10, 255}))
	a, err := s.Add(context.Background(), src, "a.png")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Add(context.Background(), src, "b.png")
	if err != nil {
		t.Fatal(err)
	}
	if a.Path != b.Path || a.Name != "a.png" || b.Name != "b.png" {
		t.Errorf("same bytes saved twice: %s vs %s (names %s, %s)", a.Path, b.Path, a.Name, b.Name)
	}
	if fi, _ := os.Stat(a.Path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("attachment mode %v, want 0600", fi.Mode().Perm())
	}
	if _, err := s.Add(context.Background(), []byte("not an image at all"), "x"); !errors.Is(err, ErrNotImage) {
		t.Errorf("text: err = %v, want ErrNotImage", err)
	}
	if _, err := s.Add(context.Background(), make([]byte, MaxSourceBytes+1), "huge"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("21 MB: err = %v, want ErrTooLarge", err)
	}
}

// HEIC converts through sips on macOS only; elsewhere it's unsupported.
// webp passes through untouched off macOS.
func TestAdd_HEICAndWebp(t *testing.T) {
	heic := ftyp("heic", "mif1", "heic")
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 40)...)

	oldOS, oldRun := goos, runCmd
	t.Cleanup(func() { goos, runCmd = oldOS, oldRun })

	goos = "linux"
	if _, err := newStore(t).Add(context.Background(), heic, "IMG_1.HEIC"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("HEIC on linux: err = %v, want ErrUnsupported", err)
	}
	img, err := newStore(t).Add(context.Background(), webp, "x.webp")
	if err != nil || img.MIME != "image/webp" || img.Width != 0 {
		t.Errorf("webp on linux = %+v, %v; want passed through", img, err)
	}

	goos = "darwin"
	var jpg bytes.Buffer
	_ = jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 30, 20)), nil)
	var ran []string
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args[:3], " "))
		return nil, os.WriteFile(args[len(args)-1], jpg.Bytes(), 0o600)
	}
	img, err = newStore(t).Add(context.Background(), heic, "IMG_1.HEIC")
	if err != nil || img.MIME != "image/jpeg" || img.Width != 30 || img.Height != 20 {
		t.Fatalf("HEIC on macOS = %+v, %v; want a 30×20 JPEG", img, err)
	}
	if len(ran) != 1 || ran[0] != "sips -s format jpeg" {
		t.Errorf("ran %q, want sips -s format jpeg", ran)
	}
	// A webp sips can't read still passes through.
	runCmd = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("unsupported"), errors.New("exit 13")
	}
	if img, err = newStore(t).Add(context.Background(), webp, "y.webp"); err != nil || img.MIME != "image/webp" {
		t.Errorf("webp when sips fails = %+v, %v; want passed through", img, err)
	}
}

func TestFit(t *testing.T) {
	for _, c := range []struct{ w, h, mw, mh int }{
		{800, 600, 800, 600},
		{1568, 700, 1568, 700},
		{3136, 1000, 1568, 500},
	} {
		w, h := fit(c.w, c.h)
		if w != c.mw || h != c.mh {
			t.Errorf("fit(%d,%d) = %d,%d, want %d,%d", c.w, c.h, w, h, c.mw, c.mh)
		}
	}
	if w, h := fit(1500, 1500); w*h > MaxArea {
		t.Errorf("fit(1500,1500) = %d×%d, over the area cap", w, h)
	}
}
