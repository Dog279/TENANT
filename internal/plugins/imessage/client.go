package imessage

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Chat is a normalized conversation.
type Chat struct {
	GUID         string
	Name         string
	Participants []string
	LastMessage  string
}

// Message is a normalized iMessage.
type Message struct {
	GUID     string
	Text     string
	From     string
	IsFromMe bool
	Date     string
}

// --- reads ---

// ListChats returns recent conversations, most-recent first.
func (s *Service) ListChats(ctx context.Context, limit int) ([]Chat, error) {
	if limit <= 0 {
		limit = 25
	}
	body := map[string]any{
		"limit": limit, "offset": 0,
		"with": []string{"lastMessage", "participants"},
		"sort": "lastmessage",
	}
	var raw []bbChat
	if err := s.a.do(ctx, "POST", "/chat/query", nil, body, &raw); err != nil {
		return nil, err
	}
	out := make([]Chat, 0, len(raw))
	for _, c := range raw {
		out = append(out, c.normalize())
	}
	return out, nil
}

// ChatMessages returns the most recent messages in a chat.
func (s *Service) ChatMessages(ctx context.Context, chatGUID string, limit int) ([]Message, error) {
	if strings.TrimSpace(chatGUID) == "" {
		return nil, fmt.Errorf("imessage: chat guid required")
	}
	if limit <= 0 {
		limit = 25
	}
	q := url.Values{
		"with":  {"handle"},
		"limit": {fmt.Sprintf("%d", limit)},
		"sort":  {"DESC"},
	}
	var raw []bbMessage
	if err := s.a.do(ctx, "GET", "/chat/"+url.PathEscape(chatGUID)+"/message", q, nil, &raw); err != nil {
		return nil, err
	}
	return normalizeMsgs(raw), nil
}

// SearchMessages finds messages whose text matches (case-insensitive
// substring), newest first. Uses BlueBubbles' message query `where`.
func (s *Service) SearchMessages(ctx context.Context, text string, limit int) ([]Message, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("imessage: search text required")
	}
	if limit <= 0 {
		limit = 25
	}
	body := map[string]any{
		"limit": limit, "offset": 0,
		"with": []string{"handle", "chat"},
		"sort": "DESC",
		"where": []map[string]any{{
			"statement": "message.text LIKE :term",
			"args":      map[string]string{"term": "%" + text + "%"},
		}},
	}
	var raw []bbMessage
	if err := s.a.do(ctx, "POST", "/message/query", nil, body, &raw); err != nil {
		return nil, err
	}
	return normalizeMsgs(raw), nil
}

// --- writes (gated by the dispatcher) ---

// SendText sends a message to an existing chat.
func (s *Service) SendText(ctx context.Context, chatGUID, text string) (string, error) {
	if strings.TrimSpace(chatGUID) == "" {
		return "", fmt.Errorf("imessage: chat guid required")
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("imessage: message text is empty")
	}
	body := map[string]any{
		"chatGuid": chatGUID,
		"tempGuid": tempGUID(),
		"message":  text,
		"method":   s.sendMethod,
	}
	var res bbMessage
	if err := s.a.do(ctx, "POST", "/message/text", nil, body, &res); err != nil {
		return "", err
	}
	return res.GUID, nil
}

// NewChat starts a conversation with an address (phone/email) and sends
// the first message.
func (s *Service) NewChat(ctx context.Context, address, text string) (string, error) {
	if strings.TrimSpace(address) == "" {
		return "", fmt.Errorf("imessage: recipient address (phone/email) required")
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("imessage: message text is empty")
	}
	body := map[string]any{
		"addresses": []string{address},
		"message":   text,
		"method":    s.sendMethod,
		"tempGuid":  tempGUID(),
	}
	var res bbMessage
	if err := s.a.do(ctx, "POST", "/chat/new", nil, body, &res); err != nil {
		return "", err
	}
	return res.GUID, nil
}

// --- BlueBubbles payload shapes (only what we use) ---

type bbHandle struct {
	Address string `json:"address"`
}

type bbChat struct {
	GUID         string     `json:"guid"`
	DisplayName  string     `json:"displayName"`
	Participants []bbHandle `json:"participants"`
	LastMessage  *bbMessage `json:"lastMessage"`
}

func (c bbChat) normalize() Chat {
	out := Chat{GUID: c.GUID, Name: c.DisplayName}
	for _, p := range c.Participants {
		if p.Address != "" {
			out.Participants = append(out.Participants, p.Address)
		}
	}
	if out.Name == "" { // 1:1 chats often have no display name
		out.Name = strings.Join(out.Participants, ", ")
	}
	if c.LastMessage != nil {
		out.LastMessage = c.LastMessage.Text
	}
	return out
}

type bbMessage struct {
	GUID        string    `json:"guid"`
	Text        string    `json:"text"`
	DateCreated int64     `json:"dateCreated"`
	IsFromMe    bool      `json:"isFromMe"`
	Handle      *bbHandle `json:"handle"`
}

func (m bbMessage) normalize() Message {
	from := ""
	if m.IsFromMe {
		from = "me"
	} else if m.Handle != nil {
		from = m.Handle.Address
	}
	return Message{
		GUID: m.GUID, Text: m.Text, From: from,
		IsFromMe: m.IsFromMe, Date: msToTime(m.DateCreated),
	}
}

func normalizeMsgs(raw []bbMessage) []Message {
	out := make([]Message, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.normalize())
	}
	return out
}
