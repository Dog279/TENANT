// Package tui is Tenant's terminal experience: a chat pane, a live
// activity feed (memory assembly, token streaming, tool calls, results,
// background self-improvement), and a status bar — one screen, built on
// Bubble Tea. The agent emits agent.Event values to a channel; the TUI
// renders them live.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tenant/internal/agent"
	modelpkg "tenant/internal/model"
)

// Config wires the TUI to an already-constructed agent whose Observer
// pushes into Events. Backend/Model/AgentID are display-only.
type Config struct {
	Agent   *agent.Agent
	Events  <-chan agent.Event
	AgentID string
	Backend string
	Model   string
	// System, if set, carries pre-formatted background feed lines
	// (e.g. self-improvement job runs) to show alongside agent activity.
	System <-chan string
	// SavePath is where Ctrl-Y dumps the transcript (for troubleshooting).
	SavePath string
	// Tools, if set, lets slash commands enable/disable tools at runtime.
	Tools ToolControl
	// Skills, if set, lets /skills manage the T4 skill library at runtime.
	Skills SkillControl
	// SkillConfig, if set, drives the `/skill` (singular) integration-config
	// flow added in TEN-64 — configure API keys, probe, clear. Distinct
	// from the T4 SkillControl above; see SkillConfigControl docs.
	SkillConfig SkillConfigControl
	// SkillSeeds, if set, installs a named starter bundle on `/skills seed
	// <name>`. Returns the number of skills installed. Bundles known so far:
	//   "gstack" — port of Garry Tan's YC operating discipline (5 skills:
	//   investigate-systematically, boil-the-lake-completeness, structured-
	//   ask, founder-voice, status-escalation). See docs/GSTACK.md.
	SkillSeeds func(bundle string) (int, error)
	// Memory, if set, powers the /memory command.
	Memory MemoryControl
	// Approvals, if set, delivers dangerous-action requests for the user
	// to decide via /approve, /approve session, /approve always, /deny.
	Approvals <-chan ApprovalRequest
	// Perms, if set, powers /permissions (per-category safety modes).
	Perms PermissionControl
	// Dash, if set, powers /dashboard: start/stop the web control panel live.
	Dash DashboardControl
	// Relay, if set, powers /relay: start/stop the offsite Discord relay live.
	Relay RelayControl
	// IMessage, if set, powers /imessage: manage the deny-by-default allowlist
	// of phone numbers / emails permitted to drive the agent over iMessage.
	IMessage IMessageControl
	// Cron, if set, powers /cron: manage recurring agent-prompt jobs.
	Cron CronControl
	// MCP, if set, powers /mcp: connect/list/remove remote MCP connector
	// servers at runtime (OAuth browser sign-in), tools gated deny-by-default.
	MCP MCPControl
	// Secrets, if set, powers `/configure` (no-arg): an arrow-key picker over
	// every keyed service (LLM providers, web-search engines, Discord, …) →
	// paste the key → stored + applied live. Write-only: List exposes presence,
	// never values.
	Secrets SecretsControl
	// Setup, if set, powers `/setup`: an arrow-key menu mirroring the external
	// `tenant setup` wizard (provider / model / endpoint / key / tool-format /
	// embeddings / gateway), edited in-TUI and applied live where possible.
	Setup SetupControl
	// Feedback, if set, powers `/ack` and `/undo`: mark the last turn as good
	// (ack) or bad (undo) — the real-world signal for the self-improvement loop.
	Feedback FeedbackControl
	// Eval, if set, powers /eval: view + live-tune the nightly eval schedule
	// (every <dur> / at HH:MM / off), queue a one-shot run, and read the
	// score trend (TEN-196).
	Eval EvalControl
	// Models, if set, powers /model: list configured model backends, switch
	// the primary on the fly, and add a new backend mid-session.
	Models ModelControl
	// Research, if set, powers /research: run a deep-research pass (plan →
	// parallel researchers → cited report). Progress streams to the feed; the
	// report lands in the chat pane.
	Research ResearchControl
	// Reconnect, if set, is notified when generation is unreachable so it can
	// run a background cascading reconnect (progress shown via System feed).
	Reconnect ReconnectControl

	// Agents, if set, powers /agents: manage named sub-agent profiles
	// (per-agent model + identity) that the orchestrator can spawn by name.
	Agents AgentControl

	// Goals, if set, powers /goal: autonomous-loop on a stop condition the
	// user types. After every turn the TUI asks the judge whether the
	// condition has been met; if not, it auto-submits a continuation. Caps
	// at a turn budget so a stuck loop self-terminates.
	Goals GoalControl
	// Review, if set, enables `/review <plan.md>` — cascading CEO/Eng/
	// Designer reviews appended to the plan file. Independent of Goals;
	// callers can wire one without the other.
	Review ReviewControl
	// TeamEvents, if set, carries spawned sub-agents' events (tagged with
	// their id) — shown in the feed and summed into the team token counter.
	TeamEvents <-chan TeamEvent

	// RecordUsage, if set, persists each MAIN-agent LLM call's token usage
	// (input, output) to the long-term cost-audit ledger. The cmd layer
	// binds this to the usage store with agentID + model closed over, and
	// swallows persistence errors so the UI never blocks (TEN-167).
	RecordUsage func(in, out int)

	// ResearchTimeline, if set, carries structured updates from /research
	// (plan ready, wave dispatched, agent status, reflection done, synth
	// starting, run done). The TUI aggregates these into a live timeline
	// pane that renders above the feed while a research run is in flight.
	// Independent of TeamEvents (which is per-agent tool activity) — the
	// timeline shows the orchestrator's phase + per-agent rollups.
	ResearchTimeline <-chan ResearchTimelineUpdate
}

// TeamEvent is one event from a spawned sub-agent, tagged with its id.
type TeamEvent struct {
	AgentID string
	Event   agent.Event
}

// MemoryControl powers /memory: inspect the memory tiers, search, and
// the limited editing surface (forget facts/episodes, soul view+import).
// Each method returns text for the chat pane.
type MemoryControl interface {
	Stats() string
	Search(query string) string
	Facts(query string) string
	Recent(n int) string
	Forget(target string) string // "fact:<id>" or "ep:<id>"
	SoulView() string
	SoulImport(path string) string
	RulesView() string
	Distill() string
	ProfileView() string    // synthesized always-on user model
	ProfileRefresh() string // re-synthesize the user model now
}

// SkillInfo is one T4 skill's state for the /skills listing.
type SkillInfo struct {
	Name        string
	Description string
	Enabled     bool
	Status      string
}

// SkillControl is how the TUI manages the T4 skill library on the fly.
type SkillControl interface {
	SkillList() []SkillInfo
	AddSkill(name, description, recipe string) error
	SetSkillEnabled(name string, on bool) (bool, error)
	ForgetSkill(name string) (bool, error)
	AcceptSkill(name string) (bool, error)

	// SkillHistory returns the prior snapshots of a skill, newest first.
	// Empty slice = a fresh skill that's never been edited (or unknown name).
	SkillHistory(name string) ([]SkillHistoryEntry, error)
	// SkillCurrent returns the live description + recipe — used by
	// /skills diff to compare against a prior version. Returns
	// (nil, nil) when the name is unknown.
	SkillCurrent(name string) (*SkillSnapshot, error)
	// SkillRevert restores a skill to a given version (1-based — version 1
	// is the OLDEST recorded edit; the highest version is the most recent).
	// The current state is snapshotted into history before being overwritten
	// so reverts are themselves reversible.
	SkillRevert(name string, version int) error

	// AutoAcceptMode returns the induced-skill auto-accept policy
	// ("off"|"on"|"trusted"). SetAutoAccept persists a new mode (TEN-152).
	// Lets the operator graduate from manual /skills accept to automatic once
	// they trust the loop — "trusted" auto-accepts only while feedback is healthy.
	AutoAcceptMode() string
	SetAutoAccept(mode string) error
}

// FeedbackControl records the operator's ack/undo on the last turn (TEN-151) —
// the real-world success signal that feeds skill induction + the eval.
type FeedbackControl interface {
	Ack() (status string, err error)
	Undo() (status string, err error)
}

// EvalControl powers /eval: view and live-tune the nightly-eval schedule
// (TEN-196) — the self-improvement loop's regression gate. Schedule changes
// persist to launchConfig AND re-arm the running scheduler in place, so no
// restart is needed; the anacron-style clock (seeded from trend.jsonl) keeps
// it to one run per period no matter how often the process relaunches.
type EvalControl interface {
	// Status describes the current schedule and the last recorded run.
	Status() string
	// SetEvery arms interval mode (Go duration, e.g. "24h").
	SetEvery(spec string) (string, error)
	// SetAt arms a daily wall-clock anchor (24h "HH:MM", e.g. "03:15").
	SetAt(spec string) (string, error)
	// Off disarms the schedule.
	Off() (string, error)
	// RunNow queues a single immediate run on the improve scheduler (fires
	// within one tick; the result lands in the feed + trend log).
	RunNow() (string, error)
	// Trend renders the last n trend entries (scores + regression verdicts).
	Trend(n int) string
	// Diff renders the per-task movers analysis between the newest eval
	// artifact and its baseline — improved/declined tables with failure
	// autopsy (TEN-198).
	Diff() (string, error)
}

// SkillHistoryEntry is one prior snapshot of a skill. Surfaced by /skills
// history; consumed by /skills diff and /skills revert.
type SkillHistoryEntry struct {
	Version          int
	PriorDescription string
	PriorRecipe      string
	PriorStatus      string
	ChangeSource     string // "operator" | "induction" | "revert" | "seed"
	ChangedAt        time.Time
}

// SkillSnapshot is the live current-state view of a skill. Used as the
// "newer" side of /skills diff.
type SkillSnapshot struct {
	Name        string
	Description string
	Recipe      string
	Status      string
}

// ToolInfo is one tool's runtime state for the /tools listing. Gated
// carries the plugin's authoritative send/destructive gate class so the
// dashboard can flag it (vs. guessing from the tool name).
type ToolInfo struct {
	Name    string
	Plugin  string
	Enabled bool
	Gated   bool
}

// SkillInfo (new — for /skill, the integration-config surface) describes
// one configurable integration's state. Distinct from the existing
// SkillControl/SkillInfo above (which is for the T4 skill MEMORY library,
// i.e. /skills plural). The naming overlap reflects that this codebase
// uses "skill" for both concepts; TEN-64 introduces /skill (singular) as
// the integration-config command, parallel to /model add-cloud.
type SkillConfigInfo struct {
	ID         string
	Label      string
	Configured bool
	Enabled    bool
	Legacy     bool   // true ⇒ shown only because it's in the old skillSpecs catalog
	SetupHint  string // optional human-readable orientation shown at the top of /configure
}

// SkillConfigControl is how the TUI runs the `/skill` (singular)
// integration-config flow — configure, probe, clear API keys for
// gsuite/discord/etc. Lives alongside SkillControl (above) which
// manages the T4 skill memory.
type SkillConfigControl interface {
	SkillList() []SkillConfigInfo
	SkillShow(id string) (string, error)
	// SkillConfigure: args[0] is the skill id; args[1:] are positional
	// or key=value pairs. noEnable suppresses auto-enable on probe
	// success.
	SkillConfigure(args []string, noEnable bool) (string, error)
	SkillProbe(id string) (string, error)
	SkillClear(id, field string) (string, error)
	// SkillFields returns the ordered field schema for an id — drives
	// the interactive `/configure` flow, which walks fields one at a
	// time. Returns an error when the id is unknown.
	SkillFields(id string) ([]SkillField, error)
}

// SkillField is one row of the schema surfaced to the interactive
// configure flow. Mirrors skillKindField but stripped of the validator
// closure (validators live behind SkillConfigure's persistence path).
type SkillField struct {
	Key          string
	Prompt       string
	Secret       bool
	Required     bool
	Default      string
	Options      []string // non-empty ⇒ picker; empty ⇒ free-text input
	OptionLabels []string // optional human-readable labels, parallel to Options
	// ShowIf, if non-nil, hides this field when it returns false. The
	// interactive flow evaluates it with the running map of answers
	// from prior fields. Used for conditional fields (e.g. gsuite's
	// sa_json shows only when auth=sa).
	ShowIf func(values map[string]string) bool
	// NoteAfter, if non-nil, fires after a value is collected. Returns
	// a guidance message + an abort flag. If abort=true, the configure
	// session stops immediately (no persistence, no probe) — used for
	// hard-blocking prerequisite failures (e.g. picking auth=gcloud
	// when gcloud isn't installed).
	NoteAfter func(value string) (msg string, abort bool)
}

// ToolControl is how the TUI manages tools on the fly (implemented by
// the tool multiplexer). SetEnabled toggles a single tool (exact name)
// or a whole plugin (by label), returning how many changed + the scope.
type ToolControl interface {
	ToolList() []ToolInfo
	// SetEnabled returns how many tools changed + the scope ("tool"/
	// "plugin"), or an error if enabling triggered a lazy activation that
	// failed (e.g. Chrome couldn't launch for the web plugin).
	SetEnabled(target string, on bool) (int, string, error)
	// SetPluginEnabled is the explicit categorical toggle: forces a
	// plugin-label sweep and never matches a single tool name. Used by
	// the `/enable skill <label>` form. Returns 0 if no plugin matched
	// so the caller can surface a "skill X doesn't exist" error.
	SetPluginEnabled(label string, on bool) (int, string, error)
	// Plugins returns the sorted unique set of plugin labels (gsuite,
	// sql, web, …). Used to render a "did you mean" hint when `/enable
	// skill <typo>` matches nothing.
	Plugins() []string
}

// ApprovalDecision is the user's answer to a dangerous-action prompt,
// modeled on the three-tier approve flow (once / session / always) plus
// an explicit deny.
type ApprovalDecision int

const (
	DenyOnce ApprovalDecision = iota
	ApproveOnce
	ApproveSession
	ApproveAlways
)

// ApprovalRequest is a dangerous action paused awaiting the user. The
// broker that raised it blocks on Reply until the TUI sends a decision
// (or the turn's context is cancelled).
type ApprovalRequest struct {
	Category string // safety category (exec, destructive, web, send)
	Action   string // specific action id (os_exec, web_transact, …)
	Detail   string // human-readable description of exactly what will run
	Reply    chan ApprovalDecision
}

// PermissionInfo is one safety category's current mode, for /permissions.
type PermissionInfo struct {
	Category string
	Mode     string // ask | allow | deny
	Desc     string
}

// PermissionControl powers /permissions: view and set per-category safety
// so the operator can turn approval on/off by command type.
type PermissionControl interface {
	Permissions() []PermissionInfo
	SetPermission(category, mode string) (bool, error)
}

// DashboardControl powers /dashboard: start/stop the web control panel at
// runtime and report its state. Enable returns the bind address (a startup
// error is surfaced to the feed asynchronously, not via this return). The
// implementation persists the on/off choice so the next launch respects it.
type DashboardControl interface {
	Enable() (addr string, err error)
	Disable() error
	Status() (running bool, addr string)
}

// RelayControl powers /relay: start/stop the offsite Discord relay at runtime,
// set the single operator's Discord user id, and report state. The
// implementation persists the on/off + operator choices.
type RelayControl interface {
	Enable() error
	Disable() error
	Status() (running bool, operatorSet bool, execOn bool)
	SetOperator(id string) error
	// SetExec toggles offsite exec mode (the gated dangerous tools, each still
	// per-action button-approved over Discord). Off = read/research/comms only.
	SetExec(on bool) error
}

// IMessageControl powers /imessage: manage the DENY-BY-DEFAULT allowlist of
// phone numbers / emails permitted to DRIVE the agent over iMessage (i.e. let
// an inbound texter invoke the agent's tools). Edits persist. An EMPTY list
// permits NOBODY — the safe default for "no unrestricted access." The list is
// the policy the inbound responder (Layer 2) gates on; storing it here is
// necessary but the enforcement check lives at the drive point.
//
// Allow/Deny return the NORMALIZED handle so the UI echoes the exact canonical
// form stored (emails lowercased; phones reduced to '+' and digits), plus a
// flag for whether the list actually changed (false = already present / absent).
type IMessageControl interface {
	AllowList() []string
	Allow(handle string) (normalized string, added bool, err error)
	Deny(handle string) (normalized string, removed bool, err error)
	Clear() (n int, err error)
}

// CronControl powers /cron: manage recurring jobs. Each job runs an agent prompt
// on a crontab/@every schedule, UNATTENDED and read/comms-safe (the cron runner
// cannot exec/write/send or schedule more jobs). Add/SetEnabled return the
// resulting job view; RunNow triggers an immediate run whose result arrives
// asynchronously in the feed.
type CronControl interface {
	Jobs() []CronJobView
	Add(spec CronAddSpec) (CronJobView, error)
	Remove(id string) (removed bool, err error)
	SetEnabled(id string, on bool) (view CronJobView, changed bool, err error)
	RunNow(id string) error
	// ExecEnabled reports the global cron-exec kill-switch; SetExec flips it.
	// When off, shell jobs and exec-opted-in prompt jobs are created but won't run.
	ExecEnabled() bool
	SetExec(on bool) error
}

// MCPControl powers /mcp: connect/list/remove remote MCP connector servers at
// runtime. Add connects via OAuth 2.1 + Dynamic Client Registration (opens a
// browser, no pre-created app) and surfaces the server's tools gated
// deny-by-default; the URL is persisted so the server re-registers next launch.
// Add MAY BLOCK on the browser callback — callers MUST run it off the UI
// goroutine (a tea.Cmd closure), never inline.
type MCPControl interface {
	List() []MCPServerInfo
	// Add connects + activates the server at url, then persists it. Blocks
	// until the OAuth browser flow completes. Returns the connected server view.
	Add(url string) (MCPServerInfo, error)
	// Remove disconnects + forgets a server (by URL or label); removed=false if
	// nothing matched.
	Remove(target string) (removed bool, err error)
}

// MCPServerInfo is one remote MCP connector's view for /mcp.
type MCPServerInfo struct {
	Label     string // e.g. "mcp:mcp.atlassian.com"
	URL       string
	Enabled   bool
	ToolCount int
}

// CronAddSpec is the input to CronControl.Add. Kind is "" (prompt), "prompt", or
// "shell"; Exec opts a prompt job into the dangerous tool surface; TZ is an
// optional IANA timezone.
type CronAddSpec struct {
	Name   string
	Spec   string
	Prompt string
	Kind   string
	Exec   bool
	TZ     string
}

// CronJobView is the TUI's render-ready view of one cron job. Time fields are
// pre-formatted strings ("" when unset).
type CronJobView struct {
	ID         string
	Name       string
	Spec       string
	Prompt     string
	Enabled    bool
	Kind       string
	Exec       bool
	TZ         string
	NextRun    string
	LastRun    string
	LastStatus string
}

// SecretsControl powers the no-arg `/configure` picker (TEN-149): list every
// keyed service (presence only — never a value), and set/remove a key. The
// cmd/tenant adapter reuses the same credentials path as the dashboard Keys page
// (TEN-145), so a key set here is applied live (TEN-147) — no restart.
type SecretsControl interface {
	List() []SecretItem
	Set(credID, value string) error
	Remove(credID string) error
}

// SecretItem is one configurable key's render-ready state: presence only, no value.
type SecretItem struct {
	CredID   string
	Name     string
	Category string // "LLM provider" | "Web search" | "Integration"
	Set      bool
}

// renderExpansion formats a rehydrated compaction span for the chat pane: a
// provenance header + the original turns (clipped + capped). (TEN-104)
func renderExpansion(exp *agent.CompactionExpansion) string {
	const maxShow, clip = 30, 300
	var b strings.Builder
	rng := "full session"
	if !exp.Source.After.IsZero() || !exp.Source.Before.IsZero() {
		rng = exp.Source.After.Format("2006-01-02 15:04") + " – " + exp.Source.Before.Format("2006-01-02 15:04")
	}
	origin := exp.Source.Origin
	if origin == "" {
		origin = "working"
	}
	fmt.Fprintf(&b, "compaction provenance — %d turns from %s (source: %s)", exp.Source.MsgCount, rng, origin)
	if len(exp.Events) == 0 {
		b.WriteString("\n(no archived turns found for this span)")
		return b.String()
	}
	shown := exp.Events
	truncated := false
	if len(shown) > maxShow {
		shown = shown[len(shown)-maxShow:]
		truncated = true
	}
	for _, ev := range shown {
		content := strings.TrimSpace(ev.Content)
		if len([]rune(content)) > clip {
			content = string([]rune(content)[:clip]) + "…"
		}
		fmt.Fprintf(&b, "\n[%s] %s", ev.Role, content)
	}
	if truncated {
		fmt.Fprintf(&b, "\n\n(showing the last %d of %d turns — full audit at /memory/provenance in the dashboard)", maxShow, len(exp.Events))
	}
	return b.String()
}

// parseOnOff reads an on/off (enable/disable) token. ok is false for anything
// else, so the caller can show usage instead of guessing.
func parseOnOff(s string) (on bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "enable", "yes", "true", "1":
		return true, true
	case "off", "disable", "no", "false", "0":
		return false, true
	}
	return false, false
}

// ModelInfo is one configured model backend's state for the /model listing.
type ModelInfo struct {
	Name     string // the operator-chosen provider name (config key)
	Kind     string // vllm | openai | anthropic | …
	Endpoint string
	Model    string // served model id ("" = auto-detect at launch)
	Active   bool   // currently the primary
	Degraded bool   // active provider is running on the echo fallback (couldn't build)
}

// ModelControl powers /model: list configured backends, switch the active
// (primary) one live, and register a new vLLM backend mid-session. Use returns
// a status line + the new active model's display name (for the status bar).
type ModelControl interface {
	ModelList() []ModelInfo
	// UseModel switches the primary provider AND optionally pins a
	// specific model variant served by that provider. modelOverride=""
	// preserves the provider's saved model. modelOverride="glm-5.1" (etc)
	// updates the provider's Model field before swap + probe. Useful for
	// providers that serve multiple variants under one endpoint (Z.ai
	// serves glm-4.5/4.6/4.7/5/5.1/...; same for OpenAI's gpt-* family).
	UseModel(name, modelOverride string) (status, activeModel string, err error)
	// ListProviderModels queries the provider's endpoint live for the
	// model catalog it serves (/v1/models with /models fallback). Used by
	// `/model models <name>` for discovery without dropping to a browser
	// or vendor dashboard. Empty name = the currently-active provider.
	ListProviderModels(name string) (models []string, err error)
	AddModel(name, endpoint, toolFormat string) (string, error)
	// AddCloudModel registers a KEYED cloud provider (Z.ai / OpenAI / Grok /
	// Anthropic / etc.) in one shot, pulling endpoint + chat path + default
	// model + tool format from the provider catalog and storing the API key
	// in credentials.json (0600). Use it from /model add-cloud <kind> <key>
	// so the user doesn't have to drop to the CLI setup wizard.
	AddCloudModel(kind, apiKey string) (string, error)
	RemoveModel(name string) (string, error)
	// ReloadKeys re-resolves the ACTIVE provider's API key (env → credentials.json)
	// and hot-swaps it into the live router, so a key rotated at runtime takes
	// effect without a restart. Returns a status line (✓/⚠). Used by /model reload.
	ReloadKeys() (status string, err error)
	// LoopCeiling reports the current planner↔tool iteration cap; SetLoopCeiling
	// retunes it live (and persists) — /ceiling.
	LoopCeiling() int
	SetLoopCeiling(n int) (string, error)
}

// ResearchControl powers /research: a long-running deep-research pass. The
// implementation streams progress to the feed itself; Research blocks until the
// report is ready (the TUI runs it off the UI goroutine under a cancel ctx).
// C3 extensions expose the persisted run history — implementations without a
// store return ResearchHistoryRow{Err:"unavailable"} from ResearchHistory and
// fail the rest with a clear error.
type ResearchControl interface {
	Research(ctx context.Context, question string) (report string, err error)

	// ResearchAfterClarify continues a research pass after the user answered
	// the clarifier's questions. The caller passes the ENRICHED query
	// (original + folded-in answer) and the implementation bypasses the
	// vague-question gate so we don't loop back to the user.
	ResearchAfterClarify(ctx context.Context, enrichedQuestion string) (report string, err error)

	// ResearchHistory returns the last `limit` runs (most recent first).
	ResearchHistory(limit int) ([]ResearchHistoryRow, error)
	// ResearchShow returns the rendered report markdown for one past run
	// (with a metadata header). os.ErrNotExist wrapped for unknown ids.
	ResearchShow(id string) (string, error)
	// ResearchReplay re-runs the same question against the CURRENT model.
	// Returns the new report (also written to the wiki + history).
	ResearchReplay(ctx context.Context, id string) (report string, err error)
	// ResearchDelete purges a past run. Deleting a missing id is fine.
	ResearchDelete(id string) error
}

// ResearchClarifyError is the TUI-facing form of the clarify-needed sentinel
// (decoupled from the cmd/tenant research package so the tui package stays
// import-clean). Implementations of ResearchControl return an error that
// satisfies this interface when the question needs user clarification.
//
// errors.As(err, &target) where target is *<concrete-type>; the TUI checks
// by errors.As'ing against the concrete cmd/tenant type — but we expose
// the contract here for clarity + test stubbing.
type ResearchClarifyError interface {
	error
	ClarifyQuestions() []string // 1-2 questions to display
	ClarifyOriginal() string    // the original question, for the enriched retry
}

// ResearchHistoryRow is the lightweight summary the /research history table
// shows. Decoupled from the storage Manifest so the TUI package doesn't import
// the research store package.
type ResearchHistoryRow struct {
	ID       string
	Question string
	Status   string
	Started  time.Time
	Duration time.Duration
	Model    string
	Cycles   int
	NumFinds int
	NumRefs  int
	ReplayOf string
}

// ResearchPhase tags the orchestrator's current step. Drives the timeline
// header's color/icon and tells the renderer whether to show "waiting for
// reflection" vs "synthesizing" vs the per-agent grid.
type ResearchPhase string

