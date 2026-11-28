package fleet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"tenant/internal/model"
)

const pausedFileName = "child-paused"

// PausedPath is the mark a pause set on the machine itself leaves beside
// child.json (`tenant child pause`). The pause is a file of its own and not
// a key of child.json: that file is what the operator wrote, and the master
// rewrites it when its addresses change.
func PausedPath(dir string) string { return filepath.Join(dir, pausedFileName) }

// Paused reports whether the child in dir is paused on the machine itself.
// A mark that cannot be looked at counts as there.
func Paused(dir string) bool {
	_, err := os.Lstat(PausedPath(dir))
	return !errors.Is(err, fs.ErrNotExist)
}

// SetPaused pauses the child in dir, or resumes it. The running child sees
// the mark at its next check, whichever process set it.
func SetPaused(dir string, paused bool) error {
	if paused {
		if err := writePrivate(PausedPath(dir), []byte(time.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
			return fmt.Errorf("fleet: pause: %w", err)
		}
		return nil
	}
	if err := os.Remove(PausedPath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fleet: resume: %w", err)
	}
	return nil
}

// Gate is what a child puts between its master and its tools: the limits the
// machine sets on itself, and the audit log. The master cannot lift either,
// because both are decided here, on the child, from what is on this
// machine's disk at that moment: child.json (ChildFile.Current) and the
// pause mark.
//
// It works in two places. Tools wraps the tool mux: a paused machine, one
// whose child.json does not load, and a tool left off the file's tools list
// are refused before the tool is reached, and every call is written to the
// audit log before it runs and again when it has ended. Confirm is the
// approval hook of every tool on the child: it applies the ceiling, and only
// then asks the master over the link.
type Gate struct {
	dir      string
	file     *ChildFile
	audit    *AuditLog
	commands func(action string) bool
	ask      func(ctx context.Context, action, detail string) bool // Confirm; tests replace it
}

// NewGate makes the gate of the child in dir, writing to audit. runsCommand
// tells, for an approval action a tool asks for, whether allowing it runs a
// command on the machine; it is asked under the no-exec ceiling, and nil
// means every action does. A nil audit log refuses every call: a child that
// cannot write its audit record does not run the call.
func NewGate(dir string, audit *AuditLog, runsCommand func(action string) bool) *Gate {
	return &Gate{dir: dir, file: OpenChild(ChildPath(dir)), audit: audit, commands: runsCommand, ask: Confirm}
}

// Current is the machine's child.json as it is on the disk now, as the gate
// reads it for every check.
func (g *Gate) Current() (Child, error) { return g.file.Current() }

// state is what the machine allows at this moment: its child.json, or, when
// it serves nothing, why in a few words and in a sentence.
func (g *Gate) state() (cur Child, why, text string) {
	if Paused(g.dir) {
		return Child{}, "paused", "fleet: this machine is paused on the machine itself (`tenant child resume` there)"
	}
	cur, err := g.file.Current()
	if err != nil {
		return Child{}, "child.json cannot be read", "fleet: this machine serves nothing while its child.json cannot be read: " + err.Error()
	}
	return cur, "", ""
}

type gateKey struct{}

// gatedCall collects what a call's tools asked for, for its audit record.
type gatedCall struct {
	mu       sync.Mutex
	asked    []AuditAsk
	more     int
	pictures int // images the tool handed to a model that is not here
}

func (c *gatedCall) picture() {
	c.mu.Lock()
	c.pictures++
	c.mu.Unlock()
}

// unsent says, after a tool's own words, that the pictures it made did not
// leave the machine. A tool hands a picture to the model beside its text
// (model.AttachImage) and words its text as if the model then saw it; on a
// child there is no model, and the link carries the text alone.
func (c *gatedCall) unsent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pictures == 0 {
		return ""
	}
	return fmt.Sprintf("\n[fleet: %d picture(s) this tool made stayed on this machine: the link to its master carries text only, so the model has not seen them]", c.pictures)
}

func (c *gatedCall) add(a AuditAsk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.asked) >= maxAuditAsks {
		c.more++
		return
	}
	c.asked = append(c.asked, a)
}

// taken is what was asked so far. A tool may still be asking from a
// goroutine it left behind.
func (c *gatedCall) taken() ([]AuditAsk, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked), c.more
}

