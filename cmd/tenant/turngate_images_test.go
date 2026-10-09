package main

import (
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// TEN-493: two image messages with the same text are different turns; the
// same picture twice is the same turn.
func TestTurnDedupeKey_Images(t *testing.T) {
	a := agent.TurnRequest{UserQuery: "(image)", Images: []model.Image{{Path: "/a/1.png"}}}
	b := agent.TurnRequest{UserQuery: "(image)", Images: []model.Image{{Path: "/a/2.png"}}}
	if turnDedupeKey(agent.RouteSurfaceTUI, a) == turnDedupeKey(agent.RouteSurfaceTUI, b) {
		t.Fatal("different images coalesced into one turn")
	}
	if turnDedupeKey(agent.RouteSurfaceTUI, a) != turnDedupeKey(agent.RouteSurfaceTUI, a) {
		t.Fatal("the same request keyed differently")
	}
}
