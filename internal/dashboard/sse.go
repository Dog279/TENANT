package dashboard

// sse.go is TEN-108: a tiny, dependency-free writer for Datastar's Server-Sent
// Event patch protocol. The official datastar-go SDK pulls in ~6 transitive
// compression deps (brotli / zstd / httpcompression / bytebufferpool / ...) we
// don't want for a local loopback panel — and the wire format is trivial, so we
// emit it directly, keeping tenant's dependency-light, single-binary discipline.
//
// Format (verified against the datastar-go v1.2.1 constants + datastar.js
// v1.0.1, which both speak `datastar-patch-elements`):
//
//	event: datastar-patch-elements
//	data: elements <html line 1>
//	data: elements <html line 2>
//	<blank line>
//
// Default mode is "outer", which keys on the element's id — so the fragment must
// carry the id of the element it replaces.

import (
	"bytes"
	"net/http"
	"strings"
)

// setSSEHeaders marks the response as an event stream. Call once, before the
// first write.
func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// patchElements builds one datastar-patch-elements SSE event. mode ("" =
// default outer; "append"/"inner"/... otherwise) and selector ("" = key on the
// element's id) are optional. The HTML is split into one `data: elements` line
// per source line, per the Datastar wire format.
func patchElements(selector, mode, elements string) []byte {
	var b bytes.Buffer
	b.WriteString("event: datastar-patch-elements\n")
	if mode != "" {
		b.WriteString("data: mode " + mode + "\n")
	}
	if selector != "" {
		b.WriteString("data: selector " + selector + "\n")
	}
	for _, line := range strings.Split(strings.TrimRight(elements, "\n"), "\n") {
		b.WriteString("data: elements ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n') // blank line terminates the event
	return b.Bytes()
}

// writeDatastarPatch sends one outer-merge patch (keys on the element's id),
// then flushes. One-shot: the caller returns afterward.
func writeDatastarPatch(w http.ResponseWriter, elements string) error {
	setSSEHeaders(w)
	_, err := w.Write(patchElements("", "", elements))
	flush(w)
	return err
}
