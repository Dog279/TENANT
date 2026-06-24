package imessage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// Policy gates iMessage actions by blast radius — same shape as
// sql/web/gsuite/x. Read (list/read/search) is always allowed. Send
// (imessage_send, imessage_new_chat) messages a real person, so it is
// denied unless AllowSend, or a per-action Confirm explicitly approves
// it (nil ⇒ deny). The model cannot change the policy.
type Policy struct {
	AllowSend bool
	Confirm   func(ctx context.Context, action, detail string) bool
}

type actionClass int

const (
	classRead actionClass = iota
	classSend
)

func (p Policy) gate(ctx context.Context, c actionClass, detail string) error {
	if c == classRead {
		return nil
	}
	if p.AllowSend {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, "imessage_send", detail) {
		return nil
	}
	return fmt.Errorf("blocked: this sends an iMessage to a real person and was not approved " +
		"— enable the send flag or confirm. This is a blast-radius boundary, not a bug")
}

// Dispatcher implements agent.ToolDispatcher for iMessage. It holds a
// transport (the BlueBubbles *Service or the native macOS transport) —
// not a concrete type — so the tools and the Policy gate are identical
// across backends. The field is kept named svc to minimize the diff;
// widening *Service → transport is additive (both satisfy the interface).
type Dispatcher struct {
	svc    transport
	policy Policy
}

// NewDispatcher takes any transport. Passing a *Service (BlueBubbles), a
// native transport, or nil (for Tools()-only use) all compile and behave
// as before.
func NewDispatcher(svc transport, policy Policy) *Dispatcher {
	return &Dispatcher{svc: svc, policy: policy}
}

