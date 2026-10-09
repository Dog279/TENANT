package eval

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// segments maps each digit to its lit seven-segment bars, as bits a..g:
// a top, b upper right, c lower right, d bottom, e lower left, f upper left,
// g middle.
var segments = [10]uint8{0x3f, 0x06, 0x5b, 0x4f, 0x66, 0x6d, 0x7d, 0x07, 0x7f, 0x6f}

// DigitsPNG draws s's digits as black seven-segment numerals on white, 100
// pixels wide each and 120 tall (the vision tasks' picture; other runes are
// left blank).
func DigitsPNG(s string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 100*len(s), 120))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	ink := image.NewUniform(color.Black)
	for i, r := range s {
		if r < '0' || r > '9' {
			continue
		}
		x := 100*i + 20
		bars := [7]image.Rectangle{
			image.Rect(x, 10, x+60, 20),     // a
			image.Rect(x+50, 10, x+60, 60),  // b
			image.Rect(x+50, 60, x+60, 110), // c
			image.Rect(x, 100, x+60, 110),   // d
			image.Rect(x, 60, x+10, 110),    // e
			image.Rect(x, 10, x+10, 60),     // f
			image.Rect(x, 55, x+60, 65),     // g
		}
		for b, rect := range bars {
			if segments[r-'0']&(1<<b) != 0 {
				draw.Draw(img, rect, ink, image.Point{}, draw.Src)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img) // encoding an in-memory RGBA can't fail
	return buf.Bytes()
}
