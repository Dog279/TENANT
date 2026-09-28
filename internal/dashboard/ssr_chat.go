package dashboard

// ssr_chat.go is TEN-109: real-time chat + activity over Server-Sent Events,
// replacing the old WebSocket-driven inline JS. `GET /events` is a
// long-lived SSE stream that subscribes to the agent event broker and APPENDS
// HTML fragments into the live containers (#chat-log on Chat, #activity-feed on
// Activity) via Datastar patches — the browser updates in real time with zero
// hand-written JS. Chat input (send / interject / stop) is stateless POSTs
// driving the Server-wide turn coordinator (session.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"tenant/internal/agent"
	"tenant/internal/logs/event"
)

func (s *Server) handleChatPage(w http.ResponseWriter, _ *http.Request) {
	s.render(w, s.tmpl.chat, layoutData{Title: "Chat", Page: "chat"})
}

// handleEventsSSE streams agent events to the browser until the request context
// is canceled (tab closed / navigation away). Every event appends to the
// activity feed; conversation-relevant kinds also append to the chat transcript.
// One stream serves both pages — a patch whose selector isn't on the current
// page is simply a no-op there.
//
// It also keeps #approval-queue current (TEN-207): a snapshot as soon as it
// has subscribed (so nothing falls between the two, a reconnect resyncs, and
// the paused banner clears), then a re-render from Pending() on every approval
// event and every queueResync. ?only=approvals sends nothing else: the Safety
// page on a phone doesn't take the activity stream.
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
	for {
		select {
		case <-r.Context().Done():
			return
		case <-resync.C:
			if !s.patchApprovals(ctx, w) {
				return
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
			// Activity feed = meaningful activity only (tool calls, lifecycle,
			// cross-agent + bus). Token deltas and other per-token noise are
			// filtered out so the feed isn't a token-by-token flood (TEN-234).
			if activityRelevant(ev) {
				if _, err := w.Write(patchElements("#activity-feed", "append", activityRow(ev))); err != nil {
					return
				}
			}
			if bubble := chatBubble(ev); bubble != "" {
				if _, err := w.Write(patchElements("#chat-log", "append", bubble)); err != nil {
					return
				}
			}
			flush(w)
		}
	}
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
		_, _ = w.Write(patchElements("#chat-log", "append",
			fmt.Sprintf(`<div class="msg user"><div class="bubble">%s</div></div>`, html.EscapeString(visibleText(text)))))
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

// activityRelevant decides what reaches the dashboard's activity feed. It's a
// DENYLIST of per-token / accounting noise — token deltas (the flood), usage
// counts, per-iteration assistant prose, and the memory-budget report — so any
// meaningful or future event kind shows by default (tool calls, lifecycle,
// cross-agent activity, bus messages). (TEN-234)
func activityRelevant(ev agent.Event) bool { return agent.Retained(ev) }

func activityRow(ev agent.Event) string {
	detail := ev.Text
	if ev.Tool != "" {
		detail = strings.TrimSpace(ev.Tool + " " + ev.Result)
	}
	// Distinct tags for the offsite/cross-agent kinds; the channel/peer detail
	// already rides in ev.Text (TEN-232/234).
	tag := string(ev.Kind)
	switch ev.Kind {
	case agent.EventIngest:
		tag = "📥 inbox"
	case agent.EventBus:
		tag = "🔀 bus"
	}
	// Attribute a cross-agent event to its source sub-agent / bus sender.
	prefix := ""
	if ev.Agent != "" {
		prefix = "[" + ev.Agent + "] "
	}
	return fmt.Sprintf(`<div class="ev"><span class="tg">%s</span> <span class="detail">%s</span></div>`,
		html.EscapeString(tag), html.EscapeString(prefix+snippetStr(detail, 200)))
}

// chatBubble renders conversation-relevant events; "" for kinds that don't
// belong in the transcript. To avoid duplicates we render the authoritative
// final response, tool calls, and tool results (not per-token deltas).
func chatBubble(ev agent.Event) string {
	// Cross-agent activity + bus traffic belong in the activity feed, not the
	// operator's conversation transcript (TEN-234).
	if ev.Agent != "" || ev.Kind == agent.EventBus {
		return ""
	}
	switch string(ev.Kind) {
	case "final", "truncated":
		if strings.TrimSpace(ev.Text) == "" {
			return ""
		}
		return fmt.Sprintf(`<div class="msg a"><div class="av">t</div><div class="bubble">%s</div></div>`,
			html.EscapeString(visibleText(ev.Text)))
	case "tool_call":
		return fmt.Sprintf(`<div class="msg a"><div class="av">t</div><div class="tool">⚡ %s <span class="dim">%s</span></div></div>`,
			html.EscapeString(ev.Tool), html.EscapeString(snippetStr(ev.Args, 120)))
	case "tool_result":
		st := "ok"
		if ev.IsErr {
			st = "error"
		}
		return fmt.Sprintf(`<div class="msg a"><div class="av">t</div><div class="tool">%s <span class="dim">[%s]</span> %s</div></div>`,
			html.EscapeString(ev.Tool), st, html.EscapeString(snippetStr(ev.Result, 200)))
	case "error":
		return fmt.Sprintf(`<div class="msg sys"><div class="dim">⚠ %s</div></div>`, html.EscapeString(snippetStr(ev.Text, 200)))
	case "notice":
		return fmt.Sprintf(`<div class="msg sys"><div class="dim">%s</div></div>`, html.EscapeString(snippetStr(ev.Text, 200)))
	}
	return ""
}

// snippetStr is s on one line, cut to at most n bytes on a character
// boundary, with its hidden characters shown (visibleText, F10): every row
// of the Activity feed and the chat transcript goes through it, so a command
// can't display reversed there beside its approval card (REVSEC-8).
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
