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

// workingImageTokens is each working message's image cost as applyImageRules
// will send it: full for the newest maxImagesPerRequest images inside the
// window when the model may see them, a note's size for the rest.
func workingImageTokens(msgs []working.Message, p model.Profile) []int {
	start := windowStart(len(msgs), func(i int) string { return msgs[i].Role })
	sees := model.CanSee(p) || model.Undecided(p)
	out := make([]int, len(msgs))
	kept := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		imgs := msgs[i].Images
		shown := 0
		if sees && i >= start {
			shown = min(len(imgs), max(0, maxImagesPerRequest-kept))
		}
		kept += shown
		out[i] = model.ImagesTokens(imgs[len(imgs)-shown:]) + agedImageTokens*(len(imgs)-shown)
	}
	return out
}

// applyImageRules turns the images a request must not carry into notes:
// those older than the window, those past the per-request cap, and all of
// them when the model can't see. A model the probe hasn't answered keeps
// them: its backend notes them and starts the probe. msgs is the system block
// and the working set, before the retrieved-context block (a user message of
// its own) is appended, so that block never counts as an exchange. msgs is never mutated; a changed message is
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
	if !model.CanSee(p) && !model.Undecided(p) {
		return model.StripImages(out, model.ReasonNoVision)
	}
	return out
}
