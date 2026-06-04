// Package research is the persistence layer for /research runs (Phase C3).
//
// Each run lives in its own directory under <data>/research/<id>/, with
// human-readable JSON + markdown files — operators can `cat` or grep without
// any tool. No SQLite, no schema migration, no CGO. The store is intentionally
// dumb: a Run is "open" once Create returns, gets findings + events appended
// during the pass, then Finalize seals it with the final report + status.
//
// Concurrency: writes during one run all serialize through a per-run mutex
// (subagents fan out, but the orchestrator collects). The store-level mutex
// guards the runs map only; per-run state lives inside the Run handle.
//
// Failure model: storage is best-effort. A write failure must NEVER kill a
// research pass — the report itself is the user's deliverable, and a missing
// audit trail is an annoyance, not a corruption. Callers log + continue.
package research

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status of a Run. Status flips from "running" to one of the terminal values
// at Finalize. "partial" covers the wave-timeout / loop-ceiling-with-salvage
// case where we got SOME findings but didn't synthesize a clean report.
type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusError   Status = "error"
	StatusPartial Status = "partial" // some findings, but no clean synthesis
)

// Finding is one sub-agent's contribution to a run. Status mirrors the team
// runtime: done / error / running (running only persists if Finalize ran before
// the agent — should be rare; treated as incomplete).
type Finding struct {
	AgentID  string    `json:"agent_id"`
	Role     string    `json:"role"`
	Task     string    `json:"task"`
	Status   string    `json:"status"`
	Result   string    `json:"-"` // body lives in findings/<agent>.md
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
}

// Event is one observable thing that happened during the run (tool call,
// tool result, error, final). Stored append-only in events.jsonl so a debug
// run can be replayed step-by-step. Fields are deliberately loose — this is
// a debug log, not an API.
type Event struct {
	Timestamp time.Time `json:"ts"`
	AgentID   string    `json:"agent"`
	Kind      string    `json:"kind"` // "tool_call" | "tool_result" | "error" | "final" | "truncated"
	Tool      string    `json:"tool,omitempty"`
	Args      string    `json:"args,omitempty"`
	Result    string    `json:"result,omitempty"`
	IsErr     bool      `json:"is_err,omitempty"`
	Iter      int       `json:"iter,omitempty"`
}

// Manifest is the JSON shape persisted as run.json. Top-level metadata about
// the run, lightweight enough to load every entry for List() without reading
// any of the per-finding files.
type Manifest struct {
	ID            string        `json:"id"`
	Question      string        `json:"question"`
	Model         string        `json:"model,omitempty"`
	Backend       string        `json:"backend,omitempty"`
	Depth         int           `json:"depth"`
	MaxAgents     int           `json:"max_agents"`
	Parallel      int           `json:"parallel"`
	AwaitTimeout  time.Duration `json:"await_timeout"`
	MaxTime       time.Duration `json:"max_time"`
	Status        Status        `json:"status"`
	Started       time.Time     `json:"started"`
	Finished      time.Time     `json:"finished,omitzero"`
	Cycles        int           `json:"cycles"`
	Findings      []Finding     `json:"findings"`
	References    []string      `json:"references,omitempty"` // global URI list, in [n] order
	ReplayOf      string        `json:"replay_of,omitempty"`
	ErrorMessage  string        `json:"error,omitempty"`
	ReportPreview string        `json:"report_preview,omitempty"` // first ~280 chars of report.md, for list display
}

// Store manages on-disk runs. Open it once at startup and share — every
// method is safe for concurrent use across goroutines.
type Store struct {
	dir string
	mu  sync.Mutex // protects the on-disk index of in-flight Run handles
	// open: in-flight Runs by id, so a sub-agent observer callback can find
	// the right Run to append to without the caller threading it through
	// every layer.
	open map[string]*Run
}

// Run is the live handle for one in-progress (or just-finalized) research
// pass. Methods serialize through mu so concurrent sub-agent finalization
// can't tear the manifest. Once Finalize returns, further AppendFinding /
// AppendEvent calls are silently no-ops (defensive — late events from a
// cancelled agent should never crash).
type Run struct {
	store     *Store
	dir       string
	mu        sync.Mutex
	manifest  Manifest
	eventsFh  *os.File // events.jsonl handle, opened on first event
	finalized bool
}

// New opens (creating if missing) a research store at <dataDir>/research.
// Returns an error only on a hard filesystem failure — a missing directory
// is created, not an error.
func New(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("research: empty data dir")
	}
	dir := filepath.Join(dataDir, "research")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("research: mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir, open: map[string]*Run{}}, nil
}

// Dir is the root research directory. Tests + operators.
func (s *Store) Dir() string { return s.dir }

