package orchestra

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// CommsTool is the agent-facing way to talk to teammates over the Bus. One
// is built per agent, bound to that agent's id, and added to its tool set —
// so an agent can message peers (or the whole team) and read replies as
// part of its own loop, resolving issues via its soul/rules without the
// user. It satisfies the toolMux plugin shape (Tools + Dispatch).
type CommsTool struct {
	Bus  *Bus
	Self string // the calling agent's id
}

func (CommsTool) specObj(props string, req ...string) json.RawMessage {
	r := ""
	for i, x := range req {
		if i > 0 {
			r += ","
		}
		r += `"` + x + `"`
	}
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
}

func (t CommsTool) Tools() []model.ToolSpec {
	return []model.ToolSpec{
		{
			Name:        "team_send",
			Description: "Send a direct message to a specific teammate by their agent id. Use to ask a question, hand off work, or resolve a disagreement. Non-blocking — they read it on their next step; check for their reply later with team_check.",
			Parameters:  t.specObj(`"to":{"type":"string","description":"recipient agent id"},"message":{"type":"string"}`, "to", "message"),
		},
		{
			Name:        "team_broadcast",
			Description: "Send a message to the whole team at once (everyone but you). Use for status updates, decisions, or to surface a blocker.",
			Parameters:  t.specObj(`"message":{"type":"string"}`, "message"),
		},
		{
			Name:        "team_check",
			Description: "Read and CLEAR your inbox of NEW messages from teammates (drains it, freeing your context). Call this to see what others said before deciding your next step.",
			Parameters:  t.specObj(``),
		},
		{
			Name:        "team_history",
			Description: "Recall the FULL team conversation relevant to you (everything you sent, were sent, or could see) WITHOUT clearing it. Use to recover context on a long task after a compaction, or to review what was already discussed.",
			Parameters:  t.specObj(``),
		},
		{
			Name:        "team_roster",
			Description: "List the agent ids currently on the team, including any spawned after you started. Check this if unsure who exists before team_send.",
			Parameters:  t.specObj(``),
		},
	}
}

func (t CommsTool) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "team_send":
		var a struct {
			To      string `json:"to"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return "invalid arguments: " + err.Error(), true, nil
		}
		if strings.TrimSpace(a.To) == "" || strings.TrimSpace(a.Message) == "" {
			return "both 'to' and 'message' are required", true, nil
		}
		if err := t.Bus.Send(Message{From: t.Self, To: a.To, Content: a.Message}); err != nil {
			return err.Error() + " (use team_check or check the roster for valid ids)", true, nil
		}
		return "sent to " + a.To, false, nil

	case "team_broadcast":
		var a struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return "invalid arguments: " + err.Error(), true, nil
		}
		if strings.TrimSpace(a.Message) == "" {
			return "message is required", true, nil
		}
		if err := t.Bus.Send(Message{From: t.Self, Content: a.Message}); err != nil {
			return err.Error(), true, nil
		}
		return "broadcast to the team", false, nil

	case "team_check":
		msgs := t.Bus.Inbox(t.Self)
		if len(msgs) == 0 {
			return "no new messages", false, nil
		}
		return "new messages:\n" + formatMessages(msgs), false, nil

	case "team_history":
		msgs := t.Bus.History(t.Self)
		if len(msgs) == 0 {
			return "no team conversation yet", false, nil
		}
		return "team conversation so far:\n" + formatMessages(msgs), false, nil

	case "team_roster":
		members := t.Bus.Members()
		others := make([]string, 0, len(members))
		for _, id := range members {
			if id != t.Self {
				others = append(others, id)
			}
		}
		if len(others) == 0 {
			return "you are the only agent on the team right now", false, nil
		}
		return "team members: " + strings.Join(others, ", "), false, nil

	default:
		return "unknown team tool: " + call.Name, true, nil
	}
}

// formatMessages renders a message list for a tool result.
func formatMessages(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		scope := m.From
		if m.Broadcast() {
			scope = m.From + " (broadcast)"
		}
		fmt.Fprintf(&b, "- from %s: %s\n", scope, m.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}
