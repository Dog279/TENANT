package fleet

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"tenant/internal/model"
)

// gateRig is a child's gate over three tools, with a master that answers
// every approval the way the test says and counts how often it was asked.
type gateRig struct {
	t     *testing.T
	dir   string
	g     *Gate
	audit *AuditLog
	tools Tools

	allow   bool         // the master's answer
	asked   atomic.Int64 // approvals that reached the master
	reached atomic.Int64 // calls that reached a tool
	during  func()       // run by the "act" tool before it asks
}

const gateFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func gateChild() Child {
	return Child{ID: "m0123456789abcdef", Name: "box", MasterAddresses: []string{"127.0.0.1:1"},
		MasterFingerprint: gateFingerprint, Sandbox: "host", Ceiling: CeilingFull}
}

func newGateRig(t *testing.T, edit func(*Child)) *gateRig {
	t.Helper()
	r := &gateRig{t: t, dir: t.TempDir(), allow: true}
	c := gateChild()
	if edit != nil {
		edit(&c)
	}
	if err := SaveChild(ChildPath(r.dir), c); err != nil {
		t.Fatal(err)
	}
	r.audit, _ = openAudit(t, r.dir)
	// Of the actions these tests use, only the os_exec family runs a command.
	r.g = NewGate(r.dir, r.audit, func(action string) bool { return action != "os_write" })
	r.g.ask = func(context.Context, string, string) bool {
		r.asked.Add(1)
		return r.allow
	}
	r.tools = r.g.Tools(r)
	return r
}

func (r *gateRig) All() []model.ToolSpec {
	return []model.ToolSpec{{Name: "read"}, {Name: "act"}, {Name: "boom"}}
}

func (r *gateRig) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	r.reached.Add(1)
	switch call.Name {
	case "read":
		return "data", false, nil
	case "boom":
		panic("boom")
	}
	if r.during != nil {
		r.during()
	}
	var a struct{ Action string }
	_ = json.Unmarshal(call.Arguments, &a)
	if !r.g.Confirm(ctx, a.Action, "the detail") {
		return "blocked: " + a.Action, true, nil
	}
	return "did " + a.Action, false, nil
}

func (r *gateRig) call(tool, action string) (string, bool) {
	r.t.Helper()
	text, failed, err := r.tools.Dispatch(context.Background(), model.ToolCall{ID: "c1", Name: tool,
		Arguments: json.RawMessage(`{"action":"` + action + `"}`)})
	if err != nil {
		r.t.Fatalf("the gate returned an error, not a result: %v", err)
	}
	return text, failed
}

// records is the audit log as it is now.
func (r *gateRig) records() []AuditRecord {
	r.t.Helper()
	var out []AuditRecord
	for _, line := range readLines(r.t, r.dir) {
		out = append(out, must(parseAuditRecord(line))(r.t))
	}
	return out
}

// rewrite replaces child.json with text put in place of old.
func (r *gateRig) rewrite(old, text string) {
	r.t.Helper()
	b := must(os.ReadFile(ChildPath(r.dir)))(r.t)
	if !strings.Contains(string(b), old) {
		r.t.Fatalf("child.json has no %s:\n%s", old, b)
	}
	if err := os.WriteFile(ChildPath(r.dir), []byte(strings.Replace(string(b), old, text, 1)), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// --- what the machine refuses by itself ---

// Under a ceiling the machine refuses without asking: its master's yes is
// never heard, because the master is never asked.
func TestGateCeilingRefusesWithoutAsking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ceiling Ceiling
		action  string
		asks    bool
	}{
		{"read-only refuses a command", CeilingReadOnly, "os_exec", false},
		{"read-only refuses a dangerous command", CeilingReadOnly, "os_exec_dangerous", false},
		{"read-only refuses a write", CeilingReadOnly, "os_write", false},
		{"read-only refuses what it has never heard of", CeilingReadOnly, "mcp:files:put", false},
		{"no-exec refuses a command", CeilingNoExec, "os_exec", false},
		{"no-exec refuses a command the sandbox turned down", CeilingNoExec, "os_exec_unsandboxed", false},
		{"no-exec refuses what it has never heard of", CeilingNoExec, "mcp:shell:run", false},
		{"no-exec asks about a write", CeilingNoExec, "os_write", true},
		{"full asks about a command", CeilingFull, "os_exec", true},
		{"full asks about a write", CeilingFull, "os_write", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGateRig(t, func(c *Child) { c.Ceiling = tc.ceiling })
			text, failed := r.call("act", tc.action)
			if asked := r.asked.Load() == 1; asked != tc.asks || failed == tc.asks {
				t.Fatalf("asked the master: %v, failed: %v (%s)", asked, failed, text)
			}
			recs := r.records()
			done := recs[len(recs)-1]
			if tc.asks {
				if len(done.Asked) != 1 || done.Asked[0] != (AuditAsk{Action: tc.action, Answer: AskAllowed}) || strings.Contains(text, "[fleet:") {
					t.Errorf("record %+v, text %q", done.Asked, text)
				}
				return
			}
			want := AuditAsk{Action: tc.action, Answer: AskRefused, Why: "ceiling " + string(tc.ceiling)}
			if len(done.Asked) != 1 || done.Asked[0] != want {
				t.Errorf("the record says %+v, want %+v", done.Asked, want)
			}
			// The master is told it was the machine, not the operator, that said no.
			if !strings.Contains(text, "this machine refused, without asking its master: "+tc.action+" (ceiling "+string(tc.ceiling)+")") {
				t.Errorf("the result does not say who refused: %q", text)
			}
		})
	}
}

