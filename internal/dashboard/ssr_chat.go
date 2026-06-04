package dashboard

// ssr_chat.go is TEN-109: real-time chat + activity over Server-Sent Events,
// replacing the old WebSocket-driven inline JS. `GET /events` is a
// long-lived SSE stream that subscribes to the agent event broker and APPENDS
// HTML fragments into the live containers (#chat-log on Chat, #activity-feed on
// Activity) via Datastar patches — the browser updates in real time with zero
// hand-written JS. Chat input (send / interject / stop) is stateless POSTs
// driving the Server-wide turn coordinator (session.go).

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"

	"tenant/internal/agent"
)

func (s *Server) handleChatPage(w http.ResponseWriter, _ *http.Request) {
	s.render(w, s.tmpl.chat, layoutData{Title: "Chat", Page: "chat"})
}

// handleEventsSSE streams agent events to the browser until the request context
// is canceled (tab closed / navigation away). Every event appends to the
// activity feed; conversation-relevant kinds also append to the chat transcript.
// One stream serves both pages — a patch whose selector isn't on the current
// page is simply a no-op there.
func (s *Server) handleEventsSSE(w http.ResponseWriter, r *http.Request) {
	setSSEHeaders(w)
	flush(w) // open the stream immediately
	ch, unsub := s.broker.Subscribe()
	defer unsub()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(patchElements("#activity-feed", "append", activityRow(ev))); err != nil {
				return
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

// handleChatSend appends the operator's message to the transcript and starts a
// background turn; the turn's events stream back over /events.
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	text := chatText(r)
	if text == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	started := s.coord.startBackground(text)
	setSSEHeaders(w)
	_, _ = w.Write(patchElements("#chat-log", "append",
		fmt.Sprintf(`<div class="msg user"><div class="bubble">%s</div></div>`, html.EscapeString(text))))
	if !started {
		_, _ = w.Write(patchElements("#chat-log", "append",
			`<div class="msg sys"><div class="dim">a turn is already running — try again in a moment</div></div>`))
	}
	flush(w)
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

func activityRow(ev agent.Event) string {
	detail := ev.Text
	if ev.Tool != "" {
		detail = strings.TrimSpace(ev.Tool + " " + ev.Result)
	}
	return fmt.Sprintf(`<div class="ev"><span class="tg">%s</span> <span class="detail">%s</span></div>`,
		html.EscapeString(string(ev.Kind)), html.EscapeString(snippetStr(detail, 200)))
}

// chatBubble renders conversation-relevant events; "" for kinds that don't
// belong in the transcript. To avoid duplicates we render the authoritative
// final response, tool calls, and tool results (not per-token deltas).
func chatBubble(ev agent.Event) string {
	switch string(ev.Kind) {
	case "final", "truncated":
		if strings.TrimSpace(ev.Text) == "" {
			return ""
		}
		return fmt.Sprintf(`<div class="msg a"><div class="av">t</div><div class="bubble">%s</div></div>`,
			html.EscapeString(ev.Text))
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

func snippetStr(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