const (
	ResearchPhasePlanning  ResearchPhase = "planning"     // initial planner call
	ResearchPhaseDispatch  ResearchPhase = "dispatched"   // wave running
	ResearchPhaseReflect   ResearchPhase = "reflecting"   // gap analysis call
	ResearchPhaseSynth     ResearchPhase = "synthesizing" // final report call
	ResearchPhaseDone      ResearchPhase = "done"
	ResearchPhaseError     ResearchPhase = "error"
	ResearchPhaseClarify   ResearchPhase = "clarify"     // C2: paused for user
	ResearchPhaseInterrupt ResearchPhase = "interrupted" // user Esc'd
)

// ResearchAgentRow is one sub-agent's live state in the timeline pane.
// Aggregated by the TUI from per-agent TeamEvents (tool calls + final).
type ResearchAgentRow struct {
	ID        string
	SubQ      string // the sub-question this agent is investigating
	Status    string // "running" | "done" | "error" | "truncated"
	NumTools  int    // total tool dispatches (calls — not pairs)
	NumOK     int    // tool RESULTS that were not errors
	NumErr    int    // tool results that were errors
	ResultLen int    // chars in the agent's final result
}

// ResearchTimelineUpdate is the structured event the orchestrator emits each
// time something changes. The TUI maintains a running snapshot from these.
// One Kind per event; only the matching pointer is set.
type ResearchTimelineUpdate struct {
	Kind  string // "started" | "plan" | "wave" | "agent_status" | "reflect_done" | "synth" | "done"
	Cycle int    // current cycle (1-based); 0 when not yet started
	Total int    // depth setting (max cycles)

	Started *ResearchStartedData
	Plan    *ResearchPlanData
	Wave    *ResearchWaveData
	Agent   *ResearchAgentRow
	Reflect *ResearchReflectData
	Synth   *ResearchSynthData
	Done    *ResearchDoneData
}

// ResearchStartedData fires once at the very top of a research pass.
type ResearchStartedData struct {
	Question string
}

// ResearchPlanData lists the sub-questions a freshly-completed planner call
// produced for the current cycle. Replaces the existing plan list in the
// snapshot (a new cycle's reflect produces a fresh list).
type ResearchPlanData struct {
	SubQuestions []string
}

// ResearchWaveData announces a wave dispatch within a cycle ("dispatched 1–3
// of 5; waiting"). Used for status text only — agent rows come via Agent.
type ResearchWaveData struct {
	From, To, Total int
}

// ResearchReflectData fires when a cycle's reflection finishes — gaps are
// the follow-up questions chosen for the NEXT cycle (empty = done early).
type ResearchReflectData struct {
	Gaps []string
}

// ResearchSynthData brackets the final tools-off synthesis call.
type ResearchSynthData struct {
	Starting bool // true=entering synthesis, false=synthesis done
}

// ResearchDoneData ends the timeline.
type ResearchDoneData struct {
	Status   string // "done" | "error" | "interrupted"
	Error    string
	NumRefs  int // refs in the final report
	NumFinds int // sub-agent findings collected
	Duration time.Duration
}

// GoalControl powers /goal: set an autonomous-loop objective, ask a judge LLM
// after each agent turn whether it's been met, auto-continue until it has been
// (or until a safety cap). Mirrors Claude Code's /goal pattern.
//
// Set replaces any prior goal and returns the first user message the TUI
// should submit to kick things off. Judge evaluates one turn's final response
// against the active condition (no-op when no goal is set; returns met=true
// to short-circuit the auto-continue). Continue returns the prompt the TUI
// should auto-submit to make ONE concrete step forward. Clear stops the loop.
type GoalControl interface {
	Set(ctx context.Context, condition string) (firstPrompt, status string, err error)
	Judge(ctx context.Context, lastResponse string) (met bool, reason string, atCap bool, err error)
	Continue(reason string) (prompt string)
	Show() GoalStatus
	Active() bool
	Clear() string
	// LoopCeiling is the per-turn loop ceiling to apply to turns run WHILE this
	// goal is active, decoupled from the global PlanLoopCeiling (TEN-216): >0
	// override, <0 unlimited, 0 inherit the global ceiling.
	LoopCeiling() int
}

// ReviewControl runs the GStack Layer 3 cascading review (`/review
// <plan.md>`) — three role-specific reviewers (CEO, Engineer, Designer)
// against a plan file, with the structured report appended to the file.
// roles empty = run all reviewers in canonical order; non-empty filters
// to the listed roles ("ceo", "eng", "design") and errors on unknown.
type ReviewControl interface {
	Review(ctx context.Context, planPath string, roles []string) (report string, err error)
}

// GoalStatus is the snapshot of an active goal — what /goal show prints
// and (later) what the status bar overlay reads.
type GoalStatus struct {
	Active     bool
	Condition  string
	Turns      int       // # of agent turns since the goal was set
	MaxTurns   int       // hard cap (loop bails out at this many)
	LastJudge  string    // judge's most recent verdict reason
	LastEval   time.Time // when the judge last ran
	Started    time.Time
	ElapsedFmt string // human-formatted elapsed time, set by impl
	Met        bool   // true once the judge said yes
	// GoalLoopCeiling is the per-turn loop ceiling applied while the goal runs
	// (TEN-216): >0 override, <0 unlimited, 0 = inherit the global ceiling.
	GoalLoopCeiling int
}

// AgentControl powers /agents: list named sub-agents, add/edit/remove them
// (with per-profile model + soul), and show a single one's full identity.
// Mutations apply LIVE: the implementation invalidates the runtime's
// per-profile router cache so the next spawn picks up the new settings.
type AgentControl interface {
	List() ([]AgentInfo, error)
	Add(name, provider, model, description, soul string) (status string, err error)
	// SetModel swaps just the provider/model pinning, preserving description
	// + soul. Live: invalidates the runtime's per-profile router cache so the
	// next spawn picks up the new model.
	SetModel(name, provider, model string) (status string, err error)
	// Rename moves a profile to a new name lossless. Refuses if the target
	// name already exists.
	Rename(oldName, newName string) (status string, err error)
	SetSoul(name, soul string) (status string, err error)
	Remove(name string) (status string, err error)
	Show(name string) (AgentDetail, error)
}

// AgentInfo is the lightweight summary row for /agents listings.
type AgentInfo struct {
	Name        string
	Provider    string // provider id (e.g. "zai", "dgx")
	Model       string // resolved model — profile override > provider default
	Description string
	HasSoul     bool // whether the profile has a custom soul markdown
	Builtin     bool // shipped in the binary (read-only; inherits the primary model)
	Valid       bool // false = misconfigured (missing provider / model)
}

// AgentDetail is the full profile for /agents show — adds the soul body.
type AgentDetail struct {
	Name        string
	Provider    string
	Model       string
	Description string
	Soul        string // the full markdown identity body
}

// ReconnectControl starts a background cascading reconnect when the generation
// endpoint is unreachable (idempotent — a second call while already retrying
// is a no-op). Progress is reported via the System feed.
type ReconnectControl interface {
	OnGenerationDown()
}

// Run starts the full-screen TUI and blocks until the user quits.
//
// Mouse capture is ON by default: the wheel scrolls the TUI panes and you stay
// locked in the alt screen (capture OFF would send wheel events to the
// terminal's scrollback, scrolling you "out of" the TUI). To SELECT text for
// copy/paste, use the standard terminal convention: hold the bypass modifier
// while dragging — ⌥ Option on macOS (iTerm2/Terminal.app), Shift on most Linux
// terminals — which selects natively even while the app captures the mouse.
// `/mouse off` drops capture entirely for plain-drag selection (TEN-181).
func Run(ctx context.Context, cfg Config) error {
	p := tea.NewProgram(newModel(ctx, cfg), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// --- styles ---

var (
	cUser          = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	cAgent         = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	cDim           = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	cTool          = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	cErr           = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	cOK            = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	cSys           = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	statusBarStyle = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15"))
	feedTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	paneBorder     = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, true, false, false).BorderForeground(lipgloss.Color("238"))

	// List / command formatting: high-contrast so /tools, /help, /skills
	// are scannable rather than a wall of dim grey.
	cHeading = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14")) // section titles (cyan)
	cKey     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")) // commands, plugin names (blue)
	cName    = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))           // active item name (near-white)
	cOnMark  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")) // ● enabled (green)
	cOffMark = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))           // ○ disabled (grey)

	// chatGutter is the left breathing room for the chat pane so the first
	// characters don't sit flush against the terminal edge.
	chatGutter = 2
	chatPad    = lipgloss.NewStyle().MarginLeft(chatGutter)
)

// --- messages ---

type eventMsg agent.Event
type sysMsg string
type teamEvtMsg TeamEvent
type researchTimelineMsg ResearchTimelineUpdate
type approvalMsg ApprovalRequest

// goalJudgeMsg carries the result of one judge call so the Update goroutine
// can decide whether to auto-continue, stop on cap, or finalize.
type goalJudgeMsg struct {
	met    bool
	reason string
	atCap  bool
	err    error
}
type compactDoneMsg struct {
	before, after int
	err           error
}
type expandDoneMsg struct {
	exp *agent.CompactionExpansion
	err error
}
type turnDoneMsg struct {
	res *agent.TurnResult
	err error
}
type researchDoneMsg struct {
	report string
	err    error
}

// --- model ---

// sysChatMsg is dispatched from tea.Cmd goroutines that need to post
// to the system-chat feed (e.g. after picker selection, after async
// add-cloud follow-ups). The Update handler unwraps it via m.sysChat.
type sysChatMsg struct {
	text string
}

// safeErr renders an error for display, returning "(none)" for nil so
// status strings stay readable when a path is healthy.
func safeErr(err error) string {
	if err == nil {
		return "(none)"
	}
	return err.Error()
}

type chatMsg struct {
	role    string // "user" | "assistant" | "system"
	content string // PLAIN text — used for the copyable transcript
	// rendered is an optional pre-styled (ANSI) version shown in the chat
	// pane. When empty, content is shown as-is. Keeping content plain means
	// Ctrl-Y copies clean text, never escape codes.
	rendered string
}

type model struct {
	ctx context.Context
	cfg Config

	chat  viewport.Model
	feed  viewport.Model
	ta    textarea.Model  // multiline chat input
	input textinput.Model // single-line for secrets/passwords (configure, setup)
	spin  spinner.Model

	// Input history (↑/↓ recall past prompts/commands, readline-style). histIdx
	// is the cursor into history; == len(history) means "the live draft" (held in
	// histDraft so it's restored when you press ↓ back past the newest entry).
	history   []string
	histIdx   int
	histDraft string
	mouseOn   bool // wheel-capture on? (default true — wheel scrolls the TUI; ⌥/Shift-drag selects)
	// selectMode (Ctrl+S) temporarily drops mouse capture so a PLAIN drag
	// highlights text for copy in ANY terminal (no modifier-key quirks).
	// Esc/Ctrl+S restores capture. (TEN-181)
	selectMode bool

	msgs      []chatMsg
	feedLines []string
	streaming bool // an assistant reply is currently streaming
	// assistantSealed is set once turnDoneMsg has finalized the turn's answer
	// and cleared at the next submit(). While sealed, the live EventToken/
	// EventAssistant handlers no-op — they'd otherwise append the SAME text a
	// second time when a trailing event is drained AFTER turnDoneMsg already
	// reconciled the bubble (the /goal inline double-post race, TEN-217).
	assistantSealed bool
	busy            bool      // a turn is in flight
	budgetPct       int       // last context-budget utilization (% of writable budget)
	budgetUsed      int       // absolute tokens in the assembled context
	budgetCap       int       // writable-budget cap (tokens)
	sessionTok      int       // cumulative tokens (in+out) for the MAIN agent this session
	sessionTokIn    int       // cumulative INPUT tokens for the MAIN agent this session
	sessionTokOut   int       // cumulative OUTPUT tokens for the MAIN agent this session
	teamTok         int       // cumulative tokens (in+out) for spawned sub-agents
	reqStart        time.Time // wall-clock start of the in-flight turn (display-only live timer)
	lastTool        string
	width           int
	height          int
	ready           bool
	err             string
	// follow flags: when true the pane sticks to the bottom as new content
	// arrives; scrolling up turns it off so the view stays put (so a long
	// /tools list doesn't get yanked away). Paging/wheeling back to the
	// bottom re-engages follow.
	chatFollow bool
	feedFollow bool
	// pending holds dangerous actions awaiting /approve or /deny, oldest
	// first (tool calls are sequential, so this is normally 0–1 deep).
	pending []ApprovalRequest
	// turnCancel cancels the in-flight turn; nil when idle. Esc invokes it to
	// interrupt a runaway loop without quitting the app.
	turnCancel context.CancelFunc
	// interrupted records that the user stopped the current turn, so the
	// turnDone handler shows "interrupted" rather than an error.
	interrupted bool
	// pendingClarify carries the C2 clarify state: when /research returned
	// a ClarifyNeededError, we display the questions and treat the user's
	// NEXT plain message (not a slash command) as the answer — then run
	// research again with the enriched query.
	pendingClarify *pendingClarifyState
	// configureSession carries the interactive `/configure <skill>` state.
	// When non-nil, the user's NEXT plain chat input is consumed as the
	// answer to the currently-active field, rather than sent to the
	// agent. Cleared on success, `/cancel`, or error. Mirrors the
	// pendingClarify pattern but for skill setup.
	configureSession *configureSessionState
	// secretEntry, if non-nil, means the operator just picked a service in the
	// no-arg `/configure` picker and the NEXT plain input is consumed as that
	// service's key/token (the input is masked while active). Cleared on save,
	// `/cancel`, or Esc. Mirrors configureSession but for a single secret.
	secretEntry *secretEntryState
	// setupEntry, if non-nil, captures the NEXT plain input as a value for the
	// in-flight `/setup` step (masked for the API-key step). Mirrors secretEntry
	// but carries an apply closure so each step routes to the right setter and
	// reopens the menu. Cleared on apply, `/cancel`, Esc, or a slash command.
	setupEntry *setupEntryState
	// picker, if non-nil, intercepts ALL key events for arrow-key list
	// selection (currently used after /model add-cloud to let the operator
	// pick a model variant from the provider's live catalog). Cleared on
	// Enter (select) or Esc (cancel). Render shows the items above the
	// input area; the input area is hidden while picker is active.
	picker *listPicker
	// researchTimeline is the live state for /research progress, fed by
	// ResearchTimeline updates from the orchestrator + TeamEvents from the
	// spawned sub-agents. Non-nil only while a research run is in flight;
	// cleared on researchDoneMsg so the timeline pane disappears when done.
	researchTimeline *researchTimelineState
	// goalAutoActive marks that the next turn was kicked off automatically
	// by the /goal loop (via Goals.Continue or the initial Set). Used to
	// avoid an infinite cancel-recovery loop on Esc — when the user
	// interrupts, we clear the goal so we don't immediately re-spawn.
	goalAutoActive bool
}

// pendingClarifyState holds the in-flight clarification a /research call
// kicked off. The orchestrator pauses here until the user answers, then we
// build an enriched query and continue with ResearchAfterClarify.
type pendingClarifyState struct {
	Original  string   // the original /research question
	Questions []string // 1-2 questions the user is being asked
}

// researchTimelineState aggregates the structured timeline updates + the
// per-agent tool activity into one consistent snapshot for the renderer.
// Updated under the model's UI goroutine (Update reads it, applyTeamEvent
// and applyResearchTimeline mutate it). No mutex needed — single-threaded
// in the bubbletea Update loop.
type researchTimelineState struct {
	Question  string
	Phase     ResearchPhase
	StartedAt time.Time
	Cycle     int
	Total     int
	// Plan: sub-questions for the CURRENT cycle (replaced on each cycle).
	Plan []string
	// Agents: rolled-up per-sub-agent state. agentByID is a parallel index
	// for O(1) updates when a TeamEvent arrives.
	Agents    []*ResearchAgentRow
	agentByID map[string]*ResearchAgentRow
	// Wave status text — "dispatched 1–3 of 5; waiting…"
	WaveStatus string
	// LastReflectGaps is the most recent reflect's gap list (NEXT cycle's plan).
	LastReflectGaps []string
	// Done details (filled at end).
	DoneStatus   string
	DoneError    string
	DoneRefs     int
	DoneFinds    int
	DoneDuration time.Duration
}

func newModel(ctx context.Context, cfg Config) *model {
	ta := textarea.New()
	ta.Placeholder = "Message the agent…"
	ta.Prompt = "┃ "
	ta.ShowLineNumbers = false
	ta.SetHeight(3)
	ta.CharLimit = 8000
	ta.Focus()
	ta.FocusedStyle.Base = lipgloss.NewStyle()       // no background — blends with alt screen
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle() // no background on cursor line either
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	ta.FocusedStyle.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	// Enter sends; Shift+Enter/Ctrl+Enter inserts a newline.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+enter"),
		key.WithHelp("shift+enter", "newline"),
	)
	ti := textinput.New()
	ti.CharLimit = 8000
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return &model{
		ctx:        ctx,
		cfg:        cfg,
		ta:         ta,
		input:      ti, // reserved for secret/password entry (configure, setup)
		spin:       sp,
		chatFollow: true,
		feedFollow: true,
		mouseOn:    true, // matches Run's WithMouseCellMotion default
		msgs:       []chatMsg{intro(cfg)},
	}
}

// intro is the welcome line: plain for copy, dimmed for display.
func intro(cfg Config) chatMsg {
	s := fmt.Sprintf("tenant — backend=%s model=%s agent=%s · type /help for commands, watch the feed on the right.",
		cfg.Backend, cfg.Model, cfg.AgentID)
	return chatMsg{role: "system", content: s, rendered: cDim.Render(s)}
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spin.Tick, m.listen()}
	if m.cfg.System != nil {
		cmds = append(cmds, m.listenSystem())
	}
	if m.cfg.Approvals != nil {
		cmds = append(cmds, m.listenApprovals())
	}
	if m.cfg.TeamEvents != nil {
		cmds = append(cmds, m.listenTeam())
	}
	if m.cfg.ResearchTimeline != nil {
		cmds = append(cmds, m.listenResearchTimeline())
	}
	return tea.Batch(cmds...)
}

// listenTeam drains sub-agent events; re-issued after each so team
// activity keeps flowing into the feed + counter.
func (m *model) listenTeam() tea.Cmd {
	return func() tea.Msg {
		te, ok := <-m.cfg.TeamEvents
		if !ok {
			return nil
		}
		return teamEvtMsg(te)
	}
}

// listenResearchTimeline drains C4 structured timeline updates from the
// orchestrator; re-issued after each so the timeline pane stays live.
func (m *model) listenResearchTimeline() tea.Cmd {
	return func() tea.Msg {
		u, ok := <-m.cfg.ResearchTimeline
		if !ok {
			return nil
		}
		return researchTimelineMsg(u)
	}
}

// listen blocks on the agent event channel and surfaces the next event
// as a tea.Msg; re-issued after each event so the feed stays live.
func (m *model) listen() tea.Cmd {
	return func() tea.Msg {
		e, ok := <-m.cfg.Events
		if !ok {
			return nil
		}
		return eventMsg(e)
	}
}

// listenSystem drains background feed lines (e.g. self-improvement
// job runs); re-issued after each so the feed stays live.
func (m *model) listenSystem() tea.Cmd {
	return func() tea.Msg {
		s, ok := <-m.cfg.System
		if !ok {
			return nil
		}
		return sysMsg(s)
	}
}