// With no way to tell which actions run a command, no-exec refuses them all.
func TestGateNoExecWithoutAClassifierRefusesEverything(t *testing.T) {
	r := newGateRig(t, func(c *Child) { c.Ceiling = CeilingNoExec })
	r.g.commands = nil
	if _, failed := r.call("act", "os_write"); !failed || r.asked.Load() != 0 {
		t.Fatalf("failed %v, asked %d", failed, r.asked.Load())
	}
}

// A child.json the machine cannot make sense of serves nothing: not a tool,
// not an approval, and not what it served a moment ago.
func TestGateRefusesEverythingOnAChildFileItCannotRead(t *testing.T) {
	for name, breakIt := range map[string]func(r *gateRig){
		"a ceiling widened by hand to a word that is none": func(r *gateRig) { r.rewrite(`"ceiling": "full"`, `"ceiling": "admin"`) },
		"a key the file does not have":                     func(r *gateRig) { r.rewrite(`"ceiling": "full"`, `"ceiling": "full", "allow_all": true`) },
		"an empty tools list":                              func(r *gateRig) { r.rewrite(`"ceiling": "full"`, `"ceiling": "full", "tools": []`) },
		"a relative secret path":                           func(r *gateRig) { r.rewrite(`"ceiling": "full"`, `"ceiling": "full", "secret_paths": ["keys"]`) },
		"cut short":                                        func(r *gateRig) { r.rewrite(`"ceiling": "full"`, `"ceiling": "fu`) },
		"gone": func(r *gateRig) {
			if err := os.Remove(ChildPath(r.dir)); err != nil {
				r.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newGateRig(t, nil)
			if text, failed := r.call("read", ""); failed || text != "data" {
				t.Fatalf("before: %q, %v", text, failed)
			}
			breakIt(r)
			if specs := r.tools.All(); len(specs) != 0 {
				t.Errorf("it lists %d tools", len(specs))
			}
			reached := r.reached.Load()
			text, failed := r.call("read", "")
			if !failed || r.reached.Load() != reached || !strings.Contains(text, "child.json cannot be read") {
				t.Errorf("a read: %q, failed %v, reached a tool: %v", text, failed, r.reached.Load() != reached)
			}
			if r.g.Confirm(context.Background(), "os_write", "x") || r.asked.Load() != 0 {
				t.Error("an approval got through, or the master was asked")
			}
			recs := r.records()
			if done := recs[len(recs)-1]; done.Outcome != AuditRefused || done.Note != "child.json cannot be read" {
				t.Errorf("the refusal is recorded as %+v", done)
			}
		})
	}
}

// The tools list is the only tools: anything else is not listed and not run.
func TestGateServesOnlyTheListedTools(t *testing.T) {
	r := newGateRig(t, func(c *Child) { c.Tools = []string{"read", "not-a-tool"} })
	if specs := r.tools.All(); len(specs) != 1 || specs[0].Name != "read" {
		t.Fatalf("listed: %+v", specs)
	}
	text, failed := r.call("act", "os_write")
	if !failed || r.reached.Load() != 0 || r.asked.Load() != 0 || !strings.Contains(text, "does not serve act") {
		t.Fatalf("a tool off the list: %q, failed %v, reached %d", text, failed, r.reached.Load())
	}
	if recs := r.records(); len(recs) != 2 || recs[1].Outcome != AuditRefused || recs[1].Note != "not served" {
		t.Errorf("records: %+v", recs)
	}
	if text, failed := r.call("read", ""); failed || text != "data" {
		t.Errorf("a tool on the list: %q, %v", text, failed)
	}
}

