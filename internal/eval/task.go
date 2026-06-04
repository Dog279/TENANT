// Package eval is Tenant's eval harness. Phase 1 ships fixture-mode tasks
// that exercise the harness end-to-end without invoking an LLM — proving
// task loading, scoring, and reporting work before Phase 2 wires real
// agent runs and the LLM judge.
//
// See tasks/eval-harness-plan-v1.md for the full design. Job-callable
// fitness API (eval.Run) lands in Phase 5.
package eval

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode selects how a task is executed. Phase 1 supports fixture only;
// Phase 2 introduces live (agent-driven) mode.
type Mode string

const (
	ModeFixture Mode = "fixture" // pre-canned tool calls + response; scored deterministically
	ModeLive    Mode = "live"    // real agent run; scored by deterministic gate + LLM judge
)

// Subset names the curated task groupings. smoke is for CI fast-path
// (no LLM); fitness is the 10-task in-Job fitness signal; full is the
// nightly suite.
type Subset string

const (
	SubsetSmoke   Subset = "smoke"
	SubsetFitness Subset = "fitness"
	SubsetFull    Subset = "full"
)

// IsValid reports whether s is one of the recognized subset constants.
// Used by the CLI to reject typos at the arg-parse layer instead of
// silently returning an empty task list.
func (s Subset) IsValid() bool {
	switch s {
	case SubsetSmoke, SubsetFitness, SubsetFull:
		return true
	}
	return false
}

// Task is one eval case. YAML on disk → Task in memory. Unknown fields
// at load time are an error (catches typos that would silently drop
// data otherwise).
type Task struct {
	ID          string   `yaml:"id"`
	Category    string   `yaml:"category"`
	Subset      Subset   `yaml:"subset"`
	Description string   `yaml:"description"`
	Prompt      string   `yaml:"prompt"`
	Mode        Mode     `yaml:"mode"`
	Fixture     *Fixture `yaml:"fixture,omitempty"`
	Expected    Expected `yaml:"expected"`
	Rollouts    int      `yaml:"rollouts,omitempty"` // default 1 in Phase 1
	Weight      float64  `yaml:"weight,omitempty"`   // default 1.0

	// InjectedFacts / InjectedEpisodes pre-seed the agent's ephemeral memory
	// before a live run, so memory-recall tasks have something to retrieve.
	// The live factory embeds + writes these to the per-task stores. Optional.
	InjectedFacts    []InjectedFact    `yaml:"injected_facts,omitempty"`
	InjectedEpisodes []InjectedEpisode `yaml:"injected_episodes,omitempty"`

	// FilePath is populated by the loader for diagnostics.
	FilePath string `yaml:"-"`
}

// Fixture is a deterministic stand-in for a real agent run. The runner
// scores Expected against the fixture directly, no LLM involved. Used
// exclusively by smoke-subset tasks in Phase 1.
type Fixture struct {
	ToolCalls []FixtureToolCall `yaml:"tool_calls"`
	Response  string            `yaml:"response"`
}

// FixtureToolCall mirrors what an agent would emit for one call.
type FixtureToolCall struct {
	Tool string `yaml:"tool"`
	Args string `yaml:"args"` // JSON-encoded
}

// Expected captures the deterministic assertions a task makes against
// either a fixture (smoke) or a live agent run (later phases). The
// Rubric is consulted only after every deterministic check passes —
// failed gate ⇒ skip judge, save tokens.
type Expected struct {
	MustCall          []ExpectedToolCall `yaml:"must_call,omitempty"`
	MustNotCall       []string           `yaml:"must_not_call,omitempty"`
	ResponseSubstrAny []string           `yaml:"response_substring_any,omitempty"`
	Rubric            *Rubric            `yaml:"rubric,omitempty"`
	RubricMinScore    int                `yaml:"rubric_min_score,omitempty"`
}

// ExpectedToolCall asserts a tool ran with arguments containing every
// listed substring (after JSON normalization is intentionally NOT
// performed — substring match keeps the assertion forgiving to arg
// ordering and whitespace).
type ExpectedToolCall struct {
	Tool        string   `yaml:"tool"`
	ArgsContain []string `yaml:"args_contain,omitempty"`
}

