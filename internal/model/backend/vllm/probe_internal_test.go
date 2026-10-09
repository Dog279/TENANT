package vllm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/model"
)

// seeingServer answers the probe by actually reading the picture: it names
// the colours of the left and right halves (swapped when liar is set).
func seeingServer(t *testing.T, path string, liar bool, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content []struct {
					ImageURL *struct{ URL string } `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		uri := req.Messages[0].Content[1].ImageURL.URL
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:image/png;base64,"))
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Errorf("probe image: %v", err)
			return
		}
		name := func(x int) string {
			r, g, b, _ := img.At(x, 16).RGBA()
			for _, c := range probeColours {
				cr, cg, cb, _ := c.c.RGBA()
				if r == cr && g == cg && b == cb {
					return c.name
				}
			}
			return "?"
		}
		l, rr := name(8), name(50)
		if liar {
			l, rr = rr, l
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "Left: " + l + ", right: " + rr + "."}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func backendFor(t *testing.T, endpoint, chatPath string) *Backend {
	t.Helper()
	inst, err := New(context.Background(), model.Profile{ID: "t", Backend: "vllm", Endpoint: endpoint, ChatPath: chatPath, Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return inst.(*Backend)
}

func TestProbeVision(t *testing.T) {
	var hits int32
	see := seeingServer(t, "/paas/v4/chat/completions", false, &hits)
	if v, err := backendFor(t, see.URL, "/paas/v4/chat/completions").ProbeVision(context.Background()); v != model.VisionYes {
		t.Fatalf("a seeing model at a custom chat path = %v (%v), want yes", v, err)
	}
	liar := seeingServer(t, "/v1/chat/completions", true, &hits)
	if v, _ := backendFor(t, liar.URL, "").ProbeVision(context.Background()); v != model.VisionNo {
		t.Fatalf("colours in the wrong order = %v, want no", v)
	}
	for name, c := range map[string]struct {
		status int
		body   string
		want   model.Vision
	}{
		"refuses images": {400, `{"error":"This model does not support image input"}`, model.VisionNo},
		"rate limited":   {429, `{"error":"slow down"}`, model.VisionUnknown},
		"wrong path":     {404, `{"error":"not found"}`, model.VisionUnknown},
		"other 400":      {400, `{"error":"max_tokens is too large"}`, model.VisionUnknown},
		"empty reply":    {200, `{"choices":[{"message":{"content":""}}]}`, model.VisionUnknown},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		if v, _ := backendFor(t, srv.URL, "").ProbeVision(context.Background()); v != c.want {
			t.Errorf("%s: %v, want %v", name, v, c.want)
		}
		srv.Close()
	}
}

// One probe at a time per model; the answer lands in the registry.
func TestProbeInBackground_OncePerModel(t *testing.T) {
	var hits int32
	srv := seeingServer(t, "/v1/chat/completions", false, &hits)
	b := backendFor(t, srv.URL, "")
	key := model.VisionKeyOf(b.profile)
	t.Cleanup(func() { model.SetVision(key, model.VisionUnknown) })
	first, second := b.ProbeAtStart(false), b.ProbeAtStart(false)
	if first == nil || first != second {
		t.Fatal("the second call should wait on the first probe, not start another")
	}
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never ended")
	}
	if model.VisionOf(key) != model.VisionYes {
		t.Fatalf("registry = %v, want yes", model.VisionOf(key))
	}
	if b.ProbeAtStart(false) != nil { // known now: no new request
		t.Fatal("a known answer started another probe")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("%d probe requests, want 1", n)
	}
	if !model.CanSee(b.profile) {
		t.Fatal("CanSee should follow the probe")
	}
}

func TestJudgeProbe(t *testing.T) {
	for text, want := range map[string]model.Vision{
		"Red, blue":                             model.VisionYes,
		"I think... maybe green? No: red, blue": model.VisionYes,
		"Blue, red":                             model.VisionNo,
		"I can't see any image":                 model.VisionUnknown,
	} {
		if got, _ := judgeProbe(text, "red", "blue"); got != want {
			t.Errorf("%q = %v, want %v", text, got, want)
		}
	}
}

// A keyed (billed) provider isn't probed at launch unless its vision is set
// to auto explicitly, or the caller asks for every model (the eval).
func TestProbeAtStart_KeyedProviders(t *testing.T) {
	for _, c := range []struct {
		vision string
		all    bool
		want   bool
	}{
		{"", false, false},
		{"auto", false, true},
		{"", true, true},
		{"off", true, false},
	} {
		var hits int32
		srv := seeingServer(t, "/v1/chat/completions", false, &hits)
		inst, err := New(context.Background(), model.Profile{ID: "k", Backend: "vllm", Endpoint: srv.URL, Model: "m", APIKey: "sk-test", Vision: c.vision}, nil)
		if err != nil {
			t.Fatal(err)
		}
		b := inst.(*Backend)
		t.Cleanup(func() { model.SetVision(model.VisionKeyOf(b.profile), model.VisionUnknown) })
		if done := b.ProbeAtStart(c.all); (done != nil) != c.want {
			t.Errorf("vision %q all %v: probed = %v, want %v", c.vision, c.all, done != nil, c.want)
		} else if done != nil {
			<-done
		}
	}
}