// A paused machine serves nothing and approves nothing, from the moment the
// mark is there until it is gone.
func TestGatePaused(t *testing.T) {
	r := newGateRig(t, nil)
	if Paused(r.dir) {
		t.Fatal("a new child is paused")
	}
	if err := SetPaused(r.dir, true); err != nil {
		t.Fatal(err)
	}
	if err := SetPaused(r.dir, true); err != nil || !Paused(r.dir) {
		t.Fatalf("pausing twice: %v, paused %v", err, Paused(r.dir))
	}
	if len(r.tools.All()) != 0 {
		t.Error("a paused machine lists tools")
	}
	if text, failed := r.call("read", ""); !failed || r.reached.Load() != 0 || !strings.Contains(text, "paused") {
		t.Errorf("a call on a paused machine: %q, %v", text, failed)
	}
	if r.g.Confirm(context.Background(), "os_write", "x") || r.asked.Load() != 0 {
		t.Error("a paused machine asked its master")
	}
	if recs := r.records(); recs[len(recs)-1].Outcome != AuditRefused || recs[len(recs)-1].Note != "paused" {
		t.Errorf("the refusal is recorded as %+v", recs[len(recs)-1])
	}
	if err := SetPaused(r.dir, false); err != nil {
		t.Fatal(err)
	}
	if err := SetPaused(r.dir, false); err != nil || Paused(r.dir) {
		t.Fatalf("resuming twice: %v, paused %v", err, Paused(r.dir))
	}
	if text, failed := r.call("read", ""); failed || text != "data" || len(r.tools.All()) != 3 {
		t.Errorf("after the resume: %q, %v", text, failed)
	}
}

// The file on the disk is what counts, at the moment of each check: a
// ceiling lowered while the child runs holds from the next call on, and from
// the next approval inside a call that is already running.
func TestGateReadsTheCeilingFromTheDiskEachTime(t *testing.T) {
	r := newGateRig(t, nil)
	if _, failed := r.call("act", "os_exec"); failed || r.asked.Load() != 1 {
		t.Fatal("a full machine did not ask")
	}
	r.rewrite(`"ceiling": "full"`, `"ceiling": "read-only"`)
	if text, failed := r.call("act", "os_exec"); !failed || r.asked.Load() != 1 {
		t.Fatalf("after the ceiling was lowered: %q, asked %d times", text, r.asked.Load())
	}
	if text, failed := r.call("read", ""); failed || text != "data" {
		t.Errorf("a read under read-only: %q, %v", text, failed)
	}
	r.rewrite(`"ceiling": "read-only"`, `"ceiling": "full"`)
	r.during = func() { r.rewrite(`"ceiling": "full"`, `"ceiling": "no-exec"`) } // while the call runs
	if text, failed := r.call("act", "os_exec"); !failed || r.asked.Load() != 1 {
		t.Fatalf("lowered inside a call: %q, asked %d times", text, r.asked.Load())
	}
}

// --- the audit log ---

// Every call is in the log before it runs and again when it has ended, with
// what was asked and how it was answered.
func TestGateRecordsEveryCall(t *testing.T) {
	r := newGateRig(t, nil)
	r.call("read", "")
	r.allow = false
	r.call("act", "os_exec")
	r.call("boom", "")
	recs := r.records()
	if len(recs) != 6 {
		t.Fatalf("%d records for three calls", len(recs))
	}
	for i, want := range []AuditRecord{
		{Event: AuditCall, Tool: "read", Args: `{"action":""}`},
		{Event: AuditDone, Tool: "read", Outcome: AuditOK, ResultBytes: 4, ResultSHA256: sum256([]byte("data"))},
		{Event: AuditCall, Tool: "act", Args: `{"action":"os_exec"}`},
		{Event: AuditDone, Tool: "act", Outcome: AuditError},
		{Event: AuditCall, Tool: "boom"},
		{Event: AuditDone, Tool: "boom", Outcome: AuditError},
	} {
		got := recs[i]
		if got.Event != want.Event || got.Tool != want.Tool || got.Call != "c1" || got.Outcome != want.Outcome ||
			(want.Args != "" && (got.Args != want.Args || got.ArgsSHA256 != sum256([]byte(want.Args)))) ||
			(want.ResultBytes != 0 && (got.ResultBytes != want.ResultBytes || got.ResultSHA256 != want.ResultSHA256)) {
			t.Errorf("record %d = %+v, want %+v", i+1, got, want)
		}
	}
	if a := recs[3].Asked; len(a) != 1 || a[0] != (AuditAsk{Action: "os_exec", Answer: AskDenied}) {
		t.Errorf("the denied approval is recorded as %+v", a)
	}
	if sum, err := VerifyAudit(r.dir); err != nil || sum.Records != 6 {
		t.Errorf("verify = %+v, %v", sum, err)
	}
}

