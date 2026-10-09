package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
)

// rotatingReg ranks a different top-2 on every Search, like an embedding
// ranker seeing a new query each turn.
type rotatingReg struct {
	specs []model.ToolSpec
	n     int
}

func (r *rotatingReg) Get(name string) (model.ToolSpec, bool) {
	for _, s := range r.specs {
		if s.Name == name {
			return s, true
		}
	}
	return model.ToolSpec{}, false
}
func (r *rotatingReg) All() []model.ToolSpec { return slices.Clone(r.specs) }
func (r *rotatingReg) Search(context.Context, []float32, int) ([]model.ToolSpec, error) {
	picks := []model.ToolSpec{r.specs[r.n%len(r.specs)], r.specs[(r.n+1)%len(r.specs)]}
	r.n++
	return picks, nil
}

// TEN-505: the tools array a session sends only grows, in a stable order, so
// it stays byte-identical turn to turn unless a new tool is added; a cleared
// context ranks afresh, and per_turn_tool_ranking restores the old behaviour.
func TestTurn_StickyToolSet(t *testing.T) {
	for _, perTurn := range []bool{false, true} {
		t.Run(fmt.Sprintf("per_turn=%v", perTurn), func(t *testing.T) {
			reg := &rotatingReg{}
			for _, n := range []string{"a", "b", "c", "d", "e"} {
				reg.specs = append(reg.specs, model.ToolSpec{Name: n, Parameters: json.RawMessage(`{"type":"object"}`)})
			}
			a, fake, _, _ := buildAgentFull(t, model.Profile{PlanLoopCeiling: 5}, nil, agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) {
				return "", false, nil
			}), nil, nil, func(c *agent.Config) {
				c.Tools = reg
				c.PerTurnToolRanking = perTurn
			})
			var sent [][]string
			fake.GenerateFn = func(_ context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
				names := make([]string, len(req.Tools))
				for i, s := range req.Tools {
					names[i] = s.Name
				}
				sent = append(sent, names)
				return &model.GenerateResponse{Text: "ok"}, nil
			}
			turn := func() {
				if _, err := a.Turn(context.Background(), agent.TurnRequest{UserQuery: "next"}); err != nil {
					t.Fatal(err)
				}
			}
			for range 5 { // ranks ab, bc, cd, de, ea
				turn()
			}
			want := [][]string{{"a", "b"}, {"a", "b", "c"}, {"a", "b", "c", "d"}, {"a", "b", "c", "d", "e"}, {"a", "b", "c", "d", "e"}}
			if perTurn {
				want = [][]string{{"a", "b"}, {"b", "c"}, {"c", "d"}, {"d", "e"}, {"e", "a"}}
			}
			for i := range want {
				if !slices.Equal(sent[i], want[i]) {
					t.Errorf("turn %d tools = %v, want %v", i+1, sent[i], want[i])
				}
			}
			a.ClearContext()
			turn() // ranks ab again, from scratch
			if got := sent[len(sent)-1]; !slices.Equal(got, []string{"a", "b"}) {
				t.Errorf("after /clear tools = %v, want [a b]", got)
			}
		})
	}
}
