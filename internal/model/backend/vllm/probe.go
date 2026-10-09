package vllm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"

	"tenant/internal/model"
)

// The vision probe (TEN-493): two squares side by side in colours drawn at
// random, and the model must name them in order. A blind model guessing gets
// it right at most one time in twelve; a server that refuses images says so
// with a 400. Anything else (network, auth, rate limit, 5xx, a reply cut off
// mid-reasoning) is inconclusive: not recorded, retried later.

var probeColours = []struct {
	name string
	c    color.RGBA
}{
	{"red", color.RGBA{220, 20, 20, 255}},
	{"green", color.RGBA{20, 160, 20, 255}},
	{"blue", color.RGBA{20, 40, 220, 255}},
	{"yellow", color.RGBA{240, 220, 20, 255}},
}

const (
	probePrompt   = "Name the two colours from left to right, one word each."
	probeMaxToks  = 4096 // room for a thinking model to finish
	probeTimeout  = 60 * time.Second
	probeRetryGap = 5 * time.Minute
)

var colourWordRe = regexp.MustCompile(`\b(red|green|blue|yellow)\b`)

// imageRefusalRe is how servers word "this model takes no images".
var imageRefusalRe = regexp.MustCompile(`image|multimodal|multi-modal|vision|mm_|media|not support`)

// ProbeVision asks this backend's model whether it can see, through the same
// chat path, auth and HTTP client as real requests (so a Z.ai-style ChatPath
// is honoured). It returns VisionUnknown with the reason when inconclusive.
func (b *Backend) ProbeVision(ctx context.Context) (model.Vision, error) {
	i := rand.IntN(len(probeColours))
	j := (i + 1 + rand.IntN(len(probeColours)-1)) % len(probeColours)
	left, right := probeColours[i], probeColours[j]
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			if x < 32 {
				img.SetRGBA(x, y, left.c)
			} else {
				img.SetRGBA(x, y, right.c)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return model.VisionUnknown, err
	}
	body := chatRequest{
		Model: b.profile.Model,
		Messages: []wireMsg{{Role: "user", Parts: []contentPart{
			{Type: "text", Text: probePrompt},
			{Type: "image_url", ImageURL: &imageURL{URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())}},
		}}},
		MaxTokens: probeMaxToks,
	}
	b.applyReasoning(&body)
	var resp chatResponse
	if err := b.postJSON(ctx, b.chatPath(), body, &resp); err != nil {
		if errors.Is(err, model.ErrInvalidRequest) && imageRefusalRe.MatchString(strings.ToLower(err.Error())) {
			return model.VisionNo, nil
		}
		return model.VisionUnknown, err
	}
	if len(resp.Choices) == 0 {
		return model.VisionUnknown, errors.New("empty reply")
	}
	text := resp.Choices[0].Message.Content
	if strings.TrimSpace(text) == "" {
		text = resp.Choices[0].Message.Reasoning
	}
	return judgeProbe(text, left.name, right.name)
}

// judgeProbe reads the model's answer: the last two colour words it names
// (a thinking model may muse first) must be left then right.
func judgeProbe(text, left, right string) (model.Vision, error) {
	words := colourWordRe.FindAllString(strings.ToLower(text), -1)
	if len(words) < 2 {
		return model.VisionUnknown, errors.New("the reply named no two colours")
	}
	last := words[len(words)-2:]
	if last[0] == left && last[1] == right {
		return model.VisionYes, nil
	}
	return model.VisionNo, nil
}

// probe state per served model: one probe at a time, and a retry gap after an
// inconclusive one, so a broken endpoint is never hammered.
var (
	probeMu    sync.Mutex
	probeState = map[model.VisionKey]*probeEntry{}
)

type probeEntry struct {
	running bool
	retryAt time.Time
}

// probeInBackground starts a probe for this backend's model unless its answer
// is known, one is already running, or the last one was inconclusive less
// than probeRetryGap ago. It never blocks.
func (b *Backend) probeInBackground() {
	key := model.VisionKeyOf(b.profile)
	if model.VisionOf(key) != model.VisionUnknown {
		return
	}
	probeMu.Lock()
	e := probeState[key]
	if e == nil {
		e = &probeEntry{}
		probeState[key] = e
	}
	if e.running || time.Now().Before(e.retryAt) {
		probeMu.Unlock()
		return
	}
	e.running = true
	probeMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		v, err := b.ProbeVision(ctx)
		probeMu.Lock()
		e.running = false
		if v == model.VisionUnknown {
			e.retryAt = time.Now().Add(probeRetryGap)
		}
		probeMu.Unlock()
		if v == model.VisionUnknown {
			model.VisionInconclusive.Log(ctx, "model", b.profile.Model, "endpoint", key.Endpoint, "err", err)
			return
		}
		model.SetVision(key, v)
		model.VisionProbed.Log(ctx, "model", b.profile.Model, "endpoint", key.Endpoint, "answer", v.String())
	}()
}

// ProbeAtStart starts the background probe for a self-hosted model whose
// vision is left to the probe: keyless (a hosted, billed provider is probed
// only when an image is first sent to it) and not set on or off.
func (b *Backend) ProbeAtStart() {
	if b.profile.APIKey != "" || !model.VisionAuto(b.profile) {
		return
	}
	b.probeInBackground()
}
