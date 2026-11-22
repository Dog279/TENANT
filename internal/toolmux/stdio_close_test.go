package toolmux

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/model"
	"tenant/internal/plugins/mcpremote"
)

// Close stops a local server's background start that is still under way and
// waits for it: nothing that start does happens after Close returns (it was
// a work directory made after a child's mux closed, which a test's temp
// cleanup then tripped over). A start that is under way when Close comes,
// and still brings its server up, has the server torn down, not adopted.
func TestCloseWaitsForABackgroundStart(t *testing.T) {
	for name, comesUp := range map[string]bool{"a start that fails": false, "a start that still comes up": true} {
		t.Run(name, func(t *testing.T) {
			m := New()
			m.SetMCPDeps(MCPDeps{Ctx: context.Background()})
			started := make(chan struct{})
			var after, tornDown atomic.Bool
			var closed atomic.Bool
			open := func(ctx context.Context, _ bool, _ mcpremote.Policy) (Plugin, func(), error) {
				close(started)
				<-ctx.Done() // the start is slow: Close is what ends it
				time.Sleep(50 * time.Millisecond)
				after.Store(!closed.Load()) // what the start did, done before Close returned
				if !comesUp {
					return nil, nil, ctx.Err()
				}
				return fakeActivatedPlugin{specs: []model.ToolSpec{{Name: "srv_tool"}}}, func() { tornDown.Store(true) }, nil
			}
			label, err := m.RegisterMCPStdio("srv", "hint", open)
			if err != nil {
				t.Fatal(err)
			}
			go m.StartMCPStdioSilently(label, open)
			<-started
			m.Close()
			closed.Store(true)
			if !after.Load() {
				t.Fatal("the background start was still working after Close returned")
			}
			if comesUp && !tornDown.Load() {
				t.Fatal("a server that came up after Close was kept")
			}
			for _, s := range m.All() {
				if s.Name == "srv_tool" {
					t.Fatal("a server that came up after Close was adopted")
				}
			}
		})
	}
	// A start that begins after Close does nothing.
	m := New()
	m.Close()
	m.StartMCPStdioSilently("mcp:x", func(context.Context, bool, mcpremote.Policy) (Plugin, func(), error) {
		t.Fatal("a start after Close ran")
		return nil, nil, nil
	})
}
