package assemble

import (
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// Image rules for what a request carries (TEN-493).
const (
	// imageWindowExchanges is how many recent exchanges keep their images;
	// an exchange starts at a user message. Older images become notes.
	imageWindowExchanges = 3
	// maxImagesPerRequest caps the images one request carries, newest first.
	maxImagesPerRequest = 8
	// agedImageTokens is roughly what an image's note costs once it's out of
	// the window.
	agedImageTokens = 40
)

// windowStart returns the index of the first message whose images are still
// shown: the start of the last imageWindowExchanges exchanges, or 0.
func windowStart(n int, role func(int) string) int {
	seen := 0
	for i := n - 1; i >= 0; i-- {
		if role(i) == "user" {
			if seen++; seen == imageWindowExchanges {
				return i
			}
		}
	}
	return 0
}

// workingImageTokens is each working message's image cost: full inside the
// window, the size of a note outside it.
func workingImageTokens(msgs []working.Message) []int {
	start := windowStart(len(msgs), func(i int) string { return msgs[i].Role })
	out := make([]int, len(msgs))
	for i, m := range msgs {
		if i >= start {
			out[i] = model.ImagesTokens(m.Images)
		} else {
			out[i] = agedImageTokens * len(m.Images)
		}
	}
	return out
}

// applyImageRules turns the images a request must not carry into notes:
// those older than the window, those past the per-request cap, and all of
// them when the model can't see. msgs is never mutated; a changed message is
// a copy. The notes are pure functions of the image, so a message whose
// images aged out stays byte-stable from then on (one prefix-cache miss per
// image, not one per turn).
func applyImageRules(msgs []model.Message, p model.Profile) []model.Message {
	if !model.HasImages(msgs) {
		return msgs
	}
	out := append([]model.Message(nil), msgs...)
	start := windowStart(len(out), func(i int) string { return out[i].Role })
	kept := 0
	for i := len(out) - 1; i >= 0; i-- {
		m := out[i]
		if len(m.Images) == 0 {
			continue
		}
		if i < start {
			out[i].Content = model.WithNotes(m.Content, m.Images, model.ReasonAgedOut)
			out[i].Images = nil
			continue
		}
		// Keep the newest images of the newest messages, up to the cap.
		room := max(0, maxImagesPerRequest-kept)
		if room >= len(m.Images) {
			kept += len(m.Images)
			continue
		}
		over := m.Images[:len(m.Images)-room]
		out[i].Content = model.WithNotes(m.Content, over, model.ReasonOverCap)
		out[i].Images = append([]model.Image(nil), m.Images[len(m.Images)-room:]...)
		kept += room
	}
	if !model.CanSee(p) {
		return model.StripImages(out, model.ReasonNoVision)
	}
	return out
}
