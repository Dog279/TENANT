package model

import "tenant/internal/logs/event"

// Events is the model router and its backends (docs/EVENTS.md). One log for
// every backend: each event names its profile and backend in fields, so
// data.backend:vllm filters one of them.
var Events = event.NewProvider("model", "model")

// Warnings (2000s): failover, health and degraded configuration.

var FellBack = Events.Event(2001, event.Warning, "Failover", "Model fallback: {from} {reason}, using {to}", "from", "to", "reason", "err").
	Doc("A model call failed on one link of its fallback chain and moved to the next.",
		"The provider was rate limited, out of credits, unreachable or returned a server error.",
		"None while the fallback serves. Out of credits: recharge the provider. Otherwise check the provider's status; it is retried after a short cooldown.")

var SlowSkipped = Events.Event(2002, event.Warning, "Health", "{profile} is slow ({average} average); skipped for {cooldown}", "profile", "average", "cooldown").
	Doc("Health gating moved calls off a profile that answers, but too slowly.",
		"Its average call time passed health_gating's threshold: an overloaded server, a cold model or a slow network.",
		"None; it is tried again after the cooldown. If it recurs, check the server's load, or raise the threshold in config.json health_gating.")

var LinkSkipped = Events.Event(2003, event.Warning, "Fallback chain", "Fallback {profile} skipped for role {role}: {err}", "profile", "role", "err").
	Doc("A fallback profile could not be added to a role's chain, so that role has one fallback fewer.",
		"The profile is not registered, or its backend could not be built: a missing key or an unknown backend.",
		"Check the provider with /model; fix its key or endpoint, then /model fallback to reinstall the chain.")

var ProviderSkipped = Events.Event(2004, event.Warning, "Fallback chain", "Fallback provider {provider} skipped: {err}", "provider", "err").
	Doc("A provider listed in config.json fallbacks was left out of the fallback chain.",
		"It is not a configured provider, or its profiles could not be built or registered.",
		"Check the name against /model, or remove it with /model fallback.")

var GatingIdle = Events.Event(2005, event.Warning, "Configuration", "health_gating is set but no fallbacks are configured; it has nothing to move to").
	Doc("Latency health gating is on, but there is no fallback to prefer over a slow model.",
		"config.json sets health_gating without fallbacks.",
		"Add a fallback with /model fallback, or remove health_gating.")

var EffortUnavailable = Events.Event(2006, event.Warning, "Reasoning effort", "Reasoning effort {effort} unavailable for {profile}; using its default: {err}", "profile", "effort", "err").
	Doc("A turn asked for a different reasoning effort, but that variant of the model could not be built.",
		"The backend refused the profile with the new effort.",
		"None; the turn ran at the profile's own effort. Check the provider's reasoning setting with /model.")

var SwapUnready = Events.Event(2007, event.Warning, "Swap", "Switched to {provider}, but it is not ready: {problem}", "provider", "model", "endpoint", "problem").
	Doc("The primary model was switched, but the check right after found it unreachable or not serving the model.",
		"The server is down or still loading, or it serves a different model name.",
		"Start the server, or fix the model name with /model; the next message retries.")

var BudgetInvalid = Events.Event(2008, event.Warning, "Budget", "Ignoring {env}={value}: not a positive token count", "env", "value").
	Doc("The claudecode backend's weekly token budget is set but unreadable, so no budget is enforced.",
		"The environment variable holds something other than a positive whole number.",
		"Set it to a token count, or unset it.")

var BudgetUnsaved = Events.Event(2009, event.Warning, "Budget", "The claudecode spend ledger was not saved: {err}", "path", "err").
	Doc("A claudecode call's tokens were not added to the weekly spend ledger, so the budget undercounts.",
		"The ledger file in the config folder could not be written: permissions or a full disk.",
		"Check that the file named by path is writable.")

var ServerNotAdded = Events.Event(2010, event.Warning, "Server", "A model server at {endpoint} was not added: {problem}", "endpoint", "problem", "err").
	Doc("The dashboard's Model page checked a server on the network before adding it as a model, and the check stopped it. The page showed problem; err is the technical reason.",
		"The address is wrong or unreachable from the computer running Tenant, the server wants an API key or refused it, it isn't OpenAI-compatible, it offers several models and none was named, or it refused a test message that offered a tool.",
		"Fix what problem names (an address, a key, the model's name, or the server's tool-calling setting), then add it again.")

