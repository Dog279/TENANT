package main

import (
	"context"
	"encoding/json"

	"tenant/internal/model"
)

// fakePlugin is a one-tool plugin ("<name>_do") that records which plugin
// served the last call.
type fakePlugin struct {
	name    string
	lastHit *string
}

func (f fakePlugin) Tools() []model.ToolSpec {
	return []model.ToolSpec{{Name: f.name + "_do", Parameters: json.RawMessage(`{}`)}}
}
func (f fakePlugin) Dispatch(_ context.Context, c model.ToolCall) (string, bool, error) {
	*f.lastHit = f.name
	return "ok:" + f.name, false, nil
}
