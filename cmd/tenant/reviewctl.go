package main

// /review <plan.md> — GStack Layer 3 cascading review.
//
// Runs three role-specific reviewers (CEO, Engineer, Designer) against a
// plan file and appends a "## GSTACK REVIEW REPORT" section to the file.
// Each reviewer is a tools-OFF LLM call with a role-specific system
// prompt. If the operator has registered a named agent profile called
// `reviewer-ceo` / `reviewer-eng` / `reviewer-design`, /review uses that
// profile's router (their custom model + their custom soul, if set) —
// otherwise it falls back to the planner LLM + the built-in soul.
//
// Independent reviewers (no chained context). Run in parallel so the
// three calls are amortized to ~the slowest one. Errors per-reviewer
// don't abort the others; the failing section carries an "ERROR:" note.

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

//go:embed builtinsouls/reviewer-ceo.md
var reviewerCEOSoul string

//go:embed builtinsouls/reviewer-eng.md
var reviewerEngSoul string

//go:embed builtinsouls/reviewer-design.md
var reviewerDesignSoul string

// reviewMaxPlanBytes — truncate the plan text fed to each reviewer at
// this size. ~25KB ≈ 6K tokens, leaving room for the system soul + the
// reviewer's own output budget without blowing past a 16K context.
// Truncation is surfaced to the reviewer (and the report header) so the
// reader knows the review didn't see the full file.
const reviewMaxPlanBytes = 25 * 1024

// reviewMaxOutputTokens — generous so thinking-model variants
// (aeon-ultimate w/ --reasoning-parser) can think AND emit a full
// structured review. Same root-cause guard as the goal judge and
// research clarifier (see tasks/lessons.md).
const reviewMaxOutputTokens = 3000

// reviewerSpec describes one reviewer in the cascade. Role is the
// canonical short id used in the agent-profile lookup (`reviewer-<role>`)
// and the --reviewers CSV.
type reviewerSpec struct {
	Role  string // "ceo" / "eng" / "design"
	Title string // section header in the output
	Soul  string // built-in fallback system prompt
}

// reviewerCatalog — canonical order. The output section preserves this
// order regardless of which reviewers ran or in what real-time order
// the parallel goroutines finished.
var reviewerCatalog = []reviewerSpec{
	{Role: "ceo", Title: "CEO Review", Soul: reviewerCEOSoul},
	{Role: "eng", Title: "Engineering Review", Soul: reviewerEngSoul},
	{Role: "design", Title: "Design Review", Soul: reviewerDesignSoul},
}

// reviewControl is the cmd-side impl wired into tui.Config.Review.
// rt is optional — when set, named agent profiles override the planner
// for any reviewer that has a matching `reviewer-<role>` profile.
type reviewControl struct {
	ag *agent.Agent
	rt *TeamRuntime // optional: enables per-reviewer profile routers
}

func newReviewControl(ag *agent.Agent, rt *TeamRuntime) *reviewControl {
	return &reviewControl{ag: ag, rt: rt}
}

