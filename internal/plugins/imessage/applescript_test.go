package imessage

import (
	"strings"
	"testing"
)

// The anti-injection property: the message text and recipient must travel
// in argv, never interpolated into the script source. A malicious body
// must therefore be impossible to find inside the script string.
func TestSendToChatScript_AntiInjection(t *testing.T) {
	evil := `"); tell application "Finder" to delete every item; (`
	script, argv := sendToChatScript("iMessage;-;+15551234567", evil)

	if !strings.Contains(script, "on run argv") {
		t.Errorf("script must read inputs from argv:\n%s", script)
	}
	if strings.Contains(script, evil) {
		t.Errorf("message text leaked into script source (injection risk):\n%s", script)
	}
	if strings.Contains(script, "+15551234567") {
		t.Errorf("recipient leaked into script source:\n%s", script)
	}
	if len(argv) != 2 || argv[0] != "iMessage;-;+15551234567" || argv[1] != evil {
		t.Errorf("argv wrong: %#v", argv)
	}
	if !strings.Contains(script, "chat id chatId") {
		t.Errorf("chat send must target by chat id:\n%s", script)
	}
}

func TestSendToBuddyScript_AntiInjection(t *testing.T) {
	script, argv := sendToBuddyScript("+15559999999", "hi there")
	if !strings.Contains(script, "on run argv") {
		t.Errorf("script must read inputs from argv:\n%s", script)
	}
	if strings.Contains(script, "+15559999999") || strings.Contains(script, "hi there") {
		t.Errorf("inputs leaked into script source:\n%s", script)
	}
	if len(argv) != 2 || argv[0] != "+15559999999" || argv[1] != "hi there" {
		t.Errorf("argv wrong: %#v", argv)
	}
	if !strings.Contains(script, "service type = iMessage") {
		t.Errorf("buddy send should target the iMessage account:\n%s", script)
	}
}
