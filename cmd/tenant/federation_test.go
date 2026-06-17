package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

// fakeDisp is a minimal plugin that answers exactly one tool name with a canned
// result; everything else is "unknown tool". Used to stand in for both the local
// wiki dispatcher and a connected peer's dispatcher in federation tests.
type fakeDisp struct {
	tool   string
	result string
	isErr  bool
	err    error
	calls  int
}

func (f *fakeDisp) Tools() []model.ToolSpec {
	return []model.ToolSpec{{Name: f.tool, Description: "search " + f.tool + " notes"}}
}

func (f *fakeDisp) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	f.calls++
	if call.Name != f.tool {
		return "unknown tool: " + call.Name, true, nil
	}
	return f.result, f.isErr, f.err
}

// fakeMultiDisp answers several tool names from a result map.
type fakeMultiDisp struct {
	tools   []string
	results map[string]string
	calls   map[string]int
}

func (f *fakeMultiDisp) Tools() []model.ToolSpec {
	out := make([]model.ToolSpec, 0, len(f.tools))
	for _, t := range f.tools {
		out = append(out, model.ToolSpec{Name: t, Description: "search " + t})
	}
	return out
}

func (f *fakeMultiDisp) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[call.Name]++
	r, ok := f.results[call.Name]
	if !ok {
		return "unknown tool: " + call.Name, true, nil
	}
	return r, false, nil
}

func dispatchTool(t *testing.T, m *toolMux, name string) (string, bool, error) {
	t.Helper()
	return m.Dispatch(context.Background(), model.ToolCall{
		Name:      name,
		Arguments: json.RawMessage(`{"query":"x"}`),
	})
}

func TestFederate_WikiSearchFansOut(t *testing.T) {
	m := newToolMux()
	m.add("wiki", &fakeDisp{tool: "wiki_search", result: "## Local notes\n- foo"})
	peer := &fakeDisp{tool: "peer_wiki_search", result: "## Wiki results from mac (2)\n- bar"}
	m.adoptPeer("mac", peer, nil)

	out, isErr, err := dispatchTool(t, m, "wiki_search")
	if err != nil || isErr {
		t.Fatalf("clean local+peer call should not error: isErr=%v err=%v", isErr, err)
	}
	if !strings.Contains(out, "Local notes") {
		t.Errorf("local result missing: %q", out)
	}
	if !strings.Contains(out, `From peer "mac"`) || !strings.Contains(out, "trust but verify") {
		t.Errorf("peer result not appended/flagged: %q", out)
	}
	if !strings.Contains(out, "bar") {
		t.Errorf("peer content missing: %q", out)
	}
	// Local-first: local content precedes the peer block.
	if strings.Index(out, "Local notes") > strings.Index(out, "From peer") {
		t.Errorf("local should precede peer block: %q", out)
	}
	if peer.calls != 1 {
		t.Errorf("peer dispatcher should be called exactly once, got %d", peer.calls)
	}
}

func TestFederate_SkipsDenialErrorAndEmpty(t *testing.T) {
	cases := []struct {
		name string
		peer *fakeDisp
	}{
		{"denied", &fakeDisp{tool: "peer_wiki_search", result: "peer denied: wiki not shared", isErr: true}},
		{"error", &fakeDisp{tool: "peer_wiki_search", err: context.DeadlineExceeded}},
		{"empty", &fakeDisp{tool: "peer_wiki_search", result: "## Wiki results from mac (0)\n"}},
		{"noresults", &fakeDisp{tool: "peer_wiki_search", result: "(no results)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newToolMux()
			m.add("wiki", &fakeDisp{tool: "wiki_search", result: "## Local notes\n- foo"})
			m.adoptPeer("mac", tc.peer, nil)

			out, _, _ := dispatchTool(t, m, "wiki_search")
			if !strings.Contains(out, "Local notes") {
				t.Errorf("local result must survive: %q", out)
			}
			if strings.Contains(out, "From peer") {
				t.Errorf("%s peer should be skipped, not appended: %q", tc.name, out)
			}
		})
	}
}

func TestFederate_NoPeersPassthrough(t *testing.T) {
	m := newToolMux()
	m.add("wiki", &fakeDisp{tool: "wiki_search", result: "local only"})

	out, isErr, err := dispatchTool(t, m, "wiki_search")
	if err != nil || isErr {
		t.Fatalf("unexpected err/isErr: %v %v", err, isErr)
	}
	if out != "local only" {
		t.Errorf("with no peers the result should be untouched: %q", out)
	}
}

