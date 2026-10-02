package main

// interrupted.go (TEN-499): the record of the turns a stop cut short, so the
// next start can tell the operator what was lost. A relay whose turn ends
// with its context cancelled while Tenant is stopping appends one entry to
// <data>/interrupted.json (temp + rename, 0600) and tells the operator at
// once, best effort; serve reads the file after its relays are up, posts one
// notice per entry to the channel the turn came from, and removes what it
// could post. The turn is never re-run: the tools that ran before the cut may
// have had side effects (an email sent, a file edited), and the archive holds
// the partial trace, so "continue" has the context.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tenant/internal/agent"
)

// stopNotice is what the operator sees when a stop cuts their turn.
const stopNotice = "I was stopped mid-task by a system shutdown or restart. Say `continue` when I'm back."

// interruptedTurn is one cut turn.
type interruptedTurn struct {
	Surface   string    `json:"surface"`    // discord | imessage
	Channel   string    `json:"channel"`    // the Discord channel or the iMessage chat
	InboundID string    `json:"inbound_id"` // the message that started the turn
	Excerpt   string    `json:"excerpt"`    // the first 120 characters of what the user asked
	ToolsRun  int       `json:"tools_run"`
	At        time.Time `json:"at"`
}

// sendFunc posts text to a channel of one surface.
type sendFunc = func(ctx context.Context, channel, text string) error

var interruptedMu sync.Mutex // the read-modify-write of the file: both relays may be cut at once

func interruptedPath(dataDir string) string { return filepath.Join(dataDir, "interrupted.json") }

// newInterrupted describes a cut turn: res is nil when the runner returned
// no result.
func newInterrupted(surface, channel, inboundID, text string, res *agent.TurnResult) interruptedTurn {
	if r := []rune(text); len(r) > 120 {
		text = string(r[:120])
	}
	e := interruptedTurn{Surface: surface, Channel: channel, InboundID: inboundID, Excerpt: text, At: time.Now().UTC()}
	if res != nil {
		e.ToolsRun = len(res.ToolTrace)
	}
	return e
}

// recordInterrupted appends e to the file. No data dir (tests) records nothing.
func recordInterrupted(dataDir string, e interruptedTurn) error {
	if dataDir == "" {
		return nil
	}
	interruptedMu.Lock()
	defer interruptedMu.Unlock()
	entries, err := readInterrupted(dataDir)
	if err != nil {
		entries = nil // a corrupt file is replaced; the archive still has the trace
	}
	return writeInterrupted(dataDir, append(entries, e))
}

// readInterrupted reads the file; none is no entries.
func readInterrupted(dataDir string) ([]interruptedTurn, error) {
	b, err := os.ReadFile(interruptedPath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []interruptedTurn
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", interruptedPath(dataDir), err)
	}
	return entries, nil
}

// writeInterrupted replaces the file atomically; no entries removes it.
func writeInterrupted(dataDir string, entries []interruptedTurn) error {
	if len(entries) == 0 {
		err := os.Remove(interruptedPath(dataDir))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(interruptedPath(dataDir), b)
}

// resumeInterrupted posts one notice per recorded cut turn through the
// surface's send path and drops what it could post. An entry whose surface
// has no running relay, or whose notice did not go out, is reported and kept
// for the next start. Call it once the relays are up.
func resumeInterrupted(dataDir string, send map[string]sendFunc, notify func(string)) {
	interruptedMu.Lock()
	defer interruptedMu.Unlock()
	entries, err := readInterrupted(dataDir)
	if err != nil {
		notify("interrupted turns: " + err.Error() + " — the record is dropped; the archive has the turns")
		_ = os.Remove(interruptedPath(dataDir))
		return
	}
	var kept []interruptedTurn
	for _, e := range entries {
		text := fmt.Sprintf("I was stopped at %s while %q (%d tool calls had run). Tell me to continue.",
			e.At.Local().Format("15:04 on Jan 2"), e.Excerpt, e.ToolsRun)
		var serr error
		if fn := send[e.Surface]; fn == nil {
			serr = errors.New("no " + e.Surface + " relay")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			serr = fn(ctx, e.Channel, text)
			cancel()
		}
		if serr != nil {
			kept = append(kept, e)
			notify(fmt.Sprintf("interrupted turn from %s: not reported (%v); kept for the next start", e.Surface, serr))
			continue
		}
		notify(fmt.Sprintf("interrupted turn from %s: the operator was told to continue it", e.Surface))
	}
	if err := writeInterrupted(dataDir, kept); err != nil {
		notify("interrupted turns: " + err.Error())
	}
}
