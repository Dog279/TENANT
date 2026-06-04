package model

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tenant/internal/model/profiles"
)

// Registry holds the merged set of Profiles available at runtime.
// Construction order: shipped defaults from go:embed first, then user
// overrides from disk. Duplicate IDs are a startup error — silent
// shadowing would let a typo in a user file replace a vetted default
// without warning.
type Registry struct {
	byID   map[string]Profile
	byRole map[Role][]Profile
}

// NewEmptyRegistry returns a registry with no profiles. Use Add to
// populate it programmatically — useful for the echo dev backend,
// where profiles are built in code rather than loaded from embedded
// YAML (which is vLLM-only by design).
func NewEmptyRegistry() *Registry {
	return &Registry{
		byID:   make(map[string]Profile),
		byRole: make(map[Role][]Profile),
	}
}

// Add registers a profile after validating it. Duplicate IDs error
// (same semantics as the embedded/disk load path).
func (r *Registry) Add(p Profile) error {
	if err := p.validate(); err != nil {
		return err
	}
	return r.add(p)
}

// NewRegistry loads shipped defaults plus optional user overrides.
// userDir may be empty to skip disk loading (useful for tests).
func NewRegistry(userDir string) (*Registry, error) {
	r := &Registry{
		byID:   make(map[string]Profile),
		byRole: make(map[Role][]Profile),
	}
	if err := r.loadFS(profiles.Embedded, "."); err != nil {
		return nil, fmt.Errorf("loading embedded profiles: %w", err)
	}
	if userDir != "" {
		if err := r.loadDisk(userDir); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("loading user profiles from %s: %w", userDir, err)
			}
		}
	}
	return r, nil
}

// loadFS walks an fs.FS for *.yaml profile files.
func (r *Registry) loadFS(fsys fs.FS, root string) error {
	return fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		p, err := LoadProfileYAML(data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return r.add(p)
	})
}

// loadDisk walks userDir for *.yaml files. Returns fs.ErrNotExist if
// userDir doesn't exist (caller treats that as "no overrides", not error).
func (r *Registry) loadDisk(userDir string) error {
	entries, err := os.ReadDir(userDir)
	if err != nil {
		return err
	}
	// Deterministic load order for reproducible "duplicate ID" diagnostics.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		full := filepath.Join(userDir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("read %s: %w", full, err)
		}
		p, err := LoadProfileYAML(data)
		if err != nil {
			return fmt.Errorf("%s: %w", full, err)
		}
		if err := r.add(p); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) add(p Profile) error {
	if _, exists := r.byID[p.ID]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateProfile, p.ID)
	}
	r.byID[p.ID] = p
	r.byRole[p.Role] = append(r.byRole[p.Role], p)
	return nil
}

// Upsert adds a profile or replaces an existing one with the same ID
// (validating first). Used for live model swaps where the same profile ID
// (e.g. "vllm-planner") is re-pointed at a new endpoint/model. NOT safe for
// concurrent use on its own — the Router serializes it under its lock.
func (r *Registry) Upsert(p Profile) error {
	if err := p.validate(); err != nil {
		return err
	}
	if old, exists := r.byID[p.ID]; exists {
		// Drop the stale entry from its role bucket before re-adding.
		bucket := r.byRole[old.Role]
		for i, q := range bucket {
			if q.ID == p.ID {
				r.byRole[old.Role] = append(bucket[:i], bucket[i+1:]...)
				break
			}
		}
		delete(r.byID, p.ID)
	}
	return r.add(p)
}

// ByID returns the profile with the given ID, or false.
func (r *Registry) ByID(id string) (Profile, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// ByRole returns all profiles registered for the given role. Order
// matches load order (embedded first, then disk alphabetical).
func (r *Registry) ByRole(role Role) []Profile {
	return r.byRole[role]
}

// IDs returns all registered profile IDs sorted, for diagnostics.
func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