// Review reads the plan at planPath, runs the requested reviewers (or
// all of reviewerCatalog when roles is nil/empty), and appends the
// formatted report to the file. Returns the rendered report so the TUI
// can echo it. Truncates the plan input at reviewMaxPlanBytes.
func (rc *reviewControl) Review(ctx context.Context, planPath string, roles []string) (string, error) {
	if rc.ag == nil {
		return "", fmt.Errorf("review: no agent wired")
	}
	planPath = strings.TrimSpace(planPath)
	if planPath == "" {
		return "", fmt.Errorf("review: plan path required")
	}
	abs, err := filepath.Abs(planPath)
	if err == nil {
		planPath = abs
	}
	raw, err := os.ReadFile(planPath)
	if err != nil {
		return "", fmt.Errorf("review: read %s: %w", planPath, err)
	}
	plan := string(raw)
	if strings.TrimSpace(plan) == "" {
		return "", fmt.Errorf("review: plan file is empty")
	}
	truncated := false
	if len(plan) > reviewMaxPlanBytes {
		plan = plan[:reviewMaxPlanBytes]
		truncated = true
	}

	specs, err := selectReviewers(roles)
	if err != nil {
		return "", err
	}

	sections := make([]reviewSection, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		i, spec := i, spec
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, rerr := rc.runOne(ctx, spec, plan, truncated)
			sections[i] = reviewSection{Title: spec.Title, Body: body, Err: rerr}
		}()
	}
	wg.Wait()

	report := renderReviewReport(sections, truncated, time.Now().UTC())
	if err := appendToFile(planPath, "\n\n"+report+"\n"); err != nil {
		return report, fmt.Errorf("review: wrote report to memory but failed to append to %s: %w", planPath, err)
	}
	return report, nil
}

// runOne resolves the LLM for this reviewer (profile router if a matching
// `reviewer-<role>` profile exists, else the shared planner) and runs a
// single tools-off Generate. Returns the trimmed response body.
func (rc *reviewControl) runOne(ctx context.Context, spec reviewerSpec, plan string, truncated bool) (string, error) {
	llm, soul, err := rc.resolveReviewer(ctx, spec)
	if err != nil {
		return "", err
	}
	truncNote := ""
	if truncated {
		truncNote = fmt.Sprintf("\n\n[NOTE: plan was truncated to the first %d bytes for this review]", reviewMaxPlanBytes)
	}
	user := fmt.Sprintf(`Review the following plan according to your role above.

PLAN:
%s%s

Now output your review using the exact format from your role instructions.`, plan, truncNote)
	resp, gerr := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: soul},
			{Role: "user", Content: user},
		},
		MaxTokens: reviewMaxOutputTokens,
		// Stop on tool-call markup so a thinking model that drifts into
		// tool-call mode doesn't pollute the review body.
		StopSequences: []string{"<tool_call", "<function="},
	})
	if gerr != nil {
		return "", gerr
	}
	body := strings.TrimSpace(resp.Text)
	if body == "" {
		return "", fmt.Errorf("reviewer returned empty response")
	}
	return body, nil
}

// resolveReviewer returns (llm, soul). When a named agent profile
// `reviewer-<role>` exists, uses its router + (if set) its soul. Else
// falls back to the shared planner + the built-in soul.
func (rc *reviewControl) resolveReviewer(ctx context.Context, spec reviewerSpec) (model.LLM, string, error) {
	profileName := "reviewer-" + spec.Role
	if rc.rt != nil {
		router, ap := rc.rt.routerForProfile(profileName)
		if ap != nil && router != nil {
			llm, _, err := router.LLMForRole(ctx, model.RolePlanner)
			if err != nil {
				return nil, "", fmt.Errorf("review %s: profile %q router error: %w", spec.Role, profileName, err)
			}
			soul := spec.Soul
			if strings.TrimSpace(ap.Soul) != "" {
				soul = ap.Soul
			}
			return llm, soul, nil
		}
	}
	llm, _, err := rc.ag.Router().LLMForRole(ctx, model.RolePlanner)
	if err != nil {
		return nil, "", fmt.Errorf("review %s: resolve planner: %w", spec.Role, err)
	}
	return llm, spec.Soul, nil
}

// reviewSection — one reviewer's contribution to the report.
type reviewSection struct {
	Title string
	Body  string
	Err   error
}