// InjectedFact pre-seeds one semantic fact into a memory-recall task's
// ephemeral store before the live run. The factory embeds Text with the active
// embedder and stamps EmbedderID; Confidence defaults to 1.0; Source is
// provenance only.
type InjectedFact struct {
	Text       string  `yaml:"text"`
	Confidence float64 `yaml:"confidence,omitempty"`
	Source     string  `yaml:"source,omitempty"`
}

// InjectedEpisode pre-seeds one prior turn (prompt + response) into a
// memory-recall task's ephemeral episodic store before the live run, so
// retrieval can surface it as recent context.
type InjectedEpisode struct {
	Prompt   string   `yaml:"prompt"`
	Response string   `yaml:"response"`
	Tags     []string `yaml:"tags,omitempty"`
	Outcome  string   `yaml:"outcome,omitempty"`
}

// LoadTask parses one YAML blob into a Task and validates it. filePath
// is recorded for diagnostics; pass "" if synthetic.
func LoadTask(data []byte, filePath string) (*Task, error) {
	var t Task
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // unknown YAML fields error out
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", filePath, err)
	}
	t.FilePath = filePath
	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("eval: validate %s: %w", filePath, err)
	}
	return &t, nil
}

// LoadTasksFromFS walks an fs.FS for *.yaml under root, loads each into
// a Task. Errors on any single bad file (no silent skipping).
func LoadTasksFromFS(fsys fs.FS, root string) ([]*Task, error) {
	var out []*Task
	err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
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
		t, err := LoadTask(data, path)
		if err != nil {
			return err
		}
		out = append(out, t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// validate enforces task-level invariants. Defaults applied here so
// callers don't have to.
func (t *Task) validate() error {
	if t.ID == "" {
		return errors.New("task: id required")
	}
	if t.Category == "" {
		return errors.New("task: category required")
	}
	if t.Subset == "" {
		return errors.New("task: subset required (smoke|fitness|full)")
	}
	switch t.Subset {
	case SubsetSmoke, SubsetFitness, SubsetFull:
	default:
		return fmt.Errorf("task: invalid subset %q", t.Subset)
	}
	if t.Mode == "" {
		t.Mode = ModeFixture // safe default while live mode lands in Phase 2
	}
	switch t.Mode {
	case ModeFixture:
		if t.Fixture == nil {
			return errors.New("task: fixture mode requires fixture block")
		}
	case ModeLive:
		// Phase 2: validator accepts live mode. The runner enforces
		// "do we have an AgentFactory to drive this?" — separation of
		// concerns per Phase 1 eng review (finding 1C).
		if t.Expected.Rubric == nil {
			return errors.New("task: live mode requires expected.rubric (anchored)")
		}
		if t.Expected.RubricMinScore < 1 || t.Expected.RubricMinScore > 5 {
			return fmt.Errorf("task: rubric_min_score must be 1-5, got %d", t.Expected.RubricMinScore)
		}
	default:
		return fmt.Errorf("task: invalid mode %q", t.Mode)
	}
	if t.Prompt == "" {
		return errors.New("task: prompt required")
	}
	for i, f := range t.InjectedFacts {
		if f.Text == "" {
			return fmt.Errorf("task: injected_facts[%d] requires text", i)
		}
	}
	for i, ep := range t.InjectedEpisodes {
		if ep.Prompt == "" || ep.Response == "" {
			return fmt.Errorf("task: injected_episodes[%d] requires prompt and response", i)
		}
	}
	if t.Rollouts == 0 {
		t.Rollouts = 1 // smoke is deterministic; 1 rollout is enough
	}
	if t.Weight == 0 {
		t.Weight = 1.0
	}
	return nil
}

// FilenameID returns the task ID derived from its filename, useful when
// the YAML omits id (we error on that, so this is currently informational).
func FilenameID(filePath string) string {
	return strings.TrimSuffix(filepath.Base(filePath), ".yaml")
}