func TestFederate_LocalErrorNotFederated(t *testing.T) {
	m := newToolMux()
	// Local search itself errors → we must NOT staple peer data onto an error.
	m.add("wiki", &fakeDisp{tool: "wiki_search", result: "wiki index unavailable", isErr: true})
	peer := &fakeDisp{tool: "peer_wiki_search", result: "## Wiki results from mac (2)\n- bar"}
	m.adoptPeer("mac", peer, nil)

	out, isErr, _ := dispatchTool(t, m, "wiki_search")
	if !isErr {
		t.Fatalf("local error should propagate as isErr")
	}
	if strings.Contains(out, "From peer") || peer.calls != 0 {
		t.Errorf("peers must not be consulted when local errors: out=%q calls=%d", out, peer.calls)
	}
}

func TestFederate_NonFederatableUnaffected(t *testing.T) {
	m := newToolMux()
	m.add("os", &fakeDisp{tool: "os_read", result: "file contents"})
	peer := &fakeDisp{tool: "peer_wiki_search", result: "## Wiki results from mac (3)\n- x"}
	m.adoptPeer("mac", peer, nil)

	out, _, _ := dispatchTool(t, m, "os_read")
	if out != "file contents" {
		t.Errorf("non-federatable tool must not fan out: %q", out)
	}
	if peer.calls != 0 {
		t.Errorf("peer should not be called for a non-federatable tool, got %d", peer.calls)
	}
}

func TestFederate_AwarenessDescription(t *testing.T) {
	m := newToolMux()
	m.add("wiki", &fakeDisp{tool: "wiki_search", result: "x"})
	base := m.byName["wiki_search"].spec.Description
	if strings.Contains(base, "peer") {
		t.Fatalf("base description should not mention peers: %q", base)
	}

	m.adoptPeer("mac", &fakeDisp{tool: "peer_wiki_search", result: "y"}, nil)
	desc := m.byName["wiki_search"].spec.Description
	if !strings.Contains(desc, "mac") || !strings.Contains(desc, "VERIFY") {
		t.Errorf("with a peer, description should name it + cue verification: %q", desc)
	}
	if m.baseDesc["wiki_search"] != base {
		t.Errorf("baseDesc should remember the original description; got %q", m.baseDesc["wiki_search"])
	}

	// A second peer is named too, sorted.
	m.adoptPeer("box", &fakeDisp{tool: "peer_wiki_search"}, nil)
	desc = m.byName["wiki_search"].spec.Description
	if !strings.Contains(desc, "box, mac") {
		t.Errorf("both peers should be listed (sorted): %q", desc)
	}
}

func TestAdoptPeer_ExposesNonFederatedHidesFederated(t *testing.T) {
	m := newToolMux()
	m.add("wiki", &fakeDisp{tool: "wiki_search", result: "local notes"})
	peer := &fakeMultiDisp{
		tools: []string{"peer_wiki_search", "peer_memory_search"},
		results: map[string]string{
			"peer_wiki_search":   "## Wiki results from mac (1)\n- w",
			"peer_memory_search": "## Memory from mac (1)\n- mem",
		},
	}
	m.adoptPeer("mac", peer, nil)

	// The federated counterpart is folded → not directly callable.
	if _, ok := m.byName["peer_wiki_search"]; ok {
		t.Error("federated counterpart peer_wiki_search should be hidden from the agent")
	}
	// A non-federated peer tool stays reachable (memory until Phase B).
	if _, ok := m.byName["peer_memory_search"]; !ok {
		t.Fatal("non-federated peer_memory_search should be exposed so capability isn't lost")
	}
	out, isErr, err := dispatchTool(t, m, "peer_memory_search")
	if err != nil || isErr || !strings.Contains(out, "mem") {
		t.Errorf("peer_memory_search should route to the peer: %q isErr=%v err=%v", out, isErr, err)
	}
	// wiki_search still folds the hidden peer_wiki_search in.
	out, _, _ = dispatchTool(t, m, "wiki_search")
	if !strings.Contains(out, "local notes") || !strings.Contains(out, `From peer "mac"`) {
		t.Errorf("wiki_search should fold the peer in: %q", out)
	}
}

func TestAdoptPeer_IdempotentDropsDuplicate(t *testing.T) {
	m := newToolMux()
	m.add("wiki", &fakeDisp{tool: "wiki_search", result: "x"})
	m.adoptPeer("mac", &fakeDisp{tool: "peer_wiki_search"}, nil)

	cleaned := make(chan struct{})
	m.adoptPeer("mac", &fakeDisp{tool: "peer_wiki_search"}, func() { close(cleaned) })
	select {
	case <-cleaned:
	case <-time.After(2 * time.Second):
		t.Error("duplicate adopt should run the new connection's cleanup")
	}
	if len(m.peerDisp) != 1 {
		t.Errorf("duplicate adopt must not add a second dispatcher, have %d", len(m.peerDisp))
	}
	if !m.hasPeer("mac") {
		t.Error("hasPeer should report an adopted peer")
	}
	if m.hasPeer("ghost") {
		t.Error("hasPeer should be false for an unknown peer")
	}
}
