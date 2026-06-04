package model_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tenant/internal/model"
)

func TestRegistry_LoadsEmbeddedDefaults(t *testing.T) {
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// At minimum, our shipped profiles must load.
	expectIDs := []string{"qwen3.6-72b", "qwen3.6-35b-a3b", "gemma4-70b", "qwen3-embedding-8b"}
	for _, id := range expectIDs {
		if _, ok := reg.ByID(id); !ok {
			t.Errorf("missing embedded profile %s; have %v", id, reg.IDs())
		}
	}
}

func TestRegistry_ByRole(t *testing.T) {
	reg, err := model.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	planners := reg.ByRole(model.RolePlanner)
	if len(planners) == 0 {
		t.Fatalf("ByRole(planner) returned empty; want >= 1 shipped planner")
	}
	embedders := reg.ByRole(model.RoleEmbedder)
	if len(embedders) == 0 {
		t.Fatalf("ByRole(embedder) returned empty; want >= 1 shipped embedder")
	}
}

func TestRegistry_DiskOverrideMergesByID(t *testing.T) {
	dir := t.TempDir()
	// New profile via disk — should appear in registry alongside embedded ones.
	custom := []byte(`
id: my-custom-planner
role: planner
backend: vllm
endpoint: http://mac-studio:8000
model: my/model
context_length: 64000
`)
	if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), custom, 0o644); err != nil {
		t.Fatalf("write custom yaml: %v", err)
	}
	reg, err := model.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, ok := reg.ByID("my-custom-planner"); !ok {
		t.Fatalf("custom profile not loaded; have %v", reg.IDs())
	}
}

func TestRegistry_DuplicateIDIsError(t *testing.T) {
	dir := t.TempDir()
	// Disk file with same ID as a shipped profile.
	dup := []byte(`
id: qwen3.6-72b
role: planner
backend: vllm
endpoint: http://other:8000
model: m
context_length: 128000
`)
	if err := os.WriteFile(filepath.Join(dir, "dup.yaml"), dup, 0o644); err != nil {
		t.Fatalf("write dup: %v", err)
	}
	_, err := model.NewRegistry(dir)
	if !errors.Is(err, model.ErrDuplicateProfile) {
		t.Fatalf("err = %v, want ErrDuplicateProfile", err)
	}
}

func TestRegistry_MissingUserDirIsOK(t *testing.T) {
	reg, err := model.NewRegistry(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if len(reg.IDs()) == 0 {
		t.Fatal("expected embedded profiles to still load")
	}
}