// Long arguments are kept by their start and their hash, and a call that
// asks more often than a record lists is counted.
func TestGateRecordIsBounded(t *testing.T) {
	r := newGateRig(t, func(c *Child) { c.Ceiling = CeilingReadOnly })
	long := `{"action":"` + strings.Repeat("é", 4000) + `"}`
	g := r.g.Tools(toolFunc(func(ctx context.Context, _ model.ToolCall) (string, bool, error) {
		for range maxAuditAsks + 5 {
			r.g.Confirm(ctx, strings.Repeat("a", 500), "x")
		}
		return "ok", false, nil
	}))
	if _, _, err := g.Dispatch(context.Background(), model.ToolCall{ID: "c1", Name: "any", Arguments: json.RawMessage(long)}); err != nil {
		t.Fatal(err)
	}
	recs := r.records()
	if got := recs[0]; len(got.Args) > maxAuditArgs || got.ArgsSHA256 != sum256([]byte(long)) || !strings.HasPrefix(long, got.Args) {
		t.Errorf("arguments kept: %d bytes, hash %s", len(got.Args), got.ArgsSHA256)
	}
	if got := recs[1]; len(got.Asked) != maxAuditAsks || got.AskedMore != 5 || len(got.Asked[0].Action) > maxToolLen {
		t.Errorf("asked: %d listed, %d more, action of %d bytes", len(got.Asked), got.AskedMore, len(got.Asked[0].Action))
	}
	for _, line := range readLines(t, r.dir) {
		if len(line) > maxAuditLine {
			t.Errorf("a record of %d bytes", len(line))
		}
	}
}

type toolFunc func(ctx context.Context, call model.ToolCall) (string, bool, error)

func (toolFunc) All() []model.ToolSpec { return nil }
func (f toolFunc) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	return f(ctx, call)
}

// A child that cannot write its audit record does not run the call.
func TestGateRefusesTheCallItCannotRecord(t *testing.T) {
	r := newGateRig(t, nil)
	_ = r.audit.Close()
	text, failed := r.call("read", "")
	if !failed || r.reached.Load() != 0 || !strings.Contains(text, "could not write its audit record, so it did not run the call") {
		t.Fatalf("with no audit log: %q, failed %v, reached a tool %d times", text, failed, r.reached.Load())
	}
	// And a gate that was never given a log.
	g := NewGate(r.dir, nil, nil).Tools(r)
	if text, failed, _ := g.Dispatch(context.Background(), model.ToolCall{ID: "c", Name: "read"}); !failed || r.reached.Load() != 0 {
		t.Fatalf("with a nil audit log: %q, %v", text, failed)
	}
}

// A call whose end cannot be written did run: its result says so, and
// carries the hash of the record of its arrival.
func TestGateSaysWhenTheEndOfACallCouldNotBeRecorded(t *testing.T) {
	r := newGateRig(t, nil)
	r.during = func() { _ = r.audit.Close() } // the log goes away while the tool runs
	ask := &asker{id: "c1"}
	text, failed, _ := r.tools.Dispatch(context.WithValue(context.Background(), askKey{}, ask),
		model.ToolCall{ID: "c1", Name: "act", Arguments: json.RawMessage(`{"action":"os_write"}`)})
	if failed || !strings.HasPrefix(text, "did os_write") || !strings.Contains(text, "how it ended could not be written to this machine's audit log") {
		t.Fatalf("result: %q, failed %v", text, failed)
	}
	lines := readLines(t, r.dir)
	if len(lines) != 1 || ask.audit != sum256(lines[0]) {
		t.Errorf("%d records; the result carries %s", len(lines), ask.audit)
	}
}

// --- over the link ---

