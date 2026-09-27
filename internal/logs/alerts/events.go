package alerts

import "tenant/internal/logs/event"

// Events is the alerts provider, writing the alerts log: every firing of a
// rule, so alerts can be found (log:alerts) and audited, and what kept a
// rule or an action from working. It has a log of its own, not logbook's,
// because the built-in rules read logbook's events: the evaluator skips
// log:alerts, so an alert never sets off another.
var Events = event.NewProvider("alerts", "alerts")

var Fired = Events.Event(1001, event.Warning, "Alert", "Alert {rule}: {source} {source_level}: {message}", "rule", "action", "seq", "source", "source_level", "message", "count").
	Doc("An alert rule matched an event and fired: its banner went to the status feed, and a discord, imessage or cron action was sent (alerts 3001 when it could not be). seq is the event's number, source its provider and ID; count is how many matched within a threshold's window.",
		"The event matched the rule's filter, as often as its threshold asks, and the rule had not fired within its throttle window.",
		"Open the event on the dashboard (/logs/event/<seq>): its Help tab says what it means. If the rule is noise, change or disable it in logs.alerts.")

var ActionFailed = Events.Event(3001, event.Error, "Alert", "Alert {rule} was not sent by {action}: {err}", "rule", "action", "seq", "err").
	Doc("An alert fired, but its Discord, iMessage or cron action failed. The alert itself is still in the alerts log.",
		"The Discord relay has no bot or operator, imessage.operator is unset or Messages can't send from this process, or no cron scheduler runs in the process that evaluates the rules.",
		"Fix what the error names, or point the rule at another action in logs.alerts.")

var RuleInvalid = Events.Event(3002, event.Error, "Rule", "Alert rule {rule} is skipped: {err}", "rule", "err").
	Doc("An entry of logs.alerts could not be used, so it never fires. The other rules still run.",
		"Its filter does not parse, a duration or the action is not valid, or two entries share a name.",
		"Correct the entry in logs.alerts in config.json; the next evaluation picks it up.")
