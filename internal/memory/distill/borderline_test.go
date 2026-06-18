package distill

import (
	"context"
	"testing"

	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

// classifyBorderline adjudicates borderline-similar fact pairs via the
// summarizer into same / supersedes / distinct (Phase 2).
func TestClassifyBorderline(t *testing.T) {
	d := &Distiller{}
	fakeWith := func(jsonOut string) *testllm.Fake {
		f := testllm.New()
		f.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
			return &model.GenerateResponse{Text: jsonOut, FinishReason: "stop"}, nil
		}
		return f
	}

	same, err := d.classifyBorderline(context.Background(), fakeWith(`{"verdict":"same"}`),
		"Tenant is a Go MCP framework", "Tenant: an MCP framework in Go")
	if err != nil || same != verdictSame {
		t.Fatalf("same: verdict=%v err=%v, want verdictSame/nil", same, err)
	}

	distinct, err := d.classifyBorderline(context.Background(), fakeWith(`{"verdict":"distinct"}`),
		"User prefers Go", "User lives in Colorado")
	if err != nil || distinct != verdictDistinct {
		t.Fatalf("distinct: verdict=%v err=%v, want verdictDistinct/nil", distinct, err)
	}

	supersede, err := d.classifyBorderline(context.Background(), fakeWith(`{"verdict":"supersedes"}`),
		"User now works at Globex", "User works at Acme")
	if err != nil || supersede != verdictSupersedes {
		t.Fatalf("supersede: verdict=%v err=%v, want verdictSupersedes/nil", supersede, err)
	}

	// Noisy fenced output still parses.
	noisy, err := d.classifyBorderline(context.Background(), fakeWith("Sure:\n```json\n{\"verdict\": \"same\"}\n```"),
		"a", "b")
	if err != nil || noisy != verdictSame {
		t.Fatalf("noisy: verdict=%v err=%v, want verdictSame/nil", noisy, err)
	}

	// An unknown/garbled verdict degrades to distinct (safe insert).
	unknown, err := d.classifyBorderline(context.Background(), fakeWith(`{"verdict":"maybe"}`), "a", "b")
	if err != nil || unknown != verdictDistinct {
		t.Fatalf("unknown: verdict=%v err=%v, want verdictDistinct/nil", unknown, err)
	}
}