// The hash of a call's audit record travels with its result, and with the
// result asked for again after a dropped link.
func TestAuditHashTravelsWithTheResult(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	audit, _ := openAudit(t, k.dir)
	g := NewGate(k.dir, audit, nil)
	k.c.cfg.Tools = g.Tools(k.tools)
	k.tools.fns["act"] = func(ctx context.Context, _ json.RawMessage) (string, bool, error) {
		if g.Confirm(ctx, "os_exec", "uptime") {
			return "ran", false, nil
		}
		return "blocked", true, nil
	}
	k.start()
	k.await(LinkConnected)
	r.awaitOnline(t, k.mach.ID, true)

	res, err := r.call(t, k.mach.ID, "echo", `"hi"`)
	if err != nil || res.Text != `"hi"` {
		t.Fatalf("call = %+v, %v", res, err)
	}
	lines := readLines(t, k.dir)
	if len(lines) != 2 || res.Audit != sum256(lines[1]) {
		t.Fatalf("the result carries %q; the record of its end hashes to %s", res.Audit, sum256(lines[len(lines)-1]))
	}
	if rec := must(parseAuditRecord(lines[1]))(t); rec.Call != res.CallID || rec.Tool != "echo" || rec.Outcome != AuditOK {
		t.Errorf("the record: %+v", rec)
	}
	again, err := r.mgr.Result(ctxFor(t), k.mach.ID, res.CallID)
	if err != nil || again.Audit != res.Audit {
		t.Errorf("fleet/result = %+v, %v", again, err)
	}
	// A tool that asks goes through the gate's ceiling and then up the link.
	if err := UpdateChild(ChildPath(k.dir), func(c *Child) error { c.Ceiling = CeilingReadOnly; return nil }); err != nil {
		t.Fatal(err)
	}
	var asked atomic.Bool
	res, err = r.mgr.Call(ctxFor(t), k.mach.ID, Call{Tool: "act", Confirm: func(context.Context, string, string) bool {
		asked.Store(true)
		return true
	}})
	if err != nil || !res.IsError || asked.Load() || !strings.Contains(res.Text, "os_exec (ceiling read-only)") {
		t.Errorf("under read-only: %+v, %v; the master was asked: %v", res, err, asked.Load())
	}
	// And with the ceiling lifted again, the master's yes is what runs it.
	if err := UpdateChild(ChildPath(k.dir), func(c *Child) error { c.Ceiling = CeilingFull; return nil }); err != nil {
		t.Fatal(err)
	}
	res, err = r.mgr.Call(ctxFor(t), k.mach.ID, Call{Tool: "act", Confirm: func(_ context.Context, action, detail string) bool {
		return action == "os_exec" && detail == "uptime"
	}})
	if err != nil || res.IsError || res.Text != "ran" {
		t.Errorf("under full, approved: %+v, %v", res, err)
	}
}

// What a machine sends as the hash is its word: anything that is not a hash
// is dropped.
func TestResultAuditIsBounded(t *testing.T) {
	good := strings.Repeat("0a", 32)
	for in, want := range map[string]string{
		good:                     good,
		"":                       "",
		"not a hash":             "",
		strings.ToUpper(good):    "",
		good + "0":               "",
		strings.Repeat("x", 1e5): "",
	} {
		if got := (callResult{Text: "t", Audit: in}).result("c").Audit; got != want {
			t.Errorf("audit %.20q arrives as %q", in, got)
		}
	}
	// It is left out of the wire when there is none, as before this field.
	if got := string(must(encode(callResult{Text: "t"}))(t)); got != `{"text":"t"}` {
		t.Errorf("a result without a hash is sent as %s", got)
	}
}

// pictureTools is a tool that hands the model pictures, as web_screenshot
// and os_read_image do, and one that hands it none.
type pictureTools struct{}

func (pictureTools) All() []model.ToolSpec { return []model.ToolSpec{{Name: "shots"}, {Name: "plain"}} }

func (pictureTools) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if call.Name == "shots" {
		model.AttachImage(ctx, []byte("png"), "one.png")
		model.AttachImage(ctx, []byte("png"), "two.png")
	}
	return "saved", false, nil
}

// A picture a tool makes on a machine is not sent: the link carries a call's
// text and nothing else, and no model on the machine sees it. The result says
// so, after the tool's own words, which read as if the picture had been shown.
func TestGateSaysAPictureWasNotSent(t *testing.T) {
	r := newGateRig(t, nil)
	tools := r.g.Tools(pictureTools{})
	text, failed, err := tools.Dispatch(context.Background(), model.ToolCall{ID: "c1", Name: "shots"})
	const want = "saved\n[fleet: 2 picture(s) this tool made stayed on this machine: the link to its master carries text only, so the model has not seen them]"
	if err != nil || failed || text != want {
		t.Errorf("a tool that made two pictures answered %q (failed %v, %v), want %q", text, failed, err, want)
	}
	if text, _, _ := tools.Dispatch(context.Background(), model.ToolCall{ID: "c2", Name: "plain"}); text != "saved" {
		t.Errorf("a tool that made no picture answered %q", text)
	}
}
