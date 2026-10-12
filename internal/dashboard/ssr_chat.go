package dashboard

// ssr_chat.go is TEN-109: real-time chat over Server-Sent Events, replacing
// the old WebSocket-driven inline JS. `GET /events` is a long-lived SSE
// stream that subscribes to the agent event broker and APPENDS HTML
// fragments to #chat-log via Datastar patches — the browser updates in real
// time with zero hand-written JS. Chat input (send / interject / stop) is stateless POSTs
// driving the Server-wide turn coordinator (session.go).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"tenant/internal/agent"
	"tenant/internal/logs/event"
	"tenant/internal/md"
	"tenant/internal/memory/working"
	"tenant/internal/uicomp"
)

// chatPageData is the Chat page: the shell data and the conversation so far.
type chatPageData struct {
	layoutData
	Transcript []template.HTML
}

func (s *Server) handleChatPage(w http.ResponseWriter, _ *http.Request) {
	s.render(w, s.tmpl.chat, chatPageData{layoutData: layoutData{Title: "Chat", Page: "chat"}, Transcript: s.transcriptHTML(20)})
}

// SetTranscript wires the conversation so far, shown when Chat opens (TEN-515).
func (s *Server) SetTranscript(fn func() []working.Message) { s.transcript = fn }

// transcriptHTML renders the last n exchanges of the conversation (TEN-515):
// each message you sent, the agent's answers as Markdown, and its tool calls
// as settled cards, so an opened or reloaded page starts where things are.
// A summary left by compaction is a dim row. nil when no source is wired.
func (s *Server) transcriptHTML(n int) []template.HTML {
	if s.transcript == nil {
		return nil
	}
	msgs := s.transcript()
	start := len(msgs)
	for users := 0; start > 0 && users < n; {
		start--
		if msgs[start].Role == "user" && msgs[start].Kind == "" {
			users++
		}
	}
	results := map[string]working.Message{}
	called := map[string]bool{} // results whose call is in the window
	for _, m := range msgs[start:] {
		switch m.Role {
		case "tool":
			if m.ToolCallID != "" {
				results[m.ToolCallID] = m
			}
		case "assistant":
			for _, tc := range m.ToolCalls {
				called[tc.ID] = true
			}
		}
	}
	var out []template.HTML
	add := func(h string) { out = append(out, template.HTML(h)) } //nolint:gosec // built by the escaping renderers below
	summarized := false
	for _, m := range msgs[start:] {
		switch {
		case m.Kind != "":
			if !summarized { // one row however many summaries the window holds
				summarized = true
				add(sysRow("Earlier messages were summarized to save room."))
			}
		case m.Role == "user":
			add(userBubble(m.Content))
		case m.Role == "assistant":
			if strings.TrimSpace(m.Content) != "" {
				add(answerBubble(m.Content, ""))
			}
			for _, tc := range m.ToolCalls {
				r, ok := results[tc.ID]
				add(toolCard(toolCardData{id: tc.ID, name: tc.Name, args: string(tc.Arguments), done: ok, result: r.Content}))
			}
		case m.Role == "tool" && !called[m.ToolCallID]:
			// Its call was compacted away: the result still shows, on its own.
			add(toolCard(toolCardData{id: m.ToolCallID, name: "tool", result: m.Content, done: true}))
		}
	}
	return out
}

