package catalog

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"tenant/internal/logs/event"
)

// The event reference: docs/EVENTS.md, the dashboard's Help tab and
// tenant logs catalog all render from the registered definitions through
// these, so they never drift apart.

// Specs lists the registered definitions of provider, or of every provider
// when it is "", by provider and then ID.
func Specs(provider string) []event.Spec {
	var out []event.Spec
	for _, p := range event.Providers() {
		if provider != "" && p.Name != provider {
			continue
		}
		for _, d := range p.Defs() {
			out = append(out, d.Spec())
		}
	}
	return out
}

// Lookup finds the definition of event id of provider.
func Lookup(provider string, id uint32) (event.Spec, bool) {
	for _, s := range Specs(provider) {
		if s.ID == id {
			return s, true
		}
	}
	return event.Spec{}, false
}

// Check holds specs to the catalog's conventions (docs/design/logging-v2.md,
// Appendix B). Every definition is documented. A live one's ID sits in a
// block, the 2000s are warnings, the 3000s errors, and the 9000s, and only
// they, the diagnostic channel. The rules run one way only: a critical
// lifecycle event or a warning in the 4000s is fine.
//
// prev is a previous rendering (the committed docs/EVENTS.md): an ID it lists
// must still be declared, since IDs are never reused. A dropped definition
// must stay, marked Retired, instead.
func Check(specs []event.Spec, prev string) error {
	var errs []error
	have := map[string]bool{}
	for _, s := range specs {
		have[heading(s)] = true
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("event %s/%d: %s", s.Provider, s.ID, fmt.Sprintf(format, args...)))
		}
		if s.Meaning == "" {
			fail("not documented; add .Doc(meaning, cause, action)")
		}
		if s.Retired {
			continue
		}
		switch id := s.ID; {
		case id >= 6000 && id < 9000 || id > 9999:
			fail("the ID is in no block: 1–999 lifecycle, 1000s operations, 2000s warnings, 3000s errors, 4000s security, 5000s configuration, 9000s diagnostics")
		case id >= 2000 && id < 3000 && s.Level != event.Warning:
			fail("the 2000s are warnings, not %s", s.Level)
		case id >= 3000 && id < 4000 && s.Level > event.Error:
			fail("the 3000s are errors, not %s", s.Level)
		}
		if diag := s.ID >= 9000 && s.ID <= 9999; diag != (s.Channel == event.Diagnostic) {
			fail("the 9000s, and only they, are the diagnostic channel (.Diagnostic())")
		}
	}
	for _, m := range headingRe.FindAllStringSubmatch(prev, -1) {
		if !have[m[1]] {
			errs = append(errs, fmt.Errorf("event %s was catalogued but is no longer declared; an ID is never reused, so keep its definition and mark it .Retired()", strings.Replace(m[1], " ", "/", 1)))
		}
	}
	return errors.Join(errs...)
}

// headingRe finds the entries of a rendering by their headings.
var headingRe = regexp.MustCompile(`(?m)^### (\S+ \d+)$`)

func heading(s event.Spec) string { return s.Provider + " " + strconv.FormatUint(uint64(s.ID), 10) }

// Filter is the filter that finds s's events in the log book (Appendix A):
// its count, first sighting and trend come from the store's statistics for
// it.
func Filter(s event.Spec) string {
	return "provider:" + s.Provider + " id:" + strconv.FormatUint(uint64(s.ID), 10)
}

// Help renders one definition in Markdown: what it means, why it happens and
// what to do. It is an event's Help tab, and its entry in the reference.
func Help(s event.Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n%s\n\n", heading(s), code(s.Template))
	if s.Retired {
		b.WriteString("**Retired:** no longer written. The ID stays reserved.\n\n")
	}
	fmt.Fprintf(&b, "- **Level:** %s · **Channel:** %s · **Task:** %s · **Log:** %s\n", s.Level, channel(s), s.Task, s.Log)
	if len(s.Fields) > 0 {
		fmt.Fprintf(&b, "- **Fields:** %s", codes(s.Fields))
		if len(s.Sensitive) > 0 {
			fmt.Fprintf(&b, " (sensitive, masked: %s)", codes(s.Sensitive))
		}
		b.WriteString("\n")
	}
	if len(s.Keywords) > 0 {
		fmt.Fprintf(&b, "- **Keywords:** %s\n", codes(s.Keywords))
	}
	fmt.Fprintf(&b, "- **Find it:** %s\n\n", code(Filter(s)))
	fmt.Fprintf(&b, "**What it means.** %s\n\n**Why it happens.** %s\n\n**What to do.** %s\n", s.Meaning, s.Cause, s.Action)
	return b.String()
}

// Markdown renders specs, sorted as Specs sorts them, as the event
// reference: for each provider an index table, then each entry's Help.
func Markdown(specs []event.Spec) string {
	var b strings.Builder
	for i := 0; i < len(specs); {
		j := i
		for j < len(specs) && specs[j].Provider == specs[i].Provider {
			j++
		}
		p := specs[i:j]
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "## %s\n\nWrites the `%s` log.\n\n| ID | Level | Task | Message |\n|---|---|---|---|\n", p[0].Provider, p[0].Log)
		for _, s := range p {
			lvl := s.Level.String()
			if s.Retired {
				lvl += " (retired)"
			}
			// The link is GitHub's anchor for the entry's heading.
			anchor := strings.NewReplacer(".", "", " ", "-").Replace(heading(s))
			fmt.Fprintf(&b, "| [%d](#%s) | %s | %s | %s |\n", s.ID, anchor, lvl, cell(s.Task), cell(code(s.Template)))
		}
		for _, s := range p {
			b.WriteString("\n" + Help(s))
		}
		i = j
	}
	return b.String()
}

func channel(s event.Spec) string {
	if s.Channel == event.Diagnostic {
		return "diagnostic"
	}
	return "operational"
}

// code is s as a Markdown code span, which may itself hold a backtick.
func code(s string) string {
	if strings.Contains(s, "`") {
		return "`` " + s + " ``"
	}
	return "`" + s + "`"
}

func codes(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = code(s)
	}
	return strings.Join(out, ", ")
}

// cell keeps s inside one table cell.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}
