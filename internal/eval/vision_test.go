package eval

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"
)

// The 4817 picture: 4 lights f but not a, 1 lights b only, 8 lights g.
func TestDigitsPNG(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(DigitsPNG("4817")))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 400 || b.Dy() != 120 {
		t.Fatalf("size %v, want 400×120", b)
	}
	dark := func(x, y int) bool { r, _, _, _ := img.At(x, y).RGBA(); return r < 0x8000 }
	for _, c := range []struct {
		x, y int
		lit  bool
		what string
	}{
		{50, 15, false, "4's top bar"},
		{25, 30, true, "4's upper left bar"},
		{150, 60, true, "8's middle bar"},
		{250, 15, false, "1's top bar"},
		{275, 30, true, "1's upper right bar"},
		{5, 5, false, "the margin"},
	} {
		if dark(c.x, c.y) != c.lit {
			t.Errorf("%s at %d,%d: lit = %v, want %v", c.what, c.x, c.y, !c.lit, c.lit)
		}
	}
}

type imageFakeRunner struct {
	fakeRunner
	got [][]byte
}

func (r *imageFakeRunner) RunWithImages(_ context.Context, _ string, imgs [][]byte) (string, []FixtureToolCall, error) {
	r.got = imgs
	return "The number is 4817.", nil, nil
}

// The shipped vision task sends its picture and passes on 4817; it stays out
// of full; a runner that can't send images leaves it ungraded.
func TestRunLive_VisionTask(t *testing.T) {
	h, err := LoadHarness(EmbeddedTasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range h.FilterSubset(SubsetFull) {
		if task.Subset == SubsetVision {
			t.Errorf("full includes the vision task %s", task.ID)
		}
	}
	if v := h.FilterSubset(SubsetVision); len(v) != 1 || v[0].ImageDigits != "4817" {
		t.Fatalf("vision subset = %v", v)
	}
	r := &imageFakeRunner{}
	h.AgentFactory = func(context.Context, string) (AgentRunner, func() error, error) { return r, nil, nil }
	h.Grader = &FixtureJudge{Score: 5}
	rep, err := h.Run(context.Background(), SubsetVision)
	if err != nil || rep.Aggregates.PassCount != 1 {
		t.Fatalf("report %+v, %v", rep.Results, err)
	}
	if len(r.got) != 1 {
		t.Fatalf("sent %d images, want 1", len(r.got))
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(r.got[0])); err != nil || cfg.Width != 400 {
		t.Errorf("sent image %+v, %v", cfg, err)
	}

	h.AgentFactory = func(context.Context, string) (AgentRunner, func() error, error) { return &fakeRunner{}, nil, nil }
	rep, _ = h.Run(context.Background(), SubsetVision)
	if res := rep.Results[0]; !res.Ungraded || res.ErrorClass != "harness" {
		t.Errorf("text-only runner: %+v, want ungraded (harness)", res)
	}
}
