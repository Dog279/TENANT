package distill

import (
	"context"
	"testing"

	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

// isRestatement adjudicates borderline-similar fact pairs via the summarizer.
func TestIsRestatement(t *testing.T) {
	d := &Distiller{}
	fakeWith := func(jsonOut string) *testllm.Fake {
		f := testllm.New()
		f.GenerateFn = func(_ context.Context, _ model.GenerateRequest) (*model.GenerateResponse, error) {
			return &model.GenerateResponse{Text: jsonOut, FinishReason: "stop"}, nil
		}
		return f
	}

	same, err := d.isRestatement(context.Background(), fakeWith(`{"same":true}`),
		"Tenant is a Go MCP framework", "Tenant: an MCP framework in Go")
	if err != nil || !same {
		t.Fatalf("restatement: same=%v err=%v, want true/nil", same, err)
	}

	distinct, err := d.isRestatement(context.Background(), fakeWith(`{"same":false}`),
		"User prefers Go", "User lives in Colorado")
	if err != nil || distinct {
		t.Fatalf("distinct: same=%v err=%v, want false/nil", distinct, err)
	}

	// Noisy fenced output still parses.
	noisy, err := d.isRestatement(context.Background(), fakeWith("Sure:\n```json\n{\"same\": true}\n```"),
		"a", "b")
	if err != nil || !noisy {
		t.Fatalf("noisy: same=%v err=%v, want true/nil", noisy, err)
	}
}