func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	return []model.ToolSpec{
		{Name: "imessage_list_chats", Description: "List recent iMessage conversations (most recent first): chat guid, name/participants, last message.",
			Parameters: obj(`"limit":{"type":"integer","description":"max chats (default 25)"}`)},
		{Name: "imessage_read_chat", Description: "Read recent messages in a conversation by its chat guid (from imessage_list_chats).",
			Parameters: obj(`"chat_guid":{"type":"string"},"limit":{"type":"integer","description":"max messages (default 25)"}`, "chat_guid")},
		{Name: "imessage_search", Description: "Search messages by text (substring match), newest first. Returns text/sender/date.",
			Parameters: obj(`"text":{"type":"string"},"limit":{"type":"integer"}`, "text")},
		{Name: "imessage_send", Description: "Send a text message to an existing conversation by chat guid. GATED: requires operator approval (messages a real person).",
			Parameters: obj(`"chat_guid":{"type":"string"},"text":{"type":"string"}`, "chat_guid", "text"), Gated: true},
		{Name: "imessage_new_chat", Description: "Start a new conversation with a phone number or email and send the first message. GATED: requires operator approval.",
			Parameters: obj(`"address":{"type":"string","description":"phone number or email"},"text":{"type":"string"}`, "address", "text"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "imessage_list_chats":
		return d.listChats(ctx, call.Arguments)
	case "imessage_read_chat":
		return d.readChat(ctx, call.Arguments)
	case "imessage_search":
		return d.search(ctx, call.Arguments)
	case "imessage_send":
		return d.send(ctx, call.Arguments)
	case "imessage_new_chat":
		return d.newChat(ctx, call.Arguments)
	default:
		return "unknown imessage tool: " + call.Name, true, nil
	}
}

func unmarshal(args json.RawMessage, v any) (string, bool) {
	if err := json.Unmarshal(args, v); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	return "", false
}

func fmtMsg(m Message) string {
	return fmt.Sprintf("- [%s] %s: %s", m.Date, m.From, strings.ReplaceAll(m.Text, "\n", " "))
}

func (d *Dispatcher) listChats(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Limit int `json:"limit"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	chats, err := d.svc.ListChats(ctx, a.Limit)
	if err != nil {
		return "imessage list_chats failed: " + err.Error(), true, nil
	}
	if len(chats) == 0 {
		return "no conversations found", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d conversation(s):\n", len(chats))
	for _, c := range chats {
		fmt.Fprintf(&b, "- guid=%s | %s\n  last: %s\n", c.GUID, c.Name, clip(c.LastMessage, 120))
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) readChat(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ChatGUID string `json:"chat_guid"`
		Limit    int    `json:"limit"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	msgs, err := d.svc.ChatMessages(ctx, a.ChatGUID, a.Limit)
	if err != nil {
		return "imessage read_chat failed: " + err.Error(), true, nil
	}
	if len(msgs) == 0 {
		return "no messages in that conversation", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d message(s):\n", len(msgs))
	for _, m := range msgs {
		b.WriteString(fmtMsg(m) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) search(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Text  string `json:"text"`
		Limit int    `json:"limit"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	if strings.TrimSpace(a.Text) == "" {
		return "text is required", true, nil
	}
	msgs, err := d.svc.SearchMessages(ctx, a.Text, a.Limit)
	if err != nil {
		return "imessage search failed: " + err.Error(), true, nil
	}
	if len(msgs) == 0 {
		return fmt.Sprintf("no messages matched %q", a.Text), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es):\n", len(msgs))
	for _, m := range msgs {
		b.WriteString(fmtMsg(m) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) send(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ChatGUID string `json:"chat_guid"`
		Text     string `json:"text"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	detail := fmt.Sprintf("message to chat %s: %q (%d chars)", a.ChatGUID, a.Text, len([]rune(a.Text)))
	// Gate ONCE for the whole message, before any chunking — the operator
	// approves the full text they see, not per-bubble fragments.
	if err := d.policy.gate(ctx, classSend, detail); err != nil {
		return err.Error(), true, nil
	}
	// Long replies degrade on iMessage; split on paragraph boundaries and
	// send each bubble in order. Short messages chunk to a single bubble,
	// so behavior is unchanged in the common case.
	chunks := chunkParagraphs(a.Text, maxBubbleSize)
	var guid string
	for _, ch := range chunks {
		g, err := d.svc.SendText(ctx, a.ChatGUID, ch)
		if err != nil {
			return "imessage send failed: " + err.Error(), true, nil
		}
		guid = g
	}
	if guid == "" {
		// The native (AppleScript) transport returns no message guid.
		if len(chunks) > 1 {
			return fmt.Sprintf("message sent in %d parts", len(chunks)), false, nil
		}
		return "message sent", false, nil
	}
	if len(chunks) > 1 {
		return fmt.Sprintf("sent message %s (%d parts)", guid, len(chunks)), false, nil
	}
	return "sent message " + guid, false, nil
}

func (d *Dispatcher) newChat(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		Address string `json:"address"`
		Text    string `json:"text"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	detail := fmt.Sprintf("new chat to %s: %q (%d chars)", a.Address, a.Text, len([]rune(a.Text)))
	// Gate ONCE for the whole message, before any chunking.
	if err := d.policy.gate(ctx, classSend, detail); err != nil {
		return err.Error(), true, nil
	}
	// CONSTRAINT: NewChat opens a conversation, but the transport returns
	// a *message* guid (BlueBubbles) or nothing (native AppleScript) — not
	// an addressable *chat* guid. SendText needs a chat guid, so follow-up
	// chunks cannot be routed to the just-created conversation. We
	// therefore send the FIRST chunk to open the chat and surface that the
	// remainder was not delivered (rather than misdeliver to a message
	// guid or silently drop). The model can follow up with imessage_send
	// once the chat is listable via imessage_list_chats.
	chunks := chunkParagraphs(a.Text, maxBubbleSize)
	guid, err := d.svc.NewChat(ctx, a.Address, chunks[0])
	if err != nil {
		return "imessage new_chat failed: " + err.Error(), true, nil
	}
	extra := ""
	if len(chunks) > 1 {
		extra = fmt.Sprintf(" — note: only the first of %d parts was sent; "+
			"use imessage_send to the new chat for the rest", len(chunks))
	}
	if guid == "" {
		// The native (AppleScript) transport returns no message guid.
		return fmt.Sprintf("started chat with %s%s", a.Address, extra), false, nil
	}
	return fmt.Sprintf("started chat with %s (message %s)%s", a.Address, guid, extra), false, nil
}
