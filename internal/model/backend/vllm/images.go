package vllm

import (
	"encoding/json"
	"strings"

	"tenant/internal/model"
)

// contentPart is one element of an OpenAI-style content array (TEN-493).
type contentPart struct {
	Type     string    `json:"type"` // text | image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// MarshalJSON writes content as a parts array when the message carries
// images, and otherwise exactly what the plain struct writes (content omitted
// when empty, which every assistant tool-call replay relies on), so a request
// without images is byte-identical to one before images existed.
func (w wireMsg) MarshalJSON() ([]byte, error) {
	type plain wireMsg
	if len(w.Parts) == 0 {
		return json.Marshal(plain(w))
	}
	return json.Marshal(struct {
		plain
		Content []contentPart `json:"content"`
	}{plain(w), w.Parts})
}

// wireMessages maps msgs to the wire for this backend's model: images go as
// image_url parts when the model can see, and as notes when it can't.
func (b *Backend) wireMessages(msgs []model.Message) []wireMsg {
	if model.HasImages(msgs) && !model.CanSee(b.profile) {
		b.probeInBackground()
		msgs = model.StripImages(msgs, model.ReasonNoVision)
	}
	return toWireMessages(msgs)
}

// imageParts turns images into image_url parts; an image whose file is gone
// becomes a note on the text instead.
func imageParts(imgs []model.Image) (parts []contentPart, notes []model.Image) {
	for _, img := range imgs {
		uri, err := model.ImageDataURI(img)
		if err != nil {
			notes = append(notes, img)
			continue
		}
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: uri}})
	}
	return parts, notes
}

// userParts is a user message's content with its images: text first (with
// the notes of any image that couldn't be read), then the pictures.
func userParts(text string, imgs []model.Image) []contentPart {
	parts, gone := imageParts(imgs)
	text = model.WithNotes(text, gone, model.ReasonFileGone)
	var out []contentPart
	if text != "" {
		out = append(out, contentPart{Type: "text", Text: text})
	}
	return append(out, parts...)
}

// toolImageMessage is the user message that carries the images a run of tool
// results returned. OpenAI and llama.cpp take text only in role "tool", so
// the pictures follow the results instead; it comes after the whole run, so
// it never separates an assistant's tool calls from their results.
func toolImageMessage(tools []string, imgs []model.Image) wireMsg {
	label := "[images returned by " + strings.Join(dedupe(tools), ", ") + "]"
	return wireMsg{Role: "user", Parts: userParts(label, imgs)}
}

func dedupe(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n == "" {
			n = "a tool"
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// probeInBackground is filled in by the vision probe (commit D).
func (b *Backend) probeInBackground() {}
