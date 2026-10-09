package model

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Image is one picture attached to a message (TEN-493): a normalized copy
// saved under <data>/attachments by internal/attach, referenced by path and
// read only when a backend sends it, so no image bytes ride the working set,
// the archive or a log line.
type Image struct {
	Path   string `json:"path"`
	MIME   string `json:"mime"`        // image/png | image/jpeg | image/gif | image/webp
	Width  int    `json:"w,omitempty"` // 0 when unknown (webp passed through)
	Height int    `json:"h,omitempty"`
	Name   string `json:"name,omitempty"` // what the operator or tool called it
	// Labels are the info-flow labels of where the image came from (a tool's
	// ResultLabels, a relay's inbound flow), carried like a text result's.
	Labels []InfoFlowLabel `json:"labels,omitempty"`
}

// maxImageTokens is what one image counts for in the budget when its size
// is unknown, and the most any image counts for: internal/attach scales to
// at most 1568 px and 1.15 MP, which is about 1600 tokens at W×H/750.
const maxImageTokens = 1600

// ImageTokens estimates the context one image costs (Anthropic's W×H/750,
// a fair stand-in for other vision encoders).
func ImageTokens(img Image) int {
	if img.Width <= 0 || img.Height <= 0 {
		return maxImageTokens
	}
	return min((img.Width*img.Height+749)/750, maxImageTokens)
}

// ImagesTokens sums ImageTokens over imgs.
func ImagesTokens(imgs []Image) int {
	n := 0
	for _, img := range imgs {
		n += ImageTokens(img)
	}
	return n
}

// Label is how an image is named in a note or placeholder: its name, or the
// file name, plus its size when known.
func (img Image) Label() string {
	name := img.Name
	if name == "" {
		name = filepath.Base(img.Path)
	}
	if img.Width > 0 && img.Height > 0 {
		return fmt.Sprintf("%s %d×%d", name, img.Width, img.Height)
	}
	return name
}

// ImageNote is the text that stands in for an image a model won't be shown:
// what it was, where it is saved, and why it isn't shown. The note is a pure
// function of the image and the reason, so a message that switched to notes
// stays byte-stable from then on (prefix caching, TEN-505).
func ImageNote(img Image, reason string) string {
	note := fmt.Sprintf("[image: %s, saved at %s", img.Label(), img.Path)
	if reason != "" {
		note += "; " + reason
	}
	return note + "]"
}

// Reasons an image becomes a note.
const (
	ReasonNoVision  = "the current model can't see images"
	ReasonAgedOut   = "no longer shown"
	ReasonOverCap   = "not shown: too many images in this request"
	ReasonFileGone  = "the file has since been removed"
	ReasonNoPreview = "this backend can't show images; open the file to look at it"
)

// WithNotes returns content with one note per image appended, each on its own
// line.
func WithNotes(content string, imgs []Image, reason string) string {
	if len(imgs) == 0 {
		return content
	}
	notes := make([]string, len(imgs))
	for i, img := range imgs {
		notes[i] = ImageNote(img, reason)
	}
	if content == "" {
		return strings.Join(notes, "\n")
	}
	return content + "\n" + strings.Join(notes, "\n")
}

// ImageDataURI reads img's saved file and returns it as a data: URI, which is
// how both the OpenAI-compatible and Anthropic wire formats take an image.
func ImageDataURI(img Image) (string, error) {
	data, err := ImageData(img)
	if err != nil {
		return "", err
	}
	return "data:" + img.MIME + ";base64," + data, nil
}

// ImageData reads img's saved file and returns it base64-encoded.
func ImageData(img Image) (string, error) {
	b, err := os.ReadFile(img.Path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

type imageSinkKey struct{}

// WithImageSink returns ctx carrying sink, which receives the images a tool
// attaches during one dispatch (TEN-493). The agent installs one per call;
// tools call AttachImage and never import the agent.
func WithImageSink(ctx context.Context, sink func(data []byte, name string)) context.Context {
	return context.WithValue(ctx, imageSinkKey{}, sink)
}

// AttachImage hands the image a tool produced to the model: the bytes the
// tool already read through its own vetted path (never a path to re-open,
// which a symlink could swap in between) and a display name. A no-op without
// a sink, so tools work the same outside an agent turn.
func AttachImage(ctx context.Context, data []byte, name string) {
	if sink, ok := ctx.Value(imageSinkKey{}).(func(data []byte, name string)); ok && sink != nil && len(data) > 0 {
		sink(data, name)
	}
}
