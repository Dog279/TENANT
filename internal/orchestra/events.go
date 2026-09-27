package orchestra

import "tenant/internal/logs/event"

// Subagents is the team runtime's event provider: sub-agents spawning and
// finishing, the orchestra bus, and the team-run budget. A sub-agent's own
// turn events (tool calls, errors, …) are the turns provider's, filed in
// this log.
var Subagents = event.NewProvider("subagents", "subagents")

// SubagentRun is one sub-agent, as an activity: start when it is spawned,
// then stop or fail with its duration. Its related ID is the turn that
// spawned it (or the research run); run is the trace of its own turn.
var SubagentRun = Subagents.Event(1, event.Info, "Sub-agent", "Sub-agent {agent} ({role})", "agent", "role", "run", "prompt").
	Sensitive("prompt").
	Doc("A sub-agent started work on its task (prompt), or finished: a stop or a fail, with its duration. Its run field is the trace of its own turn.",
		"The orchestrator spawned it (the spawn_agent tool), or /research did. It fails when its turn fails, or the team run's budget stops it.",
		"None when it stops. When it fails, read the error, then its turn (filter trace:<run>).")

var TeamBudget = Subagents.Event(1101, event.Info, "Team budget", "team budget", "agent", "text", "team_budget").
	Doc("The team run's usage against its budget (text; team_budget holds it as JSON): sub-agents, tool calls, tokens, cost, browsers and wall time.",
		"Written when the number of sub-agents or browsers changes, and every 10 seconds while a team run is active.",
		"None. The limits are set in config.json (budgets).")

var BusMessage = Subagents.Event(1201, event.Info, "Bus", "bus message", "agent", "text").Sensitive("text").
	Doc("A message between agents on the orchestra bus: agent is the sender; text is the recipient and the message.",
		"A sub-agent or the orchestrator used a team tool (team_send, team_broadcast), or a sub-agent reported that it was done.",
		"None.")

var ProfileRouterFailed = Subagents.Event(2001, event.Warning, "Spawn", "No router for agent profile {profile}; the sub-agent uses the shared router: {err}", "profile", "err").
	Doc("A sub-agent's profile pins a model, but its router could not be built, so the sub-agent runs on the orchestrator's model instead.",
		"The profile names a provider or model that isn't configured, its key is missing, or config.json could not be read.",
		"Check the profile (/agents) and its provider's configuration.")

var SpawnRoleUnbound = Subagents.Event(2002, event.Warning, "Spawn", "Spawn rule role {role} is not bound on the spawn router; using the planner", "agent", "role").
	Doc("A spawn routing rule picked a role for a sub-agent that its router has no model for, so it runs on the planner role.",
		"The routing rule names a role that no configured model is bound to.",
		"Bind a model to the role, or change the rule (route in config.json).")

var SpawnRefused = Subagents.Event(2003, event.Warning, "Spawn", "Sub-agent {role} not spawned: {err}", "role", "err").
	Doc("The orchestrator asked for a sub-agent, and the team runtime refused. The orchestrator is told why.",
		"Too many sub-agents are running at once, or the team run's budget (agents, tokens, cost or wall time) is spent.",
		"None when the orchestrator waits for the running ones. Raise the limits in config.json (budgets) if they are too tight.")

var TeamTerminated = Subagents.Event(2101, event.Warning, "Team budget", "team run terminated: {reason}", "agent", "reason", "text", "team_budget").
	Doc("The team run hit a budget limit and was stopped: its running sub-agents were cancelled.",
		"The run used up its tool calls, tokens, cost or wall time (reason).",
		"Raise the limit in config.json (budgets) if the task needs it, or split the task.")

var ClassifierPanicked = Subagents.Event(3001, event.Error, "Spawn", "Spawn classifier panicked; using the planner role: {panic}", "panic", "stack").
	Doc("The spawn routing classifier's code panicked; the sub-agent was spawned on the planner role.",
		"A bug in the spawn classifier or its rules.",
		"Report it with the stack.")
