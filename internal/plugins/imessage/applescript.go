package imessage

// AppleScript send builders. These are pure functions: they return the
// script source and the argv to hand to `osascript … on run argv`. The
// message text and recipient are passed as argv, never interpolated into
// the script source — so a message body can never break out of a string
// literal or inject AppleScript (the anti-injection property the design
// requires). The actual `osascript` exec lives in native_darwin.go; the
// builders are tag-free so they can be unit-tested anywhere.

// sendToChatScript builds an AppleScript that sends `text` to an existing
// conversation addressed by its Apple "chat id" (the chat.guid we expose,
// e.g. "iMessage;-;+15551234567" for 1:1 or "iMessage;+;chatNNN" for a
// group). argv = [chatGUID, text].
func sendToChatScript(chatGUID, text string) (script string, argv []string) {
	script = `on run argv
	set chatId to item 1 of argv
	set msg to item 2 of argv
	tell application "Messages"
		set targetChat to a reference to chat id chatId
		send msg to targetChat
	end tell
end run`
	return script, []string{chatGUID, text}
}

// sendToBuddyScript builds an AppleScript that starts/uses a direct
// conversation with a buddy (phone/email) on the iMessage account and
// sends `text`. Used by NewChat. argv = [address, text].
func sendToBuddyScript(address, text string) (script string, argv []string) {
	script = `on run argv
	set addr to item 1 of argv
	set msg to item 2 of argv
	tell application "Messages"
		set targetService to 1st account whose service type = iMessage
		set targetBuddy to participant addr of targetService
		send msg to targetBuddy
	end tell
end run`
	return script, []string{address, text}
}
