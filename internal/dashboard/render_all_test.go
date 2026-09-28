package dashboard

// render_all_test.go is the page harness every dashboard page test plugs into
// (TEN-211). It is frozen after the foundation: a page added later registers
// its fakes and pages from an init() in its own test file,
//
//	func init() {
//		harnessFakes = append(harnessFakes, func(s *Server, mode fakeMode) { s.SetGoal(...) })
//		harnessPages = append(harnessPages, "/research/r-1")
//	}
//
// so nobody edits this file. renderAllPages renders every GET page in one of
// two modes:
//   - populated: the 390px worst case. 60-character unbroken names,
//     200-character URLs, an approval detail of 300 characters with no spaces
//     plus 40 newlines, and 12 or more rows in every list.
//   - failing: every read that can fail does, with the raw text the real
//     control returns, so the plain-language sweep sees what a failure would
//     put on the page.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/logs/event"
)

type fakeMode int

const (
	populated fakeMode = iota
	failing
)

// harnessFakes install one control's fake each. The foundation's covers every
// control that existed before Dashboard v2; the new pages append their own.
var harnessFakes = []func(s *Server, mode fakeMode){installExistingFakes}

// harnessPages are the GET pages renderAllPages visits: every nav page, and
// the pages under them that a fake makes reachable.
var harnessPages = []string{
	"/", "/chat", "/goal", "/logs",
	"/memory", "/memory/soul", "/memory/facts", "/memory/facts/removed", "/memory/facts/temporal", "/memory/provenance", "/memory/facts/1",
	"/skills", "/eval", "/research",
	"/tools", "/agents", "/mcp", "/cron",
	"/safety", "/access",
	"/models", "/integrations", "/settings/keys",
}

// Raw control errors, verbatim from cmd/tenant, for the failing fakes. None
// of them may reach a page as they are (D21): the handler shows a plain
// message instead, unless the adapter wrapped a PlainError.
const (
	rawProviderErr = "provider \"cloud\" is not configured (see /model or `tenant model list`)" // agentctl.go:91
	rawNoGoal      = "no goal is active"                                                        // goalctl.go:151
	rawGoalCleared = "🎯 goal cleared: keep the build green"                                     // goalctl.go:157
	rawNoHistory   = "research history is unavailable (no store configured)"                    // research.go:1157
	rawSavedToWiki = "📄 saved to wiki: research/what-is-fakeness.md"                            // research.go:1136
	rawStoreClosed = "sql: database is closed"                                                  // any store read during shutdown
)

// longName is a 60-character name with no break opportunity.
func longName(i int) string {
	s := fmt.Sprintf("n%02d_", i)
	return s + strings.Repeat("x", 60-len(s))
}

// longURL is a 200-character URL with no break opportunity.
func longURL(i int) string {
	s := fmt.Sprintf("https://tools-%02d.example.com/path/", i)
	return s + strings.Repeat("a", 200-len(s))
}

// longDetail is an approval detail built to overflow: 300 characters with no
// spaces, then 40 newlines, then the line that matters.
func longDetail() string {
	return strings.Repeat("r", 300) + strings.Repeat("\n", 40) + "rm -rf /important"
}

// harnessServer is a Server wired with every registered fake in mode. The
// tools, memory and log book are built here: New and SetLogs take them.
func harnessServer(t *testing.T, cfg Config, mode fakeMode, runner AgentRunner) *Server {
	t.Helper()
	ft := &fakeTools{}
	for i := range 14 {
		ft.tools = append(ft.tools, ToolInfo{Name: longName(i), Plugin: []string{"web", longName(90), "files"}[i%3], Enabled: i%2 == 0, Gated: i%4 == 0})
	}
	ft.plugins = []string{"web", longName(90), "files"}
	fm := newFakeMem()
	for i := range 12 {
		fm.addFact(int64(10+i), longName(i)+" lives at "+longURL(i), 0.7)
		fm.userFacts = append(fm.userFacts, SoulItem{ID: fmt.Sprintf("uf-%d", 10+i), Text: longName(i)})
	}
	var mem MemoryControl = fm
	if mode == failing {
		mem = failingMem{fm}
	}
	s := New(cfg, runner, ft, mem, agent.NewBroker(0), nil)
	_, hub, st := logsBook(t)
	var evs []event.Event
	for i := range 15 {
		evs = append(evs, event.Event{Log: "cron", Task: "Run", Msg: longName(i) + " " + longURL(i)})
	}
	commit(t, st, evs...)
	s.SetLogs(hub)
	for _, f := range harnessFakes {
		f(s, mode)
	}
	return s
}

// renderAllPages GETs every harness page from a server in mode, and returns
// each page's HTML by route. A page that doesn't answer 200 fails the test.
func renderAllPages(t *testing.T, mode fakeMode) map[string]string {
	t.Helper()
	s := harnessServer(t, Config{}, mode, nil)
	out := make(map[string]string, len(harnessPages))
	for _, p := range harnessPages {
		rec := get(t, s, p)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200:\n%s", p, rec.Code, rec.Body.String())
		}
		out[p] = rec.Body.String()
	}
	return out
}

// failingMem is the memory control with every read failing.
type failingMem struct{ *fakeMem }

func (failingMem) Soul() (SoulView, error) { return SoulView{}, errors.New(rawStoreClosed) }
func (failingMem) Facts(string, int, string) ([]FactView, string, error) {
	return nil, "", errors.New(rawStoreClosed)
}
func (failingMem) FactProvenance(int64) ([]EpisodeView, error) {
	return nil, errors.New(rawStoreClosed)
}
func (failingMem) RemovedFacts(int) ([]FactView, error) { return nil, errors.New(rawStoreClosed) }
func (failingMem) TemporalFacts() ([]TemporalFactView, MemStats, error) {
	return nil, MemStats{}, errors.New(rawStoreClosed)
}
func (failingMem) UserProfile() (string, error) { return "", errors.New(rawStoreClosed) }
func (failingMem) CompactionProvenance() (*CompactionProvenanceView, error) {
	return nil, errors.New(rawStoreClosed)
}

