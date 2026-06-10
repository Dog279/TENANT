package main

import "testing"

// ctxFilterAgent must mirror the assembler's filterAgent so the trace reflects
// the agent's real retrieval scope (TEN-49).
func TestCtxFilterAgent(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"main", []string{"main", "main-*"}}, // orchestrator: self + own sub-agents
		{"main-Programmer", []string{"main-Programmer"}}, // sub-agent: self only
	}
	for _, c := range cases {
		got := ctxFilterAgent(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ctxFilterAgent(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ctxFilterAgent(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestCtxClip(t *testing.T) {
	if got := ctxClip("short", 80); got != "short" {
		t.Errorf("under-limit changed: %q", got)
	}
	if got := ctxClip("abcdef", 4); got != "abc…" {
		t.Errorf("clip = %q, want abc…", got)
	}
}
