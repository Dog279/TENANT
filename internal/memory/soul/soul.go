// Package soul implements T0 of Tenant's memory architecture: the
// agent's identity, persona, persistent user facts, and operating
// instructions. Souls are TOML files at ~/.config/tenant/soul/{agent}.toml
// (or the OS equivalent). They are hand-curated by default; agents may
// propose edits via ProposeEdit, which lands in a review queue and
// requires explicit human acceptance — per the design decision that
// soul changes are NEVER auto-applied for local-model deployments.
package soul

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Soul is the persistent identity payload always-included in the
// system prompt. Sized to live inside Profile.ReserveSoul tokens.
type Soul struct {
	Agent        Agent        `toml:"agent"`
	Values       Values       `toml:"values"`
	User         User         `toml:"user"`
	Instructions Instructions `toml:"instructions"`
	Meta         Meta         `toml:"meta"`
}

// Agent block: who this assistant is.
type Agent struct {
	ID    string `toml:"id"`
	Name  string `toml:"name"`
	Role  string `toml:"role"`
	Voice string `toml:"voice"`
}

// Values block: what the assistant cares about.
type Values struct {
	Items []string `toml:"items"`
}

// User block: persistent facts about the human the agent serves.
// Grows over time but is human-reviewed via ProposeEdit, never silently
// mutated by the model.
type User struct {
	Name        string   `toml:"name"`
	Timezone    string   `toml:"timezone,omitempty"`
	Preferences []string `toml:"preferences,omitempty"`
	Facts       []string `toml:"facts,omitempty"`
}

// Instructions block: operating rules the agent follows on every turn.
type Instructions struct {
	Items []string `toml:"items"`
}

// Meta block: provenance + versioning. Updated automatically on Save.
type Meta struct {
	CreatedAt time.Time `toml:"created_at"`
	UpdatedAt time.Time `toml:"updated_at"`
	Version   int       `toml:"version"`
}

// ErrNotFound means no soul TOML exists for the requested agent ID.
var ErrNotFound = errors.New("soul: not found")

// DefaultDir returns the OS-appropriate base directory for soul files,
// without the trailing /soul segment. Use SoulDir(baseDir) to attach it.
func DefaultDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "tenant"), nil
}

// SoulDir returns the soul directory under baseDir (e.g. tenant root).
func SoulDir(baseDir string) string {
	return filepath.Join(baseDir, "soul")
}

// proposedDir is where agent-proposed edits land for review.
func proposedDir(baseDir string) string {
	return filepath.Join(baseDir, "soul", "proposed")
}

// Path returns the full file path for a given agent's soul under baseDir.
func Path(baseDir, agentID string) string {
	return filepath.Join(SoulDir(baseDir), agentID+".toml")
}

// Load reads agent's soul TOML. Returns ErrNotFound if the file doesn't
// exist — callers can fall back to NewDefault for first-run cases.
func Load(baseDir, agentID string) (*Soul, error) {
	p := Path(baseDir, agentID)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		return nil, fmt.Errorf("soul: read %s: %w", p, err)
	}
	var s Soul
	if err := toml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("soul: parse %s: %w", p, err)
	}
	if s.Agent.ID == "" {
		s.Agent.ID = agentID // recover ID from filename if absent
	}
	return &s, nil
}