// harnessPerms is every safety check, allowed; with newOnes the three an
// update added ask and are tagged New.
func harnessPerms(newOnes bool) []PermissionRow {
	var out []PermissionRow
	for _, c := range []struct{ cat, title, can string }{
		{"exec", "Run commands", "run commands and programs on this computer"},
		{"write", "Change files", "create and change files on this computer"},
		{"destructive", "Irreversible actions", "do things that can't be undone"},
		{"web", "Act on websites", "click, type and submit forms on websites"},
		{"send", "Send messages", "send email, texts and posts in your name"},
		{"egress", "Share private data", "send private information it has read to an outside service"},
		{"mcp", "Connect remote services", "connect new remote services and trust their tools"},
		{"sandbox", "Leave the sandbox", "run a command outside the safety box after the box stopped it"},
	} {
		isNew := newOnes && (c.cat == "egress" || c.cat == "mcp" || c.cat == "sandbox")
		mode := "allow"
		if isNew {
			mode = "ask"
		}
		out = append(out, PermissionRow{Category: c.cat, Mode: mode, Desc: c.can, Title: c.title, Can: c.can, New: isNew})
	}
	return out
}

// installExistingFakes wires a fake for every control that existed before
// Dashboard v2. Only the GitHub repo list and memory have reads that fail.
func installExistingFakes(s *Server, mode fakeMode) {
	var (
		jobs     []CronJobView
		keys     []ServiceKeyView
		points   []EvalTrendPoint
		skills   []SkillView
		models   []ModelView
		servers  []MCPServerView
		integ    []IntegrationView
		repos    []GitHubRepoRow
		rules    []ToolRuleRow
		handles  []string
		approval []PendingApproval
	)
	for i := range 12 {
		n := longName(i)
		jobs = append(jobs, CronJobView{ID: fmt.Sprintf("j%d", i), Name: n, Spec: "0 3 * * *", Prompt: longURL(i), Enabled: i%2 == 0, Kind: "prompt", NextRun: "tomorrow 03:00", LastRun: "today 03:00", LastStatus: "ok"})
		keys = append(keys, ServiceKeyView{CredID: n, Name: n, Category: "model", Set: i%2 == 0, EnvVar: strings.ToUpper(n)})
		points = append(points, EvalTrendPoint{When: fmt.Sprintf("2026-09-%02d", i+1), Score: float64(70 + i), HasBase: true})
		skills = append(skills, SkillView{Name: n, Description: longURL(i), Status: []string{"live", "proposed"}[i%2], Enabled: true, Untrusted: i%3 == 0, Origin: "agent", Recipe: longURL(i)})
		models = append(models, ModelView{Name: n, Kind: "openai", Model: longName(i + 20), Active: i == 0, Degraded: i == 1})
		servers = append(servers, MCPServerView{Label: n, URL: longURL(i), Enabled: true, ToolCount: i, Trust: []string{"ask", "allow", "deny"}[i%3], NeedsReauth: i == 2, AuthSince: "2026-09-18 14:02 UTC", AuthErr: "invalid_grant: " + longName(i)})
		integ = append(integ, IntegrationView{ID: n, Label: n, Configured: i%2 == 0, Enabled: i%3 == 0, SetupHint: longURL(i)})
		repos = append(repos, GitHubRepoRow{Slug: n + "/" + n, Write: i%2 == 0, Primary: i == 0})
		rules = append(rules, ToolRuleRow{Pattern: "mcp:" + longURL(i) + ":*", Mode: []string{"ask", "allow", "deny"}[i%3], Kind: "glob"})
		handles = append(handles, "+1555"+strings.Repeat("0", 7)+fmt.Sprint(i)+"@"+n+".example.com")
		approval = append(approval, PendingApproval{ID: fmt.Sprintf("ap-%d", i), Category: "exec", Action: "os_exec", Detail: longDetail(), AgeSecs: i * 400, Origin: "local", SessionOK: true})
	}
	gh := &fakeGitHubRepos{rows: repos}
	if mode == failing {
		gh.listErr = errors.New(rawStoreClosed)
	}
	s.SetCron(&fakeCronCtl{jobs: jobs, execOn: true})
	s.SetGitHubRepos(gh)
	s.SetSecrets(&fakeSecrets{views: keys})
	s.SetEval(&fakeEvalCtl{sched: EvalScheduleView{Desc: "daily at 03:15", HasRun: true, LastWhen: "today 03:15", LastScore: 81, LastPass: 40, LastTotal: 50, Trend: "up"}, points: points, diff: longURL(0), judge: longName(0)})
	s.SetSkills(&fakeSkillCtl{skills: skills, autoMode: "off"})
	s.SetModels(&fakeModelCtl{models: models, ceiling: 30})
	s.SetMCP(&fakeMCPCtl{servers: servers})
	s.SetIntegrations(&fakeIntegrationsCtl{list: integ})
	s.SetAccess(&fakeAccess{view: AccessView{
		IMessageAvailable: true, ResponderAvailable: true, ResponderOn: true, Handles: handles, IMessagePerms: harnessPerms(false),
		DiscordAvailable: true, RelayConfigured: true, RelayRunning: true, OperatorSet: true, OperatorID: strings.Repeat("9", 60), ExecOn: true, DiscordPerms: harnessPerms(true),
	}})
	s.SetApprovals(&fakeApprovals{pending: approval})
	s.SetRules(&fakeRules{rows: rules})
}
