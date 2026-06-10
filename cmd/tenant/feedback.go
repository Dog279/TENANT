package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"tenant/internal/memory/episodic"
)

// feedbackMark renders an episode's user-feedback as a single-glyph marker for
// list views (TEN-151 visibility): ✓ acked, ✗ undone, space = none (so columns
// stay aligned).
func feedbackMark(fb string) string {
	switch fb {
	case episodic.FeedbackAck:
		return "✓"
	case episodic.FeedbackUndo:
		return "✗"
	default:
		return " "
	}
}

// feedbackControl adapts the episodic store to tui.FeedbackControl so the TUI's
// `/ack` and `/undo` mark the operator's last turn (TEN-151).
type feedbackControl struct {
	es      *episodic.Store
	agentID string
}

func (f feedbackControl) Ack() (string, error)  { return f.set(episodic.FeedbackAck) }
func (f feedbackControl) Undo() (string, error) { return f.set(episodic.FeedbackUndo) }

func (f feedbackControl) set(fb string) (string, error) {
	if f.es == nil {
		return "", fmt.Errorf("episodic store unavailable")
	}
	ctx := context.Background()
	id, err := f.es.LatestID(ctx, f.agentID)
	if err != nil {
		return "", fmt.Errorf("no recent turn to mark: %w", err)
	}
	if err := f.es.SetUserFeedback(ctx, id, fb); err != nil {
		return "", err
	}
	return fmt.Sprintf("marked last turn (#%d) as %s", id, fb), nil
}

// cmdFeedback implements `tenant ack` / `tenant undo` (TEN-151): record the
// operator's satisfaction on the LAST agent turn. This is the real-world
// success signal that feeds skill induction (and the "trusted" auto-accept
// gate) + the eval. Operates on the most-recent episode for the agent.
func cmdFeedback(ctx context.Context, args []string, feedback string) error {
	fs := flag.NewFlagSet(feedback, flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	es, err := episodic.Open(filepath.Join(c.dataDir, "episodes.db"))
	if err != nil {
		return fmt.Errorf("open episodes db: %w", err)
	}
	defer es.Close()
	id, err := es.LatestID(ctx, c.agent)
	if err != nil {
		return fmt.Errorf("no recent turn to mark for agent %q: %w", c.agent, err)
	}
	if err := es.SetUserFeedback(ctx, id, feedback); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "marked episode %d as %q for agent %q\n", id, feedback, c.agent)
	return nil
}