// handleEventsSSE streams agent events to the browser until the request context
// is canceled (tab closed / navigation away). Conversation-relevant kinds
// append to the chat transcript.
//
// It also keeps #approval-queue current (TEN-207): a snapshot as soon as it
// has subscribed (so nothing falls between the two, a reconnect resyncs, and
// the paused banner clears), then a re-render from Pending() on every approval
// event and every queueResync. ?only=approvals sends nothing else: the Safety
// page doesn't take the chat.
func (s *Server) handleEventsSSE(w http.ResponseWriter, r *http.Request) {
	setSSEHeaders(w)
	flush(w) // open the stream immediately
	ctx, act := event.Start(r.Context(), Stream, "stream", "events", "remote", r.RemoteAddr)
	defer act.End(nil)
	ch, unsub := s.broker.Subscribe()
	defer unsub()
	onlyApprovals := r.URL.Query().Get("only") == "approvals"
	if !s.patchApprovals(ctx, w) {
		return
	}
	resync := time.NewTicker(queueResync)
	defer resync.Stop()
	lagged := false
	// The model call in flight shows as a "thinking…" row that counts up each
	// tick, so a slow model reads apart from a hung one (TEN-342). think is
	// its start; zero when none is.
	var think time.Time
	tick := time.NewTicker(thinkingEvery)
	defer tick.Stop()
	// The answer streams into one bubble (TEN-515): tokens gather here and
	// go out at most every streamEvery, so a fast model can't flood the page
	// or fill the broker's buffer behind a slow browser.
	cs := newChatStream()
	stream := time.NewTicker(streamEvery)
	defer stream.Stop()
	if !onlyApprovals {
		// A reconnect opens a fresh stream over a page that may still hold the
		// previous connection's live elements; clear them before anything new.
		for _, p := range cs.start() {
			if _, err := w.Write(p); err != nil {
				return
			}
		}
		flush(w)
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-resync.C:
			if !s.patchApprovals(ctx, w) {
				return
			}
		case <-tick.C:
			if think.IsZero() {
				continue
			}
			if _, err := w.Write(patchElements("", "", thinkingRow(time.Since(think)))); err != nil {
				return
			}
			flush(w)
		case <-stream.C:
			if p := cs.flush(); p != nil {
				if _, err := w.Write(p); err != nil {
					return
				}
				flush(w)
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			behind(ctx, ch, "events", r.RemoteAddr, &lagged)
			if ev.Kind == agent.EventApprovalPending || ev.Kind == agent.EventApprovalResolved {
				if !s.patchApprovals(ctx, w) {
					return
				}
			}
			if onlyApprovals {
				continue
			}
			switch {
			case ev.Agent != "": // another agent's call, not the chat's
			case ev.Kind == agent.EventThinking:
				if think.IsZero() {
					if _, err := w.Write(patchElements("#chat-log", "append", thinkingRow(0))); err != nil {
						return
					}
				}
				think = time.Now()
			case (ev.Kind == agent.EventThought || ev.Kind == agent.EventTurnEnd) && !think.IsZero():
				think = time.Time{}
				if _, err := w.Write(patchElements("#chat-thinking", "remove", "")); err != nil {
					return
				}
			}
			for _, p := range cs.patches(ev) {
				if _, err := w.Write(p); err != nil {
					return
				}
			}
			flush(w)
		}
	}
}

// streamEvery is how often the answer being written is sent to the page.
var streamEvery = 100 * time.Millisecond

// chatStream is one connection's view of the answer being written and the
// tool calls in flight (TEN-515).
type chatStream struct {
	cur     strings.Builder     // the answer so far
	open    bool                // #chat-cur is on the page
	dirty   bool                // tokens arrived since the last send
	n       int                 // ids for calls the model gave none
	pending map[string][]string // tool name → ids of its open cards, oldest first
	// Card ids are tag-turn-id: tag is this connection's, turn counts the
	// turns it has seen, so a model that reuses call ids across turns, or a
	// reconnect that opens a fresh stream over a page that still holds the
	// old cards, never has two cards with one id. sent is the cards this
	// stream put on the page, so a result whose call it never saw (the broker
	// dropped it, or the page opened mid-turn) is appended, not lost.
	tag  string
	turn int
	sent map[string]bool
}

// newChatStream starts a stream's state with its own tag.
func newChatStream() *chatStream {
	return &chatStream{tag: strings.ToLower(rand.Text()[:8]), sent: map[string]bool{}}
}

// start is the sweep a stream sends as it opens: the answer an earlier
// connection was writing, its thinking row and its cards still marked running
// (each a no-op when absent), so a reconnect never leaves a frozen bubble, a
// second thinking row or a spinner that nothing will settle. The results that
// follow still show: a result whose card this stream never sent is appended
// on its own.
func (cs *chatStream) start() [][]byte {
	return [][]byte{
		patchElements("#chat-cur", "remove", ""),
		patchElements("#chat-thinking", "remove", ""),
		patchElements("details.tool.running", "remove", ""),
	}
}

// cardID is the page id of the call id's card in this stream.
func (cs *chatStream) cardID(id string) string {
	return cs.tag + "-" + strconv.Itoa(cs.turn) + "-" + id
}

