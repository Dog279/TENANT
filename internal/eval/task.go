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
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode selects how a task is executed. Phase 1 supports fixture only;
// Phase 2 introduces live (agent-driven) mode.
type Mode string

const (
	ModeFixture         Mode = "fixture"          // pre-canned tool calls + response; scored deterministically
	ModeLive            Mode = "live"             // real agent run; scored by deterministic gate + LLM judge
	ModeRouteClassifier Mode = "route_classifier" // live prompt classifier; scored by numeric deterministic gates
)

// Subset names the curated task groupings. smoke is for CI fast-path
// (no LLM); fitness is the 10-task in-Job fitness signal; full is the
// nightly suite.
type Subset string

const (
	SubsetSmoke   Subset = "smoke"
	SubsetFitness Subset = "fitness"
	SubsetFull    Subset = "full"
	// SubsetUI checks the model answers with a component when the answer is one
	// (TEN-517); live, part of full.
	SubsetUI Subset = "ui"
	// SubsetVision needs a model that can see images (TEN-493), so it is
	// opt-in: full leaves it out.
	SubsetVision Subset = "vision"
)

// IsValid reports whether s is one of the recognized subset constants.
// Used by the CLI to reject typos at the arg-parse layer instead of
// silently returning an empty task list.
func (s Subset) IsValid() bool {
	switch s {
	case SubsetSmoke, SubsetFitness, SubsetFull, SubsetVision, SubsetUI:
		return true
	}
	return false
}

// Task is one eval case. YAML on disk → Task in memory. Unknown fields
// at load time are an error (catches typos that would silently drop
// data otherwise).
type Task struct {
	ID          string      `yaml:"id"`
	Category    string      `yaml:"category"`
	Subset      Subset      `yaml:"subset"`
	Description string      `yaml:"description"`
	Prompt      string      `yaml:"prompt"`
	Mode        Mode        `yaml:"mode"`
	Fixture     *Fixture    `yaml:"fixture,omitempty"`
	Expected    Expected    `yaml:"expected"`
	RouteCases  []RouteCase `yaml:"route_cases,omitempty"`
	Rollouts    int         `yaml:"rollouts,omitempty"` // default 1 in Phase 1
	Weight      float64     `yaml:"weight,omitempty"`   // default 1.0

	// InjectedFacts / InjectedEpisodes pre-seed the agent's ephemeral memory
	// before a live run, so memory-recall tasks have something to retrieve.
	// The live factory embeds + writes these to the per-task stores. Optional.
	InjectedFacts    []InjectedFact    `yaml:"injected_facts,omitempty"`
	InjectedEpisodes []InjectedEpisode `yaml:"injected_episodes,omitempty"`
	// InjectedWiki pre-writes a per-task markdown wiki corpus before a live
	// run so content-dependent wiki tasks (runbook lookups, cross-linked notes)
	// are self-contained instead of depending on the operator's real wiki. The
	// live factory writes these to an isolated temp dir + indexes it (TEN-220).
	InjectedWiki []InjectedWikiDoc `yaml:"injected_wiki,omitempty"`
	// ImageDigits, when set, sends a picture of these digits (seven-segment,
	// black on white) with the prompt (TEN-493). Live mode only.
	ImageDigits string `yaml:"image_digits,omitempty"`

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
	Tool   string `yaml:"tool"`
	Args   string `yaml:"args"`             // JSON-encoded
	Result string `yaml:"result,omitempty"` // what the tool answered, for result_contains
}

// RouteCase is one hand-labeled prompt in a route_classifier task. Name is
// optional diagnostic context; Label is deliberately limited to the only two
// actions the prompt router can select.
type RouteCase struct {
	Name   string `yaml:"name,omitempty"`
	Prompt string `yaml:"prompt"`
	Label  string `yaml:"label"`
}

// Expected captures the deterministic assertions a task makes against a
// fixture, live agent run, or route-classifier sample set. The
// Rubric is consulted only after every deterministic check passes —
// failed gate ⇒ skip judge, save tokens.
type Expected struct {
	MustCall          []ExpectedToolCall `yaml:"must_call,omitempty"`
	MustNotCall       []string           `yaml:"must_not_call,omitempty"`
	ResponseSubstrAny []string           `yaml:"response_substring_any,omitempty"`
	Rubric            *Rubric            `yaml:"rubric,omitempty"`
	RubricMinScore    int                `yaml:"rubric_min_score,omitempty"`

	// Deterministic route-classifier graduation thresholds. Latency is a
	// strict upper bound; rate minima are inclusive.
	ParseSuccessRateMin     float64 `yaml:"parse_success_rate_min,omitempty"`
	LabelAgreementRateMin   float64 `yaml:"label_agreement_rate_min,omitempty"`
	P95ClassifyLatencyMSMax int64   `yaml:"p95_classify_latency_ms_max,omitempty"`
}

// ExpectedToolCall asserts a tool ran with arguments containing every
// listed substring (after JSON normalization is intentionally NOT
// performed — substring match keeps the assertion forgiving to arg
// ordering and whitespace).
type ExpectedToolCall struct {
	Tool        string   `yaml:"tool"`
	ArgsContain []string `yaml:"args_contain,omitempty"`
	// ResultContains asserts the tool's answer carried every listed substring
	// (TEN-517: "shown: table" means the component validated).
	ResultContains []string `yaml:"result_contains,omitempty"`
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

// InjectedWikiDoc is one markdown file seeded into a task's ephemeral wiki
// before a live run. Path is the file's location relative to the temp wiki
// root (forward slashes, no ".." and no leading "/" — enforced in validate);
// Content is the raw markdown the wiki indexer embeds for retrieval (TEN-220).
type InjectedWikiDoc struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
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
		return errors.New("task: subset required (smoke|fitness|ui|full|vision)")
	}
	switch t.Subset {
	case SubsetSmoke, SubsetFitness, SubsetFull, SubsetVision, SubsetUI:
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
	case ModeRouteClassifier:
		if len(t.RouteCases) == 0 {
			return errors.New("task: route_classifier mode requires route_cases")
		}
		for i, c := range t.RouteCases {
			if strings.TrimSpace(c.Prompt) == "" {
				return fmt.Errorf("task: route_cases[%d] requires prompt", i)
			}
			if c.Label != "small" && c.Label != "big" {
				return fmt.Errorf("task: route_cases[%d] label %q must be small or big", i, c.Label)
			}
		}
		if min := t.Expected.ParseSuccessRateMin; min <= 0 || min > 1 {
			return fmt.Errorf("task: parse_success_rate_min must be in (0,1], got %g", min)
		}
		if min := t.Expected.LabelAgreementRateMin; min <= 0 || min > 1 {
			return fmt.Errorf("task: label_agreement_rate_min must be in (0,1], got %g", min)
		}
		if max := t.Expected.P95ClassifyLatencyMSMax; max <= 0 {
			return fmt.Errorf("task: p95_classify_latency_ms_max must be positive, got %d", max)
		}
	default:
		return fmt.Errorf("task: invalid mode %q", t.Mode)
	}
	if t.Mode != ModeRouteClassifier && t.Prompt == "" {
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
	for i, w := range t.InjectedWiki {
		if w.Path == "" || w.Content == "" {
			return fmt.Errorf("task: injected_wiki[%d] requires path and content", i)
		}
		if strings.Contains(w.Path, "..") || strings.HasPrefix(w.Path, "/") {
			return fmt.Errorf("task: injected_wiki[%d] path %q must be relative with no '..'", i, w.Path)
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
