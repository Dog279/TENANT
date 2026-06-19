package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// TEN-259: tool results that re-enter the model's CONTEXT must be capped so a
// weak 30B isn't drowned by a 20K web_read / wide SQL dump — while the ARCHIVE
// keeps the full body. capToolResultForContext is the load-bearing helper.
func TestCapToolResultForContext(t *testing.T) {
	big := strings.Repeat("x", 10_000)

	t.Run("disabled is a no-op (frontier path)", func(t *testing.T) {
		if got := capToolResultForContext(big, 0); got != big {
			t.Fatalf("cap=0 must be a no-op; got %d bytes, want %d", len(got), len(big))
		}
		if got := capToolResultForContext(big, -1); got != big {
			t.Fatalf("negative cap must be a no-op; got %d bytes", len(got))
		}
	})

	t.Run("under cap is unchanged", func(t *testing.T) {
		small := "short result"
		if got := capToolResultForContext(small, 2048); got != small {
			t.Fatalf("under-cap content must be unchanged; got %q", got)
		}
	})

	t.Run("over cap is truncated and marked", func(t *testing.T) {
		const maxTokens = 64 // ~256 chars
		got := capToolResultForContext(big, maxTokens)
		if len(got) >= len(big) {
			t.Fatalf("over-cap content must shrink: got %d bytes, input %d", len(got), len(big))
		}
		if !strings.Contains(got, "truncated") {
			t.Fatalf("truncated content must carry the marker; got %q", got)
		}
		head := got[:strings.Index(got, "\n…[truncated")]
		if maxChars := maxTokens * 4; len(head) > maxChars { // helper uses ~4 chars/token
			t.Fatalf("kept body %d chars exceeds cap %d", len(head), maxChars)
		}
	})

	t.Run("never emits invalid UTF-8 at a rune boundary", func(t *testing.T) {
		multibyte := strings.Repeat("世", 5_000) // 3 bytes each → byte cut lands mid-rune
		got := capToolResultForContext(multibyte, 64)
		if !utf8.ValidString(got) {
			t.Fatalf("result must be valid UTF-8 after a mid-rune cut")
		}
	})
}

// feedToolResult must cap the working-set (context) copy while the cap=0 path
// stays byte-identical. (Archive nil → archiveEvent is a no-op here; the
// archive-keeps-full guarantee is structural: feedToolResult passes the full
// `content` to the archive event and only the working copy through the cap.)
func TestFeedToolResult_CapsContextCopy(t *testing.T) {
	ws := working.New()
	a := &Agent{cfg: Config{Working: ws}}
	big := strings.Repeat("y", 10_000)

	a.feedToolResult(context.Background(), ToolCallResult{
		Call:   model.ToolCall{ID: "t1"},
		Result: big,
	}, 64)
	last := lastMsg(t, ws)
	if last.Role != "tool" {
		t.Fatalf("expected a tool message, got role %q", last.Role)
	}
	if len(last.Content) >= len(big) || !strings.Contains(last.Content, "truncated") {
		t.Fatalf("context copy must be capped+marked; got %d bytes, marker=%v",
			len(last.Content), strings.Contains(last.Content, "truncated"))
	}

	a.feedToolResult(context.Background(), ToolCallResult{
		Call:   model.ToolCall{ID: "t2"},
		Result: big,
	}, 0)
	if got := lastMsg(t, ws).Content; got != big {
		t.Fatalf("cap=0 must feed the full result; got %d bytes, want %d", len(got), len(big))
	}
}

func lastMsg(t *testing.T, ws *working.Set) working.Message {
	t.Helper()
	msgs := ws.Messages()
	if len(msgs) == 0 {
		t.Fatal("working set is empty")
	}
	return msgs[len(msgs)-1]
}
