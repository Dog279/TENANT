package distill

import (
	"log/slog"
	"testing"

	"tenant/internal/model"
)

// TEN-195: the distillation summarizer can be pinned to a stronger reasoning
// model via SummarizerRouter, while the embedder always stays on the main
// Router (embedding-space consistency).
func TestDistiller_SummarizerRouterSelection(t *testing.T) {
	main := model.NewRouter(model.NewEmptyRegistry(), slog.Default())
	pinned := model.NewRouter(model.NewEmptyRegistry(), slog.Default())

	d := &Distiller{Router: main}
	if got := d.summarizerRouter(); got != main {
		t.Fatalf("nil SummarizerRouter should resolve to the main Router")
	}

	d2 := &Distiller{Router: main, SummarizerRouter: pinned}
	if got := d2.summarizerRouter(); got != pinned {
		t.Fatalf("SummarizerRouter should win for the summarizer LLM")
	}
	if d2.Router != main {
		t.Fatalf("Router (embedder source) must stay the main router")
	}
}
