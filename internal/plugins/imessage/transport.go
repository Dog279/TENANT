package imessage

import "context"

// transport is the minimal set of operations the Dispatcher needs from
// any iMessage backend. It is platform-neutral on purpose: the
// dispatcher, the tools, and the Policy gate are all written against
// this interface, so swapping the underlying connector (BlueBubbles vs
// the native chat.db + AppleScript transport) changes nothing above it.
//
// Two implementations satisfy it today:
//   - *Service       — the BlueBubbles REST client (bluebubbles.go).
//   - *nativeService  — the native macOS transport (native_darwin.go),
//     reading ~/Library/Messages/chat.db and sending via osascript.
//
// Adding this interface is purely additive: *Service already had all
// five methods, so existing behavior (and the Windows build) is
// unchanged — see the compile-time assertion below.
type transport interface {
	ListChats(ctx context.Context, limit int) ([]Chat, error)
	ChatMessages(ctx context.Context, chatGUID string, limit int) ([]Message, error)
	SearchMessages(ctx context.Context, text string, limit int) ([]Message, error)
	SendText(ctx context.Context, chatGUID, text string) (string, error)
	NewChat(ctx context.Context, address, text string) (string, error)
}

// Compile-time proof the BlueBubbles Service still satisfies the
// dispatcher's contract unchanged.
var _ transport = (*Service)(nil)