// patches turns one event into the SSE patches it means for the chat.
func (cs *chatStream) patches(ev agent.Event) [][]byte {
	if ev.Agent != "" || ev.Kind == agent.EventBus {
		return nil
	}
	switch ev.Kind {
	case agent.EventTurnStart:
		cs.turn++
		cs.sent = map[string]bool{} // a result answers a call of its own turn
		return nil
	case agent.EventToken:
		cs.cur.WriteString(ev.Text)
		if !cs.open {
			cs.open = true
			return [][]byte{patchElements("#chat-log", "append", streamingBubble(cs.cur.String()))}
		}
		cs.dirty = true
		return nil
	case agent.EventFinal, agent.EventTruncated, agent.EventStuck:
		text := ev.Text
		if strings.TrimSpace(text) == "" {
			text = cs.cur.String()
		}
		cs.cur.Reset()
		cs.dirty = false
		if strings.TrimSpace(text) == "" {
			if cs.open {
				cs.open = false
				return [][]byte{patchElements("#chat-cur", "remove", "")}
			}
			return nil
		}
		if cs.open {
			cs.open = false
			return [][]byte{patchElements("#chat-cur", "replace", answerBubble(text, ""))}
		}
		return [][]byte{patchElements("#chat-log", "append", answerBubble(text, ""))}
	case agent.EventToolCall:
		id := ev.ToolCallID
		if id == "" {
			cs.n++
			id = fmt.Sprintf("n%d", cs.n)
			if cs.pending == nil {
				cs.pending = map[string][]string{}
			}
			cs.pending[ev.Tool] = append(cs.pending[ev.Tool], id)
		}
		// Text written before the call stays as its own settled bubble, so the
		// card sits where the call happened.
		var out [][]byte
		if cs.open {
			cs.open, cs.dirty = false, false
			out = append(out, patchElements("#chat-cur", "replace", answerBubble(cs.cur.String(), "")))
			cs.cur.Reset()
		}
		cid := cs.cardID(id)
		cs.sent[cid] = true
		return append(out, patchElements("#chat-log", "append", toolCard(toolCardData{id: cid, name: ev.Tool, args: ev.Args, trace: ev.TraceID})))
	case agent.EventToolResult:
		id := ev.ToolCallID
		if id == "" {
			if ids := cs.pending[ev.Tool]; len(ids) > 0 {
				id, cs.pending[ev.Tool] = ids[0], ids[1:]
			}
		}
		if id == "" { // no card to settle: the result stands alone
			return [][]byte{patchElements("#chat-log", "append", toolCard(toolCardData{name: ev.Tool, args: ev.Args, result: ev.Result, dur: ev.Duration, done: true, isErr: ev.IsErr, trace: ev.TraceID}))}
		}
		cid := cs.cardID(id)
		card := toolCard(toolCardData{id: cid, name: ev.Tool, args: ev.Args, result: ev.Result, dur: ev.Duration, done: true, isErr: ev.IsErr, trace: ev.TraceID})
		if cs.sent[cid] {
			return [][]byte{patchElements("", "", card)}
		}
		// This stream never put the call's card on the page (the broker
		// dropped the call, or the page opened mid-turn): the settled card
		// stands alone. start() cleared any running card a previous
		// connection left.
		return [][]byte{patchElements("#chat-log", "append", card)}
	}
	if b := chatBubble(ev); b != "" {
		return [][]byte{patchElements("#chat-log", "append", b)}
	}
	return nil
}

// flush is the streaming bubble with the tokens that arrived since the last
// send; nil when nothing did.
func (cs *chatStream) flush() []byte {
	if !cs.open || !cs.dirty {
		return nil
	}
	cs.dirty = false
	return patchElements("", "", streamingBubble(cs.cur.String()))
}

// behind reports, once per stream, a broker subscriber that fell behind: its
// buffer filled, and the broker drops events for a full subscriber rather
// than stall the turn.
// ponytail: a full buffer, not a count of what was dropped; count drops in
// agent.Broker if the figure matters.
func behind(ctx context.Context, ch <-chan agent.Event, stream, remote string, reported *bool) {
	if !*reported && len(ch) >= cap(ch)-1 {
		*reported = true
		StreamLagging.Log(ctx, "stream", stream, "remote", remote)
	}
}

// chatText reads the message from either a Datastar JSON signals body or a plain
// form POST, so the handler is robust to how the request was encoded.
func chatText(r *http.Request) string {
	if strings.Contains(r.Header.Get("Content-Type"), "json") {
		var sig struct {
			Text string `json:"text"`
		}
		if json.NewDecoder(r.Body).Decode(&sig) == nil && strings.TrimSpace(sig.Text) != "" {
			return strings.TrimSpace(sig.Text)
		}
	}
	return strings.TrimSpace(r.FormValue("text"))
}

// clearChatBox empties the composer (its data-bind:text signal) once a message
// is sent. A fixed event: no request text rides in a signals patch.
const clearChatBox = "event: datastar-patch-signals\ndata: signals {\"text\":\"\"}\n\n"

