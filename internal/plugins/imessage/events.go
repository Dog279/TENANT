package imessage

import "tenant/internal/logs/event"

// IMessage is the iMessage responder's event provider (docs/EVENTS.md). Its
// log is v1's imessage folder. Handles, chats and message text are personal:
// sensitive fields, never in a template.
var IMessage = event.NewProvider("imessage", "imessage")

// Responder is the responder's run, an activity from /imessage on (or serve
// starting with it on) until it is turned off or Tenant stops.
var Responder = IMessage.Event(1, event.Info, "Responder", "iMessage responder, polling every {interval}", "interval").
	Doc("The iMessage responder: it polls Messages for new texts and answers them with the iMessage agent.",
		"Start: /imessage on, or Tenant started with it on. Stop: /imessage off, or Tenant stopped.",
		"None.")

var Received = IMessage.Event(1001, event.Info, "Message", "Message {row} received ({bytes} bytes)", "row", "bytes", "from", "chat", "text").
	Sensitive("from", "chat", "text").
	Doc("The responder picked up a text that may drive the agent: an approval reply, or a message it will answer.",
		"Someone on the allowlist (/imessage allow), or the operator, texted this Mac 1:1 over iMessage while the responder answers.",
		"None. Every other text is recorded as observed, and this Mac's own are skipped without an event.")

// Observed is a text the responder read but will not answer (TEN-485).
var Observed = IMessage.Event(1004, event.Info, "Observed", "Message {row} observed ({reason})", "row", "reason", "from", "name", "chat", "text").
	Sensitive("from", "name", "chat", "text").
	Doc("The responder read a text it will not answer. name is the sender's name in Contacts, when there is one. reason is passive (the responder only reads), not_allowed (the sender is not on the allowlist), group (group chats never drive) or not_imessage (SMS or RCS).",
		"Anyone texted this Mac. Log alerts on the imessage log can act on it.",
		"None. To let a sender drive the agent, /imessage allow them and /imessage respond.")

var Turn = IMessage.Event(1002, event.Info, "Turn", "iMessage turn", "chat").Sensitive("chat").
	Doc("A turn of the iMessage agent, from the text that started it until its answer is ready. A fail is a turn that ended with an error; the texter is told to try again. A stopped responder's turn is not a fail.",
		"An allowed handle texted a message that is not an approval reply.",
		"On a fail, read the error and the turns log for the same trace.")

var Delivery = IMessage.Event(1003, event.Info, "Delivery", "iMessage of {bytes} bytes", "bytes", "chat").Sensitive("chat").
	Doc("The responder sending one iMessage: an answer, a busy or unavailable notice, or an approval prompt.",
		"A fail is a text Messages did not send: osascript failed, Messages is not signed in, or Tenant lacks the Automation permission for Messages.",
		"On a fail, open Messages and check it is signed in, and allow Tenant (or its terminal) under System Settings > Privacy & Security > Automation. The text is not retried.")

var PollFailed = IMessage.Event(2001, event.Warning, "Poll", "The responder cannot read new messages: {err}", "err").
	Doc("Polling Messages for new texts failed, so the responder is not answering. It keeps polling every few seconds and records this again only when the error changes.",
		"Tenant lacks Full Disk Access (needed to read ~/Library/Messages/chat.db), the database is locked or moved, or the cursor could not be saved.",
		"Grant Full Disk Access to Tenant (or its terminal) under System Settings > Privacy & Security, then /imessage off and on.")

// Poll is one poll of Messages, every few seconds while the responder runs:
// diagnostic, for timing and the error of each failed poll.
var Poll = IMessage.Event(9001, event.Verbose, "Poll", "Poll for new iMessages").Diagnostic().
	Doc("One poll of Messages for new texts, with how long it took.",
		"The responder polls every few seconds while it is on.",
		"None. A failed poll also records the responder's poll warning, once per error.")
