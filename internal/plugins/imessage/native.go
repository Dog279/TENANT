package imessage

import "errors"

// This file holds the platform-neutral surface of the native transport:
// its config, its public interface, and the "macOS only" sentinel. The
// actual constructor OpenNative is platform-split — native_darwin.go does
// the read-only chat.db open and osascript send; native_other.go returns
// errMacOnly so Windows/Linux keep compiling. cmd/tenant calls only
// OpenNative + this interface, so no darwin symbol leaks into the
// cross-platform build.

// errMacOnly is returned by OpenNative on non-darwin platforms. The
// native transport reads the local Messages chat.db and sends via
// AppleScript — both are macOS-only. Use BlueBubbles (--bb-url) elsewhere.
var errMacOnly = errors.New("imessage: native iMessage transport is macOS-only — use --bb-url for BlueBubbles on this platform")

// NativeConfig configures the native chat.db + AppleScript transport.
type NativeConfig struct {
	// DBPath overrides the chat.db location. Empty ⇒
	// ~/Library/Messages/chat.db (the standard path).
	DBPath string
}

// Native is an opened native transport: the five iMessage operations
// plus Close to release the chat.db handle.
type Native interface {
	transport
	Close() error
}
