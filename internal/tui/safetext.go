package tui

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"tenant/internal/agent"
	"tenant/internal/control"
)

// Text the app did not write reaches the terminal through three doors: an
// agent event (the model's words, a tool's arguments and its output), a
// sub-agent's event, and an approval request (whose detail is a command the
// model chose). A terminal obeys what it is sent: a carriage return puts the
// next words over the last ones, an escape sequence recolors, clears or
// retitles the screen. So none of that text is drawn as it came (TEN-468):
// each door passes it through termText, and an approval, where a person
// decides on what they read, through the stricter promptText.

// completeEsc is an escape sequence that is whole: a CSI, or a string (OSC,
// DCS, SOS, PM, APC) with its terminator. Unlike escSeq it never matches a
// string left open, which would swallow everything after it.
var completeEsc = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|[\]PX^_][^\x07\x1b]*(?:\x07|\x1b\\))`)

// termText is outside text made safe to draw. Whole escape sequences go (a
// tool's colors draw nothing themselves, so nothing a person would read is
// lost); a carriage return becomes a line break, so words meant to overwrite
// each other sit one under the other; every control character left, a
// stray escape among them, shows as "⟨U+XXXX⟩". Format characters stay: they
// join emoji and shape scripts in ordinary prose.
func termText(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, drawsNothingSafe) < 0 {
		return s
	}
	s = completeEsc.ReplaceAllString(strings.ReplaceAll(s, "\r\n", "\n"), "")
	var b strings.Builder
	for _, r := range s { // ranging turns a byte that is not UTF-8 into U+FFFD
		switch {
		case r == '\r':
			b.WriteByte('\n')
		case drawsNothingSafe(r):
			fmt.Fprintf(&b, "⟨U+%04X⟩", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ownSGR is a color or weight sequence, the only kind the app writes.
var ownSGR = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// styledText is termText for text the app has already styled: color and
// weight sequences stay, except one that conceals (the app never writes
// that), and everything else is treated as termText treats it. Stored text
// inside the styling can therefore still change a color; it cannot hide
// text outright, move the cursor, clear, retitle or overwrite.
func styledText(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, drawsNothingSafe) < 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, at := range ownSGR.FindAllStringIndex(s, -1) {
		b.WriteString(termText(s[last:at[0]]))
		if !conceals(s[at[0]:at[1]]) {
			b.WriteString(s[at[0]:at[1]])
		}
		last = at[1]
	}
	b.WriteString(termText(s[last:]))
	return b.String()
}

// conceals reports whether a color sequence turns on concealed text
// (parameter 8). An 8 that is part of a color (38;5;8, 48;2;8;8;8) is not it.
func conceals(sgr string) bool {
	params := strings.Split(strings.TrimSuffix(strings.TrimPrefix(sgr, "\x1b["), "m"), ";")
	for i := 0; i < len(params); i++ {
		switch params[i] {
		case "8":
			return true
		case "38", "48", "58": // an extended color takes the parameters after it
			if i+1 < len(params) && params[i+1] == "5" {
				i += 2
			} else if i+1 < len(params) && params[i+1] == "2" {
				i += 4
			}
		}
	}
	return false
}

// drawsNothingSafe reports a control character termText must not pass on.
func drawsNothingSafe(r rune) bool {
	return r != '\n' && r != '\t' && unicode.IsControl(r)
}

// promptText is the text of an approval prompt: nothing is removed and
// nothing is hidden. Every character that draws nothing or moves other text
// (controls, bidi and zero-width characters, the rest of
// control.HiddenRune) shows as "⟨U+XXXX⟩", as the dashboard's approval card
// shows it, so what is approved is what was read.
func promptText(s string) string {
	return control.VisibleText(strings.ToValidUTF8(s, string(utf8.RuneError)))
}

// safeEvent is e with every string a model, a tool or another agent wrote
// made safe to draw. The approval's fields are left for showApproval, which
// is stricter.
func safeEvent(e agent.Event) agent.Event {
	e.Text, e.Tool, e.Args, e.Result = termText(e.Text), termText(e.Tool), termText(e.Args), termText(e.Result)
	e.Agent, e.Source = termText(e.Agent), termText(e.Source)
	return e
}

// safeTimeline is u with a model's words (the question, the sub-questions,
// the gaps, an agent's row) made safe to draw. The parts are copied: the
// sender keeps its own.
func safeTimeline(u ResearchTimelineUpdate) ResearchTimelineUpdate {
	each := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = termText(s)
		}
		return out
	}
	if u.Started != nil {
		u.Started = &ResearchStartedData{Question: termText(u.Started.Question)}
	}
	if u.Plan != nil {
		u.Plan = &ResearchPlanData{SubQuestions: each(u.Plan.SubQuestions)}
	}
	if u.Reflect != nil {
		u.Reflect = &ResearchReflectData{Gaps: each(u.Reflect.Gaps)}
	}
	if u.Agent != nil {
		row := *u.Agent
		row.ID, row.SubQ, row.Status = termText(row.ID), termText(row.SubQ), termText(row.Status)
		u.Agent = &row
	}
	if u.Done != nil {
		done := *u.Done
		done.Status, done.Error = termText(done.Status), termText(done.Error)
		u.Done = &done
	}
	return u
}

// safeApproval is r as an approval prompt may show it.
func safeApproval(r ApprovalRequest) ApprovalRequest {
	r.Category, r.Action, r.Detail = promptText(r.Category), promptText(r.Action), promptText(r.Detail)
	return r
}
