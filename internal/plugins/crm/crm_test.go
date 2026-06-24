package crm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

// fakeRunner records the (name, args) it was called with and returns a
// canned result. It NEVER spawns the real (operator-only) binary.
type fakeRunner struct {
	called  bool
	gotName string
	gotArgs []string
	out     []byte
	err     error
}

func (f *fakeRunner) run(ctx context.Context, name string, args []string) ([]byte, error) {
	f.called = true
	f.gotName = name
	f.gotArgs = args
	return f.out, f.err
}

// newSvc builds a Service with an injected fake runner (bypasses Open's real
// runner wiring so no process is ever spawned).
func newSvc(fr *fakeRunner) *Service {
	return &Service{path: "/fake/crm-tool", timeout: defaultTimeout, run: fr.run}
}

func call(name, query string) model.ToolCall {
	args := json.RawMessage(`{}`)
	if query != "" {
		b, _ := json.Marshal(struct {
			Query string `json:"query"`
		}{query})
		args = b
	}
	return model.ToolCall{Name: name, Arguments: args}
}

// TestReadAllowedWithoutConfirm: a read subcommand runs with no Confirm and
// AllowMutate=false.
func TestReadAllowedWithoutConfirm(t *testing.T) {
	fr := &fakeRunner{out: []byte("result rows")}
	d := NewDispatcher(newSvc(fr), Policy{AllowMutate: false, Confirm: nil})

	res, isErr, err := d.Dispatch(context.Background(), call("crm_search", "alice"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if isErr {
		t.Fatalf("read should not be an error: %q", res)
	}
	if !fr.called {
		t.Fatal("runner should have been called for a read subcommand")
	}
	if !strings.Contains(res, "result rows") {
		t.Fatalf("missing output: %q", res)
	}
}

// TestGatedBlockedWhenNoApproval: a gated subcommand is BLOCKED with
// AllowMutate=false and Confirm=nil — deny-by-default. The runner must NOT be
// reached.
func TestGatedBlockedWhenNoApproval(t *testing.T) {
	for _, tc := range []struct{ tool, sub string }{
		{"crm_ask", "ask"},
		{"crm_align", "align"},
		{"crm_commitments_list", "commitments-list"},
	} {
		fr := &fakeRunner{out: []byte("should not run")}
		d := NewDispatcher(newSvc(fr), Policy{AllowMutate: false, Confirm: nil})

		res, isErr, err := d.Dispatch(context.Background(), call(tc.tool, "q"))
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", tc.tool, err)
		}
		if !isErr {
			t.Fatalf("%s: gated op with no approval must be an error: %q", tc.tool, res)
		}
		if !strings.Contains(res, "blocked") {
			t.Fatalf("%s: expected a 'blocked' message, got %q", tc.tool, res)
		}
		if fr.called {
			t.Fatalf("%s: runner must NOT be reached when blocked", tc.tool)
		}
	}
}

// TestGatedBlockedWhenConfirmDenies: a gated subcommand is BLOCKED when
// Confirm returns false.
func TestGatedBlockedWhenConfirmDenies(t *testing.T) {
	fr := &fakeRunner{out: []byte("should not run")}
	deny := func(ctx context.Context, action, detail string) bool { return false }
	d := NewDispatcher(newSvc(fr), Policy{AllowMutate: false, Confirm: deny})

	res, isErr, _ := d.Dispatch(context.Background(), call("crm_ask", "q"))
	if !isErr || !strings.Contains(res, "blocked") {
		t.Fatalf("deny-Confirm should block: isErr=%v res=%q", isErr, res)
	}
	if fr.called {
		t.Fatal("runner must NOT be reached when Confirm denies")
	}
}

// TestGatedAllowedWhenConfirmApproves: a gated subcommand RUNS when Confirm
// returns true.
func TestGatedAllowedWhenConfirmApproves(t *testing.T) {
	fr := &fakeRunner{out: []byte("ok")}
	approve := func(ctx context.Context, action, detail string) bool { return true }
	d := NewDispatcher(newSvc(fr), Policy{AllowMutate: false, Confirm: approve})

	res, isErr, err := d.Dispatch(context.Background(), call("crm_ask", "what is due"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if isErr {
		t.Fatalf("approved gated op should succeed: %q", res)
	}
	if !fr.called {
		t.Fatal("runner should run after approval")
	}
	if fr.gotName != "ask" {
		t.Fatalf("subcommand should be 'ask', got %q", fr.gotName)
	}
}

// TestGatedAllowedWhenAllowMutate: AllowMutate=true blanket-permits gated ops
// (no Confirm needed).
func TestGatedAllowedWhenAllowMutate(t *testing.T) {
	fr := &fakeRunner{out: []byte("ok")}
	d := NewDispatcher(newSvc(fr), Policy{AllowMutate: true, Confirm: nil})

	_, isErr, err := d.Dispatch(context.Background(), call("crm_align", "alice"))
	if err != nil || isErr {
		t.Fatalf("AllowMutate should permit gated op: isErr=%v err=%v", isErr, err)
	}
	if fr.gotName != "align" {
		t.Fatalf("subcommand should be 'align', got %q", fr.gotName)
	}
}

// TestUnknownSubcommandRejectedBeforeExec: Service.Exec rejects a
// non-allowlisted subcommand WITHOUT ever calling the runner.
func TestUnknownSubcommandRejectedBeforeExec(t *testing.T) {
	fr := &fakeRunner{out: []byte("should not run")}
	svc := newSvc(fr)

	_, err := svc.Exec(context.Background(), "drop-database", "x")
	if err == nil {
		t.Fatal("unknown subcommand must error")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected 'not allowed', got %v", err)
	}
	if fr.called {
		t.Fatal("runner must NOT be reached for a disallowed subcommand")
	}
}

// TestUnknownToolRejected: an unknown tool name never resolves to a
// subcommand and never reaches the runner.
func TestUnknownToolRejected(t *testing.T) {
	fr := &fakeRunner{}
	d := NewDispatcher(newSvc(fr), Policy{})
	res, isErr, _ := d.Dispatch(context.Background(), model.ToolCall{Name: "crm_evil", Arguments: json.RawMessage(`{}`)})
	if !isErr || !strings.Contains(res, "unknown crm tool") {
		t.Fatalf("unknown tool should error: %q", res)
	}
	if fr.called {
		t.Fatal("runner must NOT be reached for an unknown tool")
	}
}

// TestPositionalArgvNoShell: the fake runner receives EXACTLY
// [query-as-single-arg] — proving positional argv with no shell splitting.
// A query containing shell metacharacters must arrive as ONE argv element.
func TestPositionalArgvNoShell(t *testing.T) {
	fr := &fakeRunner{out: []byte("ok")}
	d := NewDispatcher(newSvc(fr), Policy{})

	// Metacharacters that a shell would split/expand — must stay literal.
	query := "alice; rm -rf / && echo $HOME `id` *"
	if _, _, err := d.Dispatch(context.Background(), call("crm_search", query)); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fr.gotName != "search" {
		t.Fatalf("name should be 'search', got %q", fr.gotName)
	}
	want := []string{query}
	if !reflect.DeepEqual(fr.gotArgs, want) {
		t.Fatalf("args should be exactly the single positional query.\n got=%#v\nwant=%#v", fr.gotArgs, want)
	}
}

// TestNoQueryNoArgs: a subcommand called with no query (e.g.
// commitments-list) passes ZERO positional args.
func TestNoQueryNoArgs(t *testing.T) {
	fr := &fakeRunner{out: []byte("ok")}
	d := NewDispatcher(newSvc(fr), Policy{AllowMutate: true})

	if _, _, err := d.Dispatch(context.Background(), call("crm_commitments_list", "")); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if fr.gotName != "commitments-list" {
		t.Fatalf("name should be 'commitments-list', got %q", fr.gotName)
	}
	if len(fr.gotArgs) != 0 {
		t.Fatalf("expected zero args, got %#v", fr.gotArgs)
	}
}

// TestExecAllowlistIsAuthoritative: Exec is the choke point — even a name the
// dispatcher would map is re-checked by Exec's allowlist.
func TestExecAllowlistAcceptsEachAllowed(t *testing.T) {
	for sub := range allowedSubcommands {
		fr := &fakeRunner{out: []byte("ok")}
		svc := newSvc(fr)
		if _, err := svc.Exec(context.Background(), sub); err != nil {
			t.Fatalf("allowed subcommand %q should run: %v", sub, err)
		}
		if !fr.called {
			t.Fatalf("runner should run for allowed subcommand %q", sub)
		}
	}
}

// TestTimeoutSurfaced: the real runner returns a clear timeout error when the
// context deadline is exceeded. Uses a tiny-timeout Service against `sleep`.
func TestTimeoutSurfaced(t *testing.T) {
	// Drive the REAL runner so the timeout path is exercised. We point at a
	// shim that just sleeps; the allowlist is bypassed by calling realRunner
	// directly (the subcommand name is cosmetic here).
	s := &Service{path: "/bin/sleep", timeout: 50 * time.Millisecond}
	s.run = s.realRunner
	// realRunner builds argv = [name, args...] = ["search", "2"] → `sleep
	// search 2` would error on "search"; instead call with a numeric "name"
	// so /bin/sleep gets a valid duration and actually sleeps past the
	// deadline. We bypass Exec's allowlist deliberately to test the timeout.
	_, err := s.realRunner(context.Background(), "2", nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected 'timed out', got %v", err)
	}
}

// TestExecErrorSurfacesStderr: a non-zero exit surfaces the stderr preview.
func TestExecErrorSurfacesStderr(t *testing.T) {
	fr := &fakeRunner{err: errors.New("crm-tool search failed: no such column: meeting_date")}
	svc := newSvc(fr)
	out, err := svc.Exec(context.Background(), "search", "x")
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	_ = out
	if !strings.Contains(err.Error(), "meeting_date") {
		t.Fatalf("stderr preview lost: %v", err)
	}
}

// TestToolsSpecGating: read tools are not Gated; the heavier set is.
func TestToolsSpecGating(t *testing.T) {
	d := NewDispatcher(nil, Policy{})
	gated := map[string]bool{}
	for _, s := range d.Tools() {
		gated[s.Name] = s.Gated
	}
	read := []string{"crm_search", "crm_lookup", "crm_history", "crm_show"}
	mut := []string{"crm_ask", "crm_align", "crm_commitments_list"}
	for _, n := range read {
		if gated[n] {
			t.Fatalf("%s should NOT be Gated", n)
		}
	}
	for _, n := range mut {
		if !gated[n] {
			t.Fatalf("%s SHOULD be Gated", n)
		}
	}
}

// TestNilServiceReportsUnconfigured: Dispatch on a nil-svc dispatcher (the
// stub path) reports a configuration error, not a panic.
func TestNilServiceReportsUnconfigured(t *testing.T) {
	d := NewDispatcher(nil, Policy{})
	res, isErr, _ := d.Dispatch(context.Background(), call("crm_search", "x"))
	if !isErr || !strings.Contains(res, "not configured") {
		t.Fatalf("nil svc should report unconfigured: %q", res)
	}
}