// handleChatSend appends the operator's message to the transcript and starts a
// background turn; the turn's events stream back over /events. The dashboard
// runs one chat at a time (TEN-211): a second send is refused with a reason
// and no bubble, and its text stays in the box. Both answers refresh the
// status strip.
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	text := chatText(r)
	if text == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	setSSEHeaders(w)
	switch {
	case s.agent == nil:
		_, _ = w.Write(patchElements("#chat-log", "append",
			`<div class="msg sys"><div class="dim">Chat isn't available on this Tenant.</div></div>`))
	case s.coord.startBackground(text):
		_, _ = w.Write(patchElements("#chat-log", "append", userBubble(text)))
		_, _ = w.Write([]byte(clearChatBox))
		s.writeStatusPatch(w)
	default:
		_, _ = w.Write(patchElements("#chat-log", "append",
			`<div class="msg sys"><div class="dim">Not sent: your agent is still answering. Your message is still in the box — send it again when it's done, or press Stop.</div></div>`))
		s.writeStatusPatch(w)
	}
	flush(w)
}

// writeStatusPatch adds a #ui-status patch to an SSE answer (none on a render
// error: the strip's own poll catches up).
func (s *Server) writeStatusPatch(w http.ResponseWriter) {
	if part, err := s.uiPart("ui-status", s.statusView()); err == nil {
		_, _ = w.Write(patchElements("", "", part))
	}
}