// renderReviewReport formats the aggregated report. Stable section order
// (CEO → Eng → Design). Failed reviewers get an "ERROR:" body so the
// reader sees which dimension didn't render.
func renderReviewReport(sections []reviewSection, truncated bool, at time.Time) string {
	var b strings.Builder
	b.WriteString("## GSTACK REVIEW REPORT\n")
	fmt.Fprintf(&b, "_Generated %s by tenant /review_\n", at.Format("2006-01-02 15:04 UTC"))
	if truncated {
		fmt.Fprintf(&b, "_Plan truncated to first %d bytes for reviewers._\n", reviewMaxPlanBytes)
	}
	for _, s := range sections {
		fmt.Fprintf(&b, "\n### %s\n", s.Title)
		if s.Err != nil {
			fmt.Fprintf(&b, "ERROR: %v\n", s.Err)
			continue
		}
		b.WriteString(strings.TrimSpace(s.Body))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// selectReviewers maps a CSV/slice of role short-ids to specs in
// reviewerCatalog order. Empty input → all reviewers. Unknown role →
// error so the operator notices the typo instead of getting a silently
// short report.
func selectReviewers(roles []string) ([]reviewerSpec, error) {
	if len(roles) == 0 {
		return reviewerCatalog, nil
	}
	want := map[string]bool{}
	for _, r := range roles {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" {
			continue
		}
		want[r] = true
	}
	if len(want) == 0 {
		return reviewerCatalog, nil
	}
	out := make([]reviewerSpec, 0, len(reviewerCatalog))
	for _, s := range reviewerCatalog {
		if want[s.Role] {
			out = append(out, s)
			delete(want, s.Role)
		}
	}
	if len(want) > 0 {
		unknown := make([]string, 0, len(want))
		for k := range want {
			unknown = append(unknown, k)
		}
		return nil, fmt.Errorf("review: unknown reviewer(s): %s (known: ceo, eng, design)", strings.Join(unknown, ", "))
	}
	return out, nil
}

// appendToFile appends text to path, creating the file if missing. The
// caller is expected to have validated the path; we don't create
// parent directories.
func appendToFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		return err
	}
	return nil
}

// --- CLI: `tenant review <path> [--reviewers csv]` ---

func cmdReview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	c := bindCommon(fs)
	reviewers := fs.String("reviewers", "", "comma-separated subset (ceo,eng,design); empty = all")
	// Allow positional plan path BEFORE flags, mirroring `tenant skills
	// seed gstack --...` so the flag parser doesn't choke on a path that
	// happens to come first.
	leading := []string{}
	rest := args
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		leading = append(leading, rest[0])
		rest = rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	positional := append([]string{}, leading...)
	positional = append(positional, fs.Args()...)
	if len(positional) == 0 {
		return fmt.Errorf("usage: tenant review <plan.md> [--reviewers ceo,eng,design]")
	}
	planPath := positional[0]

	if err := c.resolve(); err != nil {
		return err
	}
	log := newLogger()
	router, err := buildRouter(c, log)
	if err != nil {
		return err
	}
	st, closeStores, err := openStores(c)
	if err != nil {
		return err
	}
	defer closeStores()

	ag, err := agent.New(agent.Config{
		AgentID:    c.agent,
		Router:     router,
		Soul:       st.soul,
		Working:    working.New(),
		Archive:    st.archive,
		Episodic:   st.episodic,
		Semantic:   st.semantic,
		Tools:      agent.NewStaticRegistry(),
		Dispatcher: noopDispatcher{},
		Logger:     log,
	})
	if err != nil {
		return err
	}
	// CLI path doesn't construct a TeamRuntime — review falls back to the
	// shared planner LLM + built-in souls for every reviewer. Named-profile
	// overrides are a TUI-only feature (where rt is already wired).
	rc := newReviewControl(ag, nil)
	var roles []string
	if strings.TrimSpace(*reviewers) != "" {
		for _, r := range strings.Split(*reviewers, ",") {
			if r = strings.TrimSpace(r); r != "" {
				roles = append(roles, r)
			}
		}
	}
	report, err := rc.Review(ctx, planPath, roles)
	if err != nil {
		return err
	}
	fmt.Println(report)
	fmt.Printf("\n(appended to %s)\n", planPath)
	return nil
}
