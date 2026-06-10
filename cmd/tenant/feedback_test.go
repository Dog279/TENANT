package main

import (
	"context"
	"path/filepath"
	"testing"

	"tenant/internal/memory/episodic"
)

func TestFeedbackControl_PersistsAckUndo(t *testing.T) {
	ctx := context.Background()
	es, err := episodic.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	fc := feedbackControl{es: es, agentID: "main"}

	// No turns yet → error, no panic.
	if _, err := fc.Ack(); err == nil {
		t.Error("ack with no recent turn should error")
	}

	if _, err := es.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "p", Response: "r", EmbedderID: "x", Embedding: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := fc.Ack(); err != nil {
		t.Fatal(err)
	}
	eps, _ := es.Recent(ctx, "main", 1)
	if len(eps) != 1 || eps[0].UserFeedback != episodic.FeedbackAck {
		t.Fatalf("ack not persisted: %+v", eps)
	}

	if _, err := fc.Undo(); err != nil {
		t.Fatal(err)
	}
	eps, _ = es.Recent(ctx, "main", 1)
	if eps[0].UserFeedback != episodic.FeedbackUndo {
		t.Fatalf("undo did not overwrite: %q", eps[0].UserFeedback)
	}
}

func TestSkillControl_AutoAcceptPersist(t *testing.T) {
	dir := t.TempDir()
	if err := (&launchConfig{Provider: "echo"}).save(dir); err != nil {
		t.Fatal(err)
	}
	sc := skillControl{cfgDir: dir}

	if got := sc.AutoAcceptMode(); got != "off" {
		t.Fatalf("default mode=%q want off", got)
	}
	if err := sc.SetAutoAccept("trusted"); err != nil {
		t.Fatal(err)
	}
	if got := sc.AutoAcceptMode(); got != "trusted" {
		t.Fatalf("mode=%q want trusted", got)
	}
	lc, _ := loadLaunchConfig(dir)
	if lc.Improve.AutoAccept != "trusted" {
		t.Errorf("not persisted to config: %q", lc.Improve.AutoAccept)
	}
	// off is stored as empty (default) but reads back as "off".
	if err := sc.SetAutoAccept("off"); err != nil {
		t.Fatal(err)
	}
	if got := sc.AutoAcceptMode(); got != "off" {
		t.Errorf("mode=%q want off", got)
	}
	if err := sc.SetAutoAccept("bogus"); err == nil {
		t.Error("invalid mode should error")
	}
	// No cfgDir → toggle disabled (used by the seed-only construction).
	if err := (skillControl{}).SetAutoAccept("on"); err == nil {
		t.Error("empty cfgDir should refuse the toggle")
	}
	if got := (skillControl{}).AutoAcceptMode(); got != "off" {
		t.Errorf("empty cfgDir mode=%q want off", got)
	}
}