func (s *Server) handleChatInterject(w http.ResponseWriter, r *http.Request) {
	if t := chatText(r); t != "" {
		s.coord.interject(t)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChatStop(w http.ResponseWriter, _ *http.Request) {
	s.coord.stopActive()
	w.WriteHeader(http.StatusNoContent)
}

// --- fragment renderers (every dynamic value is html-escaped) ---

// chatBubble renders one event as a chat row on its own, without stream
// state: the answer as Markdown, a tool call as a card, the thinking and
// timing rows; "" for kinds that don't belong in the transcript. The live
// stream goes through chatStream, which adds the streaming bubble and settles
// cards in place; this is what a transcript or a one-off patch uses.
func chatBubble(ev agent.Event) string {
	// Cross-agent activity + bus traffic stay out of the operator's
	// conversation transcript (TEN-234).
	if ev.Agent != "" || ev.Kind == agent.EventBus {
		return ""
	}
	switch string(ev.Kind) {
	case "final", "truncated", "stuck":
		if strings.TrimSpace(ev.Text) == "" {
			return ""
		}
		return answerBubble(ev.Text, "")
	case "tool_call":
		return toolCard(toolCardData{id: ev.ToolCallID, name: ev.Tool, args: ev.Args, trace: ev.TraceID})
	case "tool_result":
		return toolCard(toolCardData{id: ev.ToolCallID, name: ev.Tool, args: ev.Args, result: ev.Result, dur: ev.Duration, done: true, isErr: ev.IsErr, trace: ev.TraceID})
	case "thought":
		t := "thought for " + ev.Duration.Round(tenth).String()
		if ev.IsErr {
			t += " (the call failed)"
		}
		return sysRow(t)
	case "turn_end":
		if ev.Think == 0 && ev.Tools == 0 {
			return ""
		}
		return sysRow("turn " + ev.Duration.Round(tenth).String() + " — thinking " + ev.Think.Round(tenth).String() + " · tools " + ev.Tools.Round(tenth).String())
	case "error":
		return sysRow("⚠ " + snippetStr(ev.Text, 200))
	case "notice":
		return sysRow(snippetStr(ev.Text, 200))
	}
	return ""
}

// sysRow is a dim centred line between messages.
func sysRow(text string) string {
	return `<div class="msg sys"><div class="dim">` + html.EscapeString(text) + `</div></div>`
}

// userBubble is a message the operator sent.
func userBubble(text string) string {
	return `<div class="msg user"><div class="bubble">` + html.EscapeString(visibleText(text)) + `</div></div>`
}

// answerBubble is the agent's answer rendered from Markdown (TEN-515):
// headings, lists, code, tables and task boxes; HTML inside the text is text
// (internal/md has no raw-HTML parser) and script links are dropped. id is
// the element's id while it is the answer being written, "" once settled.
func answerBubble(text, id string) string {
	idAttr := ""
	if id != "" {
		idAttr = ` id="` + html.EscapeString(id) + `"`
	}
	return `<div class="msg a"` + idAttr + `><div class="av">t</div><div class="bubble md">` + string(md.HTML(visibleText(text))) + `</div></div>`
}

// streamingBubble is the answer so far, as plain text: Markdown renders once
// the answer is complete, so a half-written list or fence never flickers.
func streamingBubble(text string) string {
	return `<div class="msg a" id="chat-cur"><div class="av">t</div><div class="bubble">` + html.EscapeString(visibleText(text)) + `</div></div>`
}

// toolCardData is one tool call as the chat shows it.
type toolCardData struct {
	id, name, args, result, trace string
	dur                           time.Duration
	done, isErr                   bool
}

// toolCard is a tool call as a native <details> card (TEN-515): the summary
// line is the name, a short argument and a status pill; the body is what was
// asked and what came back, capped, with a link to the Logs page filtered by
// the message's trace. A failed call is open; the rest open on a tap.
func toolCard(d toolCardData) string {
	if h, ok := componentCard(d); ok {
		return h
	}
	var b strings.Builder
	b.WriteString(`<details class="tool`)
	if !d.done {
		b.WriteString(` running`) // what a fresh stream's sweep removes
	}
	b.WriteString(`"`)
	if d.id != "" {
		b.WriteString(` id="tool-` + html.EscapeString(d.id) + `"`)
	}
	if d.isErr {
		b.WriteString(" open")
	}
	b.WriteString(`><summary>⚡ ` + html.EscapeString(d.name))
	if a := snippetStr(d.args, 80); a != "" {
		b.WriteString(` <span class="dim">` + html.EscapeString(a) + `</span>`)
	}
	took := ""
	if d.dur > 0 {
		took = " " + d.dur.Round(tenth).String()
	}
	switch {
	case !d.done:
		b.WriteString(` <span class="pill run">running…</span>`)
	case d.isErr:
		b.WriteString(` <span class="pill err">error` + took + `</span>`)
	default:
		b.WriteString(` <span class="pill ok">ok` + took + `</span>`)
	}
	b.WriteString(`</summary><div class="tool-body">`)
	if a := strings.TrimSpace(d.args); a != "" {
		b.WriteString(`<div class="lbl">asked</div><pre class="tool-io">` + html.EscapeString(visibleText(capText(a, 2000))) + `</pre>`)
	}
	if d.done {
		label := "got"
		if d.isErr {
			label = "error"
		}
		b.WriteString(`<div class="lbl">` + label + `</div><pre class="tool-io">` + html.EscapeString(visibleText(capText(d.result, 4000))) + `</pre>`)
	}
	if d.trace != "" {
		b.WriteString(`<a class="dim" href="` + html.EscapeString(logsHref(filterTerm("trace", d.trace))) + `">open in Logs</a>`)
	}
	b.WriteString(`</div></details>`)
	return b.String()
}

// componentCard draws a ui_render call as the component it names (TEN-517)
// in place of the tool card: the arguments are the component, so it is drawn
// once, at the call, and the same way from the transcript. It keeps the
// card's id, so the settling patch morphs it in place. A call whose
// arguments do not validate (or that failed) falls back to the ordinary
// card, which shows the error the model got.
func componentCard(d toolCardData) (string, bool) {
	if d.name != uicomp.ToolName || d.isErr {
		return "", false
	}
	c, _, err := uicomp.Parse([]byte(d.args))
	if err != nil {
		return "", false
	}
	var b strings.Builder
	b.WriteString(`<div class="msg comp-wrap"`)
	if d.id != "" {
		b.WriteString(` id="tool-` + html.EscapeString(d.id) + `"`)
	}
	b.WriteString(`>` + uicomp.HTML(c, visibleText) + `</div>`)
	return b.String(), true
}

// capText is s cut to at most n bytes on a character boundary, lines kept.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// thinkingEvery is how often the chat's thinking row counts up.
var thinkingEvery = time.Second

// tenth is how finely the chat's timing lines round ("8.2s", "4m12s").
const tenth = 100 * time.Millisecond

// thinkingRow is the chat's live row for the model call in flight, d into it.
// Its id keys each tick's patch to it, and the call's end removes it.
func thinkingRow(d time.Duration) string {
	t := "thinking…"
	if d >= time.Second {
		t += " " + shortDur(d)
	}
	return `<div class="msg sys" id="chat-thinking"><div class="dim">` + t + `</div></div>`
}

// snippetStr is s on one line, cut to at most n bytes on a character
// boundary, with its hidden characters shown (visibleText, F10): every row
// of the chat transcript goes through it, so a command can't display
// reversed there beside its approval card (REVSEC-8).
func snippetStr(s string, n int) string {
	s = strings.Join(strings.Fields(visibleText(s)), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