// Save writes the soul atomically (write to tmp, fsync, rename) so a
// crashed write never leaves a half-baked TOML in place. Updates
// Meta.UpdatedAt and bumps Version on every save.
func (s *Soul) Save(baseDir string) error {
	if s.Agent.ID == "" {
		return errors.New("soul: cannot save without Agent.ID")
	}
	now := time.Now().UTC()
	if s.Meta.CreatedAt.IsZero() {
		s.Meta.CreatedAt = now
	}
	s.Meta.UpdatedAt = now
	s.Meta.Version++

	dir := SoulDir(baseDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("soul: mkdir %s: %w", dir, err)
	}
	final := Path(baseDir, s.Agent.ID)
	tmp, err := os.CreateTemp(dir, ".soul-*.tmp")
	if err != nil {
		return fmt.Errorf("soul: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	enc := toml.NewEncoder(tmp)
	enc.SetIndentTables(true)
	if err := enc.Encode(s); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("soul: encode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("soul: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("soul: close tmp: %w", err)
	}
	// Windows os.Rename can't overwrite — remove the existing target
	// first. POSIX rename is atomic and replaces in place; the explicit
	// remove on Windows opens a small window where the file is absent.
	// Acceptable for personal-tier use; if we ever need crash-safe
	// soul replacement on Windows, move to a transactional file API.
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(final); err == nil {
			_ = os.Remove(final)
		}
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("soul: rename: %w", err)
	}
	return nil
}

// NewDefault returns a minimal soul suitable as a first-run scaffold
// for agentID. Caller should Save it (and let the user edit in place
// or via the propose-edit flow).
func NewDefault(agentID string) *Soul {
	return &Soul{
		Agent: Agent{
			ID:    agentID,
			Name:  "Tenant",
			Role:  "personal assistant",
			Voice: "concise, direct, technical when appropriate, plain when not",
		},
		Values: Values{
			Items: []string{
				"Honesty over flattery — say when something is wrong, hard, or unknown.",
				"User privacy — never share or transmit personal data without explicit user action.",
				"Useful over impressive — ship the right answer, not the long one.",
			},
		},
		Instructions: Instructions{
			Items: []string{
				"Default to concise responses unless the user asks for detail.",
				"When you use a tool, explain in one sentence why before the call.",
				"Cite sources when stating facts you retrieved.",
				"If a request is ambiguous, ask one clarifying question rather than guessing.",
				"Delegate to your built-in specialist sub-agents to keep your own context clean — Programmer (code), Researcher (sourced research), Writer (docs and prose), QA (adversarial verification), and Strategist (scoping and judgment) — giving each the goal and its definition of done, then verifying what they return.",
			},
		},
	}
}

// Render formats the soul into a markdown-style system-prompt fragment.
// The format is intentionally LLM-family-agnostic — markdown headers
// and bullet lists parse cleanly under every chat template we ship.
//
// Token cost is the caller's concern (use LLM.TokenCount on the result
// and check against Profile.ReserveSoul before injection). Render does
// not truncate — it emits what's there. Truncation is the assembler's
// job and surfaces a visible warning when it fires.
func (s *Soul) Render() string {
	var b strings.Builder

	if s.Agent.Name != "" || s.Agent.Role != "" || s.Agent.Voice != "" {
		b.WriteString("# About You\n")
		if s.Agent.Name != "" {
			fmt.Fprintf(&b, "You are %s", s.Agent.Name)
			if s.Agent.Role != "" {
				fmt.Fprintf(&b, ", %s", s.Agent.Role)
			}
			b.WriteString(".\n")
		} else if s.Agent.Role != "" {
			fmt.Fprintf(&b, "You are a %s.\n", s.Agent.Role)
		}
		if s.Agent.Voice != "" {
			fmt.Fprintf(&b, "Voice: %s.\n", s.Agent.Voice)
		}
		b.WriteString("\n")
	}

	if len(s.Values.Items) > 0 {
		b.WriteString("# Your Values\n")
		for _, v := range s.Values.Items {
			fmt.Fprintf(&b, "- %s\n", v)
		}
		b.WriteString("\n")
	}

	if s.User.Name != "" || s.User.Timezone != "" || len(s.User.Preferences) > 0 || len(s.User.Facts) > 0 {
		b.WriteString("# About the User\n")
		if s.User.Name != "" {
			fmt.Fprintf(&b, "- Name: %s\n", s.User.Name)
		}
		if s.User.Timezone != "" {
			fmt.Fprintf(&b, "- Timezone: %s\n", s.User.Timezone)
		}
		for _, p := range s.User.Preferences {
			fmt.Fprintf(&b, "- Preference: %s\n", p)
		}
		for _, f := range s.User.Facts {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}

	if len(s.Instructions.Items) > 0 {
		b.WriteString("# Operating Instructions\n")
		for _, i := range s.Instructions.Items {
			fmt.Fprintf(&b, "- %s\n", i)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