// makeID derives a sortable, human-readable run id:
// YYYYMMDD-HHMMSS-<question-slug>. Sortable because ASCII lex order matches
// chronological; readable because the slug tells you what it was at a glance.
// Collisions across sub-second runs of the same question are vanishingly
// rare; if they happen, we append "-2", "-3" etc.
func (s *Store) makeID(question string) string {
	now := time.Now().UTC()
	stamp := now.Format("20060102-150405")
	slug := slugify(question)
	if slug == "" {
		slug = "topic"
	}
	base := stamp + "-" + slug
	candidate := base
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(s.dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, n)
		if n > 50 {
			// pathological — fall back to nanosecond suffix
			return fmt.Sprintf("%s-%d", base, now.UnixNano())
		}
	}
}

// slugify keeps the id readable: lowercase, hyphenated, up to 6 words.
// Diverges from researchFilename in cmd/tenant only in word cap (here we
// trade a bit of length for chronological precision in the prefix).
func slugify(s string) string {
	var parts []string
	for _, w := range strings.Fields(strings.ToLower(s)) {
		clean := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return -1
		}, w)
		if clean != "" {
			parts = append(parts, clean)
		}
		if len(parts) >= 6 {
			break
		}
	}
	return strings.Join(parts, "-")
}

// Create allocates a new run, writes its initial manifest, and registers it
// with the store. The returned Run is live — call AppendFinding /
// AppendEvent / Finalize on it. Initial manifest is also persisted so a
// crash mid-run leaves a tombstone the operator can see in List().
func (s *Store) Create(m Manifest) (*Run, error) {
	if m.ID == "" {
		m.ID = s.makeID(m.Question)
	}
	if m.Started.IsZero() {
		m.Started = time.Now().UTC()
	}
	m.Status = StatusRunning
	dir := filepath.Join(s.dir, m.ID)
	if err := os.MkdirAll(filepath.Join(dir, "findings"), 0o755); err != nil {
		return nil, fmt.Errorf("research: mkdir run %s: %w", m.ID, err)
	}
	r := &Run{store: s, dir: dir, manifest: m}
	if err := r.writeManifest(); err != nil {
		_ = os.RemoveAll(dir) // don't leave a half-created run on disk
		return nil, err
	}
	s.mu.Lock()
	s.open[m.ID] = r
	s.mu.Unlock()
	return r, nil
}

// ID exposes the manifest id of a Run.
func (r *Run) ID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manifest.ID
}

// Dir exposes the on-disk directory.
func (r *Run) Dir() string { return r.dir }

// AppendFinding records one sub-agent's contribution: writes the body to
// findings/<agent-id>.md and appends a manifest entry. Replaces any prior
// finding for the same agent id (so a re-spawn — currently impossible —
// would overwrite, not duplicate).
func (r *Run) AppendFinding(f Finding) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return nil // late writes after Finalize are silently dropped
	}
	if strings.TrimSpace(f.AgentID) == "" {
		return fmt.Errorf("research: AppendFinding: empty agent id")
	}
	// Write the body file first; on failure, don't update the manifest.
	bodyPath := filepath.Join(r.dir, "findings", safeFilename(f.AgentID)+".md")
	if err := os.WriteFile(bodyPath, []byte(f.Result), 0o644); err != nil {
		return fmt.Errorf("research: write finding %s: %w", f.AgentID, err)
	}
	// Replace by agent id, or append.
	replaced := false
	for i, existing := range r.manifest.Findings {
		if existing.AgentID == f.AgentID {
			r.manifest.Findings[i] = f
			replaced = true
			break
		}
	}
	if !replaced {
		r.manifest.Findings = append(r.manifest.Findings, f)
	}
	return r.writeManifestLocked()
}

// AppendEvent appends one observability record to events.jsonl. Lazily opens
// the file on first call — many runs in tests don't emit events. Errors are
// swallowed except for the very first open (so the caller knows storage is
// broken); subsequent write errors are dropped — debug logs must never break
// the parent run.
func (r *Run) AppendEvent(e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return nil
	}
	if r.eventsFh == nil {
		fh, err := os.OpenFile(filepath.Join(r.dir, "events.jsonl"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("research: open events.jsonl: %w", err)
		}
		r.eventsFh = fh
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil // unmarshalable event = log noise, ignore
	}
	_, _ = r.eventsFh.Write(append(b, '\n'))
	return nil
}

