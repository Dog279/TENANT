package soul

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Proposal is one pending agent-proposed edit to a soul. Lives at
// {baseDir}/soul/proposed/{agent_id}-{timestamp}-{slug}.toml until
// the user accepts (moves to live soul) or rejects (deletes).
//
// Auto-apply is deliberately not implemented. Soul changes affect
// every future turn; silent self-modification is how products quietly
// drift in directions users didn't intend. The propose-edit flow keeps
// improvement loud and reversible.
type Proposal struct {
	ID         string    `toml:"-"` // derived from filename, not stored in TOML
	AgentID    string    `toml:"agent_id"`
	ProposedAt time.Time `toml:"proposed_at"`
	Reason     string    `toml:"reason"` // why the agent proposed this edit
	Soul       Soul      `toml:"soul"`   // full replacement soul; we don't do partial diffs in v1
}

// ProposeEdit writes a new candidate Soul to the proposed/ directory.
// Returns the proposal ID (filename without extension) so the caller
// can show it to the user.
func ProposeEdit(baseDir, agentID, reason string, soul *Soul) (string, error) {
	if agentID == "" {
		return "", errors.New("soul: ProposeEdit requires agentID")
	}
	if soul == nil {
		return "", errors.New("soul: ProposeEdit requires non-nil soul")
	}
	dir := proposedDir(baseDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("soul: mkdir %s: %w", dir, err)
	}
	ts := time.Now().UTC()
	// Nanosecond precision so two proposals for the same agent+reason within the
	// same second don't collide (and silently overwrite) on the filename.
	id := fmt.Sprintf("%s-%s-%s", agentID, ts.Format("20060102T150405.000000000Z"), slugify(reason))
	path := filepath.Join(dir, id+".toml")

	p := Proposal{
		AgentID:    agentID,
		ProposedAt: ts,
		Reason:     reason,
		Soul:       *soul,
	}
	data, err := toml.Marshal(&p)
	if err != nil {
		return "", fmt.Errorf("soul: marshal proposal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("soul: write proposal: %w", err)
	}
	return id, nil
}

// ListProposals returns all pending proposals for agentID, oldest first.
// Returns an empty slice (not error) if the proposed/ directory does
// not exist yet.
func ListProposals(baseDir, agentID string) ([]Proposal, error) {
	dir := proposedDir(baseDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("soul: read %s: %w", dir, err)
	}
	prefix := agentID + "-"
	var out []Proposal
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("soul: read %s: %w", full, err)
		}
		var p Proposal
		if err := toml.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("soul: parse %s: %w", full, err)
		}
		p.ID = strings.TrimSuffix(e.Name(), ".toml")
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProposedAt.Before(out[j].ProposedAt) })
	return out, nil
}

// Accept applies a proposal by saving its Soul as the live soul for
// the agent and deleting the proposal file. Atomic from the user's
// perspective: either both happen or the proposal file stays put.
func Accept(baseDir, proposalID string) error {
	path := filepath.Join(proposedDir(baseDir), proposalID+".toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("soul: accept read %s: %w", path, err)
	}
	var p Proposal
	if err := toml.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("soul: accept parse: %w", err)
	}
	if p.Soul.Agent.ID == "" {
		p.Soul.Agent.ID = p.AgentID
	}
	if err := p.Soul.Save(baseDir); err != nil {
		return fmt.Errorf("soul: accept save: %w", err)
	}
	if err := os.Remove(path); err != nil {
		// Live soul is saved; failure to remove the proposal is a
		// disk-state cleanup issue, not a correctness issue. Surface
		// it so the user can clean up manually.
		return fmt.Errorf("soul: accept (soul saved) but proposal cleanup failed: %w", err)
	}
	return nil
}

// Reject deletes a proposal without applying it.
func Reject(baseDir, proposalID string) error {
	path := filepath.Join(proposedDir(baseDir), proposalID+".toml")
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("soul: reject: %w", err)
	}
	return nil
}

// slugify produces a filename-safe lowercase slug from arbitrary text.
// Used to make proposal filenames human-readable in directory listings.
func slugify(s string) string {
	if s == "" {
		return "edit"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevDash = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
		if b.Len() >= 32 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "edit"
	}
	return out
}
