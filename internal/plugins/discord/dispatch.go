package discord

// Dispatcher implements agent.ToolDispatcher for Discord. Matches the
// imessage plugin shape: Policy gates mutating actions (send, react)
// behind AllowSend or a Confirm callback; reads pass through. The
// shape was chosen so an operator running tenant with --discord-bot-token
// but without --discord-allow-send gets a safe read-only bot by default.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// Policy gates Discord actions by blast radius. Mirrors imessage.Policy
// (internal/plugins/imessage/dispatch.go:17). Read (read_channel,
// list_channels, list_guilds) is always allowed. Mutating (send,
// react) is denied unless AllowSend OR per-action Confirm approves.
// Nil Confirm with !AllowSend ⇒ hard deny — the model cannot post on
// the operator's behalf without explicit opt-in.
type Policy struct {
	AllowSend bool
	Confirm   func(ctx context.Context, action, detail string) bool
}

type actionClass int

const (
	classRead actionClass = iota
	classMutate
)

func (p Policy) gate(ctx context.Context, c actionClass, action, detail string) error {
	if c == classRead {
		return nil
	}
	if p.AllowSend {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, action, detail) {
		return nil
	}
	return fmt.Errorf("blocked: this %s and was not approved "+
		"— relaunch with --discord-allow-send or confirm interactively. "+
		"This is a blast-radius boundary, not a bug", action)
}

// Dispatcher implements agent.ToolDispatcher.
type Dispatcher struct {
	svc    *Service
	policy Policy
}

// NewDispatcher wires a Discord Service + Policy. svc may be nil — in
// that case the dispatcher serves Tools() for the stub catalog at
// toolmux.go but Dispatch() always returns "not configured." This
// matches the wiki.NewDispatcher(nil) pattern documented in
// docs/PLUGINS.md §5.
func NewDispatcher(svc *Service, policy Policy) *Dispatcher {
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
		{Name: "discord_list_guilds", Description: "List the Discord servers (guilds) this bot is a member of. Returns id+name for each.",
			Parameters: obj(``)},
		{Name: "discord_list_channels", Description: "List channels in a Discord guild by guild id (from discord_list_guilds). Includes channel id, name, type, topic.",
			Parameters: obj(`"guild_id":{"type":"string"}`, "guild_id")},
		{Name: "discord_read_channel", Description: "Read recent messages from a Discord channel by channel id (newest first). limit defaults to 25, max 100.",
			Parameters: obj(`"channel_id":{"type":"string"},"limit":{"type":"integer","description":"max messages (default 25, cap 100)"}`, "channel_id")},
		{Name: "discord_send_message", Description: "Post a message to a Discord channel by channel id. GATED: requires operator approval (posts publicly as the bot). @everyone and role-pings are auto-blocked.",
			Parameters: obj(`"channel_id":{"type":"string"},"content":{"type":"string","description":"the message text"}`, "channel_id", "content"), Gated: true},
		{Name: "discord_react", Description: "Add an emoji reaction to a Discord message as the bot user. emoji is a unicode glyph (e.g. \"👍\") or a custom-emoji \"name:id\". GATED: requires operator approval.",
			Parameters: obj(`"channel_id":{"type":"string"},"message_id":{"type":"string"},"emoji":{"type":"string"}`, "channel_id", "message_id", "emoji"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if d.svc == nil {
		return "discord is not configured — relaunch with --discord-bot-token=<token> or set it via tenant setup", true, nil
	}
	switch call.Name {
	case "discord_list_guilds":
		return d.listGuilds(ctx)
	case "discord_list_channels":
		return d.listChannels(ctx, call.Arguments)
	case "discord_read_channel":
		return d.readChannel(ctx, call.Arguments)
	case "discord_send_message":
		return d.sendMessage(ctx, call.Arguments)
	case "discord_react":
		return d.react(ctx, call.Arguments)
	default:
		return "unknown discord tool: " + call.Name, true, nil
	}
}

func unmarshal(args json.RawMessage, v any) (string, bool) {
	if err := json.Unmarshal(args, v); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	return "", false
}

func (d *Dispatcher) listGuilds(ctx context.Context) (string, bool, error) {
	guilds, err := d.svc.ListGuilds(ctx)
	if err != nil {
		return "discord list_guilds failed: " + err.Error(), true, nil
	}
	if len(guilds) == 0 {
		return "this bot is not in any guilds yet — invite it via the Developer Portal OAuth2 URL with the `bot` scope", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d guild(s):\n", len(guilds))
	for _, g := range guilds {
		fmt.Fprintf(&b, "- id=%s | %s\n", g.ID, g.Name)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) listChannels(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		GuildID string `json:"guild_id"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	chans, err := d.svc.ListChannels(ctx, a.GuildID)
	if err != nil {
		return "discord list_channels failed: " + err.Error(), true, nil
	}
	if len(chans) == 0 {
		return "no channels visible — bot may lack VIEW_CHANNEL permission", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d channel(s):\n", len(chans))
	for _, c := range chans {
		topic := ""
		if c.Topic != "" {
			topic = " — " + clip(c.Topic, 80)
		}
		fmt.Fprintf(&b, "- id=%s | %s (type=%d)%s\n", c.ID, c.Name, c.Type, topic)
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) readChannel(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ChannelID string `json:"channel_id"`
		Limit     int    `json:"limit"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	msgs, err := d.svc.ReadChannel(ctx, a.ChannelID, a.Limit)
	if err != nil {
		return "discord read_channel failed: " + err.Error(), true, nil
	}
	if len(msgs) == 0 {
		return "no messages in that channel (or bot lacks READ_MESSAGE_HISTORY)", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d message(s) (newest first):\n", len(msgs))
	for _, m := range msgs {
		name := m.Author.Username
		if m.Author.GlobalName != "" {
			name = m.Author.GlobalName
		}
		botTag := ""
		if m.Author.Bot {
			botTag = " [bot]"
		}
		fmt.Fprintf(&b, "- [%s] %s%s: %s\n", m.Timestamp, name, botTag, strings.ReplaceAll(m.Content, "\n", " "))
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) sendMessage(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ChannelID string `json:"channel_id"`
		Content   string `json:"content"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	detail := fmt.Sprintf("message to channel %s: %q (%d chars)", a.ChannelID, clip(a.Content, 80), len([]rune(a.Content)))
	if err := d.policy.gate(ctx, classMutate, "posts a Discord message", detail); err != nil {
		return err.Error(), true, nil
	}
	msg, err := d.svc.SendMessage(ctx, a.ChannelID, a.Content)
	if err != nil {
		return "discord send_message failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("sent message id=%s to channel %s", msg.ID, msg.ChannelID), false, nil
}

func (d *Dispatcher) react(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		ChannelID string `json:"channel_id"`
		MessageID string `json:"message_id"`
		Emoji     string `json:"emoji"`
	}
	if m, bad := unmarshal(args, &a); bad {
		return m, true, nil
	}
	detail := fmt.Sprintf("reaction %q to message %s in channel %s", a.Emoji, a.MessageID, a.ChannelID)
	if err := d.policy.gate(ctx, classMutate, "adds a Discord reaction", detail); err != nil {
		return err.Error(), true, nil
	}
	if err := d.svc.AddReaction(ctx, a.ChannelID, a.MessageID, a.Emoji); err != nil {
		return "discord react failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("reacted %s to message %s", a.Emoji, a.MessageID), false, nil
}

// clip is the local string-truncation helper used in this package's
// human-readable result formatting. Avoids importing a shared util just
// for one function.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
