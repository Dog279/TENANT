package route

import "tenant/internal/logs/event"

// Events is the prompt router (docs/EVENTS.md). Each decision's event carries
// its decision_id, the id of its row in route-decisions.jsonl.
var Events = event.NewProvider("route", "route")

var Decided = Events.Event(1001, event.Info, "Decision", "Route decision {decision_id}: {summary}", "decision_id", "summary", "mode", "status", "tier", "role", "profile", "reason", "agent", "session", "endpoint", "model", "latency_ms").
	Doc("The router classified a prompt and chose where it runs (or, in shadow mode, where it would have). When it asked the classifier, endpoint, model and latency_ms say which one and how long it took.",
		"A turn started with routing on.",
		"None. /route why shows the latest decisions in full; route-decisions.jsonl has every one by decision_id.")

var DecideFailed = Events.Event(2001, event.Warning, "Decision", "Route decision {decision_id} fell back to the planner: {err}", "decision_id", "summary", "mode", "agent", "session", "err", "endpoint", "model", "latency_ms").
	Doc("The router could not classify a prompt or apply its policy, so the turn ran on the planner. When it asked the classifier, endpoint, model and latency_ms say which one and how long it took.",
		"The classifier is down, slow or returned something unreadable, or a bound profile changed mid-decision.",
		"Check /route status. /route why shows the decision and the classifier's raw output.")

var BreakerOpened = Events.Event(2002, event.Warning, "Classifier", "The route classifier failed repeatedly; classification is skipped until it recovers", "decision_id").
	Doc("The router's circuit breaker opened: turns skip the classifier and run on the planner for a while.",
		"Several classifier calls in a row failed or timed out.",
		"Check the classifier's server (/route status). The breaker retries by itself.")

var WarmFailed = Events.Event(2003, event.Warning, "Classifier", "Route classifier warm-up failed at {endpoint}: {err}", "endpoint", "model", "err").
	Doc("The classifier model could not be pre-loaded, so the first decisions may time out.",
		"Its server is down or unreachable, or does not serve the configured model.",
		"Start the classifier's server, or fix route.endpoint and route.model in config.json.")

var RuleWeak = Events.Event(2004, event.Warning, "Rules", "Route rule {rule} for role {role}: {problem}", "rule", "role", "profile", "problem").
	Doc("A routing rule points at a role that can't serve it well, or at no model at all.",
		"The rule's role has no bound profile, or its profile is tagged low quality for the rule's kind of prompt.",
		"Bind the role to a suitable provider, or change the rule in config.json route.rules.")

var RoleUnavailable = Events.Event(2005, event.Warning, "Decision", "Routed role {role} unavailable; using {fallback}: {err}", "decision_id", "role", "fallback", "err").
	Doc("The router chose a role, but it could not be used for this turn, so the turn ran elsewhere.",
		"The role's binding changed after the decision, or never_cloud ruled out its profile.",
		"Check the role's binding with /route status and /model.")

var PruneFailed = Events.Event(2006, event.Warning, "Decision log", "Route decision log prune failed: {err}", "err").
	Doc("Old decisions were not removed from route-decisions.jsonl, so it keeps growing.",
		"The file or its lock could not be read or rewritten: permissions, a full disk, or another process holding it.",
		"Check the file in the data folder; the next daily prune retries.")

var BigRoleIdle = Events.Event(2007, event.Warning, "Configuration", "Planner-big provider {provider} unavailable while routing is off: {err}", "provider", "err").
	Doc("route.big_role names a provider that can't be built. Routing is off, so nothing uses it yet.",
		"The provider was removed or renamed, or its key is missing.",
		"Fix or clear route.big_role before turning routing on.")

var RecordFailed = Events.Event(3001, event.Error, "Decision log", "Route decision {decision_id} was not saved: {err}", "decision_id", "err").
	Doc("A decision was not written to route-decisions.jsonl; /route why still shows it until restart.",
		"The data folder or the file's lock could not be written: permissions or a full disk.",
		"Check that route-decisions.jsonl in the data folder is writable.")

var ConfirmFailed = Events.Event(3002, event.Error, "Decision log", "The operator's confirmation of route decision {decision_id} was not saved: {err}", "decision_id", "err").
	Doc("Accepting an assisted suggestion with !big was not recorded as a gold label.",
		"route-decisions.jsonl could not be rewritten: permissions, a full disk, or its lock.",
		"Check the file in the data folder; the turn itself ran as asked.")

var Panicked = Events.Event(3003, event.Error, "Guard", "Route {stage} panicked: {panic}", "stage", "panic", "stack").
	Doc("A bug in the router was caught; the turn ran on the planner, or without the decision's annotation.",
		"A programming error in the classifier, the policy input or a decision annotator.",
		"Report it with the stack; routing keeps working for other turns.")

var ForceArmed = Events.Event(4001, event.Warning, "Privacy override", "Route forced to profile {profile}; never_cloud is pierced", "profile", "binding").
	Keywords("audit").
	Doc("The operator forced routing to one profile, overriding never_cloud.",
		"/route force, or route.force_profile in config.json at start.",
		"None when intended. Any other /route mode (on, shadow, off, …) clears it and restores never_cloud.")

var ForceUsed = Events.Event(4002, event.Warning, "Privacy override", "Route decision {decision_id} used forced profile {profile}, piercing never_cloud", "decision_id", "agent", "profile", "role").
	Keywords("audit").
	Doc("A turn ran on the forced profile although never_cloud would have kept it local.",
		"A privacy override (4001) is armed.",
		"None when intended; otherwise switch /route to another mode, which clears the force.")
