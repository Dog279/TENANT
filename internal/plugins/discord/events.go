package discord

import "tenant/internal/logs/event"

// Discord is the relay's event provider (docs/EVENTS.md): the gateway
// connection, messages in and out, and the approval cards' delivery. Its log
// is v1's discord folder. Message text is personal: a sensitive field, never
// in a template.
var Discord = event.NewProvider("discord", "discord")

// The gateway connection: lifecycle (1–999) and its warnings (2000s).
var Connection = Discord.Event(1, event.Info, "Connection", "Discord gateway connection").
	Doc("The relay's connection to the Discord gateway, from the relay starting to it stopping. Drops in between are resumed or re-identified inside it; it fails only when Discord refuses the bot for good.",
		"Start: the relay was turned on. Stop: it was turned off, its token was swapped, or Tenant stopped. Fail: Discord closed with a fatal code (4004 bad token, 4013/4014 intents the bot may not use).",
		"On a fail, check the bot token (/skill configure discord) and that the Message Content intent is enabled in the Discord Developer Portal, then turn the relay on again (/relay on).")

var SessionReady = Discord.Event(2, event.Info, "Connection", "Gateway session ready ({mode})", "mode", "bot").
	Doc("The gateway accepted the bot: a new session (identified) or the previous one picked up where it left off (resumed).",
		"The relay connected, or reconnected after a drop.",
		"None. After a drop, a resumed session missed no messages; an identified one may have.")

var ReconnectAsked = Discord.Event(3, event.Info, "Connection", "Discord asked the gateway to reconnect; {next}", "next").
	Doc("Discord asked the relay to reconnect (opcode 7), and it did.",
		"Routine: Discord moves sessions between servers.",
		"None.")

var SessionEnded = Discord.Event(2001, event.Warning, "Connection", "Gateway session ended; {next}: {err}", "next", "err").
	Doc("A gateway session ended unexpectedly, and the relay is reconnecting: resuming the same session, or identifying a new one.",
		"The network dropped, Discord closed the connection, a RESUME or IDENTIFY could not be sent, Discord invalidated the session, or a heartbeat went unanswered.",
		"None when it is rare: the relay reconnects by itself with backoff. If it repeats, check the host's network; a new session (re-identifying) may have missed messages sent meanwhile.")

var ConnectFailed = Discord.Event(2002, event.Warning, "Connection", "Gateway could not connect ({step}); retrying: {err}", "step", "err").
	Doc("The relay could not reach the Discord gateway, and retries with backoff (up to a minute apart).",
		"The host is offline, DNS or a proxy fails, or Discord is down; fetching the gateway URL also fails with a revoked bot token.",
		"Check the host's network and discordstatus.com. If the error is 401, the bot token was revoked: configure a new one (/skill configure discord).")

var HeartbeatFailed = Discord.Event(2003, event.Warning, "Connection", "Gateway heartbeat failed: {err}", "err").
	Doc("A heartbeat could not be sent to the gateway. When the timed heartbeat fails, the relay drops the connection and reconnects.",
		"The connection broke under the relay: a network drop or a closed socket.",
		"None when it is rare; the session-ended event that follows says how the relay reconnects.")

var HeartbeatUnacked = Discord.Event(2004, event.Warning, "Connection", "Gateway heartbeat not acknowledged; dropping the connection").
	Doc("Discord did not acknowledge the last heartbeat before the next was due, so the connection is a zombie: the relay closes it and reconnects.",
		"The connection stalled: a half-open socket, a sleeping host, or network loss that the socket has not noticed yet.",
		"None when it is rare. If it repeats, check for the host sleeping or a flaky network.")

var Undecodable = Discord.Event(2005, event.Warning, "Connection", "Undecodable {payload} from the gateway; {effect}: {err}", "payload", "effect", "err").
	Doc("The gateway sent a payload the relay could not decode. The effect says what happened instead: a message or click dropped, a session that cannot resume.",
		"Discord changed a payload's shape, or the frame was corrupted.",
		"If it repeats, report it with the error: Tenant's gateway client needs updating.")

// Messages (1000s) and their warnings (2100s).
var Received = Discord.Event(1001, event.Info, "Message", "Message {message} from {author} in channel {channel}",
	"message", "channel", "guild", "author", "bytes", "attachments", "text").Sensitive("text").
	Doc("The relay accepted a message: a DM from the operator, or a server message that mentions the bot.",
		"Someone wrote to the bot.",
		"None. Messages from anyone else, and server messages that don't mention the bot, are ignored without an event.")

var RelayTurn = Discord.Event(1002, event.Info, "Relay turn", "Relay turn in channel {channel} (approvals: {approvals})", "channel", "approvals").
	Doc("A turn of the Discord agent, from the message that started it until its answer is ready. Approvals says where its gated actions ask: the broker (relay.permissions), the approver (buttons) or none.",
		"The operator or a server member asked the bot something. A fail is a turn that ended with an error, which the relay also replies in Discord; a stopped turn is not a fail.",
		"On a fail, read the error and the turns log for the same trace.")

var Delivery = Discord.Event(1003, event.Info, "Delivery", "Message to channel {channel}", "channel", "bytes").
	Doc("The bot posting one message to a channel or DM: a relay answer, a notice, an operator DM or an agent's discord_send.",
		"A fail is a message Discord did not accept: the bot lacks access to the channel (50001), the message is empty or too long, Discord rate-limited it, or the network failed.",
		"On a fail, check the error: grant the bot access to the channel, or check the host's network. The message was not sent and is not retried.")

