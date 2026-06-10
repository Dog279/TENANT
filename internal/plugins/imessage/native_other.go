//go:build !darwin

package imessage

// OpenNative is unavailable off macOS: the native transport reads the
// local Messages chat.db and sends via osascript, neither of which exists
// on Windows/Linux. Returning errMacOnly (rather than omitting the
// symbol) keeps cmd/tenant's transport-selection code compiling on every
// platform — the Windows build is byte-for-byte unaffected.
func OpenNative(_ NativeConfig) (Native, error) {
	return nil, errMacOnly
}