var ServerToolsUnconfirmed = Events.Event(2011, event.Warning, "Server", "{provider}'s tool calls weren't confirmed ({tools}): {detail}", "provider", "model", "tools", "format", "detail").
	Doc("A model server was added, but its test message didn't show a tool call Tenant can read: the model answered without using the tool it was offered (tools none), or the test didn't finish in time (tools unchecked). Tool calls are read in format.",
		"The server's tool calling is off (vLLM without --enable-auto-tool-choice and a --tool-call-parser, llama.cpp without --jinja), the model doesn't do tool calls, or it was still loading.",
		"Turn tool calling on in the server and add the model again, or run tenant doctor --deep with it as the main model to repeat the check.")

// Errors (3000s).

var Unavailable = Events.Event(3001, event.Error, "Call", "Model {profile} unavailable ({reason}): {err}", "profile", "backend", "model", "reason", "err").
	Doc("A model call failed because its provider could not serve it.",
		"The server is down or unreachable, rate limited, out of credits, or returned a server error.",
		"Check the provider (/model, doctor). With fallbacks configured the call moved on; otherwise the turn failed.")

var UsageUnrecorded = Events.Event(3002, event.Error, "Usage", "Token usage for {model} was not recorded: {err}", "model", "err").
	Doc("One model call's tokens are missing from the usage ledger (usage.db).",
		"The ledger could not be written: a locked or damaged database, or a full disk.",
		"Check usage.db in the data folder; the per-call tokens are also in model's diagnostic events (9002).")

// Configuration (5000s).

var Switched = Events.Event(5001, event.Info, "Swap", "Primary model switched to {provider} ({model})", "provider", "model", "backend", "endpoint").
	Keywords("config").
	Doc("The operator switched the primary model live; every agent and job uses it from the next call.",
		"/model use, or a key reload that re-applies the active provider.",
		"None.")

var ServerAdded = Events.Event(5002, event.Info, "Server", "Added {provider}: {kind} server at {endpoint} with {model}", "provider", "kind", "endpoint", "model", "auto", "tools", "format").
	Keywords("config").
	Doc("A model server on the network was checked and added as a model from the dashboard's Model page. kind is how Tenant talks to it; auto means its model is found at each start; tools and format say how the test message's tool call came back and how Tenant reads tool calls.",
		"The operator added a server on the Model page.",
		"None. It answers once the operator chooses Use this model, or gives it to a helper.")

// Diagnostics (9000s): one activity per call, with its tokens.

var Call = Events.Event(9001, event.Info, "Call", "Model call to {profile}", "profile", "backend", "model").
	Diagnostic().
	Doc("One call to a model, as an activity under its turn: start, then stop with its duration, or fail with the error.",
		"The agent, a sub-agent or a job asked the model for a completion.",
		"None. For slow or failing calls, compare durations across backend and profile.")

var CallUsage = Events.Event(9002, event.Info, "Call", "{profile} used {prompt_tokens} prompt and {completion_tokens} completion tokens", "profile", "backend", "model", "prompt_tokens", "completion_tokens").
	Diagnostic().
	Doc("The tokens one model call used, as its backend reported them.",
		"A call finished and its backend reported usage.",
		"None. Sum them by model or trace to see what a turn or a job cost.")

var VisionProbed = Events.Event(1001, event.Info, "Vision", "Model {model} at {endpoint} can see images: {answer}", "model", "endpoint", "answer").
	Doc("Tenant asked a model whether it can see images, with a two-colour test picture, and recorded the answer (TEN-493).",
		"Launch, a model switch, or the first image sent to a model whose answer wasn't known yet.",
		"None. A model that can't see gets a note with the image's saved path instead. To override the answer, use /model vision on|off.")

var VisionInconclusive = Events.Event(2012, event.Warning, "Vision", "Couldn't tell whether {model} at {endpoint} can see images: {err}", "model", "endpoint", "err").
	Doc("The vision probe got no clear answer, so images to this model go as notes for now; it tries again in five minutes (TEN-493).",
		"The server was unreachable, refused the key, rate-limited the request, failed, or the reply named no colours (a thinking model cut off).",
		"None if it settles. To decide it yourself, use /model vision on or off.")
