package imessage

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"
	"unicode/utf8"
)

// On modern macOS (Big Sur+) the plain `message.text` column is
// frequently NULL; the real message text lives in `attributedBody`, a
// serialized NSAttributedString stored in Apple's NeXTSTEP "streamtyped"
// archive format (NOT a plist / NSKeyedArchiver). This file is a small,
// pure-Go extractor for the string payload of that archive.
//
// Why a parser and not a byte-grab: the typedstream length prefix is a
// variable-width integer. A naive "split on NSString and trim N bytes"
// heuristic miscounts whenever the length crosses the 1→2 byte boundary
// (text ≥ 128 bytes) and corrupts multi-byte UTF-8 (emoji) at the
// boundary. We read the length the way the format specifies, then decode
// the exact byte span — so emoji and long messages survive intact.
//
// On any decode failure we return unsupportedMessage rather than erroring
// the whole read: one weird row must never block listing a conversation.

const unsupportedMessage = "[unsupported message]"

// decodeAttributedBody returns the message text contained in a
// streamtyped attributedBody BLOB. It never errors: undecodable blobs
// yield unsupportedMessage so a single bad row can't fail a read.
func decodeAttributedBody(blob []byte) string {
	if s, ok := parseAttributedBody(blob); ok {
		return s
	}
	return unsupportedMessage
}

// stringClassMarkers are the length-prefixed class-name tokens that
// precede the encoded string payload, most-specific first. The text of
// an NSAttributedString is held by an NSString (UTF-8) or, for some
// content, an NSMutableString; the class name appears verbatim in the
// archive right before the string's type descriptor.
var stringClassMarkers = [][]byte{
	[]byte("NSString"),
	[]byte("NSMutableString"),
}

// parseAttributedBody extracts the text payload. Returns ("", false)
// when the blob doesn't look like a decodable streamtyped string.
//
// Layout after the class name (observed across macOS versions):
//
//	... 4E 53 53 74 72 69 6E 67   "NSString"
//	    01 95                      class version / inheritance bookkeeping
//	    84 01 2B                   type descriptor: 0x84 marker, len=1, '+'
//	    <varint length> <bytes>    the string itself
//
// The '+' type tag means "byte array"; the bytes are UTF-8 on modern
// macOS. Older/!ASCII content can be UTF-16LE — we detect that by an
// invalid-UTF-8 check and fall back to UTF-16 decoding.
func parseAttributedBody(blob []byte) (string, bool) {
	if len(blob) == 0 {
		return "", false
	}
	start := classNameEnd(blob)
	if start < 0 {
		return "", false
	}
	// Scan forward for the type-descriptor signature 0x84 0x01 <typechar>.
	// 0x84 is the typedstream "new type/object" marker; 0x01 is the length
	// of the one-character type-encoding string that follows.
	p := -1
	for i := start; i+2 < len(blob); i++ {
		if blob[i] == 0x84 && blob[i+1] == 0x01 {
			p = i + 2
			break
		}
	}
	if p < 0 || p >= len(blob) {
		return "", false
	}
	typeChar := blob[p]
	p++
	if typeChar != '+' && typeChar != '*' {
		// Unknown encoding for the string payload.
		return "", false
	}
	n, next, ok := readVarLen(blob, p)
	if !ok || n < 0 || next+n > len(blob) {
		return "", false
	}
	raw := blob[next : next+n]
	if utf8.Valid(raw) {
		return string(raw), true
	}
	if s, ok := decodeUTF16LE(raw); ok {
		return s, true
	}
	return "", false
}

// classNameEnd returns the index just past the first string-class name
// token in the blob (e.g. the byte after "NSString"). Returns -1 if no
// known string class is present.
func classNameEnd(blob []byte) int {
	best := -1
	for _, marker := range stringClassMarkers {
		if i := bytes.Index(blob, marker); i >= 0 {
			end := i + len(marker)
			if best < 0 || end < best {
				best = end
			}
		}
	}
	return best
}

// readVarLen reads a typedstream variable-width unsigned length starting
// at p. Values 0x00..0x7F are stored in a single byte. 0x81 introduces a
// little-endian uint16; 0x82 a little-endian uint32. Returns the value,
// the index just past it, and ok.
func readVarLen(b []byte, p int) (val int, next int, ok bool) {
	if p < 0 || p >= len(b) {
		return 0, p, false
	}
	switch c := b[p]; c {
	case 0x81:
		if p+3 > len(b) {
			return 0, p, false
		}
		return int(binary.LittleEndian.Uint16(b[p+1 : p+3])), p + 3, true
	case 0x82:
		if p+5 > len(b) {
			return 0, p, false
		}
		return int(binary.LittleEndian.Uint32(b[p+1 : p+5])), p + 5, true
	default:
		if c >= 0x80 {
			// 0x80 and 0x83+ are not valid length prefixes here.
			return 0, p, false
		}
		return int(c), p + 1, true
	}
}

// decodeUTF16LE decodes a little-endian UTF-16 byte slice. Used as the
// fallback when the payload bytes aren't valid UTF-8 (NSUnicodeString /
// older Unicode content).
func decodeUTF16LE(b []byte) (string, bool) {
	if len(b) == 0 || len(b)%2 != 0 {
		return "", false
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u)), true
}