// Finalize seals the run: writes report.md, updates the manifest's status +
// finished + reference list + report preview, closes the events handle, and
// removes the run from the store's open map. Idempotent — a second call is
// a no-op.
func (r *Run) Finalize(report string, references []string, status Status, errMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return nil
	}
	r.finalized = true
	if status == "" {
		status = StatusDone
	}
	r.manifest.Status = status
	r.manifest.Finished = time.Now().UTC()
	r.manifest.References = references
	r.manifest.ErrorMessage = errMsg
	preview := strings.TrimSpace(report)
	if len(preview) > 280 {
		preview = preview[:280] + "…"
	}
	r.manifest.ReportPreview = preview
	if report != "" {
		if err := os.WriteFile(filepath.Join(r.dir, "report.md"), []byte(report), 0o644); err != nil {
			return fmt.Errorf("research: write report.md: %w", err)
		}
	}
	if r.eventsFh != nil {
		_ = r.eventsFh.Close()
		r.eventsFh = nil
	}
	if err := r.writeManifestLocked(); err != nil {
		return err
	}
	r.store.mu.Lock()
	delete(r.store.open, r.manifest.ID)
	r.store.mu.Unlock()
	return nil
}

// Close releases any held file handles WITHOUT writing report.md or flipping
// status — for callers that bail out before they can synthesize (e.g. an
// orchestrator-level panic). Idempotent. After Close, the run still appears
// in List() with whatever status it had at close time; use Finalize when
// you DO have a terminal status to record.
func (r *Run) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.eventsFh != nil {
		_ = r.eventsFh.Close()
		r.eventsFh = nil
	}
}

// SetCycles updates the cycles-completed count (called by deepResearch
// between reflection cycles). Cheap, frequent — uses the manifest lock.
func (r *Run) SetCycles(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return
	}
	r.manifest.Cycles = n
	_ = r.writeManifestLocked()
}

// Manifest returns a snapshot of the current manifest (safe to inspect from
// outside without holding any lock).
func (r *Run) Manifest() Manifest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manifest
}

func (r *Run) writeManifest() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeManifestLocked()
}

func (r *Run) writeManifestLocked() error {
	b, err := json.MarshalIndent(r.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("research: marshal manifest: %w", err)
	}
	// Atomic write — temp file + rename — so a crash mid-write never leaves
	// half a manifest that breaks List().
	tmp := filepath.Join(r.dir, "run.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("research: write tmp manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(r.dir, "run.json")); err != nil {
		return fmt.Errorf("research: rename manifest: %w", err)
	}
	return nil
}

// --- list / get / delete (the read side) ---

// List returns manifests for every persisted run, most recent first. Bad
// manifests (corrupt JSON, missing run.json) are skipped — one bad row should
// never block the list from rendering. limit ≤0 means no cap.
func (s *Store) List(limit int) ([]Manifest, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("research: read dir: %w", err)
	}
	var out []Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := s.readManifest(e.Name())
		if err != nil {
			continue // tolerate corrupt entries — skip, keep listing
		}
		out = append(out, m)
	}
	// Started-time desc; fall back to id desc (which is timestamp-prefixed).
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.After(out[j].Started)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get returns the manifest + the full report body for one run, or os.ErrNotExist.
func (s *Store) Get(id string) (Manifest, string, error) {
	m, err := s.readManifest(id)
	if err != nil {
		return Manifest{}, "", err
	}
	body, err := os.ReadFile(filepath.Join(s.dir, id, "report.md"))
	if err != nil && !os.IsNotExist(err) {
		return m, "", fmt.Errorf("research: read report: %w", err)
	}
	return m, string(body), nil
}

// GetFinding returns one sub-agent's raw finding body, or os.ErrNotExist.
func (s *Store) GetFinding(id, agentID string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, id, "findings", safeFilename(agentID)+".md"))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Delete removes a run's entire directory. Idempotent — deleting a missing
// run is not an error (the operator's intent — "make this go away" — is
// satisfied either way).
func (s *Store) Delete(id string) error {
	if !safeID(id) {
		return fmt.Errorf("research: refusing to delete unsafe id %q", id)
	}
	path := filepath.Join(s.dir, id)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("research: delete %s: %w", id, err)
	}
	return nil
}

func (s *Store) readManifest(id string) (Manifest, error) {
	if !safeID(id) {
		return Manifest{}, fmt.Errorf("research: unsafe id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, id, "run.json"))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("research: parse manifest %s: %w", id, err)
	}
	return m, nil
}

// --- helpers ---

// safeID rejects path traversal / separator-bearing ids. Defensive even
// though our own makeID generates clean ones — an operator might paste a
// hand-typed id with a stray "../".
func safeID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if strings.ContainsAny(id, `/\:*?"<>|`) {
		return false
	}
	if strings.Contains(id, "..") {
		return false
	}
	return true
}

// safeFilename sanitizes an agent id for use as a filename (subagent ids are
// already slug-like in tenant — "main-researcher-3" — but defensive against a
// future change in id format).
func safeFilename(s string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_':
			return r
		default:
			return '_'
		}
	}, s)
	if clean == "" {
		return "agent"
	}
	if len(clean) > 80 {
		clean = clean[:80]
	}
	return clean
}
