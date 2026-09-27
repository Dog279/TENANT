package route

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

type panicClassifier struct{}

func (panicClassifier) Classify(context.Context, string) (Signal, error) { panic("classifier bug") }

// Each guard logs the panic it contains, with the stack, through slog's
// default logger (TEN-365); the annotator's guard used to swallow it. Not
// parallel: it swaps the process-wide default logger.
func TestPanicGuardsLogTheStack(t *testing.T) {
	var buf bytes.Buffer
	prev, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev); log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	annotateSafely(func(*Decision) { panic("annotator bug") }, &Decision{})
	_, _ = classifySafely(panicClassifier{}, context.Background(), "p")
	_ = refreshPolicyInput(func(*PolicyInput) { panic("refresh bug") }, &PolicyInput{})

	lines := strings.Split(buf.String(), "\n")
	for _, want := range []string{"annotator bug", "classifier bug", "refresh bug"} {
		if !slices.ContainsFunc(lines, func(l string) bool {
			return strings.Contains(l, `panic="`+want+`"`) && strings.Contains(l, "stack=")
		}) {
			t.Fatalf("no stack logged for %q:\n%s", want, buf.String())
		}
	}
}