// listenApprovals blocks on the approval channel; re-issued after each so
// further dangerous actions keep prompting.
func (m *model) listenApprovals() tea.Cmd {
	return func() tea.Msg {
		r, ok := <-m.cfg.Approvals
		if !ok {
			return nil
		}
		return approvalMsg(r)
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	// Picker-start: dispatched by a tea.Cmd after a successful
	// /model add-cloud (or any future "pick from list" flow). Set the
	// picker BEFORE the key handler runs so the very next keypress
	// already routes to it.
	if start, ok := msg.(pickerStartMsg); ok {
		m.picker = start.picker
		return m, nil
	}
	// Async post to system chat (from picker callbacks, list fetchers,
	// etc.). Just appends to the chat pane.
	if sc, ok := msg.(sysChatMsg); ok {
		m.sysChat(sc.text)
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		m.ready = true
	case tea.KeyMsg:
		// Picker mode: ALL keys route to picker — no slash dispatch, no
		// input editing, no scroll. Picker selection (Enter) returns the
		// next tea.Cmd to chain (e.g. apply the chosen variant + refresh
		// status). Cancel (Esc) just clears.
		if m.picker != nil {
			if cmd := m.handlePickerKey(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return m, tea.Batch(cmds...)
		}
		switch msg.String() {
		case "ctrl+s":
			// Toggle SELECT MODE: drop mouse capture so a plain click-drag
			// highlights text for copy in any terminal; Ctrl+S/Esc restores.
			// (TEN-181 — modifier-bypass keys vary per terminal, this doesn't.)
			if m.selectMode {
				m.selectMode = false
				m.sysChat("✂ select mode OFF — mouse back to the TUI (wheel scrolls panes)")
				if m.mouseOn {
					cmds = append(cmds, tea.Cmd(tea.EnableMouseCellMotion))
				}
			} else {
				m.selectMode = true
				m.sysChat("✂ SELECT MODE — drag to highlight, ⌘C to copy. Ctrl+S or Esc to return. (wheel scrolls the terminal while active)")
				cmds = append(cmds, tea.Cmd(tea.DisableMouse))
			}
			return m, tea.Batch(cmds...)
		case "ctrl+c", "esc":
			// Esc during a masked /configure key entry cancels it (and unmasks) —
			// the natural "bail out" key, matching every picker. Must run before
			// the busy/idle branches so the operator is never stranded masked.
			if m.secretEntry != nil {
				m.clearSecretEntry()
				m.sysChat("configuration cancelled")
				break
			}
			if m.setupEntry != nil {
				m.clearSetupEntry()
				m.sysChat("setup cancelled")
				break
			}
			if m.selectMode {
				m.selectMode = false
				m.sysChat("✂ select mode OFF — mouse back to the TUI")
				if m.mouseOn {
					cmds = append(cmds, tea.Cmd(tea.EnableMouseCellMotion))
				}
				break
			}
			// Neither key quits — only /exit (or /quit) closes the app, so a
			// stray Ctrl-C never loses your session. While a turn is running
			// they interrupt it (the runaway-loop escape hatch); when idle they
			// just remind you how to leave.
			if m.busy {
				m.interrupt()
			} else {
				m.sysChat("type /exit to quit (Ctrl-C and Esc won't close the app)")
			}
		case "ctrl+y":
			m.copyTranscript()
		case "enter":
			var q string
			if m.secretEntry != nil || m.setupEntry != nil || m.configureSession != nil {
				q = strings.TrimSpace(m.input.Value())
			} else {
				q = strings.TrimSpace(m.ta.Value())
			}
			switch {
			case m.busy && q != "" && !strings.HasPrefix(q, "/"):
				// A plain message typed mid-turn is a soft interrupt: fold it
				// into the running turn (the agent addresses it, then resumes
				// unless it overrides the task). Slash commands still flow to
				// submit() so /approve, /deny, /permissions work mid-turn.
				m.interject(q)
			case !m.busy || strings.HasPrefix(q, "/"):
				if cmd := m.submit(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		case "up":
			// Readline-style history: recall the previous prompt/command when the
			// cursor is on the FIRST line of the input; otherwise let the textarea
			// move the cursor up within a multiline draft (TEN-181).
			if m.inChatInput() && m.ta.Line() == 0 && m.historyPrev() {
				m.refresh()
				return m, tea.Batch(cmds...)
			}
		case "down":
			// Mirror: recall the next entry only from the LAST line of the input.
			if m.inChatInput() && m.ta.Line() == m.ta.LineCount()-1 && m.historyNext() {
				m.refresh()
				return m, tea.Batch(cmds...)
			}
		case "pgup":
			m.scrollChat(-m.pageStep())
		case "pgdown":
			m.scrollChat(m.pageStep())
		case "shift+up":
			m.scrollChat(-1)
		case "shift+down":
			m.scrollChat(1)
		}
	case tea.MouseMsg:
		// Wheel scrolls the pane under the cursor (chat left, feed right).
		if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
			const step = 3
			up := msg.Button == tea.MouseButtonWheelUp
			if msg.X < m.chat.Width {
				if up {
					m.chat.LineUp(step)
				} else {
					m.chat.LineDown(step)
				}
				m.chatFollow = m.chat.AtBottom()
			} else {
				if up {
					m.feed.LineUp(step)
				} else {
					m.feed.LineDown(step)
				}
				m.feedFollow = m.feed.AtBottom()
			}
		}
	case eventMsg:
		m.applyEvent(agent.Event(msg))
		cmds = append(cmds, m.listen()) // keep listening
	case sysMsg:
		m.appendFeed(cSys.Render("⚙ " + string(msg)))
		cmds = append(cmds, m.listenSystem())
	case teamEvtMsg:
		m.applyTeamEvent(TeamEvent(msg))
		cmds = append(cmds, m.listenTeam())
	case researchTimelineMsg:
		m.applyResearchTimeline(ResearchTimelineUpdate(msg))
		cmds = append(cmds, m.listenResearchTimeline())
	case approvalMsg:
		r := ApprovalRequest(msg)
		m.pending = append(m.pending, r)
		m.showApproval(r)
		m.appendFeed(cErr.Render("⚠ approval needed: " + r.Category))
		cmds = append(cmds, m.listenApprovals())
	case compactDoneMsg:
		switch {
		case msg.err != nil:
			m.sysChat("compaction failed: " + msg.err.Error())
		case msg.before == msg.after:
			m.sysChat("nothing to compact yet (conversation is still short)")
		default:
			m.sysChat(fmt.Sprintf("compacted context: %d → %d messages", msg.before, msg.after))
		}
	case expandDoneMsg:
		switch {
		case msg.err != nil:
			m.sysChat("expand failed: " + msg.err.Error())
		case msg.exp == nil:
			m.sysChat("nothing has been compacted yet — no summary to expand")
		default:
			m.sysChat(renderExpansion(msg.exp))
		}
	case turnDoneMsg:
		m.busy = false
		m.streaming = false
		// Seal the turn: finalizeAssistant (below) is now the authoritative
		// source for this answer, so any trailing EventAssistant/EventToken
		// still queued from the just-finished turn must be ignored rather than
		// re-appended (TEN-217). Cleared at the next submit().
		m.assistantSealed = true
		if m.turnCancel != nil {
			m.turnCancel()
			m.turnCancel = nil
		}
		switch {
		case m.interrupted:
			m.sysChatStyled("⏹ interrupted — agent stopped.",
				cSys.Render("⏹ interrupted — agent stopped. Ask me something else."))
			m.appendFeed(cSys.Render("⏹ interrupted"))
		case errors.Is(msg.err, modelpkg.ErrEndpointDown):
			// Generation is unreachable — surface it and kick off the
			// background cascading reconnect (it reports to the feed).
			m.appendFeed(cErr.Render("✗ generation endpoint unreachable"))
			m.sysChat("generation endpoint unreachable — auto-reconnecting (watch the activity feed). Resend once it's back.")
			if m.cfg.Reconnect != nil {
				m.cfg.Reconnect.OnGenerationDown()
			}
		case msg.err != nil:
			m.appendFeed(cErr.Render("✗ turn error: " + clip(msg.err.Error(), 80)))
		default:
			// Replay the authoritative final answer from the result. Live
			// token events build the bubble as they stream, but they're
			// dropped under load (non-blocking Observer), so reconcile here:
			// the TurnResult.Response is the source of truth.
			if msg.res != nil {
				m.finalizeAssistant(msg.res.Response)
			}
			// /goal autonomous loop — if a goal is active and this turn was
			// NOT a user interrupt, kick off the judge to decide whether to
			// auto-continue. The judge runs in a tea.Cmd off the UI goroutine.
			// Esc / Ctrl-C → m.interrupted=true → goal stops (clear handled below).
			if m.cfg.Goals != nil && m.cfg.Goals.Active() && m.pendingClarify == nil &&
				m.researchTimeline == nil { // don't double-loop during /research
				resp := ""
				if msg.res != nil {
					resp = msg.res.Response
				}
				gc := m.cfg.Goals
				ctx := m.ctx
				cmds = append(cmds, func() tea.Msg {
					met, reason, atCap, err := gc.Judge(ctx, resp)
					return goalJudgeMsg{met: met, reason: reason, atCap: atCap, err: err}
				})
			}
		}
		// Interrupt during a goal loop → tear the goal down so we don't
		// immediately re-spawn the next turn.
		if m.interrupted && m.cfg.Goals != nil && m.cfg.Goals.Active() {
			status := m.cfg.Goals.Clear()
			m.appendFeed(cSys.Render("🎯 " + status + " (interrupted)"))
		}
		m.interrupted = false
	case researchDoneMsg:
		m.busy = false
		if m.turnCancel != nil {
			m.turnCancel()
			m.turnCancel = nil
		}
		interrupted := m.interrupted
		m.interrupted = false
		switch {
		case interrupted:
			m.appendFeed(cSys.Render("⏹ research interrupted"))
			m.sysChat("⏹ research interrupted")
		case msg.err != nil:
			// C2 — clarifier asked the user something. Display the questions
			// and wait for the next plain message as the answer; on receipt,
			// build the enriched query and re-run research.
			var clar ResearchClarifyError
			if errors.As(msg.err, &clar) {
				qs := clar.ClarifyQuestions()
				m.pendingClarify = &pendingClarifyState{
					Original:  clar.ClarifyOriginal(),
					Questions: qs,
				}
				var b strings.Builder
				b.WriteString("🤔 This question is a bit vague — quick clarification:\n")
				for i, q := range qs {
					fmt.Fprintf(&b, "  %d. %s\n", i+1, q)
				}
				b.WriteString("\nReply with your answer and I'll proceed. ")
				b.WriteString("Type /research! <q> to skip clarification for any query, or /cancel-clarify to drop this prompt.")
				m.msgs = append(m.msgs, chatMsg{role: "assistant", content: b.String()})
				m.appendFeed(cSys.Render("⏸ research paused — waiting for clarification"))
			} else {
				m.appendFeed(cErr.Render("✗ research failed: " + clip(msg.err.Error(), 80)))
				m.sysChat("research failed: " + msg.err.Error())
			}
		default:
			m.appendFeed(cOK.Render("✦ research report ready"))
			m.msgs = append(m.msgs, chatMsg{role: "assistant", content: msg.report})
		}
		// C4: tear down the live timeline pane on terminal events (clarify
		// pause is NOT terminal — we kept the pane up to show the partial
		// state if the user resumes). For clarify the pane disappears when
		// the user submits the answer and a fresh "started" arrives.
		var clar ResearchClarifyError
		if msg.err == nil || !errors.As(msg.err, &clar) {
			m.researchTimeline = nil
		}
	case goalJudgeMsg:
		// /goal autonomous-loop verdict landed. Three terminal outcomes
		// (met / cap / error) clear the goal + post a system message; the
		// not-met-under-cap path auto-submits the next turn.
		gc := m.cfg.Goals
		switch {
		case msg.err != nil:
			m.sysChat("🎯 goal judge failed: " + clip(msg.err.Error(), 100) + " — stopping the loop")
			if gc != nil {
				_ = gc.Clear()
			}
		case msg.met:
			m.appendFeed(cOK.Render("🎯 ✦ goal met"))
			m.sysChat("🎯 ✦ goal met — autonomous loop complete.")
			if gc != nil {
				_ = gc.Clear()
			}
		case msg.atCap:
			m.appendFeed(cSys.Render("🎯 ⚠ goal hit turn cap"))
			m.sysChat("🎯 ⚠ goal hit the turn cap — stopping. Last judge: " + clip(msg.reason, 200))
			if gc != nil {
				_ = gc.Clear()
			}
		default:
			// Auto-continue: build the next prompt and submit it as if the
			// user typed it. Reuses the existing submit() path so cancellation,
			// streaming, and the activity feed all work normally.
			if gc == nil {
				break
			}
			prompt := gc.Continue(msg.reason)
			m.ta.SetValue(prompt)
			m.goalAutoActive = true
			if subCmd := m.submit(); subCmd != nil {
				cmds = append(cmds, subCmd)
			}
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
	}
	// (sysChatMsg is caught earlier in Update at the picker/async layer.)

	var icmd tea.Cmd
	m.ta, icmd = m.ta.Update(msg)
	cmds = append(cmds, icmd)
	m.refresh()
	return m, tea.Batch(cmds...)
}

// inChatInput reports whether the multiline chat textarea (not a masked secret /
// setup / configure single-line prompt) currently owns input — i.e. whether
// ↑/↓ history recall applies.
func (m *model) inChatInput() bool {
	return m.secretEntry == nil && m.setupEntry == nil && m.configureSession == nil
}

// pushHistory records a submitted prompt/command for ↑/↓ recall (dedupes a
// consecutive repeat) and resets the cursor to the live draft. (TEN-181)
func (m *model) pushHistory(s string) {
	s = strings.TrimSpace(s)
	if s != "" {
		if n := len(m.history); n == 0 || m.history[n-1] != s {
			m.history = append(m.history, s)
		}
	}
	m.histIdx = len(m.history)
	m.histDraft = ""
}

// historyPrev recalls an older entry; returns true if it changed the input
// (so the caller consumes the key instead of moving the textarea cursor).
func (m *model) historyPrev() bool {
	if len(m.history) == 0 || m.histIdx == 0 {
		return false
	}
	if m.histIdx >= len(m.history) {
		m.histDraft = m.ta.Value() // entering history from the live draft — save it
	}
	m.histIdx--
	m.setInputValue(m.history[m.histIdx])
	return true
}

// historyNext recalls a newer entry, restoring the live draft past the newest.
func (m *model) historyNext() bool {
	if m.histIdx >= len(m.history) {
		return false
	}
	m.histIdx++
	if m.histIdx == len(m.history) {
		m.setInputValue(m.histDraft)
	} else {
		m.setInputValue(m.history[m.histIdx])
	}
	return true
}

func (m *model) setInputValue(s string) {
	m.ta.SetValue(s)
	m.ta.CursorEnd()
}

func (m *model) submit() tea.Cmd {
	var q string
	if m.secretEntry != nil || m.setupEntry != nil || m.configureSession != nil {
		q = strings.TrimSpace(m.input.Value())
	} else {
		q = strings.TrimSpace(m.ta.Value())
		m.pushHistory(q) // record prompts + commands for ↑/↓ recall (TEN-181)
	}
	m.streaming = false       // defensive reset: new turn gets a fresh assistant message
	m.assistantSealed = false // unseal: this turn's live events may build a bubble (TEN-217)
	// TEN-65 follow-up fix: empty input in configure-session mode is
	// meaningful — it means "use the Default / skip if optional".
	// Before this, the early-return at the top swallowed empty Enter
	// and the operator was locked out of the session (they couldn't
	// take the default or skip optional fields). The session intercept
	// MUST run before the empty short-circuit.
	// Secret entry from the /configure picker: consume the next plain input as
	// the chosen service's key. Do NOT echo it to chat (it's a secret).
	if m.secretEntry != nil && !strings.HasPrefix(q, "/") {
		m.input.Reset()
		m.chatFollow = true
		return m.saveSecretEntry(q)
	}
	// A slash command (other than /cancel, which has its own handler) while a key
	// entry is armed aborts it + unmasks, so the operator never types a command
	// into a hidden, still-armed field.
	if m.secretEntry != nil && strings.HasPrefix(q, "/") && q != "/cancel" {
		m.clearSecretEntry()
		m.sysChat("configure: key entry cancelled")
	}
	// /setup step entry: capture the next plain input as the step's value.
	if m.setupEntry != nil && !strings.HasPrefix(q, "/") {
		se := m.setupEntry
		m.clearSetupEntry()
		m.chatFollow = true
		if se.apply != nil {
			return se.apply(strings.TrimSpace(q))
		}
		return nil
	}
	if m.setupEntry != nil && strings.HasPrefix(q, "/") && q != "/cancel" {
		m.clearSetupEntry()
		m.sysChat("setup: cancelled")
	}
	if m.configureSession != nil && !strings.HasPrefix(q, "/") {
		m.ta.Reset()
		m.chatFollow = true
		m.msgs = append(m.msgs, chatMsg{role: "user", content: q})
		m.handleConfigureAnswer(q)
		return m.advanceConfigureSession()
	}
	if q == "" {
		return nil
	}
	m.ta.Reset()
	m.chatFollow = true // sending re-engages follow so you see the reply/output
	if strings.HasPrefix(q, "/") {
		return m.handleSlash(q)
	}
	// C2 — if /research paused for clarification, this message IS the answer.
	// Fold it into the original question and resume research (clarify off).
	if m.pendingClarify != nil {
		pc := m.pendingClarify
		m.pendingClarify = nil
		m.msgs = append(m.msgs, chatMsg{role: "user", content: q})
		enriched := strings.TrimSpace(pc.Original) + "\n\nAdditional context from user: " + q
		m.busy = true
		m.reqStart = time.Now()
		m.interrupted = false
		m.chatFollow = true
		m.appendFeed(cSys.Render("🔎 resuming research with your clarification"))
		rc := m.cfg.Research
		rctx, cancel := context.WithCancel(m.ctx)
		m.turnCancel = cancel
		return func() tea.Msg {
			report, err := rc.ResearchAfterClarify(rctx, enriched)
			cancel()
			return researchDoneMsg{report: report, err: err}
		}
	}
	m.msgs = append(m.msgs, chatMsg{role: "user", content: q})
	m.busy = true
	m.reqStart = time.Now()
	m.interrupted = false
	m.err = ""
	// Run the turn off the UI goroutine; the agent's Observer streams
	// events into m.cfg.Events which listen() drains. The turn runs under a
	// per-turn cancel context so Esc can interrupt it (see interrupt()).
	ag := m.cfg.Agent
	turnCtx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	req := agent.TurnRequest{UserQuery: q}
	// While a /goal loop is active, decouple this turn's iteration budget from
	// the global loop ceiling so a long autonomous run can iterate freely
	// without raising the ceiling every normal turn shares (TEN-216).
	if m.cfg.Goals != nil && m.cfg.Goals.Active() {
		req.LoopCeiling = m.cfg.Goals.LoopCeiling()
	}
	return func() tea.Msg {
		res, err := ag.Turn(turnCtx, req)
		cancel() // release ctx resources once the turn returns
		return turnDoneMsg{res: res, err: err}
	}
}

// finalizeAssistant reconciles the chat with the authoritative final answer
// from the TurnResult. Streamed token events build the assistant bubble live,
// but they can be dropped under load (non-blocking Observer), so we replace
// the last assistant bubble's text with the full response — or append one if
// no bubble was created (all tokens dropped / non-stream path).
func (m *model) finalizeAssistant(response string) {
	if strings.TrimSpace(response) == "" {
		return
	}
	if n := len(m.msgs); n > 0 && m.msgs[n-1].role == "assistant" {
		m.msgs[n-1].content = response
		m.msgs[n-1].rendered = ""
		return
	}
	m.msgs = append(m.msgs, chatMsg{role: "assistant", content: response})
}

// handleMCP powers /mcp: connect/list/remove remote MCP connector servers
// (TEN-164). `add` runs the OAuth browser flow as a tea.Cmd closure (off the UI
// goroutine, mirroring the /configure async pattern); its result returns via
// sysChatMsg. list/remove are synchronous.
func (m *model) handleMCP(arg string) tea.Cmd {
	if m.cfg.MCP == nil {
		m.sysChat("remote MCP connectors not available in this session")
		return nil
	}
	fields := strings.Fields(arg)
	sub, rest := "", ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
	}
	switch sub {
	case "", "list":
		servers := m.cfg.MCP.List()
		if len(servers) == 0 {
			m.sysChat("no remote MCP servers connected. Add one:  /mcp add https://mcp.atlassian.com/v1/mcp")
			return nil
		}
		var b strings.Builder
		b.WriteString("Remote MCP connectors:\n")
		for _, s := range servers {
			var state string
			switch {
			case s.Enabled:
				state = fmt.Sprintf("on, %d tools", s.ToolCount)
			case s.ToolCount > 0:
				state = fmt.Sprintf("connected, %d tools (disabled)", s.ToolCount)
			default:
				state = "registered — /enable " + s.Label + " to connect"
			}
			b.WriteString(fmt.Sprintf("  • %-26s %s  [%s]\n", s.Label, s.URL, state))
		}
		m.sysChat(strings.TrimRight(b.String(), "\n"))
		return nil
	case "add":
		url := strings.TrimSpace(rest)
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			m.sysChat("usage: /mcp add <https://server/mcp>   (e.g. https://mcp.atlassian.com/v1/mcp)")
			return nil
		}
		m.sysChat("🌐 connecting to " + url + " — a browser will open to authorize. If the server shows two screens (approve, then pick a site), click through once each; don't re-click.")
		ctl := m.cfg.MCP
		return func() tea.Msg {
			info, err := ctl.Add(url) // BLOCKS: DCR + browser + callback
			if err != nil {
				return sysChatMsg{text: "✗ MCP connect failed: " + err.Error()}
			}
			return sysChatMsg{text: fmt.Sprintf("✓ connected %s — %d tools live (gated; /tools to review, /disable %s to turn off)", info.Label, info.ToolCount, info.Label)}
		}
	case "remove", "rm", "forget":
		target := strings.TrimSpace(rest)
		if target == "" {
			m.sysChat("usage: /mcp remove <url|label>")
			return nil
		}
		removed, err := m.cfg.MCP.Remove(target)
		switch {
		case err != nil:
			m.sysChat("✗ " + err.Error())
		case !removed:
			m.sysChat("no remote MCP server matched " + target)
		default:
			m.sysChat("✓ removed " + target)
		}
		return nil
	default:
		m.sysChat("usage: /mcp [list] | /mcp add <url> | /mcp remove <url|label>")
		return nil
	}
}

// interject hands a mid-turn message to the running agent (the Hermes-style
// soft interrupt). The turn keeps running; the agent folds the message in,
// addresses it, and resumes unless it overrides the task. Esc remains the
// hard stop.
func (m *model) interject(q string) {
	m.ta.Reset()
	m.chatFollow = true
	m.msgs = append(m.msgs, chatMsg{role: "user", content: q})
	m.appendFeed(cSys.Render("⏎ interjected — agent will address it, then resume"))
	if m.cfg.Agent != nil {
		m.cfg.Agent.Interject(q)
	}
}

// interrupt cancels the in-flight turn — the escape hatch when the agent gets
// stuck in a loop. The agent loop checks ctx at every boundary and the model
// backends map a cancelled ctx to ErrCancelled, so generation stops promptly;
// a turn paused on an approval also unblocks (the broker selects on
// ctx.Done()). The UI returns to idle when the turnDoneMsg arrives.
func (m *model) interrupt() {
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	m.pending = nil // any pending approval is moot once the turn is cancelled
	m.interrupted = true
	m.appendFeed(cSys.Render("⏹ interrupting…"))
}

// handleEval powers /eval: view + live-tune the nightly eval schedule
// (TEN-196). Mutations persist via the control and re-arm the scheduler in
// place; "now" rides the improve scheduler's own goroutine, so the run lands
// in the feed and trend log exactly like a scheduled one.
func (m *model) handleEval(arg string) {
	if m.cfg.Eval == nil {
		m.sysChat("eval schedule not available in this session")
		return
	}
	say := func(msg string, err error) {
		if err != nil {
			m.sysChat("eval: " + err.Error())
			return
		}
		m.sysChat(msg)
	}
	f := strings.Fields(arg)
	switch {
	case len(f) == 0:
		m.sysChat(m.cfg.Eval.Status())
	case f[0] == "every" && len(f) == 2:
		say(m.cfg.Eval.SetEvery(f[1]))
	case f[0] == "at" && len(f) == 2:
		say(m.cfg.Eval.SetAt(f[1]))
	case f[0] == "off" && len(f) == 1:
		say(m.cfg.Eval.Off())
	case f[0] == "now" && len(f) == 1:
		say(m.cfg.Eval.RunNow())
	case f[0] == "trend":
		n := 10
		if len(f) == 2 {
			if v, err := strconv.Atoi(f[1]); err == nil && v > 0 {
				n = v
			}
		}
		m.sysChat(m.cfg.Eval.Trend(n))
	case f[0] == "diff" && len(f) == 1:
		say(m.cfg.Eval.Diff())
	default:
		m.sysChat("usage: /eval | /eval every <dur> | /eval at <HH:MM> | /eval off | /eval now | /eval trend [n] | /eval diff")
	}
}

// sysChat appends a plain system message (shown in the terminal's default
// foreground — readable, not dim — and copyable as-is).
func (m *model) sysChat(s string) {
	m.msgs = append(m.msgs, chatMsg{role: "system", content: s})
}

// sysChatStyled appends a system message with a styled display form and a
// separate plain form for the transcript. An empty styled falls back to
// plain at render time.
func (m *model) sysChatStyled(plain, styled string) {
	m.msgs = append(m.msgs, chatMsg{role: "system", content: plain, rendered: styled})
}

// helpSection groups related commands under a title + id. The id is the
// keyword the operator types with /help <id> to expand that section.
type helpSection struct {
	id      string      // /help <id> opens this section
	title   string      // display title in the section header
	tagline string      // one-liner shown in the /help index
	rows    [][2]string // {command, description}
	hidden  bool        // when true, omitted from the /help index (still expandable)
}

// helpSections lists categories in the order they appear in the /help index.
// Each section is independently small enough to fit on a typical 30-row chat
// pane without scrolling — the WHOLE POINT of the cascading design.
var helpSections = []helpSection{
	{
		id: "model", title: "Models",
		tagline: "switch the primary LLM backend (vLLM / Z.ai / OpenAI / Anthropic …)",
		rows: [][2]string{
			{"/model", "list configured model backends + the active one"},
			{"/model pick", "arrow-key picker: pick a provider → its live cloud models → swap (or bare /model use)"},
			{"/model use <name> [<model>]", "switch primary; optional model variant (e.g. /model use zai glm-5.1)"},
			{"/model models [<name>]", "list model variants served by the provider's endpoint (live)"},
			{"/model add-cloud <kind> <key>", "register a keyed cloud provider (zai/openai/grok/anthropic)"},
			{"/model add <name> <endpoint> [fmt]", "register a self-hosted vLLM backend mid-session"},
			{"/model remove <name>", "delete a configured backend (not the active one)"},
			{"/model reload", "re-resolve the active provider's key live (after a rotation)"},
			{"/setup", "arrow-key setup menu — provider, model, endpoint, key, embeddings, gateway"},
			{"/configure", "arrow-key key picker — set an API key for any service (providers, web search, Discord)"},
			{"/ceiling [n]", "view / live-tune the loop ceiling (max tool calls per turn)"},
		},
	},
	{
		id: "agents", title: "Sub-agents",
		tagline: "named sub-agent profiles the orchestrator spawns (own model + identity)",
		rows: [][2]string{
			{"/agents", "list named sub-agent profiles"},
			{"/agents add <name> <provider> [model] [-- desc]", "register a sub-agent"},
			{"/agents model <name> <provider> [model]", "swap an agent's model (live; soul preserved)"},
			{"/agents rename <old> <new>", "rename an agent (all fields preserved)"},
			{"/agents soul <name> <markdown>", "set the agent's identity (empty clears)"},
			{"/agents show <name>", "show an agent's full identity + model"},
			{"/agents remove <name>", "delete an agent profile"},
		},
	},
	{
		id: "research", title: "Deep research",
		tagline: "plan → parallel researchers → cited markdown report",
		rows: [][2]string{
			{"/research <question>", "kick off a research run"},
			{"/research! <question>", "skip the vague-query clarification step"},
			{"/cancel-clarify", "drop a pending clarification prompt"},
			{"/research history [N]", "list past runs (newest first)"},
			{"/research show <id>", "show a past run's report + metadata"},
			{"/research replay <id>", "re-run a past question against the current model"},
			{"/research delete <id>", "purge a past run from disk"},
		},
	},
	{
		id: "memory", title: "Memory",
		tagline: "inspect tiers, search, edit operator soul/rules, compact",
		rows: [][2]string{
			{"/memory", "stats: episodes, facts, skills, soul"},
			{"/memory search <q>", "hybrid search over facts + episodes"},
			{"/memory facts [q]", "list distilled facts (T3)"},
			{"/memory recent [n]", "recent episodes (T2)"},
			{"/memory forget fact:<id>|ep:<id>", "forget a fact or episode"},
			{"/memory soul", "view the soul (T0)"},
			{"/memory soul import <path>", "replace rules from a .md file or folder"},
			{"/memory rules", "view operating rules (the instructions)"},
			{"/memory rules import <path>", "set rules (same as soul import)"},
			{"/memory profile [refresh]", "view / rebuild the learned user model"},
			{"/memory distill", "run a distillation pass now"},
			{"/compress", "summarize old turns to free up context now"},
		},
	},
	{
		id: "tools", title: "Tools & plugins",
		tagline: "see what tools are available, toggle them on/off (live)",
		rows: [][2]string{
			{"/tools", "list all tools and their on/off state"},
			{"/enable <name>", "turn on a tool or whole plugin (smart match — e.g. /enable os, /enable os_sysinfo)"},
			{"/enable skill <plugin>", "explicit categorical toggle — turn on every tool in a plugin (e.g. /enable skill gsuite)"},
			{"/disable <name>", "turn off — it leaves the prompt entirely (no context/compute)"},
			{"/disable skill <plugin>", "categorical disable — mirror of /enable skill"},
			{"/configure <id>", "interactive walkthrough — asks for each field one at a time (e.g. /configure gsuite)"},
			{"/configure <id> <args>", "one-shot with key=value (e.g. /configure gsuite auth=gcloud)"},
			{"/cancel", "abort an in-flight /configure session"},
			{"/skill list", "list configurable integrations (gsuite, discord, …) and their state"},
			{"/skill show <id>", "show one integration's fields + masked values"},
			{"/skill configure <id> <args>", "alias of /configure — set an integration's API key/auth + probe + auto-enable"},
			{"/skill probe <id>", "re-test an integration's credentials without changing config"},
			{"/skill clear <id> <field>", "remove a stored credential (auto-disables if required field cleared)"},
		},
	},
	{
		id: "mcp", title: "MCP connectors",
		tagline: "connect remote MCP servers (browser OAuth, no app setup) and bring their tools live",
		rows: [][2]string{
			{"/mcp", "list connected remote MCP servers + state"},
			{"/mcp add <url>", "connect a remote MCP server (opens a browser to authorize) — surfaces its tools, gated"},
			{"/mcp remove <url>", "disconnect a remote MCP server and forget it"},
		},
	},
	{
		id: "skills", title: "Skills (T4)",
		tagline: "reusable recipes the agent retrieves into its prompt",
		rows: [][2]string{
			{"/skills", "list reusable skill recipes"},
			{"/skills add <name> | <desc> | <recipe>", "save a skill"},
			{"/skills enable|disable|forget|accept <name>", "manage a skill"},
			{"/skills history <name>", "audit log of prior versions (Option A: skill changelog)"},
			{"/skills diff <name> [vN]", "diff current vs a prior version (default: most recent prior)"},
			{"/skills revert <name> vN", "restore a prior version (current state saved as new history entry)"},
			{"/skills auto [off|on|trusted]", "auto-accept induced skills (trusted = only while feedback is healthy)"},
			{"/skills seed gstack", "install Garry Tan's CEO/founder-mode skill bundle (5 skills)"},
			{"/ack", "mark the last turn as good — the self-improvement success signal"},
			{"/undo", "mark the last turn as bad (suspends trusted auto-accept)"},
		},
	},
	{
		id: "eval", title: "Eval & quality",
		tagline: "the nightly regression gate: schedule it, fire it, read the trend",
		rows: [][2]string{
			{"/eval", "show the nightly-eval schedule + last recorded run"},
			{"/eval every <dur> | at <HH:MM> | off", "re-tune the schedule live (persists; one run per period, relaunch-proof)"},
			{"/eval now", "queue one eval run on the improve scheduler (fires within a minute)"},
			{"/eval trend [n]", "recent eval scores + regression verdicts (trend.jsonl)"},
			{"/eval diff", "per-task movers vs the baseline — what improved, what declined and why"},
		},
	},
	{
		id: "safety", title: "Safety & approvals",
		tagline: "per-category permission modes; approve/deny paused actions",
		rows: [][2]string{
			{"/permissions", "view per-category safety (ask/allow/deny)"},
			{"/permissions set <cat> <mode>", "e.g. /permissions set exec allow"},
			{"/approve [session|always]", "approve a paused dangerous action"},
			{"/deny", "reject a paused dangerous action"},
			{"/imessage [list]", "view the iMessage drive-allowlist (deny-by-default)"},
			{"/imessage allow <handle>", "permit a phone/email to drive the agent over iMessage"},
			{"/imessage deny <handle> | clear", "remove one handle, or empty the allowlist"},
		},
	},
	{
		id: "automation", title: "Automation (cron)",
		tagline: "recurring agent-prompt jobs on a crontab/@every schedule (read/comms-safe)",
		rows: [][2]string{
			{"/cron [list]", "view scheduled recurring jobs"},
			{"/cron add <sched> | <prompt>", "schedule a job, e.g. 0 9 * * 1-5 | run the tests"},
			{"/cron enable <id> | disable <id>", "pause or resume a job"},
			{"/cron run <id>", "run a job once now (result appears in the feed)"},
			{"/cron rm <id>", "delete a job"},
			{"/cron exec on|off", "global kill-switch for dangerous shell/exec jobs (default off)"},
		},
	},
	{
		id: "goal", title: "Goal (autonomous loop)",
		tagline: "set a stop condition; judge LLM auto-continues turns until met",
		rows: [][2]string{
			{"/goal <condition>", "set + start the autonomous loop (e.g. /goal write a test for feature X)"},
			{"/goal show", "current status — condition, turns used, judge's last verdict"},
			{"/goal clear", "stop the loop (aliases: stop, off, reset, cancel)"},
			{"Esc / Ctrl-C", "interrupt the in-flight turn and CLEAR the goal"},
		},
	},
	{
		id: "review", title: "Plan review (GStack)",
		tagline: "cascading CEO / Engineer / Designer review appended to a plan.md",
		rows: [][2]string{
			{"/review <plan.md>", "run all 3 reviewers; report appended to the file"},
			{"/review <plan.md> ceo,eng", "subset (ceo, eng, design — comma-separated)"},
		},
	},
	{
		id: "session", title: "Session",
		tagline: "help, exit, transcript copy, interrupt keys",
		rows: [][2]string{
			{"/whoami", "show the agent ID + active backend + active model (runtime truth — trust over the model's self-answer)"},
			{"/dashboard [on|off|status]", "start/stop the web control panel (auto-launches by default; choice persists)"},
			{"/help", "show top-level categories"},
			{"/help <category>", "show commands in one category (e.g. /help agents)"},
			{"/help all", "dump every command in every category (legacy view)"},
			{"/exit, /quit", "close the app (the ONLY way out — Ctrl-C/Esc don't quit)"},
			{"/clear", "wipe the agent's context (fresh conversation) + screen; keeps facts/episodes/archive"},
			{"/cls", "clear just the screen/scrollback (context untouched)"},
			{"Ctrl+S", "select mode: plain drag highlights text for copy (any terminal); Ctrl+S/Esc returns"},
			{"/mouse on|off", "on (default) = wheel scrolls the TUI; off = plain-drag selection (wheel scrolls terminal)"},
			{"↑ / ↓", "recall previous prompts/commands (cursor on first/last input line)"},
			{"(type mid-turn)", "send a message while busy to steer the agent — it addresses it, then resumes"},
			{"Esc / Ctrl-C", "hard-stop the running turn (a stuck/looping agent); never closes the app"},
			{"Ctrl-Y", "copy the transcript to clipboard + file"},
			{"PgUp/PgDn, Shift+↑/↓", "scroll the chat pane (works with or without mouse capture)"},
		},
	},
}

// helpSectionByID returns the section matching id, with light alias support
// (singular/plural, common typo). Returns nil when not found.
func helpSectionByID(id string) *helpSection {
	id = strings.ToLower(strings.TrimSpace(id))
	// Aliases — keep the canonical id distinct in helpSections so the index
	// stays clean.
	switch id {
	case "models":
		id = "model"
	case "agent":
		id = "agents"
	case "research", "deep":
		id = "research"
	case "mem":
		id = "memory"
	case "tool", "plugins", "plugin":
		id = "tools"
	case "skill":
		id = "skills"
	case "perms", "permissions", "approve", "deny":
		id = "safety"
	}
	for i := range helpSections {
		if helpSections[i].id == id {
			return &helpSections[i]
		}
	}
	return nil
}

// renderHelpIndex builds the top-level /help table of contents. One row per
// section: `/help <id>  <title>  <tagline>`. Compact — fits in ~10 lines.
func renderHelpIndex() (plain, styled string) {
	const pad = 18 // "/help <id>" + padding
	var p, s strings.Builder
	p.WriteString("Tenant — command help. Type /help <category> to expand one:\n\n")
	s.WriteString(cHeading.Render("Tenant — command help.") + cDim.Render(" Type ") + cKey.Render("/help <category>") + cDim.Render(" to expand one:") + "\n\n")
	for _, sec := range helpSections {
		if sec.hidden {
			continue
		}
		key := "/help " + sec.id
		gap := pad - len(key)
		if gap < 2 {
			gap = 2
		}
		sp := strings.Repeat(" ", gap)
		p.WriteString("  " + key + sp + sec.title + "  —  " + sec.tagline + "\n")
		s.WriteString("  " + cKey.Render(key) + sp + cName.Render(sec.title) + cDim.Render("  —  "+sec.tagline) + "\n")
	}
	// Surface a high-value direct knob people hunt for but wouldn't guess the
	// category of (/ceiling lives under Models but is a turn control). Keeps the
	// index a section TOC while making the most-asked tunable one glance away.
	p.WriteString("\nHandy: /ceiling <n> — cap tool calls per turn before forced synthesis")
	s.WriteString("\n" + cDim.Render("Handy: ") + cKey.Render("/ceiling <n>") + cDim.Render(" — cap tool calls per turn before forced synthesis"))
	p.WriteString("\nFull dump: /help all")
	s.WriteString("\n" + cDim.Render("Full dump: ") + cKey.Render("/help all"))
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// renderHelpSection renders ONE section (called by /help <id>). Keeps the
// command/description alignment from the old monolithic renderer.
func renderHelpSection(sec *helpSection) (plain, styled string) {
	const pad = 38 // alignment column for this section's commands
	var p, s strings.Builder
	p.WriteString(sec.title + " — " + sec.tagline + "\n")
	s.WriteString(cHeading.Render(sec.title) + cDim.Render(" — "+sec.tagline) + "\n")
	for _, r := range sec.rows {
		cmd, desc := r[0], r[1]
		gap := pad - len(cmd)
		if gap < 2 {
			gap = 2
		}
		sp := strings.Repeat(" ", gap)
		p.WriteString("  " + cmd + sp + desc + "\n")
		s.WriteString("  " + cKey.Render(cmd) + sp + cDim.Render(desc) + "\n")
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// renderHelp builds the command help as (plain, styled). Commands align
// in a colored column with dim descriptions, grouped by section. Used by
// `/help all` — the cascading default is renderHelpIndex.
func renderHelp() (plain, styled string) {
	const pad = 30 // alignment column; longer commands overflow gracefully
	var p, s strings.Builder
	for i, sec := range helpSections {
		if i > 0 {
			p.WriteString("\n")
			s.WriteString("\n")
		}
		p.WriteString(sec.title + "\n")
		s.WriteString(cHeading.Render(sec.title) + "\n")
		for _, r := range sec.rows {
			cmd, desc := r[0], r[1]
			gap := pad - len(cmd)
			if gap < 2 {
				gap = 2
			}
			sp := strings.Repeat(" ", gap)
			p.WriteString("  " + cmd + sp + desc + "\n")
			s.WriteString("  " + cKey.Render(cmd) + sp + cDim.Render(desc) + "\n")
		}
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// renderIMessageAllow formats the iMessage drive-allowlist as (plain, styled).
// An empty list is rendered as an explicit deny-by-default notice so the
// operator is never misled into thinking "empty == open."
func (m *model) renderIMessageAllow() (string, string) {
	handles := m.cfg.IMessage.AllowList()
	var p, s strings.Builder
	const title = "iMessage drive-allowlist"
	p.WriteString(title + "\n")
	s.WriteString(cHeading.Render(title) + "\n")
	if len(handles) == 0 {
		const empty = "(empty) — deny-by-default: nobody can drive the agent over iMessage."
		const hint = "Add one with /imessage allow <phone-or-email>."
		p.WriteString("  " + empty + "\n  " + hint)
		s.WriteString("  " + cOffMark.Render(empty) + "\n  " + cDim.Render(hint))
		return p.String(), s.String()
	}
	for _, h := range handles {
		p.WriteString("  • " + h + "\n")
		s.WriteString("  " + cOnMark.Render("●") + " " + cName.Render(h) + "\n")
	}
	hint := fmt.Sprintf("%d handle(s) allowed — only these may drive the agent once inbound iMessage is live.", len(handles))
	p.WriteString("  " + hint)
	s.WriteString("  " + cDim.Render(hint))
	return p.String(), s.String()
}

// renderCronList formats the recurring-job list as (plain, styled).
func (m *model) renderCronList() (string, string) {
	jobs := m.cfg.Cron.Jobs()
	var p, s strings.Builder
	const title = "Cron jobs"
	execState := "exec: off (dangerous jobs inert)"
	execStyle := cDim
	if m.cfg.Cron.ExecEnabled() {
		execState = "exec: ON (shell/exec jobs run unattended)"
		execStyle = cErr
	}
	p.WriteString(title + "  [" + execState + "]\n")
	s.WriteString(cHeading.Render(title) + "  " + execStyle.Render("["+execState+"]") + "\n")
	if len(jobs) == 0 {
		const empty = "(no jobs) — recurring agent-prompt jobs run unattended, read/comms-safe."
		const hint = "Add one with /cron add <schedule> | <prompt>  (e.g. 0 9 * * 1-5 | run the tests)."
		p.WriteString("  " + empty + "\n  " + hint)
		s.WriteString("  " + cOffMark.Render(empty) + "\n  " + cDim.Render(hint))
		return p.String(), s.String()
	}
	for _, j := range jobs {
		name := j.Name
		if name == "" {
			name = j.ID
		}
		mark, markStyled := "○", cOffMark.Render("○")
		if j.Enabled {
			mark, markStyled = "●", cOnMark.Render("●")
		}
		mode := j.Kind
		if mode == "" {
			mode = "prompt"
		}
		if j.Exec {
			mode += "+exec"
		}
		if j.TZ != "" {
			mode += " " + j.TZ
		}
		head := fmt.Sprintf("%s %s  %s  [%s]  id:%s  next:%s", mark, name, j.Spec, mode, j.ID, cronDash(j.NextRun))
		p.WriteString("  " + head + "\n")
		modeStyle := cDim
		if j.Exec || j.Kind == "shell" {
			modeStyle = cErr // flag the dangerous surfaces
		}
		s.WriteString("  " + markStyled + " " + cName.Render(name) + "  " +
			cKey.Render(j.Spec) + "  " + modeStyle.Render("["+mode+"]") + " " +
			cDim.Render("id:"+j.ID+" next:"+cronDash(j.NextRun)) + "\n")

		prompt := "    " + cronClip(j.Prompt, 88)
		p.WriteString(prompt + "\n")
		s.WriteString("    " + cDim.Render(cronClip(j.Prompt, 88)) + "\n")

		if j.LastStatus != "" {
			st := "    last: " + j.LastStatus + " @ " + cronDash(j.LastRun)
			p.WriteString(st + "\n")
			style := cDim
			switch j.LastStatus {
			case "ok":
				style = cOK
			case "error":
				style = cErr
			}
			s.WriteString("    " + style.Render("last: "+j.LastStatus+" @ "+cronDash(j.LastRun)) + "\n")
		}
	}
	hint := fmt.Sprintf("%d job(s) — /cron run <id> to run now, /cron disable <id> to pause.", len(jobs))
	p.WriteString("  " + hint)
	s.WriteString("  " + cDim.Render(hint))
	return p.String(), s.String()
}

// parseCronAdd parses the part before the "|": optional leading flags
// (shell|exec|tz=<zone>) followed by the schedule. The schedule is whatever
// remains after the recognized leading flags.
func parseCronAdd(specPart string) (CronAddSpec, error) {
	var s CronAddSpec
	toks := strings.Fields(specPart)
	i := 0
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case strings.EqualFold(t, "shell"):
			s.Kind = "shell"
		case strings.EqualFold(t, "exec"):
			s.Exec = true
		case strings.HasPrefix(strings.ToLower(t), "tz="):
			s.TZ = t[len("tz="):]
		default:
			// First non-flag token starts the schedule.
			s.Spec = strings.TrimSpace(strings.Join(toks[i:], " "))
			return s, nil
		}
	}
	// No schedule found (only flags).
	return s, nil
}

// cronDash renders an empty schedule field as "-".
func cronDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// cronClip truncates s to n runes with an ellipsis, for compact job rows.
func cronClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// handleSlash runs a /command and returns an optional tea.Cmd (e.g. quit).
func (m *model) handleSlash(line string) tea.Cmd {
	fields := strings.Fields(line)
	cmd := strings.ToLower(fields[0])
	arg := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	switch cmd {
	case "/whoami", "/who":
		// Runtime introspection — answers "what model am I on?" / "which
		// agent am I?" from CONFIG, not from the LLM. The model itself
		// is unreliable at self-identification (it pulls from training
		// data + system prompt context, not runtime). This is the
		// authoritative answer; trust this over the model's answer.
		var b strings.Builder
		fmt.Fprintf(&b, "agent:    %s\n", m.cfg.AgentID)
		fmt.Fprintf(&b, "backend:  %s\n", m.cfg.Backend)
		fmt.Fprintf(&b, "model:    %s\n", m.cfg.Model)
		// Don't render endpoint here — it's not load-bearing for the
		// "who am I" question and we never want it in the chat scroll
		// where it could be Ctrl-Y'd back into a transcript.
		m.sysChat(strings.TrimRight(b.String(), "\n"))
	case "/help", "/?":
		// Cascading help: bare /help shows the index of categories; /help all
		// dumps every command (legacy); /help <category> expands one section.
		sub := strings.TrimSpace(arg)
		switch strings.ToLower(sub) {
		case "":
			m.sysChatStyled(renderHelpIndex())
		case "all", "everything", "full":
			m.sysChatStyled(renderHelp())
		default:
			sec := helpSectionByID(sub)
			if sec == nil {
				m.sysChat(fmt.Sprintf("no help category named %q — type /help to see the categories", sub))
				return nil
			}
			m.sysChatStyled(renderHelpSection(sec))
		}
	case "/quit", "/exit":
		return tea.Quit
	case "/tools":
		m.sysChatStyled(m.renderToolList())
	case "/skills":
		// TEN-64: /skill (singular) was previously an alias for /skills
		// (plural, T4 skill memory library). It now routes to the
		// integration-config surface (configure API keys for gsuite,
		// discord, etc.). Use the plural `/skills` for the T4 library.
		m.sysChatStyled(m.handleSkills(arg))
	case "/memory", "/mem":
		m.sysChat(m.handleMemory(arg))
	case "/compress", "/compact":
		if m.cfg.Agent == nil {
			m.sysChat("compaction is not available in this session")
			break
		}
		m.sysChat("compacting context…")
		ag, ctx := m.cfg.Agent, m.ctx
		return func() tea.Msg {
			before, after, err := ag.CompactNow(ctx)
			return compactDoneMsg{before: before, after: after, err: err}
		}
	case "/expand":
		// Rehydrate the latest compaction summary's source span from the archive
		// (read-only) — "paged out, not lost." (TEN-104)
		if m.cfg.Agent == nil {
			m.sysChat("expansion is not available in this session")
			break
		}
		ag, ctx := m.cfg.Agent, m.ctx
		m.sysChat("expanding the latest compaction…")
		return func() tea.Msg {
			exp, err := ag.ExpandLatestCompaction(ctx)
			return expandDoneMsg{exp: exp, err: err}
		}
	case "/clear":
		// Wipe the agent's working context (fresh conversation) AND the screen.
		// Durable memory is preserved. (TEN-181)
		if m.busy {
			m.sysChat("can't clear context mid-turn — let it finish or /cancel first")
			break
		}
		if m.cfg.Agent == nil {
			m.sysChat("context clearing is not available in this session")
			break
		}
		n := m.cfg.Agent.ClearContext()
		m.clearFeed()
		m.sysChat(fmt.Sprintf("context cleared (%d message(s)) — fresh start. Facts, episodes, and the archive are preserved; recall still works.", n))
	case "/cls":
		// Clear just the screen/scrollback; the agent's context is untouched. (TEN-181)
		m.clearFeed()
		m.sysChat("screen cleared (context kept — use /clear to also reset the agent's context)")
	case "/mouse":
		// Toggle mouse wheel-capture. ON (default): wheel scrolls the TUI panes
		// and you stay in the alt screen — to SELECT text, hold the terminal's
		// bypass modifier while dragging (⌥ Option on macOS, Shift on most Linux
		// terminals). OFF: plain drag selects, but the wheel scrolls the
		// terminal's scrollback instead of the TUI. (TEN-181)
		switch strings.ToLower(strings.TrimSpace(arg)) {
		case "on":
			m.mouseOn, m.selectMode = true, false
			m.sysChat("🖱 wheel scrolling ON (default). To copy: press Ctrl+S (select mode — plain drag works in any terminal), or your terminal's bypass drag (iTerm2 ⌥-drag, Apple Terminal Fn-drag, Linux Shift-drag).")
			return tea.EnableMouseCellMotion
		case "off":
			m.mouseOn, m.selectMode = false, false
			m.sysChat("🖱 mouse capture OFF — plain drag selects/copies. Note: the wheel now scrolls the TERMINAL, not the TUI (PgUp/PgDn / Shift+↑/↓ still scroll the panes). `/mouse on` to restore.")
			return tea.DisableMouse
		case "", "toggle":
			m.mouseOn = !m.mouseOn
			m.selectMode = false
			if m.mouseOn {
				m.sysChat("🖱 wheel scrolling ON. Copy text with Ctrl+S select mode (or iTerm2 ⌥-drag / Apple Terminal Fn-drag).")
				return tea.EnableMouseCellMotion
			}
			m.sysChat("🖱 mouse capture OFF — plain drag selects. Wheel scrolls the terminal; use PgUp/PgDn for the panes. `/mouse on` to restore.")
			return tea.DisableMouse
		default:
			m.sysChat("usage: /mouse on|off   (on = wheel scrolls the TUI, Ctrl+S to select text; off = plain-drag selection)")
		}
	case "/approve", "/approve!":
		m.resolveApproval(arg)
	case "/deny", "/reject":
		m.sendDecision(DenyOnce)
	case "/permissions", "/perms":
		if m.cfg.Perms == nil {
			m.sysChat("permissions are not available in this session")
			break
		}
		f := strings.Fields(arg)
		switch {
		case len(f) == 0:
			m.sysChatStyled(m.renderPermissions())
		case strings.ToLower(f[0]) == "set" && len(f) >= 3:
			cat, mode := strings.ToLower(f[1]), strings.ToLower(f[2])
			ok, err := m.cfg.Perms.SetPermission(cat, mode)
			switch {
			case err != nil:
				m.sysChat("error: " + err.Error())
			case !ok:
				m.sysChat("no such category " + f[1] + " (see /permissions)")
			default:
				m.sysChat("set " + cat + " → " + mode)
			}
		default:
			m.sysChat("usage: /permissions   |   /permissions set <category> <ask|allow|deny>")
		}
	case "/dashboard":
		// Start/stop the web control panel live; the choice persists.
		if m.cfg.Dash == nil {
			m.sysChat("dashboard not available in this session")
			break
		}
		switch strings.ToLower(strings.TrimSpace(arg)) {
		case "", "status":
			if running, addr := m.cfg.Dash.Status(); running {
				m.sysChat("dashboard: running at http://" + addr)
			} else {
				m.sysChat("dashboard: stopped")
			}
		case "on", "start", "enable":
			addr, err := m.cfg.Dash.Enable()
			if err != nil {
				m.sysChat("dashboard: " + err.Error())
			} else {
				m.sysChat("dashboard: serving at http://" + addr)
			}
		case "off", "stop", "disable":
			if err := m.cfg.Dash.Disable(); err != nil {
				m.sysChat("dashboard: " + err.Error())
			} else {
				m.sysChat("dashboard: stopped")
			}
		default:
			m.sysChat("usage: /dashboard [status|on|off]")
		}
	case "/relay":
		// Start/stop the offsite Discord relay live; the choice persists.
		if m.cfg.Relay == nil {
			m.sysChat("discord relay not available in this session")
			break
		}
		fields := strings.Fields(arg)
		sub, rest := "", ""
		if len(fields) > 0 {
			sub = strings.ToLower(fields[0])
			rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
		}
		switch sub {
		case "", "status":
			running, opset, execOn := m.cfg.Relay.Status()
			state := "stopped"
			if running {
				state = "running"
			}
			who := "no operator set — /relay allow <id>"
			if opset {
				who = "operator set"
			}
			mode := "read/comms only"
			if execOn {
				mode = "EXEC ON (dangerous tools, button-approved)"
			}
			m.sysChat("discord relay: " + state + " (" + who + "; " + mode + ")")
		case "allow":
			if rest == "" {
				m.sysChat("usage: /relay allow <your-discord-user-id>")
				break
			}
			if err := m.cfg.Relay.SetOperator(rest); err != nil {
				m.sysChat("relay: " + err.Error())
			} else {
				m.sysChat("relay: operator set to " + rest + " — run /relay on to start")
			}
		case "on", "start", "enable":
			if err := m.cfg.Relay.Enable(); err != nil {
				m.sysChat("relay: " + err.Error())
			} else {
				m.sysChat("relay: started — DM your bot from the allowed account")
			}
		case "off", "stop", "disable":
			if err := m.cfg.Relay.Disable(); err != nil {
				m.sysChat("relay: " + err.Error())
			} else {
				m.sysChat("relay: stopped")
			}
		case "exec":
			on, ok := parseOnOff(rest)
			if !ok {
				m.sysChat("usage: /relay exec on|off  (on = unlock dangerous tools offsite, each button-approved)")
				break
			}
			if err := m.cfg.Relay.SetExec(on); err != nil {
				m.sysChat("relay: " + err.Error())
			} else if on {
				m.sysChat("relay: EXEC MODE ON — offsite exec/write/destructive/send are now reachable, each requires your per-action button approval. /relay exec off to revoke.")
			} else {
				m.sysChat("relay: exec mode off — offsite session is read/research/comms only again")
			}
		default:
			m.sysChat("usage: /relay [status|allow <id>|on|off|exec on|off]")
		}
	case "/imessage", "/imsg":
		// Manage the deny-by-default allowlist of handles permitted to drive
		// the agent over iMessage. Edits persist. Empty list = nobody.
		if m.cfg.IMessage == nil {
			m.sysChat("imessage allowlist not available in this session")
			break
		}
		fields := strings.Fields(arg)
		sub, rest := "", ""
		if len(fields) > 0 {
			sub = strings.ToLower(fields[0])
			rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
		}
		switch sub {
		case "", "list", "status":
			m.sysChatStyled(m.renderIMessageAllow())
		case "allow", "add":
			if rest == "" {
				m.sysChat("usage: /imessage allow <phone-or-email>")
				break
			}
			norm, added, err := m.cfg.IMessage.Allow(rest)
			switch {
			case err != nil:
				m.sysChat("imessage: " + err.Error())
			case !added:
				m.sysChat("imessage: " + norm + " is already on the allowlist")
			default:
				m.sysChat("imessage: allowed " + norm + " — may drive the agent over iMessage once inbound is live")
			}
		case "deny", "remove", "rm", "del":
			if rest == "" {
				m.sysChat("usage: /imessage deny <phone-or-email>")
				break
			}
			norm, removed, err := m.cfg.IMessage.Deny(rest)
			switch {
			case err != nil:
				m.sysChat("imessage: " + err.Error())
			case !removed:
				m.sysChat("imessage: " + norm + " was not on the allowlist")
			default:
				m.sysChat("imessage: removed " + norm + " from the allowlist")
			}
		case "clear":
			n, err := m.cfg.IMessage.Clear()
			if err != nil {
				m.sysChat("imessage: " + err.Error())
			} else {
				m.sysChat(fmt.Sprintf("imessage: cleared %d handle(s) — allowlist is now empty (deny-by-default: nobody can drive the agent)", n))
			}
		default:
			m.sysChat("usage: /imessage [list | allow <handle> | deny <handle> | clear]")
		}
	case "/mcp":
		// Connect/manage remote MCP connector servers (TEN-164). `/mcp add
		// <url>` runs the OAuth browser flow off the UI goroutine (a tea.Cmd
		// closure) so the TUI stays responsive; tools come up gated.
		return m.handleMCP(arg)
	case "/cron", "/cronjob":
		// Manage recurring agent-prompt jobs. Each job runs UNATTENDED and
		// read/comms-safe on its schedule; definitions persist to config.json.
		if m.cfg.Cron == nil {
			m.sysChat("cron scheduling not available in this session")
			break
		}
		fields := strings.Fields(arg)
		sub, rest := "", ""
		if len(fields) > 0 {
			sub = strings.ToLower(fields[0])
			rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
		}
		switch sub {
		case "", "list", "status":
			m.sysChatStyled(m.renderCronList())
		case "add", "new":
			// Form: /cron add [shell|exec] [tz=<zone>] <schedule> | <payload>
			// Leading flags precede the schedule; "|" separates the (space-
			// containing) schedule from the prompt/command.
			specPart, prompt, ok := strings.Cut(rest, "|")
			addSpec, perr := parseCronAdd(specPart)
			prompt = strings.TrimSpace(prompt)
			if perr != nil || !ok || addSpec.Spec == "" || prompt == "" {
				m.sysChat("usage: /cron add [shell|exec] [tz=<zone>] <schedule> | <payload>\n" +
					"  e.g. /cron add 0 9 * * 1-5 | run the test suite and summarize failures\n" +
					"  e.g. /cron add @every 30m | check the build status\n" +
					"  e.g. /cron add shell @daily | go test ./...        (DANGEROUS, needs cron-exec)\n" +
					"  e.g. /cron add exec tz=America/New_York 0 8 * * * | clean up temp files")
				break
			}
			addSpec.Prompt = prompt
			j, err := m.cfg.Cron.Add(addSpec)
			if err != nil {
				m.sysChat("cron: " + err.Error())
			} else {
				mode := j.Kind
				if j.Exec {
					mode += "+exec"
				}
				m.sysChat(fmt.Sprintf("cron: scheduled %s [%s] (%s) — next run %s", j.ID, mode, j.Spec, cronDash(j.NextRun)))
			}
		case "rm", "remove", "del", "delete":
			if rest == "" {
				m.sysChat("usage: /cron rm <id>")
				break
			}
			removed, err := m.cfg.Cron.Remove(rest)
			switch {
			case err != nil:
				m.sysChat("cron: " + err.Error())
			case !removed:
				m.sysChat("cron: no job with id " + rest)
			default:
				m.sysChat("cron: removed " + rest)
			}
		case "enable", "disable":
			if rest == "" {
				m.sysChat("usage: /cron " + sub + " <id>")
				break
			}
			on := sub == "enable"
			j, changed, err := m.cfg.Cron.SetEnabled(rest, on)
			switch {
			case err != nil:
				m.sysChat("cron: " + err.Error())
			case !changed:
				m.sysChat("cron: no change for " + rest + " (not found or already " + sub + "d)")
			case on:
				m.sysChat(fmt.Sprintf("cron: enabled %s — next run %s", j.ID, cronDash(j.NextRun)))
			default:
				m.sysChat("cron: disabled " + j.ID)
			}
		case "run":
			if rest == "" {
				m.sysChat("usage: /cron run <id>")
				break
			}
			if err := m.cfg.Cron.RunNow(rest); err != nil {
				m.sysChat("cron: " + err.Error())
			} else {
				m.sysChat("cron: running " + rest + " now — result will appear in the feed")
			}
		case "exec":
			// Global kill-switch for dangerous (shell / exec-opted-in) jobs.
			arg := strings.ToLower(strings.TrimSpace(rest))
			switch arg {
			case "", "status":
				if m.cfg.Cron.ExecEnabled() {
					m.sysChat("cron exec: ON — shell jobs and exec-opted-in jobs WILL run unattended")
				} else {
					m.sysChat("cron exec: OFF — shell/exec jobs are created but won't run (deny-by-default). Enable with /cron exec on")
				}
			case "on", "off":
				on := arg == "on"
				if err := m.cfg.Cron.SetExec(on); err != nil {
					m.sysChat("cron: " + err.Error())
				} else if on {
					m.sysChat("cron exec: ENABLED — shell + exec-opted-in jobs may now run UNATTENDED with dangerous tools (irreversible actions + config writes are still blocked). Disable with /cron exec off")
				} else {
					m.sysChat("cron exec: disabled — dangerous jobs are now inert (deny-by-default)")
				}
			default:
				m.sysChat("usage: /cron exec [on | off | status]")
			}
		default:
			m.sysChat("usage: /cron [list | add <schedule> | <prompt> | enable <id> | disable <id> | run <id> | rm <id> | exec on|off]")
		}
	case "/research", "/deep", "/research!", "/deep!":
		if m.cfg.Research == nil {
			m.sysChat("deep research is not available in this session")
			break
		}
		// /research! and /deep! are the skip-clarify shortcuts (C2 opt-out).
		// The implementation: dispatch via ResearchAfterClarify with the
		// raw question (no enrichment) — that path skips the clarifier.
		skipClarify := strings.HasSuffix(cmd, "!")
		arg = strings.TrimSpace(arg)
		// Sub-commands: history, show, replay, delete. Only valid on the
		// non-shortcut form (`/research!` is for "skip clarify on the
		// question that follows", not a sub-command host).
		if !skipClarify {
			if subcmd, rest, ok := splitFirstWord(arg); ok && isResearchSub(subcmd) {
				return m.handleResearchSub(subcmd, rest)
			}
		}
		if arg == "" {
			m.sysChat("usage:\n  /research <question>\n  /research! <question>   (skip clarification)\n  /research history [N]\n  /research show <id>\n  /research replay <id>\n  /research delete <id>")
			break
		}
		if m.busy {
			m.sysChat("busy — wait for the current turn/research to finish")
			break
		}
		// A new /research while a previous one is awaiting clarification:
		// drop the pending state and treat this as the active query.
		if m.pendingClarify != nil {
			m.pendingClarify = nil
			m.appendFeed(cSys.Render("⏏ dropped pending clarification"))
		}
		m.msgs = append(m.msgs, chatMsg{role: "user", content: cmd + " " + arg})
		m.busy = true
		m.interrupted = false
		m.chatFollow = true
		m.appendFeed(cSys.Render("🔎 deep research started — Esc to stop"))
		rc := m.cfg.Research
		// Reuse the per-turn cancel so Esc interrupts research too.
		rctx, cancel := context.WithCancel(m.ctx)
		m.turnCancel = cancel
		q := arg
		skip := skipClarify
		return func() tea.Msg {
			var (
				report string
				err    error
			)
			if skip {
				report, err = rc.ResearchAfterClarify(rctx, q)
			} else {
				report, err = rc.Research(rctx, q)
			}
			cancel()
			return researchDoneMsg{report: report, err: err}
		}
	case "/goal":
		// Claude-Code-style autonomous loop. Sub-commands:
		//   /goal <condition>     — set + start
		//   /goal show            — status
		//   /goal clear|stop|...  — stop
		if m.cfg.Goals == nil {
			m.sysChat("goal autonomous loop is not available in this session")
			break
		}
		arg = strings.TrimSpace(arg)
		// Sub-command sniff: distinguish "show / clear / stop / off / reset /
		// cancel" from a regular condition starting with those words.
		if first, rest, ok := splitFirstWord(arg); ok && isGoalSub(first) && rest == "" {
			switch strings.ToLower(first) {
			case "show", "status":
				st := m.cfg.Goals.Show()
				m.sysChat(renderGoalStatus(st))
			case "clear", "stop", "off", "reset", "cancel":
				m.sysChat("🎯 " + m.cfg.Goals.Clear())
			}
			break
		}
		if arg == "" {
			if m.cfg.Goals.Active() {
				m.sysChat(renderGoalStatus(m.cfg.Goals.Show()))
			} else {
				m.sysChat("usage:\n  /goal <condition>  — set + start the autonomous loop\n  /goal show         — current status\n  /goal clear        — stop the loop (aliases: stop, off, reset, cancel)")
			}
			break
		}
		if m.busy {
			m.sysChat("busy — wait for the current turn to finish, then set the goal")
			break
		}
		firstPrompt, status, err := m.cfg.Goals.Set(m.ctx, arg)
		if err != nil {
			m.sysChat("goal: " + err.Error())
			break
		}
		m.appendFeed(cOK.Render("🎯 goal set"))
		m.sysChat(status)
		// Kick off the first turn with the goal prompt — same path as if the
		// user had typed it themselves.
		m.ta.SetValue(firstPrompt)
		m.goalAutoActive = true
		return m.submit()
	case "/review":
		// GStack Layer 3 cascading review. Usage:
		//   /review <plan.md>                — all reviewers
		//   /review <plan.md> ceo,eng        — comma-separated subset
		if m.cfg.Review == nil {
			m.sysChat("/review is not available in this session")
			break
		}
		arg = strings.TrimSpace(arg)
		if arg == "" {
			m.sysChat("usage:\n  /review <plan.md>           — run CEO + Engineer + Designer reviews\n  /review <plan.md> ceo,eng   — run a subset (ceo, eng, design)")
			break
		}
		if m.busy {
			m.sysChat("busy — wait for the current turn to finish, then /review")
			break
		}
		planPath, rolesArg, _ := splitFirstWord(arg)
		var roles []string
		if r := strings.TrimSpace(rolesArg); r != "" {
			for _, x := range strings.Split(r, ",") {
				if x = strings.TrimSpace(x); x != "" {
					roles = append(roles, x)
				}
			}
		}
		m.appendFeed(cOK.Render("📝 review starting — " + planPath))
		report, err := m.cfg.Review.Review(m.ctx, planPath, roles)
		if err != nil {
			m.sysChat("review: " + err.Error())
			if strings.TrimSpace(report) != "" {
				m.sysChat(report)
			}
			break
		}
		m.appendFeed(cOK.Render("📝 review appended to " + planPath))
		m.sysChat(report)
	case "/cancel-clarify":
		// C2: drop a pending clarification prompt without answering it.
		// Useful when the user changed their mind about /research-ing this
		// query and doesn't want their next typed message folded in.
		if m.pendingClarify == nil {
			m.sysChat("no clarification is pending")
			break
		}
		m.pendingClarify = nil
		m.sysChat("⏏ clarification dropped — your next message is a normal chat turn")
	case "/agents", "/agent":
		m.handleAgents(arg)
	case "/model", "/models":
		if cmd := m.handleModel(arg); cmd != nil {
			return cmd
		}
	case "/eval":
		m.handleEval(arg)
	case "/ceiling", "/loops", "/loop-ceiling":
		if m.cfg.Models == nil {
			m.sysChat("loop-ceiling tuning is not available in this session")
			break
		}
		if strings.TrimSpace(arg) == "" {
			m.sysChat(fmt.Sprintf("loop ceiling = %d (max planner↔tool iterations per turn, per agent) — set with /ceiling <n>", m.cfg.Models.LoopCeiling()))
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(arg))
		if err != nil {
			m.sysChat("usage: /ceiling <number>   (e.g. /ceiling 30)")
			break
		}
		status, err := m.cfg.Models.SetLoopCeiling(n)
		if err != nil {
			m.sysChat("error: " + err.Error())
			break
		}
		m.appendFeed(cOK.Render("⟳ " + status))
		m.sysChat(status)
	case "/configure", "/config":
		// TEN-65 follow-up: OpenClaw-style interactive configure.
		// Usage:
		//   /configure              → list configurable skills (no session)
		//   /configure <id>         → start interactive walkthrough
		//   /configure <id> <args>  → one-shot (same as /skill configure)
		//   /configure --no-enable <id> [<args>]  → skip auto-enable
		if m.cfg.SkillConfig == nil && m.cfg.Secrets == nil {
			m.sysChat("/configure is not available in this session")
			break
		}
		arg = strings.TrimSpace(arg)
		// Parse --no-enable from anywhere in the arg list.
		fields := strings.Fields(arg)
		noEnable := false
		remaining := make([]string, 0, len(fields))
		for _, f := range fields {
			if f == "--no-enable" {
				noEnable = true
				continue
			}
			remaining = append(remaining, f)
		}
		if len(remaining) == 0 {
			// No arg → openclaw-style arrow-key picker over every keyed service
			// (providers, web search, Discord, …). Pick one → paste its key.
			if m.cfg.Secrets != nil {
				return m.startConfigurePicker()
			}
			// Fallback (no Secrets control wired): legacy skill menu.
			if m.cfg.SkillConfig != nil {
				m.sysChat(renderSkillList(m.cfg.SkillConfig.SkillList()))
				m.sysChat("type `/configure <id>` to start an interactive walkthrough — e.g. `/configure gsuite`")
			}
			break
		}
		if m.cfg.SkillConfig == nil {
			m.sysChat("/configure <id> needs the skill-config surface, which isn't available in this session")
			break
		}
		id := remaining[0]
		rest := remaining[1:]
		if len(rest) > 0 {
			// One-shot — delegate to SkillConfigure with assembled args.
			args := append([]string{id}, rest...)
			out, err := m.cfg.SkillConfig.SkillConfigure(args, noEnable)
			if err != nil {
				m.sysChat("✗ " + err.Error())
			} else {
				m.sysChat(out)
			}
			break
		}
		// Interactive walkthrough.
		if cmd := m.startConfigure(id, noEnable); cmd != nil {
			return cmd
		}
	case "/setup":
		// TEN-150: the external `tenant setup` wizard, in-TUI. Arrow-key menu of
		// settings (provider/model/endpoint/key/tool-format/embeddings/gateway);
		// pick one → edit it (masked for the key) → applied live; back to menu.
		if m.cfg.Setup == nil {
			m.sysChat("/setup is not available in this session")
			break
		}
		return m.startSetupMenu()
	case "/ack":
		// TEN-151: mark the last turn as good (the real-world success signal).
		if m.cfg.Feedback == nil {
			m.sysChat("feedback isn't available in this session")
			break
		}
		if st, err := m.cfg.Feedback.Ack(); err != nil {
			m.sysChat("✗ " + err.Error())
		} else {
			m.sysChat("✓ " + st)
		}
	case "/undo":
		// TEN-151: mark the last turn as bad (suspends "trusted" auto-accept).
		if m.cfg.Feedback == nil {
			m.sysChat("feedback isn't available in this session")
			break
		}
		if st, err := m.cfg.Feedback.Undo(); err != nil {
			m.sysChat("✗ " + err.Error())
		} else {
			m.sysChat("✓ " + st)
		}
	case "/cancel":
		// TEN-65 follow-up: aborts an in-flight /configure session.
		// Harmless if no session is active.
		if m.secretEntry != nil {
			m.clearSecretEntry()
			m.sysChat("configuration cancelled")
			break
		}
		if m.setupEntry != nil {
			m.clearSetupEntry()
			m.sysChat("setup cancelled")
			break
		}
		if m.configureSession != nil {
			m.configureSession = nil
			m.sysChat("configuration cancelled")
			break
		}
		m.sysChat("nothing to cancel")
	case "/skill":
		// /skill (singular) — integration-config surface. Distinct from
		// /skills (plural) which manages the T4 skill memory library.
		// TEN-64 framework; per-platform catalog entries land in TEN-65+.
		if m.cfg.SkillConfig == nil {
			m.sysChat("/skill is not available in this session")
			break
		}
		sub, rest := firstField(arg)
		switch strings.ToLower(sub) {
		case "", "list":
			m.sysChat(renderSkillList(m.cfg.SkillConfig.SkillList()))
		case "show":
			id := strings.TrimSpace(rest)
			if id == "" {
				m.sysChat("usage: /skill show <id>   (see /skill list)")
				break
			}
			out, err := m.cfg.SkillConfig.SkillShow(id)
			if err != nil {
				m.sysChat(err.Error())
			} else {
				m.sysChat(out)
			}
		case "configure", "config", "set":
			// Parse optional --no-enable flag; the rest are args to
			// SkillConfigure (id + positional/kv).
			fields := strings.Fields(rest)
			noEnable := false
			remaining := make([]string, 0, len(fields))
			for _, f := range fields {
				if f == "--no-enable" {
					noEnable = true
					continue
				}
				remaining = append(remaining, f)
			}
			if len(remaining) == 0 {
				m.sysChat("usage: /skill configure <id> <value>   |   /skill configure <id> key=value [key=value …]   [--no-enable]")
				break
			}
			out, err := m.cfg.SkillConfig.SkillConfigure(remaining, noEnable)
			if err != nil {
				m.sysChat("✗ " + err.Error())
			} else {
				m.sysChat(out)
			}
		case "probe":
			id := strings.TrimSpace(rest)
			if id == "" {
				m.sysChat("usage: /skill probe <id>")
				break
			}
			out, err := m.cfg.SkillConfig.SkillProbe(id)
			if err != nil {
				m.sysChat("✗ " + err.Error())
			} else {
				m.sysChat(out)
			}
		case "clear":
			fields := strings.Fields(rest)
			if len(fields) != 2 {
				m.sysChat("usage: /skill clear <id> <field>")
				break
			}
			out, err := m.cfg.SkillConfig.SkillClear(fields[0], fields[1])
			if err != nil {
				m.sysChat("✗ " + err.Error())
			} else {
				m.sysChat(out)
			}
		default:
			m.sysChat("unknown /skill subcommand " + sub + " — try list, show, configure, probe, clear")
		}
	case "/enable", "/disable":
		on := cmd == "/enable"
		if m.cfg.Tools == nil {
			m.sysChat("no tools are configured (launch with --wiki-dir/--sql-db/--os/… )")
			break
		}
		arg = strings.TrimSpace(arg)
		if arg == "" {
			m.sysChat("usage: " + cmd + " <tool>   |   " + cmd + " skill <plugin>   (see /tools)")
			break
		}
		verb := map[bool]string{true: "enabled", false: "disabled"}[on]
		// `/enable skill <name>` — explicit categorical form. Forces a
		// plugin-label sweep and rejects single-tool names so a typo
		// surfaces as a clean error instead of silently no-op'ing the
		// smart-match path.
		fields := strings.Fields(arg)
		if len(fields) >= 2 && strings.ToLower(fields[0]) == "skill" {
			label := fields[1]
			n, _, err := m.cfg.Tools.SetPluginEnabled(label, on)
			switch {
			case err != nil:
				m.sysChat("could not " + verb + " skill " + label + ": " + err.Error())
			case n == 0:
				plugins := m.cfg.Tools.Plugins()
				hint := ""
				if len(plugins) > 0 {
					hint = " — try: " + strings.Join(plugins, ", ")
				}
				m.sysChat("no skill named " + label + hint)
			default:
				m.sysChat(fmt.Sprintf("%s %d tool(s) in skill %q", verb, n, label))
			}
			break
		}
		// `/enable <name>` — smart-match: exact tool name first, fall
		// back to plugin label. Kept for back-compat and the muscle-
		// memory case (`/enable gsuite` still works).
		n, scope, err := m.cfg.Tools.SetEnabled(arg, on)
		switch {
		case err != nil:
			m.sysChat("could not enable " + arg + ": " + err.Error())
		case n == 0:
			m.sysChat("no tool or plugin named " + arg + " (see /tools)")
		default:
			m.sysChat(fmt.Sprintf("%s %d %s tool(s) [%s]", verb, n, scope, arg))
		}
	default:
		m.sysChat("unknown command " + cmd + " — try /help")
	}
	return nil
}

// handleModel runs /model subcommands: bare list, `use <name>` (live swap of
// the primary), and `add <name> <endpoint> [tool-format]` (register a new
// vLLM backend mid-session).
// handleModel runs a /model sub-command and returns an optional tea.Cmd
// (currently only add-cloud uses this — to dispatch the variant picker
// after the live model-list fetch finishes asynchronously). Named
// return lets the existing bare `return` statements stay as no-cmd
// short-circuits without churning every case.
func (m *model) handleModel(arg string) (cmd tea.Cmd) {
	if m.cfg.Models == nil {
		m.sysChat("model switching is not available in this session")
		return
	}
	f := strings.Fields(arg)
	if len(f) == 0 {
		m.sysChatStyled(m.renderModelList())
		return
	}
	switch strings.ToLower(f[0]) {
	case "reload", "refresh":
		// Re-resolve the active provider's key (env/credentials.json) + hot-swap
		// it live — for picking up a rotated key without a restart.
		m.appendFeed(cDim.Render("⟳ reloading active provider key…"))
		status, err := m.cfg.Models.ReloadKeys()
		if err != nil {
			m.sysChat("model: reload failed: " + err.Error())
		} else if status == "" {
			m.sysChat("model: no active provider to reload")
		} else {
			m.sysChat(status)
		}
		return
	case "use", "switch":
		if len(f) < 2 {
			// No provider named → open the arrow-key picker: pick a provider →
			// fetch its live cloud models → pick → swap. (TEN-173)
			cmd = m.startModelPicker()
			return
		}
		// Optional third arg: a model variant to pin on this provider.
		// Useful for multi-variant providers (Z.ai's glm-*, OpenAI's
		// gpt-*). Empty preserves the saved model. Use `/model models
		// <name>` to discover what's served.
		modelOverride := ""
		if len(f) >= 3 {
			modelOverride = f[2]
		}
		feedMsg := "⇄ switching model → " + f[1]
		if modelOverride != "" {
			feedMsg += " (" + modelOverride + ")"
		}
		feedMsg += "…"
		m.appendFeed(cDim.Render(feedMsg))
		status, active, err := m.cfg.Models.UseModel(f[1], modelOverride)
		if err != nil {
			m.appendFeed(cErr.Render("✗ model switch failed: " + clip(err.Error(), 70)))
			m.sysChat("could not switch: " + err.Error())
			return
		}
		if active != "" {
			m.cfg.Model = active // status bar reflects the new primary
		}
		// The status leads with ✓ (connected) or ⚠ (switched but degraded);
		// color the feed line accordingly.
		feedStyle := cOK
		if !strings.HasPrefix(status, "✓") {
			feedStyle = cErr
		}
		m.appendFeed(feedStyle.Render(status))
		m.sysChat(status)
		// If the swapped-to endpoint is unreachable, start auto-reconnect now
		// (don't make the user send a doomed message first).
		if strings.Contains(status, "UNREACHABLE") && m.cfg.Reconnect != nil {
			m.cfg.Reconnect.OnGenerationDown()
		}
	case "add":
		// /model add <name> <endpoint> [tool-format]
		if len(f) < 3 {
			m.sysChat("usage: /model add <name> <endpoint> [tool-format]   e.g. /model add dgx http://localhost:8000 qwen")
			return
		}
		name, endpoint := f[1], f[2]
		doAdd := func(toolFmt string) {
			status, err := m.cfg.Models.AddModel(name, endpoint, toolFmt)
			if err != nil {
				m.appendFeed(cErr.Render("✗ add backend failed: " + clip(err.Error(), 70)))
				m.sysChat("could not add backend: " + err.Error())
				return
			}
			m.appendFeed(cOK.Render("＋ backend added: " + name + " (" + toolFmt + ")"))
			m.sysChat(status)
		}
		if len(f) >= 4 {
			doAdd(f[3]) // explicit format
			return
		}
		// No tool format given → arrow-key picker instead of silently defaulting
		// to gemma (a wrong format makes the model emit tool calls Tenant can't
		// parse). TEN-138. doAdd runs in the Update goroutine via onSelect.
		cmd = startPickerCmd(&listPicker{
			title:    "Tool format for " + name + " — how the model emits tool calls",
			hint:     "↑/↓ select · enter add · esc cancel  (match your model family; qwen for Qwen, etc.)",
			items:    modelToolFormats,
			selected: pickerIndexOf(modelToolFormats, defaultToolFormat),
			onSelect: func(choice string) tea.Cmd {
				return func() tea.Msg {
					doAdd(choice)
					return nil
				}
			},
			onCancel: func() tea.Cmd {
				return func() tea.Msg {
					return sysChatMsg{text: "model add cancelled — pick a tool format, or pass it explicitly: /model add " + name + " " + endpoint + " <qwen|gemma|llama|mistral|openai>"}
				}
			},
		})
		return
	case "add-cloud", "addcloud":
		// /model add-cloud <kind> <api-key>
		// One-shot setup for keyed cloud providers (zai, openai, grok, anthropic)
		// — pulls endpoint + default model + tool format from the catalog,
		// stores the key in credentials.json (0600). After the add succeeds,
		// fetches the live model catalog and shows an arrow-key picker so
		// the operator can pin a variant (glm-4.6 vs glm-5.1 etc.) without
		// dropping to the CLI or editing config.json.
		if len(f) < 3 {
			m.sysChat("usage: /model add-cloud <kind> <api-key>   e.g. /model add-cloud zai sk-xxxx   (kinds: zai, openai, grok, anthropic)")
			return
		}
		kind, key := f[1], f[2]
		status, err := m.cfg.Models.AddCloudModel(kind, key)
		if err != nil {
			m.appendFeed(cErr.Render("✗ add-cloud failed: " + clip(err.Error(), 70)))
			m.sysChat("could not add cloud backend: " + err.Error())
			return
		}
		m.appendFeed(cOK.Render("＋ cloud backend added: " + kind))
		m.sysChat(status)
		// Fetch live model variants in a tea.Cmd so the UI stays
		// responsive. On return, dispatch pickerStartMsg with the
		// onSelect wired to pin the choice.
		ctl := m.cfg.Models
		providerName := kind
		cmd = func() tea.Msg {
			models, err := ctl.ListProviderModels(providerName)
			if err != nil || len(models) == 0 {
				return sysChatMsg{
					text: "added but could not list model variants for picker: " + safeErr(err) +
						"\n(use `/model models " + providerName + "` later to see options)",
				}
			}
			return pickerStartMsg{picker: &listPicker{
				title: "Pick a model variant for " + providerName + " (" + fmt.Sprintf("%d available", len(models)) + ")",
				hint:  "↑/↓ select · enter pin & switch · esc keep catalog default",
				items: models,
				onSelect: func(choice string) tea.Cmd {
					return func() tea.Msg {
						s, _, perr := ctl.UseModel(providerName, choice)
						if perr != nil {
							return sysChatMsg{text: "pinned model but switch failed: " + perr.Error()}
						}
						return sysChatMsg{text: "✓ pinned " + choice + " on " + providerName + ":\n" + s}
					}
				},
				onCancel: func() tea.Cmd {
					return func() tea.Msg {
						return sysChatMsg{text: "kept catalog default for " + providerName + " (change later with `/model use " + providerName + " <model>`)"}
					}
				},
			}}
		}
	case "remove", "rm", "delete":
		if len(f) < 2 {
			m.sysChat("usage: /model remove <name>")
			return
		}
		status, err := m.cfg.Models.RemoveModel(f[1])
		if err != nil {
			m.sysChat("could not remove: " + err.Error())
			return
		}
		m.appendFeed(cSys.Render("－ backend removed: " + f[1]))
		m.sysChat(status)
	case "models", "model-list", "list-models":
		// /model models [<name>] — discover available model variants from
		// the provider's endpoint (live fetch). Without a name, queries
		// the currently-active provider.
		target := ""
		if len(f) >= 2 {
			target = f[1]
		}
		ids, err := m.cfg.Models.ListProviderModels(target)
		if err != nil {
			m.sysChat("could not list models: " + err.Error())
			return
		}
		if len(ids) == 0 {
			m.sysChat("(no models reported by the endpoint)")
			return
		}
		name := target
		if name == "" {
			name = "active"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "models served by %q (%d):\n", name, len(ids))
		for _, id := range ids {
			fmt.Fprintf(&b, "  · %s\n", id)
		}
		b.WriteString("\nswitch + pin a variant with: /model use " + name + " <model>")
		m.sysChat(strings.TrimRight(b.String(), "\n"))
	case "pick", "picker", "choose":
		// Arrow-key picker: pick provider → live cloud models → swap. (TEN-173)
		cmd = m.startModelPicker()
		return
	default:
		m.sysChat("usage: /model   |   /model pick   |   /model use <name> [<model>]   |   /model models [<name>]   |   /model add <name> <endpoint> [fmt]   |   /model add-cloud <kind> <api-key>   |   /model remove <name>")
	}
	return cmd
}

// --- C3: /research history / show / replay / delete ---

// isResearchSub returns true if word is a known /research sub-command. Keeps
// `/research <question>` working as the default when the first word isn't a
// reserved keyword.
func isResearchSub(word string) bool {
	switch strings.ToLower(word) {
	case "history", "list", "show", "replay", "delete", "rm":
		return true
	}
	return false
}

// splitFirstWord returns ("first", "rest", true) for "first rest", or ("","",false)
// for empty input. The rest is left as-is (whitespace within preserved).
func splitFirstWord(s string) (first, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:]), true
	}
	return s, "", true
}

// isGoalSub returns true if word is a known /goal management sub-command.
// Anything else (including the empty string) is treated as a CONDITION,
// matching Claude Code's UX ("/goal write a test" sets the condition).
func isGoalSub(word string) bool {
	switch strings.ToLower(word) {
	case "show", "status", "clear", "stop", "off", "reset", "cancel":
		return true
	}
	return false
}

// renderGoalStatus formats a GoalStatus for the system chat. Concise; the
// status bar will eventually carry a one-liner overlay but this is the
// detailed view for /goal show.
func renderGoalStatus(st GoalStatus) string {
	if !st.Active {
		return "🎯 no goal active. set one with /goal <condition>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🎯 goal: %s\n", st.Condition)
	fmt.Fprintf(&b, "   turns: %d / %d", st.Turns, st.MaxTurns)
	switch {
	case st.GoalLoopCeiling > 0:
		fmt.Fprintf(&b, "   loop ceiling: %d/turn", st.GoalLoopCeiling)
	case st.GoalLoopCeiling < 0:
		b.WriteString("   loop ceiling: unlimited")
	}
	if st.ElapsedFmt != "" {
		fmt.Fprintf(&b, "   elapsed: %s", st.ElapsedFmt)
	}
	if st.Met {
		b.WriteString("   status: ✦ met")
	}
	b.WriteString("\n")
	if st.LastJudge != "" {
		fmt.Fprintf(&b, "   judge: %s", st.LastJudge)
	} else {
		b.WriteString("   judge: (no evaluation yet)")
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleAgents routes /agents [sub] [args...] — manages the named sub-agent
// profile registry. Bare `/agents` lists; sub-commands `add / remove / show
// / soul` mutate. Mutations apply LIVE via the AgentControl implementation.
func (m *model) handleAgents(arg string) {
	if m.cfg.Agents == nil {
		m.sysChat("agents control is not available in this session")
		return
	}
	f := strings.Fields(arg)
	if len(f) == 0 {
		// List.
		rows, err := m.cfg.Agents.List()
		if err != nil {
			m.sysChat("agents list: " + err.Error())
			return
		}
		if len(rows) == 0 {
			m.sysChat("no named agents configured. add one with:\n  /agents add <name> <provider> [<model>] [-- <description>]\nexample:\n  /agents add researcher zai glm-4.6 -- web-research specialist on Z.ai")
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d named agent(s):\n", len(rows))
		for _, r := range rows {
			mark := "✓"
			if !r.Valid {
				mark = "⚠"
			}
			soul := ""
			if r.HasSoul {
				soul = " · has soul"
			}
			fmt.Fprintf(&b, "  %s %-20s %s/%s%s", mark, r.Name, r.Provider, r.Model, soul)
			if r.Description != "" {
				fmt.Fprintf(&b, " — %s", r.Description)
			}
			b.WriteString("\n")
		}
		b.WriteString("(use /agents show <name> to see the full identity, /agents soul <name> <md> to edit identity)")
		m.sysChat(strings.TrimRight(b.String(), "\n"))
		return
	}
	sub := strings.ToLower(f[0])
	switch sub {
	case "add", "set":
		// /agents add <name> <provider> [<model>] [-- <description>]
		// The optional `-- <description>` lets the operator write a free-form
		// description that the orchestrator sees ("when to use this agent").
		if len(f) < 3 {
			m.sysChat("usage: /agents add <name> <provider> [<model>] [-- <description>]   e.g. /agents add researcher zai glm-4.6 -- deep-web researcher")
			return
		}
		name := f[1]
		provider := f[2]
		mdl := ""
		desc := ""
		// Find the optional `--` separator for description.
		descAt := -1
		for i, w := range f {
			if w == "--" && i >= 3 {
				descAt = i
				break
			}
		}
		if descAt >= 0 {
			if descAt > 3 {
				mdl = f[3]
			}
			desc = strings.TrimSpace(strings.Join(f[descAt+1:], " "))
		} else if len(f) >= 4 {
			mdl = f[3]
		}
		status, err := m.cfg.Agents.Add(name, provider, mdl, desc, "")
		if err != nil {
			m.appendFeed(cErr.Render("✗ agents add: " + clip(err.Error(), 80)))
			m.sysChat("agents add: " + err.Error())
			return
		}
		m.appendFeed(cOK.Render("＋ agent: " + name))
		m.sysChat(status)
	case "remove", "rm", "delete":
		if len(f) < 2 {
			m.sysChat("usage: /agents remove <name>")
			return
		}
		status, err := m.cfg.Agents.Remove(f[1])
		if err != nil {
			m.sysChat("agents remove: " + err.Error())
			return
		}
		m.sysChat("🗑  " + status)
	case "rename", "mv":
		// /agents rename <old> <new> — move a profile to a new name lossless.
		if len(f) < 3 {
			m.sysChat("usage: /agents rename <old> <new>")
			return
		}
		status, err := m.cfg.Agents.Rename(f[1], f[2])
		if err != nil {
			m.appendFeed(cErr.Render("✗ agents rename: " + clip(err.Error(), 80)))
			m.sysChat("agents rename: " + err.Error())
			return
		}
		m.appendFeed(cOK.Render("✎ renamed: " + f[1] + " → " + f[2]))
		m.sysChat(status)
	case "model":
		// /agents model <name> <provider> [model]
		// Swap just the provider+model pinning. Preserves soul + description.
		if len(f) < 3 {
			m.sysChat("usage: /agents model <name> <provider> [model]   e.g. /agents model researcher zai glm-4.6")
			return
		}
		mdl := ""
		if len(f) >= 4 {
			mdl = f[3]
		}
		status, err := m.cfg.Agents.SetModel(f[1], f[2], mdl)
		if err != nil {
			m.appendFeed(cErr.Render("✗ agents model: " + clip(err.Error(), 80)))
			m.sysChat("agents model: " + err.Error())
			return
		}
		m.appendFeed(cOK.Render("⇄ " + f[1] + " → " + f[2]))
		m.sysChat(status)
	case "show":
		if len(f) < 2 {
			m.sysChat("usage: /agents show <name>")
			return
		}
		d, err := m.cfg.Agents.Show(f[1])
		if err != nil {
			m.sysChat("agents show: " + err.Error())
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "agent: %s\n", d.Name)
		fmt.Fprintf(&b, "model: %s/%s\n", d.Provider, d.Model)
		if d.Description != "" {
			fmt.Fprintf(&b, "description: %s\n", d.Description)
		}
		if strings.TrimSpace(d.Soul) != "" {
			fmt.Fprintf(&b, "\n--- soul ---\n%s", d.Soul)
		} else {
			b.WriteString("\n(no custom soul — agent uses the role-stamped base soul)")
		}
		m.msgs = append(m.msgs, chatMsg{role: "assistant", content: b.String()})
	case "soul":
		// /agents soul <name> <markdown...>
		// Multi-word soul body is joined back into one string (operators can
		// paste multi-line markdown by escaping newlines as \n — see docs).
		if len(f) < 3 {
			m.sysChat("usage: /agents soul <name> <markdown>   (empty markdown clears)")
			return
		}
		name := f[1]
		body := strings.Join(f[2:], " ")
		status, err := m.cfg.Agents.SetSoul(name, body)
		if err != nil {
			m.sysChat("agents soul: " + err.Error())
			return
		}
		m.sysChat(status)
	default:
		m.sysChat("usage: /agents [list]   |   /agents add <name> <provider> [model] [-- desc]   |   /agents model <name> <provider> [model]   |   /agents rename <old> <new>   |   /agents soul <name> <md>   |   /agents show <name>   |   /agents remove <name>")
	}
}

// handleResearchSub routes /research history|show|replay|delete to the right
// implementation. The default-of-arg matches each sub-command's expectation.
func (m *model) handleResearchSub(sub, rest string) tea.Cmd {
	rc := m.cfg.Research
	switch strings.ToLower(sub) {
	case "history", "list":
		limit := 20 // sane default for the table
		if rest != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && n > 0 {
				limit = n
			}
		}
		rows, err := rc.ResearchHistory(limit)
		if err != nil {
			m.sysChat("research history: " + err.Error())
			return nil
		}
		m.sysChatStyled(renderResearchHistory(rows))
		return nil
	case "show":
		if rest == "" {
			m.sysChat("usage: /research show <id>")
			return nil
		}
		text, err := rc.ResearchShow(rest)
		if err != nil {
			m.sysChat("research show: " + err.Error())
			return nil
		}
		// The past report goes into the chat pane like a system message —
		// reads cleanly alongside the live conversation.
		m.msgs = append(m.msgs, chatMsg{role: "assistant", content: text})
		m.refresh()
		return nil
	case "replay":
		if rest == "" {
			m.sysChat("usage: /research replay <id>")
			return nil
		}
		if m.busy {
			m.sysChat("busy — wait for the current turn/research to finish")
			return nil
		}
		m.msgs = append(m.msgs, chatMsg{role: "user", content: "/research replay " + rest})
		m.busy = true
		m.interrupted = false
		m.chatFollow = true
		m.appendFeed(cSys.Render("⟳ replaying " + rest + " — Esc to stop"))
		rctx, cancel := context.WithCancel(m.ctx)
		m.turnCancel = cancel
		id := rest
		return func() tea.Msg {
			report, err := rc.ResearchReplay(rctx, id)
			cancel()
			return researchDoneMsg{report: report, err: err}
		}
	case "delete", "rm":
		if rest == "" {
			m.sysChat("usage: /research delete <id>")
			return nil
		}
		if err := rc.ResearchDelete(rest); err != nil {
			m.sysChat("research delete: " + err.Error())
			return nil
		}
		m.sysChat("🗑  deleted " + rest)
		return nil
	}
	m.sysChat("unknown /research sub-command: " + sub)
	return nil
}

// renderResearchHistory formats the run list as a table. Plain version for
// copy/save; styled version for the chat pane.
func renderResearchHistory(rows []ResearchHistoryRow) (string, string) {
	if len(rows) == 0 {
		return "no past research runs yet — try /research <question>", ""
	}
	var plain strings.Builder
	fmt.Fprintf(&plain, "%d run(s):\n", len(rows))
	for _, r := range rows {
		dur := "—"
		if r.Duration > 0 {
			dur = r.Duration.Round(time.Second).String()
		}
		started := r.Started.Local().Format("01-02 15:04")
		extra := ""
		if r.ReplayOf != "" {
			extra = "  (replay of " + r.ReplayOf + ")"
		}
		fmt.Fprintf(&plain, "  %s  %-7s  %3dF/%dR  %5s  %s%s\n",
			started, r.Status, r.NumFinds, r.NumRefs, dur,
			clipForList(r.Question, 60), extra)
	}
	plain.WriteString("(use /research show <id>, /research replay <id>, /research delete <id>)")
	return plain.String(), ""
}

// clipForList keeps the question column to a fixed visible width.
func clipForList(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

// renderModelList builds the /model listing as (plain, styled).
func (m *model) renderModelList() (string, string) {
	models := m.cfg.Models.ModelList()
	if len(models) == 0 {
		return "no model backends configured — `/model add <name> <endpoint>` or run `tenant setup`", ""
	}
	var p, s strings.Builder
	p.WriteString("Model backends  ● active  ○ idle   ·   /model use <name>\n")
	s.WriteString(feedTitleStyle.Render("Model backends") + cDim.Render("   ● active  ○ idle   ·   /model use <name>") + "\n")
	for _, mi := range models {
		mark, marker := "○", cDim.Render("○")
		if mi.Active {
			mark, marker = "●", cOK.Render("●")
		}
		model := mi.Model
		if model == "" {
			model = "(auto)"
		}
		degr := ""
		if mi.Degraded {
			degr = "  (degraded — echo fallback; /model use to recover)"
		}
		line := fmt.Sprintf("  %s %-14s %-10s %s  %s%s", mark, mi.Name, mi.Kind, model, mi.Endpoint, degr)
		p.WriteString(line + "\n")
		styled := fmt.Sprintf("  %s %s %s %s  %s%s", marker,
			cUser.Render(fmt.Sprintf("%-14s", mi.Name)), fmt.Sprintf("%-10s", mi.Kind),
			model, cDim.Render(mi.Endpoint), cErr.Render(degr))
		s.WriteString(styled + "\n")
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// handleSkills runs /skills subcommands and returns (plain, styled) for
// the chat. Only the list view is styled; action results are plain.
func (m *model) handleSkills(arg string) (string, string) {
	if m.cfg.Skills == nil {
		return "skills are not available in this session", ""
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 { // bare /skills → list
		list := m.cfg.Skills.SkillList()
		if len(list) == 0 {
			return "no skills yet — the agent saves them via skill_save, or `/skills add <name> | <desc> | <recipe>`", ""
		}
		on := 0
		for _, s := range list {
			if s.Enabled {
				on++
			}
		}
		var p, st strings.Builder
		head := fmt.Sprintf("Skills (%d, %d on)", len(list), on)
		p.WriteString(head + "  ● on  ○ off\n")
		st.WriteString(cHeading.Render(head) + cDim.Render("  ● on  ○ off") + "\n")
		for _, s := range list {
			status := ""
			if s.Status != "live" {
				status = " [" + s.Status + "]"
			}
			p.WriteString(fmt.Sprintf("  %s %s%s — %s\n", mark(s.Enabled), s.Name, status, s.Description))
			name := cName.Render(s.Name)
			if !s.Enabled {
				name = cDim.Render(s.Name)
			}
			st.WriteString("  " + markStyled(s.Enabled) + " " + name + cSys.Render(status) + cDim.Render(" — "+s.Description) + "\n")
		}
		return strings.TrimRight(p.String(), "\n"), strings.TrimRight(st.String(), "\n")
	}
	sub := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(arg, fields[0]))
	switch sub {
	case "add":
		parts := strings.SplitN(rest, "|", 3)
		if len(parts) != 3 {
			return "usage: /skills add <name> | <description> | <recipe>", ""
		}
		name := strings.TrimSpace(parts[0])
		if err := m.cfg.Skills.AddSkill(name, strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])); err != nil {
			return "add failed: " + err.Error(), ""
		}
		return "saved skill " + name, ""
	case "auto":
		// TEN-152: graduate induced skills from manual review to auto-accept.
		mode := strings.ToLower(strings.TrimSpace(rest))
		if mode == "" {
			cur := m.cfg.Skills.AutoAcceptMode()
			if cur == "" {
				cur = "off"
			}
			return "skills auto-accept: " + cur +
				"\n  off     = every induced skill waits for /skills accept" +
				"\n  on      = accept every new induced skill immediately" +
				"\n  trusted = accept only while your recent feedback is healthy (acks, no undos)" +
				"\n  set with: /skills auto <off|on|trusted>", ""
		}
		if err := m.cfg.Skills.SetAutoAccept(mode); err != nil {
			return "error: " + err.Error(), ""
		}
		return "skills auto-accept set to " + mode + " (applies on the next induction run)", ""
	case "enable", "disable":
		on := sub == "enable"
		if rest == "" {
			return "usage: /skills " + sub + " <name>", ""
		}
		ok, err := m.cfg.Skills.SetSkillEnabled(rest, on)
		if err != nil {
			return "error: " + err.Error(), ""
		}
		if !ok {
			return "no skill named " + rest, ""
		}
		return sub + "d skill " + rest, ""
	case "forget":
		ok, err := m.cfg.Skills.ForgetSkill(rest)
		if err != nil {
			return "error: " + err.Error(), ""
		}
		if !ok {
			return "no skill named " + rest, ""
		}
		return "forgot skill " + rest, ""
	case "accept":
		ok, err := m.cfg.Skills.AcceptSkill(rest)
		if err != nil {
			return "error: " + err.Error(), ""
		}
		if !ok {
			return "no proposed skill named " + rest, ""
		}
		return "accepted skill " + rest, ""
	case "seed":
		// /skills seed <bundle> — bulk-install a named bundle of starter
		// skills. Today only the "gstack" bundle (port of Garry Tan's YC
		// operating discipline). Idempotent — safe to re-run.
		bundle := strings.ToLower(rest)
		if bundle == "" {
			return "usage: /skills seed gstack   (installs Garry Tan's CEO/founder-mode skill bundle)", ""
		}
		if m.cfg.SkillSeeds == nil {
			return "skill seeding not wired in this session", ""
		}
		n, err := m.cfg.SkillSeeds(bundle)
		if err != nil {
			return "seed failed: " + err.Error(), ""
		}
		return fmt.Sprintf("seeded %d skill(s) from bundle %q", n, bundle), ""
	case "history", "log":
		// /skills history <name> — show every prior snapshot of a skill,
		// newest first. The current live row is NOT in history; entries
		// are the predecessors. Empty history = the skill has never been
		// edited (its current row is v1).
		if rest == "" {
			return "usage: /skills history <name>", ""
		}
		entries, err := m.cfg.Skills.SkillHistory(rest)
		if err != nil {
			return "history error: " + err.Error(), ""
		}
		if len(entries) == 0 {
			return fmt.Sprintf("skill %q has no edit history (current row is v1)", rest), ""
		}
		var p strings.Builder
		fmt.Fprintf(&p, "%d prior version(s) of %q (newest first):\n", len(entries), rest)
		for _, h := range entries {
			when := h.ChangedAt.Local().Format("2006-01-02 15:04")
			fmt.Fprintf(&p, "  v%d  %s  via %-9s  status=%s  desc=%q\n",
				h.Version, when, h.ChangeSource, h.PriorStatus, clipForSkillHistory(h.PriorDescription, 60))
		}
		p.WriteString("\n(use /skills diff <name> [vN] · /skills revert <name> vN)")
		return strings.TrimRight(p.String(), "\n"), ""
	case "diff":
		// /skills diff <name> [vN]
		// Default N = the most recent prior version (1 step back). Shows a
		// side-by-side description+recipe diff so operators can SEE what
		// changed without exporting + comparing files manually.
		if rest == "" {
			return "usage: /skills diff <name> [vN]   (default: most recent prior version)", ""
		}
		dfields := strings.Fields(rest)
		name := dfields[0]
		version := 0 // 0 = "most recent prior"
		if len(dfields) >= 2 {
			n, perr := parseSkillVersion(dfields[1])
			if perr != nil {
				return "version must be a positive integer (or vN), e.g. /skills diff " + name + " v3", ""
			}
			version = n
		}
		// Resolve "most recent prior" to a concrete version number.
		if version == 0 {
			entries, err := m.cfg.Skills.SkillHistory(name)
			if err != nil {
				return "diff error: " + err.Error(), ""
			}
			if len(entries) == 0 {
				return fmt.Sprintf("skill %q has no prior versions to diff against", name), ""
			}
			version = entries[0].Version
		}
		entries, err := m.cfg.Skills.SkillHistory(name)
		if err != nil {
			return "diff error: " + err.Error(), ""
		}
		var prior *SkillHistoryEntry
		for i := range entries {
			if entries[i].Version == version {
				prior = &entries[i]
				break
			}
		}
		if prior == nil {
			return fmt.Sprintf("no v%d in history of %q (try /skills history %s)", version, name, name), ""
		}
		cur, err := m.cfg.Skills.SkillCurrent(name)
		if err != nil {
			return "diff error: " + err.Error(), ""
		}
		if cur == nil {
			return fmt.Sprintf("skill %q not found", name), ""
		}
		return renderSkillDiff(prior, cur, version), ""
	case "revert":
		// /skills revert <name> vN — restore (description, recipe, status)
		// from a prior version. Current state gets snapshotted into history
		// (as a "revert" entry) before being overwritten — so reverts are
		// themselves reversible. Embedding is refreshed against the
		// restored description.
		if rest == "" {
			return "usage: /skills revert <name> vN", ""
		}
		rfields := strings.Fields(rest)
		if len(rfields) < 2 {
			return "usage: /skills revert <name> vN", ""
		}
		name := rfields[0]
		version, perr := parseSkillVersion(rfields[1])
		if perr != nil || version < 1 {
			return "version must be a positive integer (or vN), e.g. /skills revert " + name + " v2", ""
		}
		if err := m.cfg.Skills.SkillRevert(name, version); err != nil {
			return "revert error: " + err.Error(), ""
		}
		return fmt.Sprintf("reverted %q to v%d (current state saved as new history entry)", name, version), ""
	default:
		return "unknown /skills subcommand " + sub + " (add/enable/disable/forget/accept/seed/history/diff/revert)", ""
	}
}

// parseSkillVersion accepts "N" or "vN" (case-insensitive) so the operator
// can type whichever feels natural. Returns the integer N.
func parseSkillVersion(s string) (int, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid version %q", s)
	}
	return n, nil
}

// clipForSkillHistory truncates the description column in the history
// listing so each row stays one line.
func clipForSkillHistory(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

// renderSkillDiff produces a readable side-by-side view of one prior
// version vs. the current live state. Description shown inline; recipe
// is the bulk so we use a "===" separator instead of a per-line diff
// (which would require a diff library and isn't worth the dependency).
func renderSkillDiff(prior *SkillHistoryEntry, cur *SkillSnapshot, version int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== diff %q v%d → current ===\n\n", cur.Name, version)
	fmt.Fprintf(&b, "status:      %s  →  %s\n", prior.PriorStatus, cur.Status)
	fmt.Fprintf(&b, "description (v%d):\n  %s\n\n", version, prior.PriorDescription)
	fmt.Fprintf(&b, "description (now):\n  %s\n\n", cur.Description)
	fmt.Fprintf(&b, "--- recipe v%d (%d chars) ---\n%s\n\n", version, len(prior.PriorRecipe), prior.PriorRecipe)
	fmt.Fprintf(&b, "--- recipe now (%d chars) ---\n%s", len(cur.Recipe), cur.Recipe)
	return b.String()
}

// mark / markStyled render the on/off glyph (plain and colored).
func mark(on bool) string {
	if on {
		return "●"
	}
	return "○"
}

func markStyled(on bool) string {
	if on {
		return cOnMark.Render("●")
	}
	return cOffMark.Render("○")
}

// handleMemory routes /memory subcommands to the MemoryControl.
func (m *model) handleMemory(arg string) string {
	if m.cfg.Memory == nil {
		return "memory controls are not available in this session"
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return m.cfg.Memory.Stats()
	}
	sub := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(arg, fields[0]))
	switch sub {
	case "stats":
		return m.cfg.Memory.Stats()
	case "search":
		if rest == "" {
			return "usage: /memory search <query>"
		}
		return m.cfg.Memory.Search(rest)
	case "facts":
		return m.cfg.Memory.Facts(rest)
	case "recent":
		n := 10
		if rest != "" {
			fmt.Sscan(rest, &n)
		}
		return m.cfg.Memory.Recent(n)
	case "forget":
		if rest == "" {
			return "usage: /memory forget fact:<id> | ep:<id>"
		}
		return m.cfg.Memory.Forget(rest)
	case "soul":
		sf := strings.Fields(rest)
		if len(sf) == 0 {
			return m.cfg.Memory.SoulView()
		}
		if strings.ToLower(sf[0]) == "import" {
			p := strings.TrimSpace(strings.TrimPrefix(rest, sf[0]))
			if p == "" {
				return "usage: /memory soul import <path.md>"
			}
			return m.cfg.Memory.SoulImport(p)
		}
		return "the soul is view-only here — use `/memory soul import <path.md>` or edit the TOML in the OS"
	case "rules":
		sf := strings.Fields(rest)
		if len(sf) == 0 {
			return m.cfg.Memory.RulesView()
		}
		if strings.ToLower(sf[0]) == "import" {
			p := strings.TrimSpace(strings.TrimPrefix(rest, sf[0]))
			if p == "" {
				return "usage: /memory rules import <path.md|folder>"
			}
			return m.cfg.Memory.SoulImport(p) // rules ARE the soul's operating instructions
		}
		return "rules are the soul's operating instructions — `/memory rules` to view, `/memory rules import <path>` to set"
	case "distill":
		return m.cfg.Memory.Distill()
	case "profile", "user":
		if strings.EqualFold(strings.TrimSpace(rest), "refresh") {
			return m.cfg.Memory.ProfileRefresh()
		}
		return m.cfg.Memory.ProfileView()
	default:
		return "unknown /memory subcommand " + sub + " (stats/search/facts/recent/forget/soul/rules/profile/distill)"
	}
}

// renderToolList builds the /tools view as (plain, styled): grouped by
// plugin, one tool per line with a colored on/off mark and an enabled
// count per plugin, so it's actually scannable.
// showApproval renders a pending dangerous action and the decision menu.
func (m *model) showApproval(r ApprovalRequest) {
	plain := fmt.Sprintf("⚠ approval needed — %s\n  %s\n  (action: %s)\n  /approve · /approve session · /approve always · /deny",
		r.Category, r.Detail, r.Action)
	styled := cErr.Render("⚠ approval needed — "+r.Category) + "\n" +
		"  " + cName.Render(r.Detail) + "\n" +
		"  " + cDim.Render("action: "+r.Action) + "\n  " +
		cKey.Render("/approve") + cDim.Render(" (once) · ") +
		cKey.Render("/approve session") + cDim.Render(" · ") +
		cKey.Render("/approve always") + cDim.Render(" · ") +
		cKey.Render("/deny")
	m.sysChatStyled(plain, styled)
}

// resolveApproval maps the /approve argument to a decision.
func (m *model) resolveApproval(arg string) {
	var d ApprovalDecision
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "once":
		d = ApproveOnce
	case "session":
		d = ApproveSession
	case "always":
		d = ApproveAlways
	default:
		m.sysChat("usage: /approve [session|always]")
		return
	}
	m.sendDecision(d)
}

// sendDecision answers the oldest pending approval and reports the outcome.
func (m *model) sendDecision(d ApprovalDecision) {
	if len(m.pending) == 0 {
		m.sysChat("nothing is awaiting approval")
		return
	}
	r := m.pending[0]
	m.pending = m.pending[1:]
	select {
	case r.Reply <- d:
	default: // requester gone (e.g. turn cancelled); nothing to do
	}
	verb := map[ApprovalDecision]string{
		DenyOnce:       "denied",
		ApproveOnce:    "approved (once)",
		ApproveSession: "approved for this session",
		ApproveAlways:  "approved always (saved)",
	}[d]
	m.sysChat(verb + " — " + r.Category)
}

// renderPermissions builds the /permissions view as (plain, styled).
func (m *model) renderPermissions() (string, string) {
	infos := m.cfg.Perms.Permissions()
	var p, s strings.Builder
	p.WriteString("Permissions  (ask / allow / deny)   ·   /permissions set <category> <mode>\n")
	s.WriteString(cHeading.Render("Permissions") + cDim.Render("  ask / allow / deny   ·   /permissions set <category> <mode>") + "\n")
	for _, in := range infos {
		p.WriteString(fmt.Sprintf("  %-12s %-5s  %s\n", in.Category, in.Mode, in.Desc))
		ms := cSys // ask = yellow
		switch in.Mode {
		case "allow":
			ms = cOnMark
		case "deny":
			ms = cErr
		}
		s.WriteString("  " + cKey.Render(fmt.Sprintf("%-12s", in.Category)) + " " +
			ms.Render(fmt.Sprintf("%-5s", in.Mode)) + "  " + cDim.Render(in.Desc) + "\n")
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

func (m *model) renderToolList() (string, string) {
	if m.cfg.Tools == nil {
		return "no tools configured", ""
	}
	list := m.cfg.Tools.ToolList()
	if len(list) == 0 {
		return "no tools loaded (this is a memory-only chat)", ""
	}
	var p, s strings.Builder
	p.WriteString("Tools  ● on  ○ off   ·   /enable|/disable <tool>   |   /enable skill <plugin>\n")
	s.WriteString(cHeading.Render("Tools") + cDim.Render("  ● on  ○ off   ·   /enable|/disable <tool>   |   /enable skill <plugin>") + "\n")

	seen := map[string]bool{}
	for _, t := range list {
		if seen[t.Plugin] {
			continue
		}
		seen[t.Plugin] = true
		// Collect this plugin's tools (preserve order) + enabled count.
		var tools []ToolInfo
		on := 0
		for _, u := range list {
			if u.Plugin == t.Plugin {
				tools = append(tools, u)
				if u.Enabled {
					on++
				}
			}
		}
		count := fmt.Sprintf("(%d/%d on)", on, len(tools))
		p.WriteString(fmt.Sprintf("\n%s %s\n", t.Plugin, count))
		s.WriteString("\n" + cKey.Render(t.Plugin) + " " + cDim.Render(count) + "\n")
		for _, u := range tools {
			p.WriteString("    " + mark(u.Enabled) + " " + u.Name + "\n")
			name := cName.Render(u.Name)
			if !u.Enabled {
				name = cDim.Render(u.Name)
			}
			s.WriteString("    " + markStyled(u.Enabled) + " " + name + "\n")
		}
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

func (m *model) applyEvent(e agent.Event) {
	switch e.Kind {
	case agent.EventTurnStart:
		m.appendFeed(cDim.Render(time.Now().Format("15:04:05") + " turn start"))
	case agent.EventUsage:
		// Actual tokens (input + output) reported by the backend for this
		// LLM call — summed for the session total, and split in/out so the
		// footer can show a directional counter.
		m.sessionTok += e.PromptTokens + e.CompletionTokens
		m.sessionTokIn += e.PromptTokens
		m.sessionTokOut += e.CompletionTokens
		// Persist this call's usage for long-term cost audit. MAIN agent
		// only (team usage flows through applyTeamEvent, intentionally not
		// recorded here — see TEN-167). Non-fatal: a recorder error must
		// never block the UI, so it's swallowed by the closure in wiring.
		if m.cfg.RecordUsage != nil {
			m.cfg.RecordUsage(e.PromptTokens, e.CompletionTokens)
		}
	case agent.EventMemory:
		if e.Budget != nil {
			// Gauge fullness against the model's REAL context window, not the
			// writable budget. Total includes the static reserve (soul+system+
			// tools) that WritableBudget subtracts out, so Total/WritableBudget
			// overshoots 100% (the old "173% of 62.9k" bug). Total/ContextWindow
			// is a true 0-100% reading for the model in use; fall back to the
			// writable budget only when the window is unknown.
			window := e.Budget.ContextWindow
			if window <= 0 {
				window = e.Budget.WritableBudget
			}
			if window > 0 {
				m.budgetPct = int(float64(e.Budget.Total) / float64(window) * 100)
				m.budgetUsed = e.Budget.Total
				m.budgetCap = window
				note := ""
				if e.Budget.CompactionRecommended {
					note = " " + cErr.Render("(compaction soon)")
				}
				m.appendFeed(cDim.Render(fmt.Sprintf("ctx assembled: %d tok (%d%% of context window)", e.Budget.Total, m.budgetPct)) + note)
				// Static breakdown (TEN-214): the tool-definition cost dominates a
				// full mux and is the thing that silently eats the window. Surface
				// soul/system/tools each turn, plus the writable budget the
				// variable tiers were actually sized against (measured static, not
				// the fixed reserves) so "where did my context go?" is answerable.
				stat := e.Budget.SoulTokens + e.Budget.SystemTokens + e.Budget.ToolTokens
				m.appendFeed(cDim.Render(fmt.Sprintf(
					"  static %s (soul %s · sys %s · tools %s) · working %s · writable %s",
					humanTokens(stat), humanTokens(e.Budget.SoulTokens), humanTokens(e.Budget.SystemTokens),
					humanTokens(e.Budget.ToolTokens), humanTokens(e.Budget.WorkingTokens),
					humanTokens(e.Budget.EffectiveWritable))))
			}
		}
	case agent.EventToken:
		if m.assistantSealed {
			break // stale trailing token from an already-finalized turn (TEN-217)
		}
		if !m.streaming {
			m.msgs = append(m.msgs, chatMsg{role: "assistant"})
			m.streaming = true
		}
		m.msgs[len(m.msgs)-1].content += e.Text
	case agent.EventAssistant:
		// Non-stream fallback: whole assistant text at once.
		if m.assistantSealed {
			break // turnDoneMsg already reconciled this turn — don't double-post (TEN-217)
		}
		if !m.streaming {
			m.msgs = append(m.msgs, chatMsg{role: "assistant", content: e.Text})
		}
	case agent.EventSkills:
		m.appendFeed(cSys.Render("skills: " + clip(e.Text, 60)))
	case agent.EventToolCall:
		m.lastTool = e.Tool
		m.appendFeed(cTool.Render("→ "+e.Tool) + " " + cDim.Render(clip(e.Args, 60)))
	case agent.EventToolResult:
		mark := cOK.Render("✓")
		if e.IsErr {
			mark = cErr.Render("✗")
		}
		m.appendFeed(fmt.Sprintf("%s %s %s", mark, cTool.Render(e.Tool), cDim.Render(clip(oneLine(e.Result), 70))))
	case agent.EventValidation:
		m.appendFeed(cErr.Render("! invalid tool call " + e.Tool + ": " + clip(e.Text, 60)))
	case agent.EventCompact:
		m.appendFeed(cSys.Render("⊟ context compacted: " + e.Text))
	case agent.EventInterject:
		m.appendFeed(cSys.Render("↪ folding in your message: " + clip(e.Text, 60)))
	case agent.EventFinal:
		m.streaming = false
		m.appendFeed(cDim.Render("answer ready"))
	case agent.EventTruncated:
		m.streaming = false
		m.appendFeed(cErr.Render("loop ceiling hit — synthesized"))
	case agent.EventError:
		m.err = e.Text
		m.appendFeed(cErr.Render("error: " + clip(e.Text, 80)))
	}
}

// applyTeamEvent renders a spawned sub-agent's event into the feed and
// sums its token usage into the separate team counter.
func (m *model) applyTeamEvent(te TeamEvent) {
	e := te.Event
	tag := "[" + te.AgentID + "]"
	// C4: roll tool activity into the research timeline (if a run is active).
	// The timeline only knows the per-agent SubQ from research's structured
	// updates; tool counts come from these events as they fire.
	if m.researchTimeline != nil {
		m.tallyTimelineToolEvent(te)
	}
	switch e.Kind {
	case agent.EventUsage:
		m.teamTok += e.PromptTokens + e.CompletionTokens
	case agent.EventToolCall:
		m.appendFeed(cTool.Render(tag+" → "+e.Tool) + " " + cDim.Render(clip(e.Args, 50)))
	case agent.EventToolResult:
		mark := cOK.Render("✓")
		if e.IsErr {
			mark = cErr.Render("✗")
		}
		// Show result size + preview. Without this the feed hid WHY a tool
		// failed — "✗ web_navigate" looked identical whether Chrome crashed,
		// the page 404'd, or the URL was malformed. Errors get the full first
		// line; success gets a short preview.
		preview := strings.ReplaceAll(strings.ReplaceAll(e.Result, "\n", " "), "\r", " ")
		preview = strings.TrimSpace(preview)
		max := 80
		if e.IsErr {
			max = 140 // errors carry the diagnostic — give them room
		}
		if len(preview) > max {
			preview = preview[:max] + "…"
		}
		line := mark + " " + cDim.Render(tag+" "+e.Tool)
		if preview != "" {
			line += " " + cDim.Render(fmt.Sprintf("(%d) %s", len(e.Result), preview))
		}
		m.appendFeed(line)
	case agent.EventFinal:
		m.appendFeed(cDim.Render(tag + " done"))
	case agent.EventError:
		m.appendFeed(cErr.Render(tag + " error: " + clip(e.Text, 80)))
	}
}

// tallyTimelineToolEvent increments the per-agent tool counters in the active
// timeline snapshot. Creates a placeholder row when the orchestrator's plan
// update hasn't arrived yet — better to count than to drop the event.
func (m *model) tallyTimelineToolEvent(te TeamEvent) {
	rt := m.researchTimeline
	if rt == nil {
		return
	}
	row := rt.agentByID[te.AgentID]
	if row == nil {
		row = &ResearchAgentRow{ID: te.AgentID, Status: "running"}
		rt.agentByID[te.AgentID] = row
		rt.Agents = append(rt.Agents, row)
	}
	switch te.Event.Kind {
	case agent.EventToolCall:
		row.NumTools++
	case agent.EventToolResult:
		if te.Event.IsErr {
			row.NumErr++
		} else {
			row.NumOK++
		}
	}
}

// applyResearchTimeline processes one structured update from the orchestrator,
// initializing the snapshot on `started` and clearing it on `done`. Each Kind
// only consults the matching pointer; unknown Kind = no-op.
func (m *model) applyResearchTimeline(u ResearchTimelineUpdate) {
	switch u.Kind {
	case "started":
		if u.Started == nil {
			return
		}
		m.researchTimeline = &researchTimelineState{
			Question:  u.Started.Question,
			Phase:     ResearchPhasePlanning,
			StartedAt: time.Now(),
			Total:     u.Total,
			agentByID: map[string]*ResearchAgentRow{},
		}
	case "plan":
		if m.researchTimeline == nil || u.Plan == nil {
			return
		}
		m.researchTimeline.Cycle = u.Cycle
		m.researchTimeline.Plan = u.Plan.SubQuestions
		m.researchTimeline.Phase = ResearchPhaseDispatch
		// Drop the prior cycle's agent rollups — each cycle re-spawns.
		m.researchTimeline.Agents = nil
		m.researchTimeline.agentByID = map[string]*ResearchAgentRow{}
		// Seed placeholder rows mapped to sub-questions so the user sees the
		// plan even before the first tool call fires.
		for _, sq := range u.Plan.SubQuestions {
			row := &ResearchAgentRow{SubQ: sq, Status: "pending"}
			m.researchTimeline.Agents = append(m.researchTimeline.Agents, row)
		}
	case "wave":
		if m.researchTimeline == nil || u.Wave == nil {
			return
		}
		m.researchTimeline.WaveStatus = fmt.Sprintf("dispatched %d–%d of %d", u.Wave.From, u.Wave.To, u.Wave.Total)
		// As the wave dispatches, the placeholders for THIS wave's slots
		// flip to "running". We don't know which agent id maps to which
		// placeholder yet — the orchestrator surfaces that via agent_status
		// updates. Until then the rows stay in "pending" / get rewritten.
	case "agent_status":
		if m.researchTimeline == nil || u.Agent == nil {
			return
		}
		rt := m.researchTimeline
		row := rt.agentByID[u.Agent.ID]
		if row == nil {
			// First time we've heard about this id — adopt the first pending
			// placeholder so the sub-question lines up with the agent. Falls
			// back to a fresh row if no placeholder is available.
			for _, r := range rt.Agents {
				if r.ID == "" {
					row = r
					row.ID = u.Agent.ID
					rt.agentByID[u.Agent.ID] = row
					break
				}
			}
			if row == nil {
				row = &ResearchAgentRow{ID: u.Agent.ID}
				rt.agentByID[u.Agent.ID] = row
				rt.Agents = append(rt.Agents, row)
			}
		}
		// Merge: caller may pass partial updates (e.g. just status).
		if u.Agent.SubQ != "" {
			row.SubQ = u.Agent.SubQ
		}
		if u.Agent.Status != "" {
			row.Status = u.Agent.Status
		}
		if u.Agent.ResultLen > 0 {
			row.ResultLen = u.Agent.ResultLen
		}
		// Tool counts are owned by tallyTimelineToolEvent; don't clobber here.
	case "reflect_done":
		if m.researchTimeline == nil || u.Reflect == nil {
			return
		}
		m.researchTimeline.Phase = ResearchPhaseReflect
		m.researchTimeline.LastReflectGaps = u.Reflect.Gaps
	case "synth":
		if m.researchTimeline == nil || u.Synth == nil {
			return
		}
		if u.Synth.Starting {
			m.researchTimeline.Phase = ResearchPhaseSynth
		}
	case "done":
		if m.researchTimeline == nil || u.Done == nil {
			return
		}
		rt := m.researchTimeline
		switch u.Done.Status {
		case "interrupted":
			rt.Phase = ResearchPhaseInterrupt
		case "error":
			rt.Phase = ResearchPhaseError
		default:
			rt.Phase = ResearchPhaseDone
		}
		rt.DoneStatus = u.Done.Status
		rt.DoneError = u.Done.Error
		rt.DoneRefs = u.Done.NumRefs
		rt.DoneFinds = u.Done.NumFinds
		rt.DoneDuration = u.Done.Duration
		// Don't immediately clear — let the user see the final snapshot.
		// The researchDoneMsg handler clears m.researchTimeline a moment later
		// (after the chat-pane report is posted).
	}
}

// transcript renders the conversation as plain text for copy/save.
func (m *model) transcript() string {
	var b strings.Builder
	for _, msg := range m.msgs {
		b.WriteString(strings.ToUpper(msg.role) + ": " + msg.content + "\n\n")
	}
	return b.String()
}

// copyTranscript yanks the whole conversation to the clipboard and (as
// a robust fallback for troubleshooting) writes it to SavePath, then
// reports the outcome in the feed.
func (m *model) copyTranscript() {
	txt := m.transcript()
	clipErr := clipboard.WriteAll(txt)
	saved := ""
	if m.cfg.SavePath != "" {
		if err := os.WriteFile(m.cfg.SavePath, []byte(txt), 0o644); err == nil {
			saved = m.cfg.SavePath
		}
	}
	switch {
	case clipErr == nil && saved != "":
		m.appendFeed(cSys.Render("copied transcript → clipboard + " + saved))
	case clipErr == nil:
		m.appendFeed(cSys.Render("copied transcript → clipboard"))
	case saved != "":
		m.appendFeed(cSys.Render("clipboard unavailable; saved → " + saved))
	default:
		m.appendFeed(cErr.Render("copy failed: " + clipErr.Error()))
	}
}

func (m *model) appendFeed(line string) {
	m.feedLines = append(m.feedLines, line)
	if len(m.feedLines) > 500 {
		m.feedLines = m.feedLines[len(m.feedLines)-500:]
	}
}

// --- layout / render ---

func (m *model) resize(w, h int) {
	m.width, m.height = w, h
	bodyH := h - 6 // status bar (1) + textarea (~3) + help (1) + padding
	if bodyH < 3 {
		bodyH = 3
	}
	feedW := w / 3
	if feedW < 24 {
		feedW = 24
	}
	chatW := w - feedW - 3 - chatGutter // leave room for the left gutter
	if chatW < 20 {
		chatW = 20
	}
	if !m.ready {
		m.chat = viewport.New(chatW, bodyH)
		m.feed = viewport.New(feedW, bodyH)
	} else {
		m.chat.Width, m.chat.Height = chatW, bodyH
		m.feed.Width, m.feed.Height = feedW, bodyH
	}
	m.ta.SetWidth(chatW)
	m.ta.SetHeight(3)
	m.input.Width = chatW
}

// clearFeed wipes the visible chat + activity panes (display only; the agent's
// context is untouched). Backs /cls and the visual half of /clear. (TEN-181)
func (m *model) clearFeed() {
	m.msgs = nil
	m.feedLines = nil
	m.chatFollow = true
	m.feedFollow = true
	m.refresh()
}

func (m *model) refresh() {
	if !m.ready {
		return
	}
	m.chat.SetContent(m.renderChat())
	if m.chatFollow { // only stick to the bottom if the user hasn't scrolled up
		m.chat.GotoBottom()
	}
	m.feed.SetContent(m.renderFeed())
	if m.feedFollow {
		m.feed.GotoBottom()
	}
}

// scrollChat moves the chat viewport by a signed number of lines and
// re-engages follow once the user pages back to the bottom.
func (m *model) scrollChat(lines int) {
	if lines < 0 {
		m.chat.LineUp(-lines)
	} else {
		m.chat.LineDown(lines)
	}
	m.chatFollow = m.chat.AtBottom()
}

// pageStep is a near-full page (leave one line of overlap for context).
func (m *model) pageStep() int {
	if s := m.chat.Height - 1; s > 1 {
		return s
	}
	return 1
}

func (m *model) renderChat() string {
	var b strings.Builder
	for _, msg := range m.msgs {
		switch msg.role {
		case "user":
			b.WriteString(cUser.Render("you ") + msg.content + "\n\n")
		case "assistant":
			b.WriteString(cAgent.Render("tenant ") + msg.content + "\n\n")
		default: // system: styled form if present, else plain (default fg)
			disp := msg.rendered
			if disp == "" {
				disp = msg.content
			}
			b.WriteString(disp + "\n\n")
		}
	}
	return wrap(b.String(), m.chat.Width)
}

func (m *model) renderFeed() string {
	title := feedTitleStyle.Render("activity")
	parts := []string{title}
	if m.researchTimeline != nil {
		parts = append(parts, renderResearchTimeline(m.researchTimeline), strings.Repeat("─", 30))
	}
	parts = append(parts, strings.Join(m.feedLines, "\n"))
	return strings.Join(parts, "\n")
}

// renderResearchTimeline draws the C4 live status block above the activity
// feed: one-line header (question / phase / cycle / refs / elapsed), then
// per-agent rows under the current plan. Designed to fit ~10 lines tall on
// a typical 30-row pane.
func renderResearchTimeline(rt *researchTimelineState) string {
	if rt == nil {
		return ""
	}
	// Header line — phase icon + question + cycle + elapsed.
	var elapsed string
	if !rt.StartedAt.IsZero() {
		d := time.Since(rt.StartedAt).Round(time.Second)
		if rt.Phase == ResearchPhaseDone || rt.Phase == ResearchPhaseError || rt.Phase == ResearchPhaseInterrupt {
			if rt.DoneDuration > 0 {
				d = rt.DoneDuration.Round(time.Second)
			}
		}
		elapsed = d.String()
	}
	phaseIcon, phaseStyle := researchPhaseGlyph(rt.Phase)
	cycle := ""
	if rt.Cycle > 0 {
		cycle = fmt.Sprintf("cycle %d/%d", rt.Cycle, max1(rt.Total))
	} else {
		cycle = "plan…"
	}
	q := clip(rt.Question, 40)
	header := fmt.Sprintf("%s %s · %s · %s",
		phaseStyle.Render(phaseIcon+" "+string(rt.Phase)),
		cName.Render(q), cDim.Render(cycle), cDim.Render(elapsed))
	var b strings.Builder
	b.WriteString(feedTitleStyle.Render("research") + "\n")
	b.WriteString(header + "\n")

	// Wave status / reflection / synth hint
	switch rt.Phase {
	case ResearchPhaseDispatch:
		if rt.WaveStatus != "" {
			b.WriteString(cDim.Render("  "+rt.WaveStatus) + "\n")
		}
	case ResearchPhaseReflect:
		if len(rt.LastReflectGaps) > 0 {
			b.WriteString(cDim.Render(fmt.Sprintf("  reflecting → %d follow-up(s)", len(rt.LastReflectGaps))) + "\n")
		} else {
			b.WriteString(cDim.Render("  reflecting on gaps…") + "\n")
		}
	case ResearchPhaseSynth:
		b.WriteString(cDim.Render("  synthesizing final report…") + "\n")
	case ResearchPhaseDone:
		b.WriteString(cOK.Render(fmt.Sprintf("  ✦ done — %d finding(s), %d ref(s)", rt.DoneFinds, rt.DoneRefs)) + "\n")
	case ResearchPhaseError:
		b.WriteString(cErr.Render("  ✗ "+clip(rt.DoneError, 70)) + "\n")
	case ResearchPhaseInterrupt:
		b.WriteString(cSys.Render("  ⏹ interrupted") + "\n")
	}

	// Per-agent rows.
	for i, row := range rt.Agents {
		statusGlyph, statusStyle := researchAgentGlyph(row.Status)
		num := fmt.Sprintf("%d.", i+1)
		sub := clip(row.SubQ, 50)
		if sub == "" {
			sub = cDim.Render("(awaiting plan)")
		}
		// Tool tallies — show only when there's something to show.
		tally := ""
		if row.NumTools > 0 || row.NumErr > 0 {
			tally = fmt.Sprintf("%d tools", row.NumTools)
			if row.NumErr > 0 {
				tally += fmt.Sprintf(", %d ✗", row.NumErr)
			}
			if row.ResultLen > 0 {
				tally += fmt.Sprintf(", %dch", row.ResultLen)
			}
		}
		line := "  " + cDim.Render(num) + " " + statusStyle.Render(statusGlyph) + " " + sub
		if tally != "" {
			line += " " + cDim.Render("("+tally+")")
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// researchPhaseGlyph picks the icon + style for a phase header.
func researchPhaseGlyph(p ResearchPhase) (string, lipgloss.Style) {
	switch p {
	case ResearchPhasePlanning:
		return "📝", cSys
	case ResearchPhaseDispatch:
		return "⚡", cTool
	case ResearchPhaseReflect:
		return "🔁", cSys
	case ResearchPhaseSynth:
		return "✍", cSys
	case ResearchPhaseDone:
		return "✦", cOK
	case ResearchPhaseError:
		return "✗", cErr
	case ResearchPhaseInterrupt:
		return "⏹", cSys
	case ResearchPhaseClarify:
		return "🤔", cSys
	}
	return "·", cDim
}

// researchAgentGlyph picks the per-row icon + style by agent status.
func researchAgentGlyph(s string) (string, lipgloss.Style) {
	switch s {
	case "running":
		return "↺", cTool
	case "done":
		return "✓", cOK
	case "error":
		return "✗", cErr
	case "truncated":
		return "!", cSys
	case "pending":
		return "·", cDim
	}
	return "·", cDim
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func (m *model) statusBar() string {
	state := "idle"
	if m.busy {
		state = m.spin.View() + "working — type to steer · esc to stop"
	}
	if len(m.pending) > 0 {
		state = "⚠ awaiting /approve"
	}
	left := fmt.Sprintf(" %s · %s/%s · agent=%s", state, m.cfg.Backend, m.cfg.Model, m.cfg.AgentID)
	right := "" // context % moved to the bottom-right footer
	if m.lastTool != "" {
		right = "tool:" + m.lastTool + " "
	}
	if m.ready && !m.chatFollow { // scrolled up: show position + how to return
		right = fmt.Sprintf("↑%d%% (PgDn↓) · ", int(m.chat.ScrollPercent()*100)) + right
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return statusBarStyle.Width(m.width).Render(left + strings.Repeat(" ", gap) + right)
}

// contextIndicator renders the bottom-right context-window gauge: a colored
// fill bar (green→yellow→red by utilization), the percentage, and the
// session's cumulative token count — à la Claude Code.
func (m *model) contextIndicator() string {
	bar := ctxBar(m.budgetPct, 12)
	s := fmt.Sprintf("ctx %s %d%% %s/%s · session %s", bar, m.budgetPct, humanTokens(m.budgetUsed), humanTokens(m.budgetCap), humanTokens(m.sessionTok))
	if m.teamTok > 0 {
		s += cDim.Render(" · team " + humanTokens(m.teamTok))
	}
	return s + " tok"
}

// tokenCounter renders the session's directional token counter — input
// (↑) and output (↓) — for the MAIN agent, dimmed, with a trailing space
// so it reads as a distinct chip just left of the ctx gauge. Returns an
// empty string before any usage is recorded so the footer stays clean.
func (m *model) tokenCounter() string {
	if m.sessionTokIn == 0 && m.sessionTokOut == 0 {
		return ""
	}
	return cDim.Render(fmt.Sprintf("↑%s ↓%s", humanTokens(m.sessionTokIn), humanTokens(m.sessionTokOut))) + " "
}

// reqTimer renders the live elapsed time of the in-flight turn, e.g.
// "⏱ 3.4s" (<60s), "⏱ 1:09" (m:ss, <1h), or "⏱ 1:40:05" (h:mm:ss, >=1h).
// Display-only, never persisted. Empty when no turn is in flight; the existing
// spinner tick drives the sub-second repaint, so no extra ticker is needed.
func (m *model) reqTimer() string {
	if !m.busy || m.reqStart.IsZero() {
		return ""
	}
	return cDim.Render("⏱ " + formatElapsed(time.Since(m.reqStart)))
}

// formatElapsed renders a duration as a counter that rolls minutes into an HOUR
// field past 60 min, so long-running turns keep counting (1:40:05, 25:00:00)
// instead of an unbounded minute field that looks like it resets. Under a
// minute: one-decimal seconds; under an hour: m:ss; otherwise h:mm:ss.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	total := int(d.Seconds())
	h, m, s := total/3600, (total%3600)/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// ctxBar draws a [████░░] gauge, colored by how full the context is.
func ctxBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	filled := pct * width / 100
	if filled > width {
		filled = width
	}
	st := cOK // <60% green
	switch {
	case pct >= 85:
		st = cErr // red — compaction imminent / over budget
	case pct >= 60:
		st = cSys // yellow — compaction recommended zone
	}
	return cDim.Render("[") + st.Render(strings.Repeat("█", filled)) +
		cDim.Render(strings.Repeat("░", width-filled)+"]")
}

// humanTokens formats a token count compactly (1234 → "1.2k").
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func (m *model) View() string {
	if !m.ready {
		return "starting tenant…"
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, chatPad.Render(m.chat.View()), paneBorder.Render(""), m.feed.View())
	// Footer: help (left) + context gauge (bottom-right), width-aligned,
	// sharing the chat's left gutter.
	help := strings.Repeat(" ", chatGutter) + "Enter send · /help · PgUp/PgDn scroll · Ctrl-Y copy · /exit quit"
	ctx := m.contextIndicator()
	// Right cluster, read left→right: live request timer (only while busy),
	// directional in/out token counter, then the ctx gauge. Each piece is
	// independently sized so the gap math never lets the cluster overflow.
	right := m.tokenCounter() + ctx
	if t := m.reqTimer(); t != "" {
		right = t + " " + right
	}
	if m.selectMode {
		right = cOK.Render("✂ SELECT — drag to copy · Ctrl+S/Esc to exit") + " " + right
	}
	gap := m.width - lipgloss.Width(help) - lipgloss.Width(right) - 1
	if gap < 1 {
		gap = 1
	}
	footer := cDim.Render(help) + strings.Repeat(" ", gap) + right + " "
	// Picker mode replaces the input area with the picker view. Keeps
	// the chat + feed visible above so the operator has full context
	// while choosing.
	var inputArea string
	if m.secretEntry != nil || m.setupEntry != nil || m.configureSession != nil {
		inputArea = m.input.View()
	} else {
		inputArea = m.ta.View()
	}
	if m.picker != nil {
		inputArea = m.renderPicker()
	}
	return m.statusBar() + "\n" + body + "\n" + chatPad.Render(inputArea) + "\n" + footer
}

// --- helpers ---

func clip(s string, n int) string {
	s = oneLine(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// wrap hard-wraps long lines to a VISIBLE width, never slicing inside an
// ANSI escape sequence — so styled lists/help don't get corrupted
// mid-color-code. Counts runes (display cells), copies escape sequences
// through verbatim without advancing the column.
func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(wrapLine(line, width))
	}
	return out.String()
}

func wrapLine(line string, width int) string {
	var out strings.Builder
	col := 0
	for i := 0; i < len(line); {
		if line[i] == 0x1b { // ESC: copy the whole escape sequence verbatim
			j := i + 1
			if j < len(line) && line[j] == '[' { // CSI ... final byte @-~
				j++
				for j < len(line) && (line[j] < '@' || line[j] > '~') {
					j++
				}
				if j < len(line) {
					j++ // include the final byte
				}
			}
			out.WriteString(line[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		if col >= width {
			out.WriteByte('\n')
			col = 0
		}
		out.WriteString(line[i : i+size])
		i += size
		col++
		_ = r
	}
	return out.String()
}

// --- listPicker: minimal arrow-key list selector ---
//
// Used after /model add-cloud to let the operator pick a model variant
// from the provider's live catalog (mirrors OpenClaw's add-provider
// flow). Generic enough to reuse for any future "pick one from N"
// prompt: skill bundle, agent profile, etc.
//
// Lifecycle: m.picker is non-nil → all keys route to handlePickerKey,
// rendered via renderPicker(). Selecting (Enter) calls onSelect(item)
// and clears picker. Esc/Ctrl-C clears without selecting.

type listPicker struct {
	title       string   // shown above the items
	hint        string   // footer hint (e.g. "↑/↓ select · enter pick · esc cancel")
	items       []string // selectable rows
	selected    int      // index into items
	currentMark string   // optional: items[i] == currentMark → render with a marker
	onSelect    func(choice string) tea.Cmd
	onCancel    func() tea.Cmd // optional; nil = no message on cancel
}

// pickerStartMsg asks the TUI's Update goroutine to enter picker mode.
// Dispatched as a tea.Cmd from elsewhere (e.g. after AddCloudModel +
// ListProviderModels finish, on a tea.Cmd goroutine).
type pickerStartMsg struct {
	picker *listPicker
}

func startPickerCmd(p *listPicker) tea.Cmd {
	return func() tea.Msg { return pickerStartMsg{picker: p} }
}

// handlePickerKey routes keypresses while a picker is active. Returns
// the cmd to run (typically nil — selection runs onSelect inline so we
// can chain it; explicit return type for tea conformance).
func (m *model) handlePickerKey(msg tea.KeyMsg) tea.Cmd {
	p := m.picker
	if p == nil || len(p.items) == 0 {
		m.picker = nil
		return nil
	}
	switch msg.String() {
	case "up", "k":
		p.selected--
		if p.selected < 0 {
			p.selected = len(p.items) - 1
		}
	case "down", "j":
		p.selected++
		if p.selected >= len(p.items) {
			p.selected = 0
		}
	case "home", "g":
		p.selected = 0
	case "end", "G":
		p.selected = len(p.items) - 1
	case "enter":
		choice := p.items[p.selected]
		onSelect := p.onSelect
		m.picker = nil
		if onSelect != nil {
			return onSelect(choice)
		}
	case "esc", "ctrl+c":
		onCancel := p.onCancel
		m.picker = nil
		if onCancel != nil {
			return onCancel()
		}
		m.sysChat("cancelled")
	}
	return nil
}

// renderPicker renders the picker into a string that replaces the input
// area while active. Width-aware so it fits the terminal.
func (m *model) renderPicker() string {
	p := m.picker
	if p == nil {
		return ""
	}
	_ = m.width // width-aware rendering reserved; currently fixed layout
	var b strings.Builder
	b.WriteString(cHeading.Render("⇣ "+p.title) + "\n")
	for i, item := range p.items {
		var line string
		marker := "  "
		if i == p.selected {
			marker = "▶ "
		}
		text := item
		if p.currentMark != "" && item == p.currentMark {
			text = item + cDim.Render("  (current)")
		}
		if i == p.selected {
			line = cOK.Render(marker + text)
		} else {
			line = "  " + text
		}
		b.WriteString(line + "\n")
	}
	if p.hint != "" {
		b.WriteString("\n" + cDim.Render(p.hint))
	} else {
		b.WriteString("\n" + cDim.Render("↑/↓ select · enter pick · esc cancel"))
	}
	return b.String()
}

// startModelPicker drives the arrow-key model swap (TEN-173): pick a configured
// provider → fetch its live models from the cloud → pick a model → swap live.
// Mirrors the /model add-cloud picker chain (provider pick is the extra first
// stage). Reuses listPicker + the pickerStartMsg async pattern; no backend
// changes — composes ModelList + ListProviderModels + UseModel.
func (m *model) startModelPicker() tea.Cmd {
	infos := m.cfg.Models.ModelList()
	if len(infos) == 0 {
		m.sysChat("no model providers configured — add one with `/model add-cloud <kind> <api-key>` (zai/openai/grok/anthropic) or `/setup`")
		return nil
	}
	ctl := m.cfg.Models

	// fetchAndPick (STATE 2→3): fetch one provider's live models off the UI
	// goroutine, then open the model picker. Empty list → switch to the saved
	// default; fetch error → fail closed with guidance (no swap).
	fetchAndPick := func(providerName, currentModel string) tea.Cmd {
		m.appendFeed(cDim.Render("⟳ fetching models from " + providerName + "…"))
		return func() tea.Msg {
			models, err := ctl.ListProviderModels(providerName)
			if err != nil {
				return sysChatMsg{text: "could not fetch models from " + providerName + ": " + safeErr(err) +
					"\n(try `/model use " + providerName + "` to switch without picking, or `/configure " + providerName + "` to set the key)"}
			}
			if len(models) == 0 {
				s, _, uerr := ctl.UseModel(providerName, "")
				if uerr != nil {
					return sysChatMsg{text: providerName + " reported no models, and switching failed: " + uerr.Error()}
				}
				return sysChatMsg{text: providerName + " reported no models — switched to its saved default:\n" + s}
			}
			return pickerStartMsg{picker: &listPicker{
				title:       "Pick a model for " + providerName + " (" + fmt.Sprintf("%d available", len(models)) + ")",
				hint:        "↑/↓ select · enter swap live · esc cancel",
				items:       models,
				currentMark: currentModel,
				selected:    pickerIndexOf(models, currentModel),
				onSelect: func(choice string) tea.Cmd {
					return func() tea.Msg {
						s, _, perr := ctl.UseModel(providerName, choice)
						if perr != nil {
							return sysChatMsg{text: "✗ switch to " + choice + " failed: " + perr.Error()}
						}
						return sysChatMsg{text: s}
					}
				},
				onCancel: func() tea.Cmd {
					return func() tea.Msg { return sysChatMsg{text: "kept current model for " + providerName} }
				},
			}}
		}
	}

	// STATE 0: a single provider needs no provider pick.
	if len(infos) == 1 {
		return fetchAndPick(infos[0].Name, infos[0].Model)
	}

	// STATE 1: provider picker.
	labels := make([]string, 0, len(infos))
	byLabel := make(map[string]ModelInfo, len(infos))
	activeLabel := ""
	for _, info := range infos {
		label := info.Name
		if info.Model != "" {
			label += "  ·  " + info.Model
		}
		// Guarantee label uniqueness so the label→info round-trip can't mis-route
		// (mirrors the /configure picker). Provider names are unique config keys,
		// so this only fires on pathological input — but it's a one-line backstop.
		base := label
		for n := 2; ; n++ {
			if _, dup := byLabel[label]; !dup {
				break
			}
			label = fmt.Sprintf("%s (%d)", base, n)
		}
		labels = append(labels, label)
		byLabel[label] = info
		if info.Active {
			activeLabel = label
		}
	}
	return startPickerCmd(&listPicker{
		title:       "Switch model — pick a provider",
		hint:        "↑/↓ select · enter fetch models · esc cancel",
		items:       labels,
		currentMark: activeLabel,
		selected:    pickerIndexOf(labels, activeLabel),
		onSelect: func(choice string) tea.Cmd {
			info := byLabel[choice]
			return fetchAndPick(info.Name, info.Model)
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return sysChatMsg{text: "model switch cancelled"} }
		},
	})
}

// modelToolFormats are the supported OpenAI-compatible tool-call formats,
// mirrored from cmd/tenant setup.go's toolFormatOpts. Surfaced by the /model add
// picker (TEN-138) so a self-hosted backend never silently defaults to gemma.
var modelToolFormats = []string{"qwen", "gemma", "llama", "mistral", "openai"}

// defaultToolFormat is the picker's initial highlight — the historical fallback,
// shown first so the operator sees what they'd have gotten and can change it.
const defaultToolFormat = "gemma"

// pickerIndexOf returns the index of target in items, or 0 if absent/empty.
func pickerIndexOf(items []string, target string) int {
	if target == "" {
		return 0
	}
	for i, it := range items {
		if it == target {
			return i
		}
	}
	return 0
}

// --- /configure interactive flow — TEN-65 follow-up ---
//
// Mirrors the OpenClaw/Claude Code `/configure` UX: type the command,
// the TUI walks you through each field one at a time. Enum fields
// (those with Options) get the existing listPicker modal; free-text
// fields use the chat input area with `m.configureSession` set
// (similar to `m.pendingClarify`).

// secretEntryState is the no-arg `/configure` key-entry: the operator picked a
// service and the next plain input is its key (masked while active).
type secretEntryState struct {
	credID string
	name   string
}

// startConfigurePicker opens the arrow-key picker over every keyed service. On
// select it arms secretEntry + masks the input so the next line is captured as
// that service's key. Reuses the existing listPicker modal.
func (m *model) startConfigurePicker() tea.Cmd {
	items := m.cfg.Secrets.List()
	if len(items) == 0 {
		m.sysChat("configure: no configurable services available")
		return nil
	}
	labels := make([]string, len(items))
	byLabel := make(map[string]secretEntryState, len(items))
	for i, it := range items {
		status := "○ not set"
		if it.Set {
			status = "● set"
		}
		label := fmt.Sprintf("%-26s %-13s %s", it.Name, it.Category, status)
		// Guarantee a unique map key even if two services share Name+Category+
		// status (latent today; future-proofs the label→credID round-trip).
		if _, dup := byLabel[label]; dup {
			label = label + "  " + it.CredID
		}
		labels[i] = label
		byLabel[label] = secretEntryState{credID: it.CredID, name: it.Name}
	}
	m.picker = &listPicker{
		title: "Configure — pick a service to set its API key",
		hint:  "↑/↓ select · enter set key · esc cancel",
		items: labels,
		onSelect: func(choice string) tea.Cmd {
			se := byLabel[choice]
			m.secretEntry = &se
			m.input.EchoMode = textinput.EchoPassword
			m.sysChat("paste the key/token for " + se.name + ", then Enter (input hidden · /cancel to abort)")
			return nil
		},
		onCancel: func() tea.Cmd {
			m.sysChat("configure cancelled")
			return nil
		},
	}
	return nil
}

// clearSecretEntry disarms a pending /configure key entry and restores the
// input to normal (unmasked, empty). Safe to call when nothing is armed.
func (m *model) clearSecretEntry() {
	m.secretEntry = nil
	m.input.EchoMode = textinput.EchoNormal
	m.input.Reset()
}

// saveSecretEntry persists the just-entered key via the Secrets control (which
// applies it live — TEN-147) and clears the masked-entry state. The value is
// never echoed to chat.
func (m *model) saveSecretEntry(value string) tea.Cmd {
	se := m.secretEntry
	m.clearSecretEntry()
	if se == nil {
		return nil
	}
	value = strings.TrimSpace(value)
	if value == "" {
		m.sysChat("configure: no key entered for " + se.name + " — cancelled")
		return nil
	}
	if m.cfg.Secrets == nil {
		m.sysChat("configure: not available in this session")
		return nil
	}
	if err := m.cfg.Secrets.Set(se.credID, value); err != nil {
		m.sysChat("configure: couldn't save " + se.name + " — " + err.Error())
		return nil
	}
	m.sysChat("✓ " + se.name + " key saved to credentials.json (0600) — applied live, no restart.")
	return nil
}

// configureSessionState tracks an in-flight `/configure <skill>` flow.
// Each user input is consumed as the next field's value until all
// fields are answered, then SkillConfigure runs with the assembled
// key=value args.
type configureSessionState struct {
	skillID  string
	fields   []SkillField      // ordered list from SkillConfigControl.SkillFields
	idx      int               // index of the field currently being answered
	values   map[string]string // collected so far
	noEnable bool              // if /configure --no-enable was used
}

// renderConfigurePrompt renders the next-field prompt that the TUI
// pushes to chat when advancing the session. Idempotent — calling it
// repeatedly shows the same prompt until the user answers.
//
// Format:
//
//	▸ Step N of M · gsuite.composio_api_key
//	  Paste your Composio API key (app.composio.dev → Settings → API Keys)
//	  REQUIRED · Secret (stored at credentials.json, 0600)
//	  Type your answer below, or `/cancel` to abort
//
// Required vs Optional is its own line so the operator can see at a
// glance whether they can press Enter to skip.
func (cs *configureSessionState) renderPrompt() string {
	if cs.idx >= len(cs.fields) {
		return ""
	}
	f := cs.fields[cs.idx]
	// Count visible fields (skipping ShowIf-hidden ones) so the step
	// counter reflects what the operator actually sees, not raw
	// catalog length.
	visibleTotal := 0
	visibleStep := 0
	for i, ff := range cs.fields {
		if ff.ShowIf != nil && !ff.ShowIf(cs.values) {
			continue
		}
		visibleTotal++
		if i == cs.idx {
			visibleStep = visibleTotal
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "▸ Step %d of %d · %s.%s\n  %s", visibleStep, visibleTotal, cs.skillID, f.Key, f.Prompt)
	// Status line: REQUIRED/OPTIONAL + secret + default + picker hint.
	status := []string{}
	if f.Required {
		status = append(status, "REQUIRED")
	} else {
		status = append(status, "OPTIONAL — press Enter to skip")
	}
	if f.Default != "" {
		status = append(status, fmt.Sprintf("Default: %s (Enter to use)", f.Default))
	}
	if f.Secret {
		status = append(status, "Secret (stored at credentials.json, 0600)")
	}
	if len(f.Options) > 0 {
		status = append(status, fmt.Sprintf("Pick one: %s", strings.Join(f.Options, " | ")))
	}
	b.WriteString("\n  " + strings.Join(status, " · "))
	b.WriteString("\n  Type your answer below, or `/cancel` to abort")
	return b.String()
}

// startConfigure begins an interactive `/configure <id>` session. If
// the skill is unknown OR has no fields, returns an error and does
// not enter the session state.
func (m *model) startConfigure(id string, noEnable bool) tea.Cmd {
	if m.cfg.SkillConfig == nil {
		m.sysChat("/configure is not available in this session")
		return nil
	}
	fields, err := m.cfg.SkillConfig.SkillFields(id)
	if err != nil {
		m.sysChat("✗ " + err.Error())
		return nil
	}
	if len(fields) == 0 {
		// Zero fields → run probe+enable immediately.
		out, err := m.cfg.SkillConfig.SkillConfigure([]string{id}, noEnable)
		if err != nil {
			m.sysChat("✗ " + err.Error())
		} else {
			m.sysChat(out)
		}
		return nil
	}
	m.configureSession = &configureSessionState{
		skillID:  id,
		fields:   fields,
		values:   map[string]string{},
		noEnable: noEnable,
	}
	// Friendly intro: name the skill + show its setup hint (if any) so
	// the operator understands what the walkthrough is about to ask.
	// Non-developers especially benefit from upfront context.
	m.sysChat(fmt.Sprintf("Configuring %s — I'll walk you through %d step(s). Type `/cancel` at any time to stop.", id, m.visibleFieldCount(fields)))
	if hint := m.skillSetupHint(id); hint != "" {
		m.sysChat(hint)
	}
	return m.advanceConfigureSession()
}

// visibleFieldCount returns the number of fields the operator will
// actually be prompted for, accounting for ShowIf. Used in the intro
// message so "4 fields" doesn't mislead when 2 are ShowIf-conditional.
func (m *model) visibleFieldCount(fields []SkillField) int {
	n := 0
	for _, f := range fields {
		// Show count is best-effort: ShowIf depends on prior answers
		// we don't have yet. Count fields without a ShowIf gate + 1
		// (the conditional fields might or might not appear).
		if f.ShowIf == nil {
			n++
		}
	}
	return n
}

// skillSetupHint pulls the SetupHint from the skill's catalog entry,
// if any, via the SkillConfigControl. Best-effort — returns empty
// when not available.
func (m *model) skillSetupHint(id string) string {
	if m.cfg.SkillConfig == nil {
		return ""
	}
	for _, s := range m.cfg.SkillConfig.SkillList() {
		if s.ID == id {
			return s.SetupHint
		}
	}
	return ""
}

// advanceConfigureSession picks the next action: if the current field
// has Options, launch a picker; if all fields answered, finalize;
// otherwise show the chat prompt and wait for the next user input.
//
// Skips fields whose ShowIf returns false given the values collected so
// far (TEN-65 follow-up: gsuite sa_json + subject only show when
// auth=sa).
func (m *model) advanceConfigureSession() tea.Cmd {
	cs := m.configureSession
	if cs == nil {
		return nil
	}
	// Skip past any fields hidden by ShowIf.
	for cs.idx < len(cs.fields) {
		f := cs.fields[cs.idx]
		if f.ShowIf == nil || f.ShowIf(cs.values) {
			break
		}
		cs.idx++
	}
	if cs.idx >= len(cs.fields) {
		return m.finalizeConfigureSession()
	}
	f := cs.fields[cs.idx]
	if len(f.Options) > 0 {
		// Use the existing listPicker modal for enum selection.
		// Build display items + reverse map so we store the VALUE
		// (f.Options[i]) but show the human LABEL (f.OptionLabels[i]).
		// If OptionLabels is missing or length-mismatched, fall back to
		// showing the raw values.
		items := append([]string(nil), f.Options...)
		labelToValue := map[string]string{}
		if len(f.OptionLabels) == len(f.Options) {
			items = append([]string(nil), f.OptionLabels...)
			for i, lab := range f.OptionLabels {
				labelToValue[lab] = f.Options[i]
			}
		}
		selectedIdx := 0
		for i, o := range f.Options {
			if o == f.Default {
				selectedIdx = i
				break
			}
		}
		p := &listPicker{
			title:    fmt.Sprintf("%s.%s — %s", cs.skillID, f.Key, f.Prompt),
			items:    items,
			selected: selectedIdx,
			onSelect: func(choice string) tea.Cmd {
				// Map label back to value when labels are in use; otherwise
				// `choice` IS the value (label-less picker).
				value := choice
				if v, ok := labelToValue[choice]; ok {
					value = v
				}
				m.handleConfigureAnswer(value)
				return m.advanceConfigureSession()
			},
			onCancel: func() tea.Cmd {
				m.sysChat("configuration cancelled")
				m.configureSession = nil
				return nil
			},
		}
		return startPickerCmd(p)
	}
	// Free-text field: print the prompt; the next user input will be
	// caught by submit() and routed to handleConfigureAnswer.
	m.sysChat(cs.renderPrompt())
	return nil
}

// handleConfigureAnswer accepts the operator's value for the current
// field, stores it (after applying default-on-empty), and bumps the
// index. Does NOT advance the session — the caller does that.
//
// Required + empty (no Default): re-prompts in place with a warning
// rather than silently advancing. Catches the "I forgot to paste"
// case at the field, not at the final validation pass — much better
// UX than walking through 4 fields and getting "missing required
// composio_api_key" at submit time.
func (m *model) handleConfigureAnswer(value string) {
	cs := m.configureSession
	if cs == nil || cs.idx >= len(cs.fields) {
		return
	}
	f := cs.fields[cs.idx]
	value = strings.TrimSpace(value)
	if value == "" && f.Default != "" {
		value = f.Default
	}
	// TEN-72 follow-up: required field + empty answer + no default ⇒
	// warn AND don't advance. Operator hits Enter again or types the
	// value. They can still /cancel to bail entirely.
	if value == "" && f.Required {
		m.sysChat(fmt.Sprintf("⚠ %s is REQUIRED — paste a value (or `/cancel` to abort the whole flow).",
			f.Key))
		return
	}
	// Empty + not Required + no Default ⇒ skip (don't store).
	if value != "" {
		cs.values[f.Key] = value
	}
	// TEN-65 follow-up: surface field-specific guidance + honor abort.
	// gsuite's auth field returns abort=true when the operator picks
	// gcloud but the CLI isn't installed — clean stop instead of
	// pretending to configure a broken setup.
	if value != "" && f.NoteAfter != nil {
		note, abort := f.NoteAfter(value)
		if note != "" {
			m.sysChat(note)
		}
		if abort {
			m.sysChat("configuration cancelled — prerequisite missing")
			m.configureSession = nil
			return
		}
	}
	cs.idx++
}

// finalizeConfigureSession assembles all collected values into
// key=value args and calls SkillConfigure. Reports the result, clears
// the session.
func (m *model) finalizeConfigureSession() tea.Cmd {
	cs := m.configureSession
	if cs == nil {
		return nil
	}
	args := []string{cs.skillID}
	for k, v := range cs.values {
		// SkillConfigure's key=value parser splits on the first '='.
		args = append(args, k+"="+v)
	}
	noEnable := cs.noEnable
	m.configureSession = nil

	m.sysChat("⟳ probing…")
	// SkillConfigure may hit the network (probe); run it on a tea.Cmd
	// goroutine so the TUI stays responsive.
	cfg := m.cfg.SkillConfig
	return func() tea.Msg {
		out, err := cfg.SkillConfigure(args, noEnable)
		if err != nil {
			return sysChatMsg{text: "✗ " + err.Error()}
		}
		return sysChatMsg{text: out}
	}
}

// (sysChatMsg is defined earlier; configure flow reuses it.)

// --- /skill (singular) helpers — TEN-64 integration-config surface ---

// firstField splits "<word> <rest>" on the first whitespace run. Used
// by the /skill dispatcher to peel a subcommand off the arg string.
func firstField(s string) (word, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i:])
	}
	return s, ""
}

// renderSkillList formats the /skill list output. Legacy entries
// (still living in skillSpecs, not yet migrated to skillKinds) get a
// trailing "[legacy]" marker so operators know to use `tenant setup`.
// Audit P1: this surface keeps /skill list actionable even when the
// new catalog is empty.
func renderSkillList(infos []SkillConfigInfo) string {
	if len(infos) == 0 {
		return "no skills available (build is missing the skill catalog)"
	}
	var b strings.Builder
	b.WriteString("Skills (✓ configured, ● enabled):\n")
	for _, s := range infos {
		mark := "○"
		if s.Configured {
			mark = "✓"
		}
		state := ""
		if s.Enabled {
			state = " ●"
		}
		legacy := ""
		if s.Legacy {
			legacy = "  [legacy — use `tenant setup`]"
		}
		fmt.Fprintf(&b, "  %s %s — %s%s%s\n", mark, s.ID, s.Label, state, legacy)
	}
	return strings.TrimRight(b.String(), "\n")
}
