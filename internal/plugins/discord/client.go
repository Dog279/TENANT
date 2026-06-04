package discord

// Typed REST methods on Service. Five public methods back the five tools:
//
//   ListGuilds       → GET /users/@me/guilds
//   ListChannels     → GET /guilds/{id}/channels
//   ReadChannel      → GET /channels/{id}/messages?limit=N
//   SendMessage      → POST /channels/{id}/messages (MUTATING — deny-listed for retry)
//   AddReaction      → PUT /channels/{id}/messages/{msg}/reactions/{emoji}/@me (MUTATING)
//
// Discord docs: https://docs.discord.com/developers/resources/message
// + https://docs.discord.com/developers/resources/channel
// + https://docs.discord.com/developers/resources/user

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Guild is a normalized Discord guild (server) the bot is in.
type Guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Channel is a normalized Discord channel (text/voice/category/etc).
type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     int    `json:"type"` // 0 = text, 2 = voice, 4 = category, 5 = announcement, 11/12 = thread
	GuildID  string `json:"guild_id"`
	Position int    `json:"position"`
	Topic    string `json:"topic,omitempty"`
}

// Author is the abridged author block on a message.
type Author struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name,omitempty"`
	Bot        bool   `json:"bot,omitempty"`
}

// Message is a normalized Discord channel message. We deliberately
// surface only what's useful to an agent's textual summarization —
// content + author + timestamp + id. Embeds, components, attachments
// are dropped intentionally (the agent rarely benefits from full
// fidelity, and including them blows the prompt budget).
type Message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
	Author    Author `json:"author"`
	Timestamp string `json:"timestamp"` // ISO-8601
}

// SentMessage is the response from POST /channels/{id}/messages.
type SentMessage struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
}

// GatewayURL fetches the bot's recommended Gateway WebSocket URL from
// GET /gateway/bot (the inbound "Surface B" entry point — TEN-115). The
// response also carries shard + session-start-limit hints we don't need for a
// sole-operator single-shard bot, so only the url is returned.
func (s *Service) GatewayURL(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := s.a.do(ctx, "GET", "/gateway/bot", nil, nil, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.URL) == "" {
		return "", fmt.Errorf("discord: empty gateway url from /gateway/bot")
	}
	return out.URL, nil
}

// ListGuilds returns every guild the bot is a member of. Pagination
// upper-bound is Discord's default (200 per page) which is fine for
// most bots; if a bot is in >200 servers it'll need pagination, which
// today's caller (a sole-operator deployment) does not require.
func (s *Service) ListGuilds(ctx context.Context) ([]Guild, error) {
	var out []Guild
	if err := s.a.do(ctx, "GET", "/users/@me/guilds", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListChannels returns every channel in a guild. Bot must have the
// VIEW_CHANNEL permission per-channel for it to appear.
func (s *Service) ListChannels(ctx context.Context, guildID string) ([]Channel, error) {
	if strings.TrimSpace(guildID) == "" {
		return nil, fmt.Errorf("discord: guild id required")
	}
	var out []Channel
	if err := s.a.do(ctx, "GET", "/guilds/"+url.PathEscape(guildID)+"/channels", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadChannel returns the most recent messages in a channel (newest
// first). limit is clamped to Discord's [1, 100] range.
func (s *Service) ReadChannel(ctx context.Context, channelID string, limit int) ([]Message, error) {
	if strings.TrimSpace(channelID) == "" {
		return nil, fmt.Errorf("discord: channel id required")
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100 // Discord cap
	}
	q := url.Values{"limit": {fmt.Sprintf("%d", limit)}}
	var out []Message
	if err := s.a.do(ctx, "GET", "/channels/"+url.PathEscape(channelID)+"/messages", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendMessage posts a text message to a channel. MUTATING — gated by
// Policy at the dispatcher layer + deny-listed in retry.go's
// mutatingTools map (never silently retries on transient failure).
//
// allowed_mentions defaults to parse=[]: neither @everyone, role pings,
// nor user mentions will resolve. This is the safety default matching
// Hermes's `_build_allowed_mentions()` in plugins/platforms/discord/adapter.py.
func (s *Service) SendMessage(ctx context.Context, channelID, content string) (*SentMessage, error) {
	if strings.TrimSpace(channelID) == "" {
		return nil, fmt.Errorf("discord: channel id required")
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("discord: message content required")
	}
	body := map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	var out SentMessage
	if err := s.a.do(ctx, "POST", "/channels/"+url.PathEscape(channelID)+"/messages", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddReaction adds an emoji reaction to a message as the bot user.
// emoji can be a unicode glyph (e.g. "👍") or a custom-emoji "name:id".
// MUTATING — gated + deny-listed.
//
// Returns no body (Discord 204 No Content).
func (s *Service) AddReaction(ctx context.Context, channelID, messageID, emoji string) error {
	if strings.TrimSpace(channelID) == "" || strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("discord: channel id + message id required")
	}
	if strings.TrimSpace(emoji) == "" {
		return fmt.Errorf("discord: emoji required")
	}
	path := "/channels/" + url.PathEscape(channelID) +
		"/messages/" + url.PathEscape(messageID) +
		"/reactions/" + url.PathEscape(emoji) + "/@me"
	return s.a.do(ctx, "PUT", path, nil, nil, nil)
}

// Button is one message-component button (v2 approvals). Style: 1=primary,
// 2=secondary, 3=success (green), 4=danger (red).
type Button struct {
	Label    string
	CustomID string
	Style    int
}

// SendComponents posts a message carrying a single action-row of buttons (the
// v2 approval prompt). Same safe allowed_mentions=parse:[] default as
// SendMessage. MUTATING — gated/deny-listed like SendMessage.
func (s *Service) SendComponents(ctx context.Context, channelID, content string, buttons []Button) (*SentMessage, error) {
	if strings.TrimSpace(channelID) == "" {
		return nil, fmt.Errorf("discord: channel id required")
	}
	comps := make([]map[string]any, 0, len(buttons))
	for _, b := range buttons {
		comps = append(comps, map[string]any{
			"type": 2, "style": b.Style, "label": b.Label, "custom_id": b.CustomID,
		})
	}
	body := map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components":       []map[string]any{{"type": 1, "components": comps}},
	}
	var out SentMessage
	if err := s.a.do(ctx, "POST", "/channels/"+url.PathEscape(channelID)+"/messages", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RespondInteraction ACKs a component (button) interaction by EDITING the source
// message (type 7 UPDATE_MESSAGE) to content with the buttons removed. MUST be
// called within Discord's ~3-second interaction window or the click shows
// "interaction failed."
func (s *Service) RespondInteraction(ctx context.Context, interactionID, token, content string) error {
	if strings.TrimSpace(interactionID) == "" || strings.TrimSpace(token) == "" {
		return fmt.Errorf("discord: interaction id + token required")
	}
	body := map[string]any{
		"type": 7, // UPDATE_MESSAGE
		"data": map[string]any{"content": content, "components": []any{}},
	}
	path := "/interactions/" + url.PathEscape(interactionID) + "/" + url.PathEscape(token) + "/callback"
	return s.a.do(ctx, "POST", path, nil, body, nil)
}