// Confirm has the shape of the tools' approval hooks, and on a child it is
// the hook of every one of them. It refuses, without asking the master,
// whatever this machine rules out: everything under the read-only ceiling,
// everything that runs a command under no-exec, and everything while the
// machine is paused or its child.json cannot be read. What is left is the
// master's to decide (the package's Confirm).
func (g *Gate) Confirm(ctx context.Context, action, detail string) bool {
	ask := AuditAsk{Answer: AskRefused}
	ask.Action, _ = cut(action, maxToolLen)
	switch cur, why, _ := g.state(); {
	case why != "":
		ask.Why = why
	case cur.Ceiling == CeilingFull:
		ask.Answer = ""
	case cur.Ceiling == CeilingNoExec && g.commands != nil && !g.commands(action):
		ask.Answer = ""
	default:
		ask.Why = "ceiling " + string(cur.Ceiling)
	}
	if ask.Answer == "" {
		if ask.Answer = AskDenied; g.ask(ctx, action, detail) {
			ask.Answer = AskAllowed
		}
	}
	if c, _ := ctx.Value(gateKey{}).(*gatedCall); c != nil {
		c.add(ask)
	}
	return ask.Answer == AskAllowed
}

// Tools is t as the child serves it to its master: behind the gate.
func (g *Gate) Tools(t Tools) Tools { return gated{g, t} }

type gated struct {
	g *Gate
	t Tools
}

// All lists the tools this machine serves right now: none while it is paused
// or its child.json cannot be read, and otherwise those the file allows.
func (s gated) All() []model.ToolSpec {
	all := s.t.All()
	out := make([]model.ToolSpec, 0, len(all))
	cur, why, _ := s.g.state()
	if why != "" {
		return out
	}
	for _, spec := range all {
		if cur.Serves(spec.Name) {
			out = append(out, spec)
		}
	}
	return out
}

// Dispatch runs one call of the master's. The call is in the audit log
// before the tool is reached, and how it ended is there before its result is
// returned; the result carries the hash of that record.
func (s gated) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	g := s.g
	args, _ := cut(string(call.Arguments), maxAuditArgs)
	hash, err := g.record(AuditRecord{Event: AuditCall, Call: call.ID, Tool: call.Name,
		Args: args, ArgsSHA256: sha256Hex(call.Arguments)})
	if err != nil {
		return "fleet: this machine could not write its audit record, so it did not run the call: " + err.Error(), true, nil
	}
	setAudit(ctx, hash)

	done := AuditRecord{Event: AuditDone, Call: call.ID, Tool: call.Name, Outcome: AuditRefused}
	var text string
	switch cur, why, say := g.state(); {
	case why != "":
		done.Note, text = why, say
	case !cur.Serves(call.Name):
		done.Note = "not served"
		text = "fleet: this machine does not serve " + call.Name + ": its child.json lists the only tools it does"
	default:
		c := &gatedCall{}
		var failed bool
		tctx := model.WithImageSink(context.WithValue(ctx, gateKey{}, c), func([]byte, string) { c.picture() })
		text, failed = s.run(tctx, call)
		done.Outcome = AuditOK
		if failed {
			done.Outcome = AuditError
		}
		done.Asked, done.AskedMore = c.taken()
		text += refusals(done.Asked) + c.unsent()
	}
	done.ResultSHA256, done.ResultBytes = sha256Hex([]byte(text)), len(text)
	if hash, err := g.record(done); err == nil {
		setAudit(ctx, hash)
	} else {
		// The record of the call's arrival stands, and its hash goes with the
		// result. The next call is refused until the log can be written.
		text += "\n[fleet: this call ran, but how it ended could not be written to this machine's audit log: " + err.Error() + "]"
	}
	return text, done.Outcome != AuditOK, nil
}

// run reaches the tool. A tool that panics fails its call, and the call
// still gets its record.
func (s gated) run(ctx context.Context, call model.ToolCall) (text string, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			text, failed = fmt.Sprintf("fleet: the tool panicked: %v", r), true
		}
	}()
	text, failed, err := s.t.Dispatch(ctx, call)
	if err != nil {
		return err.Error(), true
	}
	return text, failed
}

func (g *Gate) record(rec AuditRecord) (string, error) {
	if g.audit == nil {
		return "", errors.New("fleet: this machine has no audit log")
	}
	return g.audit.append(rec)
}

// refusals says, after a tool's own words, which approvals this machine
// refused by itself. A tool only learns "no", and its master would otherwise
// read that as the operator's answer.
func refusals(asked []AuditAsk) string {
	var said []string
	for _, a := range asked {
		if line := a.Action + " (" + a.Why + ")"; a.Answer == AskRefused && !slices.Contains(said, line) {
			said = append(said, line)
		}
	}
	if len(said) == 0 {
		return ""
	}
	return "\n[fleet: this machine refused, without asking its master: " + strings.Join(said, ", ") + "]"
}

// setAudit leaves the hash of a call's audit record where the link picks it
// up for the call's result.
func setAudit(ctx context.Context, hash string) {
	if a, _ := ctx.Value(askKey{}).(*asker); a != nil {
		a.audit = hash
	}
}
