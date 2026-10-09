package model

import (
	"strings"
	"sync"
)

// Vision is what Tenant knows about whether a model can see images (TEN-493).
type Vision int

const (
	VisionUnknown Vision = iota
	VisionYes
	VisionNo
)

func (v Vision) String() string {
	switch v {
	case VisionYes:
		return "yes"
	case VisionNo:
		return "no"
	}
	return "unknown"
}

// VisionKey identifies a served model: the same model behind two endpoints,
// or two chat paths, can differ.
type VisionKey struct{ Endpoint, ChatPath, Model string }

// VisionKeyOf is the key a profile's probe answer is filed under.
func VisionKeyOf(p Profile) VisionKey {
	return VisionKey{Endpoint: strings.TrimRight(p.Endpoint, "/"), ChatPath: p.ChatPath, Model: p.Model}
}

// The process-wide vision registry: the vllm backend's probe writes it,
// CanSee and the assembler read it. It lives here, not in the backend, so
// nothing has to import a backend to ask.
var (
	visionMu  sync.RWMutex
	visionMap = map[VisionKey]Vision{}
)

// SetVision records a probe's answer. VisionUnknown clears it.
func SetVision(k VisionKey, v Vision) {
	visionMu.Lock()
	defer visionMu.Unlock()
	if v == VisionUnknown {
		delete(visionMap, k)
		return
	}
	visionMap[k] = v
}

// VisionOf is the recorded answer for k.
func VisionOf(k VisionKey) Vision {
	visionMu.RLock()
	defer visionMu.RUnlock()
	return visionMap[k]
}

// VisionAuto reports whether p leaves vision to the probe: not set on or off.
func VisionAuto(p Profile) bool {
	switch strings.ToLower(strings.TrimSpace(p.Vision)) {
	case "on", "true", "yes", "off", "false", "no":
		return false
	}
	return true
}

// CanSee reports whether images may be sent to p. The operator's "on"/"off"
// wins. Otherwise: every current Claude model sees, so Anthropic defaults to
// yes; a vllm (OpenAI-compatible) model sees when its probe said so; the
// Claude Code CLI and echo take notes (Claude Code can still open the file
// the note names with its own Read tool). It only reads: nothing here waits
// on a probe.
func CanSee(p Profile) bool {
	switch strings.ToLower(strings.TrimSpace(p.Vision)) {
	case "on", "true", "yes":
		return true
	case "off", "false", "no":
		return false
	}
	switch p.Backend {
	case "anthropic":
		return true
	case "vllm":
		return VisionOf(VisionKeyOf(p)) == VisionYes
	}
	return false
}

// Undecided reports a vllm model left to the probe whose answer isn't known
// yet. Its images still go to the backend, which notes them for that request
// and starts the probe: that is how the first image asks a billed provider,
// which isn't probed at launch (TEN-493).
func Undecided(p Profile) bool {
	return p.Backend == "vllm" && VisionAuto(p) && VisionOf(VisionKeyOf(p)) == VisionUnknown
}

// StripImages returns msgs with every image turned into a note in its
// message's text; messages without images are shared, not copied.
func StripImages(msgs []Message, reason string) []Message {
	var out []Message
	for i, m := range msgs {
		if len(m.Images) == 0 {
			continue
		}
		if out == nil {
			out = append([]Message(nil), msgs...)
		}
		out[i].Content = WithNotes(m.Content, m.Images, reason)
		out[i].Images = nil
	}
	if out == nil {
		return msgs
	}
	return out
}

// HasImages reports whether any message carries an image.
func HasImages(msgs []Message) bool {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}
