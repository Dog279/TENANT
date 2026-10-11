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
	"tenant/internal/attach"
	"tenant/internal/control"
	"tenant/internal/fsperm"
	"tenant/internal/md"
	modelpkg "tenant/internal/model"
)

// Config wires the TUI to an already-constructed agent whose Observer
// pushes into Events. Backend/Model/AgentID are display-only.
type Config struct {
	Agent *agent.Agent
	// Attachments saves pasted images (TEN-493); nil means the agent's store.
	Attachments *attach.Store
	Events      <-chan agent.Event
	AgentID     string
	Backend     string
	Model       string
	// FeedOff hides the activity feed at start (the saved `/feed off`); SaveFeed,
	// if set, records a `/feed on|off` (or Alt+F) choice so it holds across
	// launches (TEN-514).
	FeedOff  bool
	SaveFeed func(on bool) error
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
	// ResolveApproval, if set, resolves a pending request BY ID through the
	// broker registry (TEN-203) — first-resolver-wins, so if another observer
	// (the dashboard) resolved it first this returns false and the TUI treats it
	// as a clean no-op. When nil, the TUI falls back to writing Reply directly
	// (legacy single-observer behavior; keeps tests + non-broker callers working).
	ResolveApproval func(id string, d ApprovalDecision) bool
	// Perms, if set, powers /permissions (per-category safety modes).
	Perms PermissionControl
	// Dash, if set, powers /dashboard: start/stop the web control panel live.
	Dash DashboardControl
	// Tailscale, if set, powers /tailscale: publish the loopback dashboard onto
	// the operator's tailnet via `tailscale serve` (TEN-233).
	Tailscale TailscaleControl
	// Quality, if set, powers /eval model: the Quality model, the model that
	// grades eval answers, saved and applied to `tenant eval` + the nightly
	// check (TEN-91, renamed in TEN-516).
	Quality QualityModelControl
	// Judge, if set, powers /judge: the Judge, a decision model in the loop
	// of a turn, live (TEN-510, TEN-516).
	Judge JudgeControl
	// Relay, if set, powers /relay: start/stop the offsite Discord relay live.
	Relay RelayControl
	// IMessage, if set, powers /imessage: manage the deny-by-default allowlist
	// of phone numbers / emails permitted to drive the agent over iMessage.
	IMessage IMessageControl
	// Cron, if set, powers /cron: manage recurring agent-prompt jobs.
	Cron CronControl
	// Logs, if set, powers /logs: the log book's events, an event's detail
	// and its correlated timeline (TEN-381).
	Logs control.LogsControl
	// MCP, if set, powers /mcp: connect/list/remove remote MCP connector
	// servers at runtime (OAuth browser sign-in), tools gated deny-by-default.
	MCP MCPControl
	// Peer, if set, powers /peer (list/show/remove) + /configure peer (the
	// per-peer share-policy editor). Federation pairing (TEN-183/188).
	Peer PeerControl

	// GitHub is the watched-repository list behind /github (nil = unavailable).
	GitHub GitHubControl
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
	// Route, if set, powers /route: probe the shadow classifier and explain
	// recent routing decisions. Status probes run off the UI goroutine.
	Route RouteControl
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
	// (input, output, and the turn's trace) to the long-term cost-audit
	// ledger. The cmd layer binds this to the usage store with agentID + model
	// closed over, and swallows persistence errors so the UI never blocks
	// (TEN-167).
	RecordUsage func(in, out int, traceID string)

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
	Trusted     bool   // only a trusted skill is offered to the agent (TEN-298)
	Origin      string // operator, agent, induction, seed, import; "" before TEN-298
}