var CaughtUp = Discord.Event(1004, event.Info, "Message", "Caught up {n} message(s) missed in channel {channel}", "channel", "n").
	Doc("After the gateway session was identified anew (a restart, or a drop that could not resume), the relay read the channel back and routed the DMs it had missed, oldest first, as if they had just arrived (TEN-499).",
		"The operator wrote while Tenant was down or disconnected.",
		"None. Each message gets its answer; a message that carried a file is routed without the file.")

var Throttled = Discord.Event(2101, event.Warning, "Message", "Messages from {author} are coming too fast; told them to slow down", "author", "channel").
	Doc("The relay dropped a message because its sender is over the relay's rate limit (about 6 messages in 30 seconds).",
		"Someone is sending messages faster than the limit: a burst, a paste split into many messages, or a flood in a server.",
		"Wait and send again. If it is a flood from a server member, remove the bot from that server or restrict who can mention it.")

var RateLimited = Discord.Event(2102, event.Warning, "Delivery", "Discord rate-limited a {method} on {resource}; retry after {retry_after}", "method", "resource", "retry_after").
	Doc("Discord refused a request with 429 Too Many Requests. The request is not retried; the event after it says what failed.",
		"The bot sent too many requests in a short time: a long answer in many chunks, a burst of approval cards, or another program using the same bot token.",
		"None when it is rare. If it repeats, make sure no other program uses this bot's token.")

var AttachmentFailed = Discord.Event(2103, event.Warning, "Message", "An attachment could not be downloaded: {err}", "file", "err").Sensitive("file").
	Doc("A file the operator sent could not be saved to the relay inbox; the turn goes ahead without it and the operator is told.",
		"Discord's CDN link expired or failed, the file is bigger than it claimed, or the inbox folder could not be written.",
		"Send the file again. If it repeats, check free disk space and the permissions of <data>/relay-inbox.")

var CatchUpFailed = Discord.Event(2105, event.Warning, "Message", "Messages missed in channel {channel} could not be fetched: {err}", "channel", "err").
	Doc("After a new gateway session the relay could not read a DM channel back, so a message sent while Tenant was down may stay unanswered (TEN-499).",
		"Discord refused or did not answer the read within 10 seconds: the network, a rate limit, or the bot lost access to the channel.",
		"Check the host's network, or send the message again. The next new session tries again from the same cursor.")

var InboxSweepIncomplete = Discord.Event(2104, event.Warning, "Relay inbox", "The relay inbox retention sweep is incomplete: {err}", "dir", "failures", "err").
	Doc("Files in the relay inbox past their retention (privacy.retention_days.relay_inbox) could not all be deleted, so they are kept longer than configured.",
		"The inbox folder or a file in it could not be read or deleted: permissions, a file held open, or a disk error.",
		"Check the permissions of <data>/relay-inbox and delete the old files by hand if needed. The sweep runs again with the first attachment after the relay restarts.")

// The approval cards' delivery (1200s and 2200s). The decisions themselves
// are the Security log's.
var CardPosted = Discord.Event(1201, event.Info, "Approval card", "Approval card {message} posted in channel {channel} ({layout})", "message", "channel", "layout", "action").
	Doc("An approval card for a gated action reached Discord, in the v2 (Components) or legacy layout.",
		"A Discord-driven turn reached an action whose category asks for approval.",
		"Approve or deny it on the card.")

var CardFallback = Discord.Event(2201, event.Warning, "Approval card", "Approval card fell back to {fallback} in channel {channel}: {err}", "fallback", "channel", "err").
	Doc("Discord rejected an approval card, so the relay posted it in a simpler form: legacy buttons, or a text prompt the operator answers with APPROVE-<nonce> or deny.",
		"Discord did not accept the card's layout (an older API surface, a missing permission), or could not be reached.",
		"None when it is rare; the prompt still reached the operator. If every card falls back, set relay.card_style to legacy; if the text prompt fails too, the action is denied when the card times out.")

var CardNoChannel = Discord.Event(2202, event.Warning, "Approval card", "Approval for {action} denied: no operator channel to post the card in", "action").
	Doc("A gated action was denied without asking, because the relay had no channel to ask the operator in.",
		"The action came from a Discord-driven turn after the relay lost its channel, or the relay is not fully configured.",
		"Turn the relay on (/relay on) with the operator set (/relay allow <id>), and ask again from Discord.")

var CardEditFailed = Discord.Event(2203, event.Warning, "Approval card", "Approval card {message} could not be updated to {verdict}: {err}", "message", "verdict", "err").
	Doc("An approval card was resolved, but its message in Discord still shows live buttons. The verdict applies anyway; a later click only gets a \"resolved\" reply.",
		"Discord refused the edit: the message was deleted, the bot lost access to the channel, or the network failed.",
		"None: the decision stands. Delete the stale card if it confuses.")

var AckFailed = Discord.Event(2204, event.Warning, "Approval card", "A button click ({click}) could not be acknowledged: {err}", "click", "err").
	Doc("Discord was not told that a button click was handled, so the clicker sees \"This interaction failed\". For a verdict click, the verdict still applies.",
		"The acknowledgement missed Discord's 3-second window (a slow network or a busy host), or Discord rejected it.",
		"None when it is rare. Check the card or the relay's reply to see what was decided.")
