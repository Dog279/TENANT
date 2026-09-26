package archive

import "time"

// turnWindow is how long after a user's message its episode may be recorded
// and still be matched to it: episodes are written when the turn ends.
const turnWindow = time.Minute

// Turn returns the archived events of one turn: from the user message that
// started it (the latest in the session with exactly this text, sent no later
// than a minute after at) up to the next user message. It is how a fact's
// source episode is shown as the conversation it came from (TEN-297). No match
// returns no events and no error; the archive may predate the episode or have
// been pruned.
func (r *Reader) Turn(sessionID, prompt string, at time.Time) ([]Event, error) {
	if sessionID == "" {
		return nil, nil
	}
	var events []Event
	for ev, err := range r.Stream(Filter{SessionID: sessionID}) {
		if err != nil {
			continue // one torn or malformed line does not hide the rest
		}
		events = append(events, ev)
	}
	start := -1
	for i, ev := range events {
		if ev.Role == "user" && ev.Content == prompt && !ev.Timestamp.After(at.Add(turnWindow)) {
			start = i
		}
	}
	if start < 0 {
		return nil, nil
	}
	end := len(events)
	for i := start + 1; i < len(events); i++ {
		if events[i].Role == "user" {
			end = i
			break
		}
	}
	return events[start:end], nil
}