// SkillDetail is one skill in full, for /skills show: its text, trust,
// provenance and required tools, and why it is not offered, if it is not.
type SkillDetail struct {
	Name, Description, Recipe, Status string
	Enabled, Trusted                  bool
	Origin, Version                   string
	RequiresTools                     []string
	NotOffered                        string // "" when the agent is offered it

	// An imported skill's package: where it came from and what it held.
	Source, Digest, License, Compatibility string
	Files                                  []string // bundled files not installed
	ImportedAt                             time.Time
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

	// Agent Skills packages and trust (TEN-298). ImportSkill imports a
	// SKILL.md package (its directory or the file), untrusted unless trust is
	// set, and returns a report; ExportSkill writes dir/<name>/SKILL.md and
	// returns the directory. SetSkillTrust trusts or distrusts a skill;
	// SkillDetail returns one skill in full (nil, nil when unknown).
	ImportSkill(path string, trust bool) (string, error)
	ExportSkill(name, dir string) (string, error)
	SetSkillTrust(name string, trusted bool) (bool, error)
	SkillDetail(name string) (*SkillDetail, error)
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
	ChangeSource     string // "operator" | "agent" | "induction" | "auto-accept" | "seed" | "import" | "trust" | "distrust" | "revert"
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
type ToolInfo = control.ToolInfo

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
	Legacy     bool   // true ⇒ wizard-only skill (wiki/sql/imessage/os), configure via tenant setup
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

// toolRankingReporter is an OPTIONAL capability of the Tools control: it reports
// the last turn's tool-ranking decision (TEN-225) — whether cosine ranking
// trimmed the catalog or the full enabled set was surfaced, and why — so /tools
// can show it on demand (the per-turn activity-feed line scrolls away).
type toolRankingReporter interface {
	RankingStatus() (ranked bool, surfaced, catalog int, reason string, ok bool)
}

// ApprovalDecision is the user's answer to a dangerous-action prompt,
// modeled on the three-tier approve flow (once / session / always) plus
// an explicit deny.
type ApprovalDecision = control.ApprovalDecision

const (
	DenyOnce       = control.DenyOnce
	ApproveOnce    = control.ApproveOnce
	ApproveSession = control.ApproveSession
	ApproveAlways  = control.ApproveAlways
)

// ApprovalRequest is a dangerous action paused awaiting the user; the TUI
// answers on its Reply channel.
type ApprovalRequest = control.ApprovalRequest

// ToolRuleInfo is one per-tool permission rule (pattern → mode).
type ToolRuleInfo = control.ToolRuleInfo

// ToolRuleControl is the optional per-tool rule surface a PermissionControl
// may also offer (/permissions add|rm). The rule set is shared by every
// surface, so it is listed identically under each permissions view.
type ToolRuleControl interface {
	Rules() []ToolRuleInfo
	SetRule(pattern, mode string) error
	RemoveRule(pattern string) (bool, error)
}

// PermissionInfo is one safety category's current mode, for /permissions.
type PermissionInfo = control.PermissionInfo

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

// TailscaleControl powers /tailscale: publish the loopback dashboard onto the
// operator's tailnet via `tailscale serve` (TEN-233) — a reverse proxy, so the
// dashboard stays bound to 127.0.0.1 (no 0.0.0.0, no cert/token to manage) and
// only the operator's tailnet devices can reach it over HTTPS. Serve/Unserve
// fail closed with operator-actionable errors (CLI missing / not logged in).
type TailscaleControl interface {
	// Status reports install/connection/serve state for the status view. The
	// error return is reserved; a reachable-but-unhappy CLI is reported in-struct.
	Status() (TailscaleStatus, error)
	// Serve publishes the dashboard's loopback port to the tailnet; returns the
	// reachable https URL.
	Serve() (url string, err error)
	// Unserve removes the tailnet serve config.
	Unserve() error
}

// JudgeControl powers /judge: choose which model grades eval answers (TEN-91).
// Default is the planner / main-agent model (self-judging, zero setup); the
// operator can pin a separate model (any wired provider kind), persisted so it
// applies to both `tenant eval` and the nightly eval/improvement gate. The API
// key is read from an env var at eval time — never entered or stored here.
// JudgeControl sets up and switches the Judge, the decision model in the
// loop of a turn (TEN-510, TEN-516).
type JudgeControl interface {
	// Status is the one-line state.
	Status() string
	// On reports whether the loop is on, and how /model should name the judge
	// ("clef-flash (clef)").
	On() (bool, string)
	// Set turns the loop on (asking the server first) or off, live, and saves it.
	Set(on bool) (string, error)
	// Use points the Judge at a server with the saved key: the kind's dialect,
	// the endpoint ("" keeps the saved one) and the model ("" = the kind's
	// default). It asks the server one question, saves and switches on.
	Use(kind, endpoint, model string) (string, error)
	// Setup is Use with a key the operator typed ("" = none): the server is
	// asked with that key before anything is saved, then the key, the server
	// and the switch are saved together.
	Setup(kind, endpoint, key, model string) (string, error)
	// Rounds sets how many times a turn may be sent back (1..5), live and saved.
	Rounds(n int) (string, error)
	// Saved is what the setup prompt offers as defaults.
	Saved() (endpoint string, hasKey bool)
}

// QualityModelControl chooses the Quality model: the model that grades eval
// answers (TEN-91).
type QualityModelControl interface {
	// Current is a render-ready description of the active Quality model.
	Current() string
	// Set saves a separate Quality model: provider kind + model id (+ optional
	// endpoint). Returns a confirmation message (may include a key-env warning).
	Set(kind, model, endpoint string) (msg string, err error)
	// Clear reverts to the default: the planner grades its own answers.
	Clear() error
}

// TailscaleStatus is the render-ready snapshot of the local tailscale state.
type TailscaleStatus struct {
	Installed bool   // the tailscale CLI was found
	LoggedIn  bool   // backend is connected (BackendState == "Running")
	State     string // raw BackendState (e.g. "Running", "Stopped", "NeedsLogin")
	DNSName   string // this node's MagicDNS name (no trailing dot)
	Serving   bool   // a tailscale serve config is active
	URL       string // https tailnet URL when connected
	Detail    string // human note when something's off (e.g. parse/CLI error)
}

// RelayControl powers /relay: start/stop the offsite Discord relay at runtime,
// set the single operator's Discord user id, and report state. The
// implementation persists the on/off + operator choices.
type RelayControl interface {
	Enable() error
	Disable() error
	Status() (running bool, operatorSet bool, execOn bool)
	SetOperator(id string) error
	// SetExec toggles offsite exec mode. Off, the Discord broker refuses exec,
	// write, destructive, sandbox and send (Discord posts excepted) whatever the
	// relay permissions say; on, those follow the relay permissions.
	SetExec(on bool) error
	// Perms exposes the Discord agent's per-category permission control (TEN-231)
	// so /relay permissions uses the SAME ask|allow|deny model as /permissions —
	// "ask" prompts a button in Discord. nil when Discord is unconfigured.
	Perms() PermissionControl
	// SetApprovalTimeout / ApprovalTimeout tune how long an approval card waits
	// for the operator's tap before auto-denying (/relay timeout <duration>).
	SetApprovalTimeout(d time.Duration) error
	ApprovalTimeout() time.Duration
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
	// ResponderOn reports whether the autonomous responder loop is running;
	// SetResponder turns it on/off live without a relaunch (TEN-230 Phase 1c).
	ResponderOn() bool
	SetResponder(on bool) (status string, err error)
	// Passive reports whether the responder only reads (TEN-485); SetPassive
	// switches mode live and starts the responder if it is off.
	Passive() bool
	SetPassive(on bool) (status string, err error)
	// Perms exposes the responder's per-category permission control (TEN-230) so
	// /imessage permissions uses the SAME ask|allow|deny model as /permissions.
	// nil when the responder is unavailable.
	Perms() PermissionControl
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
	// SetTrust changes a connector's live trust mode: "ask" (every gated call
	// prompts), "allow" (its tools run without per-call approval) or "deny"
	// (its tools are refused + hidden). Persisted; no reconnect.
	SetTrust(target, mode string) (MCPServerInfo, error)
}

// MCPServerInfo is one remote MCP connector's view for /mcp.
type MCPServerInfo struct {
	Label     string // e.g. "mcp:mcp.atlassian.com"
	URL       string
	Enabled   bool
	ToolCount int
	Trust     string // ask | allow | deny — the operator's live decision on its tools
	// NeedsReauth: the connector's sign-in died and its tools fail until the
	// operator reconnects it — shown ahead of the (stale) tool count.
	NeedsReauth bool
	AuthErr     string
	AuthSince   time.Time
}

// PeerControl drives the TUI's `/peer` and `/configure peer` surfaces over the
// federation peer store (TEN-183/188). Share editing mirrors the global
// permissions model: every sharable capability is Allow or Deny, deny-by-default.
// GitHubRepoInfo is one watched repository as the operator sees it.
type GitHubRepoInfo struct {
	Slug    string // owner/name
	Write   bool   // the agent may modify this repo (still gated per action)
	Primary bool   // the default target when a tool omits its repo argument
}

// GitHubControl is the watch-list surface behind /github. Write access is a
// PER-REPO decision: everything watched is reported on, only :write repos can
// be modified, and the plugin refuses writes to the others in code.
type GitHubControl interface {
	Repos() ([]GitHubRepoInfo, error)
	SetWrite(repo string, write bool) (GitHubRepoInfo, error)
	Add(repo string, write bool) (GitHubRepoInfo, error)
	Remove(repo string) (bool, error)
}

type PeerControl interface {
	List() []PeerInfo
	Show(name string) (PeerInfo, bool)
	Remove(name string) (bool, error)
	// SetShare flips one capability (wiki|memory|skills|exec|llm) for a peer and
	// returns the updated view.
	SetShare(name, capability string, allow bool) (PeerInfo, error)
	// Rename relabels a peer (long hostname → readable). Cosmetic; preserves the
	// token/share/url. Errors if old is unknown or new is taken.
	Rename(old, newName string) error
	// Invite begins push-pairing (TEN-239): it returns a 6-digit PIN to display
	// to BOTH operators and a run() that performs the blocking, operator-approved
	// request (call it off the UI goroutine). nil run ⇒ err explains why.
	Invite(label, url string) (pin string, run func(context.Context) (string, error), err error)
	// Serve (re)starts the in-process peer listener bound to addr (empty ⇒ a
	// reachable default) so peers can reach this instance AND inbound invites
	// prompt in this TUI. Returns the bound address.
	Serve(addr string) (boundAddr string, err error)
	// Reconnect re-dials paired peers so their shared tools come live in the
	// running agent without a relaunch. Returns how many peers it's dialing.
	Reconnect() (int, error)
	// Stats returns per-peer federated-search counters for this session (drift
	// tracking): how often each peer was queried and what came back. Empty until
	// the agent runs a federated search against a connected peer.
	Stats() []PeerFedStat
	// Health returns live per-peer liveness (TEN-250): a background heartbeat
	// probes dialable peers and records inbound auths, so /peer shows green/red.
	// Missing entries render as "unknown".
	Health() []PeerHealth
}

// PeerHealth is one peer's live liveness for the /peer status column.
type PeerHealth struct {
	Name         string
	State        string // "alive" | "dead" | "unknown"
	Detail       string // "seen 3s ago (12ms)" / "unreachable: …" / "inbound 5s ago"
	LastSeenUnix int64  // 0 = never
	// TheirShare is what the peer grants US (from peer_hello), for the /peer
	// status "them → us" column. TheirShareKnown is false until we've learned it
	// (we only learn it by dialing them, so an inbound-only peer stays unknown).
	TheirShare      []string
	TheirShareKnown bool
}

// PeerFedStat is one peer's federated-search tally for the /peer stats view.
type PeerFedStat struct {
	Peer    string
	Queries int // fan-out attempts
	Hits    int // returned usable content
	Denied  int // peer's share gate refused
	Errors  int // peer offline / transport failure
	Empty   int // allowed but nothing matched
	Bytes   int64
}

// PeerShareCaps is the canonical ordered list of sharable capabilities, shown
// by /configure peer. Kept here so the TUI renders them consistently.
var PeerShareCaps = []string{"wiki", "memory", "skills", "exec", "llm"}

// PeerInfo is one paired peer's view for /peer + /configure peer.
type PeerInfo struct {
	Name       string
	InstanceID string
	URL        string
	Dial       bool            // true = we dial them; false = they dial us
	TokenState string          // "set" | "revoked" | "rotating"
	Share      map[string]bool // capability → allowed (deny-by-default)
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
	// RepeatGuard reports how many identical rounds (same calls, same results)
	// end a turn early, <= 0 = off; SetRepeatGuard retunes it live (and
	// persists) — /ceiling repeat (TEN-506).
	RepeatGuard() int
	SetRepeatGuard(n int) (string, error)
	// Vision reports whether the active model can see images; SetVision sets
	// the active provider's vision to on, off or auto, live (TEN-493).
	Vision() string
	SetVision(v string) (string, error)
	// ReasoningSupported reports whether the ACTIVE provider accepts a reasoning
	// -effort hint (Sakana Fugu, claudecode); ReasoningEffort reports the current
	// value (""=off); ReasoningLevels lists the values the active kind accepts
	// (drives the /reasoning picker: Fugu high/xhigh, claudecode low→max);
	// SetReasoningEffort retunes it live + persists — /reasoning.
	ReasoningSupported() bool
	ReasoningEffort() string
	ReasoningLevels() []string
	SetReasoningEffort(effort string) (status string, err error)
	// Fallback returns the configured auto-fallback provider chain (TEN-246),
	// in order. Empty = no fallback.
	Fallback() []string
	// HealthGating describes the chain's latency health gating (TEN-282):
	// "off", or what it skips and for how long.
	HealthGating() string
	// SetFallback persists + live-installs the ordered fallback provider chain
	// (each name must be a configured provider). Empty clears it.
	SetFallback(names []string) (status string, err error)
}

// RouteControl powers /route. Status and Stats may perform local I/O; the TUI
// always calls them from a tea.Cmd, never the UI goroutine.
type RouteControl interface {
	Status(ctx context.Context) string
	Why(n int) string
	Stats() string
	SetMode(mode string) (string, error)
	Force(profileID string) (string, error)
	// ConfigureInfo inventories configured providers (cloud = big-leg
	// candidates, local = small-leg candidates) plus the current policy for
	// the /route configure wizard. Reads config.json — call from a tea.Cmd,
	// never the UI goroutine.
	ConfigureInfo() (RouteConfigureInfo, error)
	// SetBig persists route.big_role = provider (and, when model is
	// non-empty, the provider's model) and live-binds planner-big. Disk +
	// backend I/O — tea.Cmd only.
	SetBig(provider, model string) (string, error)
	// SetPolicy persists one policy knob: never_cloud |
	// min_complexity_for_big | sticky_session. Mode changes go through
	// SetMode (rollout ladder). Disk I/O — tea.Cmd only.
	SetPolicy(field, value string) (string, error)
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
	// Limits lists the team-run limits in effect, "off" for unset ones
	// (TEN-508) — /agents limits.
	Limits() ([]LimitRow, error)
}

// LimitRow is one team-run limit for /agents limits: what it limits, its key
// under "budgets" in config.json, its value ("off" when unset), and how to
// turn it on.
type LimitRow struct{ Name, Key, Value, Note string }

// AgentInfo is the lightweight summary row for /agents listings.
type AgentInfo struct {
	Name        string
	Provider    string // provider id (e.g. "zai", "gpu")
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
	// at is when the message was added, for the turn block's header line;
	// model names the model an assistant message came from (TEN-514).
	at    time.Time
	model string
	// tools are an assistant message's tool calls, in order; each knows where
	// in content it happened, so the block interleaves text and tool rows.
	tools []toolRow
	// final marks an assistant message whose text is complete: rendered as
	// Markdown in full. While streaming, only the closed paragraphs are.
	final bool
	// cache is the block as last rendered, valid for cacheKey (width, length,
	// state); a repaint while nothing changed is a string copy.
	cache, cacheKey string
}

// toolRow is one tool call shown inside an assistant's turn block: a single
// collapsed line, or the arguments and result when the block is open
// (Ctrl+O) or the call failed.
type toolRow struct {
	id, name, args, result string
	at                     time.Time
	dur                    time.Duration
	done, isErr            bool
	pos                    int // len(content) when the call began
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
	busy            bool             // a turn is in flight
	budgetPct       int              // last context-budget utilization (% of writable budget)
	attachments     []modelpkg.Image // images pasted into the input, by placeholder number (TEN-493)
	budgetUsed      int              // absolute tokens in the assembled context
	budgetCap       int              // writable-budget cap (tokens)
	sessionTok      int              // cumulative tokens (in+out) for the MAIN agent this session
	sessionTokIn    int              // cumulative INPUT tokens for the MAIN agent this session
	sessionTokOut   int              // cumulative OUTPUT tokens for the MAIN agent this session
	teamTok         int              // cumulative tokens (in+out) for spawned sub-agents
	reqStart        time.Time        // wall-clock start of the in-flight turn (display-only live timer)
	thinkSince      time.Time        // start of the model call in flight; zero between calls (TEN-342)
	toolsSince      time.Time        // start of the tool calls in flight; zero while thinking
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
	// toolsOpen shows every turn's tool rows with their arguments and
	// results (Ctrl+O); off, each call is one line. feedOff hides the activity
	// pane (/feed off, Alt+F). inTurn indents the feed's lines under the turn
	// header while the main agent's turn runs (TEN-514).
	toolsOpen bool
	feedOff   bool
	inTurn    bool
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
	// logs, if non-nil, is the /logs pane: it takes the chat pane's place and
	// every key until esc or q closes it (cmd_logs.go).
	logs *logsPane
	// researchTimeline is the live state for /research progress, fed by
	// ResearchTimeline updates from the orchestrator + TeamEvents from the
	// spawned sub-agents. Non-nil only while a research run is in flight;
	// cleared on researchDoneMsg so the timeline pane disappears when done.
	researchTimeline *researchTimelineState
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
	ta.CharLimit = inputCharLimit
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
		feedOff:    cfg.FeedOff,
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
	// Route-configure wizard chaining: post the applied step's status and
	// open the next picker in the same Update pass, so nothing can interleave
	// between them.
	if w, ok := msg.(routeWizardMsg); ok {
		if w.text != "" {
			m.sysChat(w.text)
		}
		if w.next != nil {
			// A wizard step arrives asynchronously (after a fetch/apply); if
			// the operator opened another picker during that gap, dropping the
			// step beats silently replacing what they're looking at.
			if m.picker != nil {
				m.sysChat("route configure: another picker opened meanwhile — wizard stopped (settings already applied are kept); re-run /route configure to continue")
			} else {
				m.picker = w.next
			}
		}
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
		if m.logs != nil {
			return m, m.logsKey(msg)
		}
		// Images (TEN-493): Ctrl+V tries the clipboard image before the text
		// paste; a paste that is nothing but image file paths (a dragged
		// file) attaches them instead of typing the path.
		if m.inChatInput() {
			if isImagePasteKey(msg.String()) {
				return m, m.pasteImageCmd()
			}
			if msg.Type == tea.KeyBackspace && m.deletePlaceholder() {
				return m, nil
			}
			if msg.Paste {
				if len(msg.Runes) == 0 { // a terminal's Cmd+V with only an image on the clipboard
					return m, m.pasteImageCmd()
				}
				if paths := pastedImagePaths(string(msg.Runes)); len(paths) > 0 {
					return m, m.attachFilesCmd(paths)
				}
				if name := string(msg.Runes); isFinderImageName(name) {
					return m, m.finderPasteCmd(name)
				}
			}
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
			if m.configureSession != nil {
				m.configureSession = nil
				m.input.Blur()
				m.input.Reset()
				m.ta.Focus()
				m.sysChat("configuration cancelled")
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
		case "ctrl+o":
			// Open or close every turn's tool rows (arguments and results) (TEN-514).
			m.toolsOpen = !m.toolsOpen
			m.refresh()
		case "alt+f":
			m.setFeed(m.feedOff, true)
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
		case "alt+up":
			m.scrollFeed(-1)
		case "alt+down":
			m.scrollFeed(1)
		case "alt+pgup", "ctrl+pgup":
			m.scrollFeed(-m.pageStep())
		case "alt+pgdown", "ctrl+pgdown":
			m.scrollFeed(m.pageStep())
		}
	case tea.MouseMsg:
		// Wheel scrolls the pane under the cursor (chat left, feed right).
		if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
			const step = 3
			up := msg.Button == tea.MouseButtonWheelUp
			if m.logs != nil && msg.X < m.chat.Width {
				m.logsWheel(up)
			} else if msg.X < m.chat.Width {
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
	case imagesAttachedMsg:
		if msg.text != "" {
			m.ta.InsertString(msg.text) // the paste wasn't an image after all
		}
		m.addAttachments(msg.imgs)
		for _, n := range msg.notes {
			m.sysChat(n)
		}
		m.refresh()
		return m, msg.then
	case logsMsg:
		cmds = append(cmds, msg.apply(m))
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
		if !m.hasPending(r.ID) { // de-dupe vs the EventApprovalPending backstop (TEN-203)
			m.pending = append(m.pending, r)
			m.showApproval(r)
			m.appendFeed(cErr.Render("⚠ approval needed: " + r.Category))
		}
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
		m.sealStreaming()
		m.inTurn = false
		m.thinkSince, m.toolsSince = time.Time{}, time.Time{}
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
			if subCmd := m.submit(); subCmd != nil {
				cmds = append(cmds, subCmd)
			}
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
		if m.busy && m.streaming {
			m.refresh() // the live line under the answer counts up (cached blocks are a copy)
		}
	}
	// (sysChatMsg is caught earlier in Update at the picker/async layer.)

	var icmd tea.Cmd
	if m.secretEntry != nil || m.setupEntry != nil || m.configureSession != nil {
		if !m.input.Focused() { // an unfocused textinput drops keys; /setup prompts never focused it
			m.ta.Blur()
			m.input.Focus()
		}
		m.input, icmd = m.input.Update(msg)
	} else {
		m.ta, icmd = m.ta.Update(msg)
	}
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
	q, routeOverride := agent.ParseRoutePrefix(q)
	if strings.TrimSpace(q) == "" {
		m.sysChat("!big and !local must be followed by a message")
		return nil
	}
	m.msgs = append(m.msgs, chatMsg{role: "user", content: q, at: time.Now()})
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
	req := agent.TurnRequest{
		UserQuery: q, RouteSurface: agent.RouteSurfaceTUI, RouteOverride: routeOverride,
		Images: m.takeAttachments(q),
	}
	// While a /goal loop is active, decouple this turn's iteration budget from
	// the global loop ceiling so a long autonomous run can iterate freely
	// without raising the ceiling every normal turn shares (TEN-216).
	if m.cfg.Goals != nil && m.cfg.Goals.Active() {
		req.LoopCeiling = m.cfg.Goals.LoopCeiling()
		req.NoJudge = true // the goal judges its own turns (TEN-510)
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
		last := &m.msgs[n-1]
		// A turn with tool calls streamed the text before them too, and the
		// final answer is its tail: keep the streamed text so the rows stay
		// where they happened. Otherwise (no rows, or tokens were dropped) the
		// result is the text and the rows go first.
		if len(last.tools) == 0 || !strings.HasSuffix(strings.TrimSpace(last.content), strings.TrimSpace(response)) {
			last.content = response
			for i := range last.tools {
				last.tools[i].pos = 0
			}
		}
		last.rendered = ""
		last.final = true
		return
	}
	m.msgs = append(m.msgs, chatMsg{role: "assistant", content: response, at: time.Now(), model: m.cfg.Model, final: true})
}

// sealStreaming ends the streaming message's live rendering: the text is
// complete, so the whole of it renders as Markdown.
func (m *model) sealStreaming() {
	if m.streaming {
		if n := len(m.msgs); n > 0 && m.msgs[n-1].role == "assistant" {
			m.msgs[n-1].final = true
		}
	}
	m.streaming = false
}

// settleToolRow fills in the row of the call e answers: by the model's call
// id when both sides carry it, else the oldest open row with that name.
func (m *model) settleToolRow(e agent.Event) {
	n := len(m.msgs)
	if n == 0 || m.msgs[n-1].role != "assistant" {
		return
	}
	rows := m.msgs[n-1].tools
	for i := range rows {
		t := &rows[i]
		if t.done || t.name != e.Tool || (e.ToolCallID != "" && t.id != "" && t.id != e.ToolCallID) {
			continue
		}
		t.done, t.isErr, t.result = true, e.IsErr, e.Result
		t.dur = e.Duration
		if t.dur == 0 {
			t.dur = time.Since(t.at)
		}
		return
	}
}

// setFeed shows or hides the activity pane and, when save is set, records the
// choice through the SaveFeed hook so the next launch keeps it (TEN-514).
func (m *model) setFeed(on, save bool) {
	m.feedOff = !on
	if m.ready {
		m.resize(m.width, m.height)
		m.refresh()
	}
	if save && m.cfg.SaveFeed != nil {
		if err := m.cfg.SaveFeed(on); err != nil {
			m.appendFeed(cErr.Render("feed choice not saved: " + safeErr(err)))
		}
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
	images := m.takeAttachments(q)
	if m.cfg.Agent != nil {
		m.cfg.Agent.InterjectWithImages(q, images)
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

// handleSlash runs a /command through the slash registry (cmd_registry.go)
// and returns an optional tea.Cmd (e.g. quit).
func (m *model) handleSlash(line string) tea.Cmd {
	_, cmd, _ := slashParts(line)
	if c := slashCommandNamed(cmd); c != nil {
		return c.Run(m, line)
	}
	m.sysChat("unknown command " + cmd + " — try /help")
	return nil
}

// --- C3: /research history / show / replay / delete ---

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
// hasPending reports whether an approval with this id is already shown (TEN-203
// de-dupe: the channel fast-path and the event backstop must not double-add).
func (m *model) hasPending(id string) bool {
	if id == "" {
		return false
	}
	for _, p := range m.pending {
		if p.ID == id {
			return true
		}
	}
	return false
}

// dropPending removes the approval with this id from the queue (TEN-203: a
// resolved-elsewhere event prunes the card live).
func (m *model) dropPending(id string) {
	out := make([]ApprovalRequest, 0, len(m.pending))
	for _, p := range m.pending {
		if p.ID == id {
			continue
		}
		out = append(out, p)
	}
	m.pending = out
}

// sendDecision answers the oldest pending approval and reports the outcome.
func (m *model) sendDecision(d ApprovalDecision) {
	if len(m.pending) == 0 {
		m.sysChat("nothing is awaiting approval")
		return
	}
	r := m.pending[0]
	m.pending = m.pending[1:]
	verb := map[ApprovalDecision]string{
		DenyOnce:       "denied",
		ApproveOnce:    "approved (once)",
		ApproveSession: "approved for this session",
		ApproveAlways:  "approved always (saved)",
	}[d]
	// Resolve by id through the broker registry (TEN-203): first-resolver-wins.
	// If the dashboard (or another surface) resolved this one first, ResolveApproval
	// returns false → the agent already unblocked, so this is a clean no-op here.
	if m.cfg.ResolveApproval != nil {
		if !m.cfg.ResolveApproval(r.ID, d) {
			m.sysChat("already resolved elsewhere — " + r.Category)
			return
		}
		m.sysChat(verb + " — " + r.Category)
		return
	}
	// Legacy fallback (no broker wired, e.g. tests): write Reply directly.
	select {
	case r.Reply <- d:
	default: // requester gone (e.g. turn cancelled); nothing to do
	}
	m.sysChat(verb + " — " + r.Category)
}

func (m *model) applyEvent(e agent.Event) {
	// A scheduled job's activity carries its cron agent's id, so it is taken
	// before the cross-agent skip below.
	if e.Source == "cron" {
		m.applyCronEvent(e)
		return
	}
	// Cross-agent events (sub-agent activity, bus traffic) are mirrored onto the
	// shared broker for the DASHBOARD (TEN-234); the TUI renders sub-agents via
	// the separate TeamEvents channel, so skip them here to avoid double-render.
	if e.Agent != "" || e.Kind == agent.EventBus {
		return
	}
	switch e.Kind {
	case agent.EventApprovalResolved:
		if e.Approval != nil && strings.HasPrefix(e.Approval.ID, "beside-") {
			// The mirror of an approval another surface owned (TEN-203) closed out.
			m.appendFeed(cDim.Render("approval in " + strings.TrimPrefix(e.Approval.ID, "beside-") + ": " + e.Approval.Outcome))
			break
		}
		// Another observer (the dashboard) resolved a pending action, OR this TUI's
		// own resolve fired — prune the matching card so it vanishes live (TEN-203).
		if e.Approval != nil {
			m.dropPending(e.Approval.ID)
		}
	case agent.EventApprovalPending:
		if e.Approval != nil && strings.HasPrefix(e.Approval.ID, "beside-") {
			// A MIRROR of an approval owned by another surface (Discord buttons,
			// TEN-203): shown so the operator knows one is outstanding, but it is
			// not a card this TUI can resolve — say where it lives instead of
			// rendering a dead Approve/Deny prompt. (That dead prompt is what read
			// as "the approval showed up on the TUI instead of Discord.")
			where := strings.TrimPrefix(e.Approval.ID, "beside-")
			m.appendFeed(cErr.Render("⏳ approval waiting in " + where + " (" + e.Approval.Category + "): " + e.Approval.Action + " — resolve it there"))
			break
		}
		// Backstop for a dropped non-blocking channel notify: if the fast-path
		// approvalMsg didn't deliver this id, surface it from the event (TEN-203).
		// De-duped by ID so the channel + event never double-add.
		if e.Approval != nil && !m.hasPending(e.Approval.ID) {
			r := ApprovalRequest{ID: e.Approval.ID, Category: e.Approval.Category, Action: e.Approval.Action, Detail: e.Approval.Detail}
			m.pending = append(m.pending, r)
			m.showApproval(r)
			m.appendFeed(cErr.Render("⚠ approval needed: " + r.Category))
		}
	case agent.EventTurnStart:
		// The turn's header in the feed: the time and the question's first
		// words; the turn's own lines indent under it until it ends (TEN-514).
		head := time.Now().Format("15:04:05")
		if q := clip(oneLine(e.Text), 60); q != "" {
			head += " " + q
		} else {
			head += " turn start"
		}
		m.inTurn = false
		m.appendFeed(cDim.Render("▸ " + head))
		m.inTurn = true
	case agent.EventRoute:
		// Room for the classifier's host and model after the decision (TEN-340).
		m.appendFeed(cDim.Render("route: " + clip(oneLine(e.Text), 140)))
	case agent.EventThinking:
		m.thinkSince, m.toolsSince = time.Now(), time.Time{}
	case agent.EventThought:
		m.thinkSince = time.Time{}
		line := "thought for " + formatElapsed(e.Duration)
		if e.IsErr {
			line += " (the call failed)"
		}
		m.appendFeed(cDim.Render(line))
	case agent.EventTurnEnd:
		m.thinkSince, m.toolsSince = time.Time{}, time.Time{}
		if e.Think > 0 || e.Tools > 0 {
			m.appendFeed(cDim.Render("turn " + formatElapsed(e.Duration) + " — thinking " + formatElapsed(e.Think) + " · tools " + formatElapsed(e.Tools)))
		}
		m.inTurn = false
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
			m.cfg.RecordUsage(e.PromptTokens, e.CompletionTokens, e.TraceID)
		}
	case agent.EventTeamBudget:
		if e.TeamBudget != nil && e.TeamBudget.TerminationReason != "" {
			m.appendFeed(cErr.Render("team " + e.Text))
		} else {
			m.appendFeed(cDim.Render(e.Text))
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
				// One line (TEN-514): the total against the window, then the static
				// breakdown (TEN-214): the tool-definition cost dominates a full mux
				// and is the thing that silently eats the window, so soul/system/
				// tools and the working set show each turn and "where did my
				// context go?" is answerable from the feed.
				m.appendFeed(cDim.Render(fmt.Sprintf("ctx %s tok (%d%%) · soul %s · sys %s · tools %s · working %s",
					humanTokens(e.Budget.Total), m.budgetPct, humanTokens(e.Budget.SoulTokens), humanTokens(e.Budget.SystemTokens),
					humanTokens(e.Budget.ToolTokens), humanTokens(e.Budget.WorkingTokens))) + note)
			}
		}
	case agent.EventToken:
		if msg := m.curAssistant(); msg != nil { // nil: a stale trailing token (TEN-217)
			msg.content += e.Text
		}
	case agent.EventAssistant:
		// Non-stream fallback: whole assistant text at once (a turn that
		// streamed nothing); nil once turnDoneMsg sealed the turn (TEN-217).
		if !m.streaming {
			if msg := m.curAssistant(); msg != nil {
				msg.content = e.Text
			}
		}
	case agent.EventSkills:
		m.appendFeed(cSys.Render("skills: " + clip(e.Text, 60)))
	case agent.EventToolCall:
		m.lastTool = e.Tool
		if m.toolsSince.IsZero() {
			m.toolsSince = time.Now()
		}
		m.appendFeed(cTool.Render("→ "+e.Tool) + " " + cDim.Render(clip(e.Args, 60)))
		if msg := m.curAssistant(); msg != nil { // the row in the turn block (TEN-514)
			msg.tools = append(msg.tools, toolRow{id: e.ToolCallID, name: e.Tool, args: e.Args, at: time.Now(), pos: len(msg.content)})
		}
	case agent.EventToolResult:
		mark := cOK.Render("✓")
		if e.IsErr {
			mark = cErr.Render("✗")
		}
		m.appendFeed(fmt.Sprintf("%s %s %s", mark, cTool.Render(e.Tool), cDim.Render(clip(oneLine(e.Result), 70))))
		m.settleToolRow(e)
	case agent.EventValidation:
		m.appendFeed(cErr.Render("! invalid tool call " + e.Tool + ": " + clip(e.Text, 60)))
	case agent.EventCompact:
		m.appendFeed(cSys.Render("⊟ context compacted: " + e.Text))
	case agent.EventInterject:
		m.appendFeed(cSys.Render("↪ folding in your message: " + clip(e.Text, 60)))
	case agent.EventJudge:
		style := cSys
		if e.IsErr {
			style = cErr
		}
		m.appendFeed(style.Render("⚖ judge " + clip(e.Text, 80)))
	case agent.EventIngest:
		// An offsite channel (Discord/iMessage) received a message and drove a
		// turn. e.Text is already prefixed "Discord: " / "iMessage: " (TEN-232).
		m.appendFeed(cSys.Render("📥 " + clip(e.Text, 80)))
	case agent.EventFinal:
		m.sealStreaming()
		m.appendFeed(cDim.Render("answer ready"))
	case agent.EventTruncated:
		m.sealStreaming()
		m.appendFeed(cErr.Render("loop ceiling hit — synthesized"))
	case agent.EventStuck:
		m.sealStreaming()
		m.appendFeed(cErr.Render("stuck repeating the same tool calls — synthesized (/ceiling repeat to tune)"))
	case agent.EventError:
		m.err = e.Text
		m.appendFeed(cErr.Render("error: " + clip(e.Text, 80)))
	}
}

// applyCronEvent renders a scheduled job's mirrored activity into the feed,
// its tools named cron:<tool> (agent.CronTool) so automated work reads apart
// from a turn's.
func (m *model) applyCronEvent(e agent.Event) {
	switch e.Kind {
	case agent.EventToolCall:
		m.appendFeed(cTool.Render("→ "+agent.CronTool(e.Tool)) + " " + cDim.Render(clip(e.Args, 60)))
	case agent.EventToolResult:
		mark := cOK.Render("✓")
		if e.IsErr {
			mark = cErr.Render("✗")
		}
		m.appendFeed(fmt.Sprintf("%s %s %s", mark, cTool.Render(agent.CronTool(e.Tool)), cDim.Render(clip(oneLine(e.Result), 70))))
	case agent.EventError:
		m.appendFeed(cErr.Render("cron error: " + clip(e.Text, 80)))
	case agent.EventFinal:
		m.appendFeed(cDim.Render("cron: job done"))
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
	case agent.EventTeamBudget:
		if e.TeamBudget != nil && e.TeamBudget.TerminationReason != "" {
			m.appendFeed(cErr.Render(tag + " " + e.Text))
		} else {
			m.appendFeed(cDim.Render(tag + " " + e.Text))
		}
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
	case agent.EventThought:
		m.appendFeed(cDim.Render(tag + " thought for " + formatElapsed(e.Duration)))
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
		if err := fsperm.WriteFile(m.cfg.SavePath, []byte(txt)); err == nil {
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

// appendFeed adds a line to the activity feed. While the main agent's turn
// runs, its lines indent under the turn's header (TEN-514); cron and
// sub-agent lines come through applyCronEvent and applyTeamEvent, which
// append with inTurn unchanged, so they read apart as before.
func (m *model) appendFeed(line string) {
	if m.inTurn {
		line = "  " + line
	}
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
	if m.feedOff { // /feed off, Alt+F: the chat takes the width (TEN-514)
		feedW = 0
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

// scrollFeed is scrollChat for the activity feed: the keyboard's way to
// scroll it, mouse capture or not (TEN-381).
func (m *model) scrollFeed(lines int) {
	if lines < 0 {
		m.feed.LineUp(-lines)
	} else {
		m.feed.LineDown(lines)
	}
	m.feedFollow = m.feed.AtBottom()
}

// pageStep is a near-full page (leave one line of overlap for context).
func (m *model) pageStep() int {
	if s := m.chat.Height - 1; s > 1 {
		return s
	}
	return 1
}

// renderChat draws the conversation as turn blocks (TEN-514): a header line
// with a gutter dot, who, the time and (for the agent) the model; the text
// rendered from Markdown at the pane width; the turn's tool calls as rows
// where they happened; a rule between turns. System lines keep their own
// styling. Each block is cached against what it was rendered from, so a
// repaint while the agent thinks costs one string copy per settled block.
func (m *model) renderChat() string {
	w := m.chat.Width
	var b strings.Builder
	seenTurn := false
	for i := range m.msgs {
		msg := &m.msgs[i]
		switch msg.role {
		case "user":
			if seenTurn {
				b.WriteString(pal.Rule.Render(strings.Repeat("─", max1(w))) + "\n\n")
			}
			seenTurn = true
			b.WriteString(m.turnBlock(msg, w) + "\n\n")
		case "assistant":
			seenTurn = true
			b.WriteString(m.turnBlock(msg, w))
			if live := m.liveLine(i); live != "" {
				b.WriteString("\n  " + live)
			}
			b.WriteString("\n\n")
		default: // system: styled form if present, else plain (default fg)
			disp := msg.rendered
			if disp == "" {
				disp = msg.content
			}
			b.WriteString(wrap(disp, w) + "\n\n")
		}
	}
	return b.String()
}

// blockIndent is the text's offset under a turn's header line.
const blockIndent = 2

// turnBlock renders one user or assistant message as a block, from cache
// when nothing it depends on changed.
func (m *model) turnBlock(msg *chatMsg, w int) string {
	done := 0
	for _, t := range msg.tools {
		if t.done {
			done++
		}
	}
	key := fmt.Sprintf("%d|%t|%d|%d|%d|%t", w, msg.final, len(msg.content), len(msg.tools), done, m.toolsOpen)
	if msg.cache != "" && msg.cacheKey == key {
		return msg.cache
	}
	var b strings.Builder
	b.WriteString(m.blockHeader(msg))
	inner := w - blockIndent
	if inner < 10 {
		inner = 10
	}
	pad := strings.Repeat(" ", blockIndent)
	body := ""
	if msg.role == "user" {
		body = wrap(msg.content, inner)
	} else {
		body = m.assistantBody(msg, inner)
	}
	if body != "" {
		for _, line := range strings.Split(body, "\n") {
			b.WriteString("\n" + pad + line)
		}
	}
	msg.cache, msg.cacheKey = b.String(), key
	return msg.cache
}

// blockHeader is "● you · 15:04" or "● tenant · 15:04 · <model>".
func (m *model) blockHeader(msg *chatMsg) string {
	who := cUser.Render("you")
	if msg.role == "assistant" {
		who = cAgent.Render("tenant")
	}
	h := pal.Glyph.Render("●") + " " + who
	if !msg.at.IsZero() {
		h += pal.Time.Render(" · " + msg.at.Format("15:04"))
	}
	if msg.role == "assistant" && msg.model != "" {
		h += pal.Time.Render(" · " + msg.model)
	}
	return h
}

// assistantBody interleaves the message's text, rendered from Markdown, with
// its tool rows at the positions they happened. A streaming message renders
// its closed paragraphs as Markdown and the open tail as plain text, so the
// renderer never re-lays the whole answer per token.
func (m *model) assistantBody(msg *chatMsg, w int) string {
	var parts []string
	start := 0
	for i, t := range msg.tools {
		pos := min(max(t.pos, start), len(msg.content))
		if seg := msg.content[start:pos]; strings.TrimSpace(seg) != "" {
			parts = append(parts, m.renderText(seg, w, true))
		}
		parts = append(parts, m.toolRowLines(t, w))
		start = pos
		_ = i
	}
	if seg := msg.content[start:]; strings.TrimSpace(seg) != "" {
		parts = append(parts, m.renderText(seg, w, msg.final))
	}
	return strings.Join(parts, "\n")
}

// renderText renders Markdown at width w. Unless the text is complete, only
// the paragraphs closed by a blank line are rendered; the rest is plain.
func (m *model) renderText(text string, w int, complete bool) string {
	text = strings.TrimSpace(text)
	if complete {
		return md.ANSI(text, w, pal.markdown)
	}
	head, tail := "", text
	if i := strings.LastIndex(text, "\n\n"); i >= 0 {
		head, tail = text[:i], strings.TrimSpace(text[i+2:])
	}
	out := ""
	if head != "" {
		out = md.ANSI(head, w, pal.markdown)
	}
	if tail != "" {
		if out != "" {
			out += "\n\n"
		}
		out += wrap(tail, w)
	}
	return out
}

// toolRowLines is a tool call inside a turn block: one line, closed
// (`⏺ web_search("…") ✓ 1.2s · 3 results`), or the arguments and result
// under it when the block is open (Ctrl+O) or the call failed.
func (m *model) toolRowLines(t toolRow, w int) string {
	head := pal.Glyph.Render("⏺") + " " + cTool.Render(t.name) + cDim.Render("("+clip(t.args, 48)+")")
	switch {
	case !t.done:
		head += " " + cDim.Render("…")
	case t.isErr:
		head += " " + cErr.Render("✗") + cDim.Render(" "+formatElapsed(t.dur))
	default:
		head += " " + cOK.Render("✓") + cDim.Render(" "+formatElapsed(t.dur)+" · "+clip(t.result, max(10, w-40)))
	}
	if !t.done || (!m.toolsOpen && !t.isErr) {
		return head
	}
	lines := []string{head}
	add := func(label, text string, limit int) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		body := strings.Split(wrap(text, max(10, w-4)), "\n")
		if len(body) > limit {
			body = append(body[:limit], cDim.Render(fmt.Sprintf("… %d more lines (see /logs)", len(body)-limit)))
		}
		for i, l := range body {
			mark := "  "
			if i == 0 {
				mark = pal.Time.Render("⎿ ")
			}
			lines = append(lines, "  "+mark+l)
		}
		_ = label
	}
	add("args", t.args, 6)
	if t.isErr {
		add("error", cErr.Render(oneLine(t.result)), 8)
	} else {
		add("result", t.result, 20)
	}
	return strings.Join(lines, "\n")
}

// liveLine is the in-flight state under the message the agent is writing:
// what it is doing and for how long, as the footer timer shows it.
func (m *model) liveLine(i int) string {
	if !m.busy || i != len(m.msgs)-1 || m.msgs[i].final {
		return ""
	}
	switch {
	case !m.thinkSince.IsZero():
		return cDim.Render(m.spin.View() + "thinking… " + formatElapsed(time.Since(m.thinkSince)))
	case !m.toolsSince.IsZero():
		name := m.lastTool
		if name != "" {
			name += " "
		}
		return cDim.Render(m.spin.View() + name + "… " + formatElapsed(time.Since(m.toolsSince)))
	}
	return ""
}

// curAssistant is the assistant message this turn is writing, made on first
// use (a turn can open with a tool call, before any text). nil once the turn
// is sealed, so a stale trailing event never starts a new bubble (TEN-217).
func (m *model) curAssistant() *chatMsg {
	if m.assistantSealed {
		return nil
	}
	if !m.streaming {
		m.msgs = append(m.msgs, chatMsg{role: "assistant", at: time.Now(), model: m.cfg.Model})
		m.streaming = true
	}
	return &m.msgs[len(m.msgs)-1]
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
// "⏱ 3.4s" (<60s), "⏱ 1:09" (m:ss, <1h), or "⏱ 1:40:05" (h:mm:ss, >=1h),
// then what it is doing: "thinking… 45.0s" while the model answers, so a slow
// model reads apart from a hung one, or "tools… 2.1s" (TEN-342).
// Display-only, never persisted. Empty when no turn is in flight; the existing
// spinner tick drives the sub-second repaint, so no extra ticker is needed.
func (m *model) reqTimer() string {
	if !m.busy || m.reqStart.IsZero() {
		return ""
	}
	s := "⏱ " + formatElapsed(time.Since(m.reqStart))
	switch {
	case !m.thinkSince.IsZero():
		s += " · thinking… " + formatElapsed(time.Since(m.thinkSince))
	case !m.toolsSince.IsZero():
		s += " · tools… " + formatElapsed(time.Since(m.toolsSince))
	}
	return cDim.Render(s)
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
	left := m.chat.View()
	if m.logs != nil {
		left = m.renderLogs()
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, chatPad.Render(left), paneBorder.Render(""), m.feed.View())
	if m.feedOff {
		body = chatPad.Render(left)
	}
	// Footer: help (left) + context gauge (bottom-right), width-aligned,
	// sharing the chat's left gutter.
	gutter := strings.Repeat(" ", chatGutter)
	helps := []string{
		gutter + "Enter send · Ctrl+V image · /help · PgUp/PgDn scroll · Ctrl-O tools · Alt-F feed · Ctrl-Y copy · /exit quit",
		gutter + "/help · Ctrl-O tools · Alt-F feed · /exit quit",
		gutter + "/help · /exit quit",
		"",
	}
	if n := m.pendingImages(); n > 0 { // TEN-493: say what rides the next message, and how to drop it
		helps = []string{gutter + fmt.Sprintf("📎 %d attached · Backspace removes an [image] tag", n), gutter + fmt.Sprintf("📎 %d attached", n), ""}
	}
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
	// The footer must never be wider than the terminal: a wider last line
	// wraps, the screen scrolls, and every later redraw lands a row off (a
	// pasted image's placeholder didn't show until a full redraw). Take the
	// longest help that fits beside the gauges, then cut whatever remains.
	help := helps[len(helps)-1]
	for _, h := range helps {
		if lipgloss.Width(h)+lipgloss.Width(right)+2 <= m.width {
			help = h
			break
		}
	}
	gap := max(1, m.width-lipgloss.Width(help)-lipgloss.Width(right)-1)
	footer := lipgloss.NewStyle().MaxWidth(m.width).Render(cDim.Render(help) + strings.Repeat(" ", gap) + right + " ")
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
	} else if m.logs != nil {
		inputArea = m.logsHint()
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

// wrap wraps long lines to a VISIBLE width at word boundaries, never slicing
// inside an ANSI escape sequence — so styled lists/help don't get corrupted
// mid-color-code. Counts runes (display cells), copies escape sequences
// through verbatim without advancing the column. A word wider than the width
// is broken at the column (a URL, a hash).
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
	// Tokens are words with their trailing space, escapes attached to the
	// word they style, so joining the lines back gives the input exactly.
	var toks []string
	var cur strings.Builder
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			j := escapeEnd(line, i)
			cur.WriteString(line[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		cur.WriteString(line[i : i+size])
		i += size
		if r == ' ' {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	var out strings.Builder
	col := 0
	for _, t := range toks {
		tw := visibleRunes(t)
		if col > 0 && col+tw > width {
			out.WriteByte('\n')
			col = 0
		}
		if tw <= width {
			out.WriteString(t)
			col += tw
			continue
		}
		// A single token wider than the pane: break it at the column.
		for i := 0; i < len(t); {
			if t[i] == 0x1b {
				j := escapeEnd(t, i)
				out.WriteString(t[i:j])
				i = j
				continue
			}
			_, size := utf8.DecodeRuneInString(t[i:])
			if col >= width {
				out.WriteByte('\n')
				col = 0
			}
			out.WriteString(t[i : i+size])
			i += size
			col++
		}
	}
	return out.String()
}

// escapeEnd returns the index just past the ANSI escape sequence at s[i].
func escapeEnd(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' { // CSI ... final byte @-~
		j++
		for j < len(s) && (s[j] < '@' || s[j] > '~') {
			j++
		}
		if j < len(s) {
			j++ // include the final byte
		}
	}
	return j
}

// visibleRunes counts the display cells of s, escape sequences excluded.
func visibleRunes(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = escapeEnd(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
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

// (sysChatMsg is defined earlier; configure flow reuses it.)

// --- /skill (singular) helpers — TEN-64 integration-config surface ---
