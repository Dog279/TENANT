// Package cmdreg is the one command registry Tenant's surfaces share
// (DOC-003 / TEN-320). The CLI's subcommands and the TUI's slash commands are
// each declared once, as Commands in a Registry, with their names, section,
// summary and usage rows next to the handler that runs them. Each surface
// dispatches through its registry and renders its help (and the CLI its shell
// completion) from that same registry, so nothing can be dispatched without
// being documented. The handler type is the surface's own.
package cmdreg

import (
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"
)

// Row is one line of help: a syntax and what it does.
type Row struct {
	Syntax string
	Desc   string
	// Section lists this row under another section as well as, or instead
	// of, its command's own. Empty means the command's section.
	Section string
}

// Section is a heading in the help.
type Section struct {
	ID      string
	Title   string
	Tagline string
	// Aliases are other words that open this section in a surface's help.
	Aliases []string
	// Keys are rows that are not commands, such as keyboard shortcuts.
	Keys []Row
}

// Command is one command a surface dispatches.
type Command[H any] struct {
	// Names are the words that run it; the first is the canonical one.
	Names []string
	// Section is the help section it belongs to.
	Section string
	// Summary is one line for indexes and completion menus.
	Summary string
	// Rows are its detailed usage. Without rows, help shows the canonical
	// name and the summary.
	Rows []Row
	// Task marks a command for the beginner help, phrased as the job it
	// does ("connect a model — run this first").
	Task string
	// Sub and Flags feed shell completion.
	Sub   []string
	Flags []string
	// Run is the surface's handler.
	Run H
}

// Registry is one surface's commands.
type Registry[H any] struct {
	sections []Section
	cmds     []*Command[H]
	byName   map[string]*Command[H]
}

// New builds a registry, refusing a command without a name, summary,
// handler or known section, and a name used twice.
func New[H any](sections []Section, cmds []*Command[H]) (*Registry[H], error) {
	r := &Registry[H]{sections: sections, byName: map[string]*Command[H]{}}
	known := map[string]bool{}
	for _, s := range sections {
		if s.ID == "" || known[s.ID] {
			return nil, fmt.Errorf("cmdreg: section %q is empty or listed twice", s.ID)
		}
		known[s.ID] = true
	}
	for _, c := range cmds {
		if len(c.Names) == 0 {
			return nil, fmt.Errorf("cmdreg: a command has no name")
		}
		if c.Summary == "" {
			return nil, fmt.Errorf("cmdreg: %s has no summary", c.Names[0])
		}
		if !known[c.Section] {
			return nil, fmt.Errorf("cmdreg: %s names unknown section %q", c.Names[0], c.Section)
		}
		if v := reflect.ValueOf(any(c.Run)); !v.IsValid() || (v.Kind() == reflect.Func && v.IsNil()) {
			return nil, fmt.Errorf("cmdreg: %s has no handler", c.Names[0])
		}
		for _, row := range c.Rows {
			if row.Section != "" && !known[row.Section] {
				return nil, fmt.Errorf("cmdreg: a row of %s names unknown section %q", c.Names[0], row.Section)
			}
		}
		for _, n := range c.Names {
			key := strings.ToLower(n)
			if other, dup := r.byName[key]; dup {
				return nil, fmt.Errorf("cmdreg: %s is a name of both %s and %s", n, other.Names[0], c.Names[0])
			}
			r.byName[key] = c
		}
		r.cmds = append(r.cmds, c)
	}
	return r, nil
}

// MustNew is New for registries built from literals at package init.
func MustNew[H any](sections []Section, cmds []*Command[H]) *Registry[H] {
	r, err := New(sections, cmds)
	if err != nil {
		panic(err)
	}
	return r
}

// Lookup finds a command by any of its names, ignoring case.
func (r *Registry[H]) Lookup(name string) *Command[H] {
	return r.byName[strings.ToLower(name)]
}

// Commands lists every command in declaration order.
func (r *Registry[H]) Commands() []*Command[H] { return r.cmds }

// Sections lists the sections in display order.
func (r *Registry[H]) Sections() []Section { return r.sections }

// Section finds a section by id or alias, ignoring case.
func (r *Registry[H]) Section(id string) (Section, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, s := range r.sections {
		if s.ID == id {
			return s, true
		}
		for _, a := range s.Aliases {
			if a == id {
				return s, true
			}
		}
	}
	return Section{}, false
}

// Rows is everything listed under a section: each command's rows that belong
// there, in declaration order, then the section's keys.
func (r *Registry[H]) Rows(sectionID string) []Row {
	var out []Row
	for _, c := range r.cmds {
		for _, row := range c.HelpRows() {
			sec := row.Section
			if sec == "" {
				sec = c.Section
			}
			if sec == sectionID {
				out = append(out, row)
			}
		}
	}
	for _, s := range r.sections {
		if s.ID == sectionID {
			out = append(out, s.Keys...)
		}
	}
	return out
}

// Tasks lists the commands marked for beginner help, in declaration order.
func (r *Registry[H]) Tasks() []*Command[H] {
	var out []*Command[H]
	for _, c := range r.cmds {
		if c.Task != "" {
			out = append(out, c)
		}
	}
	return out
}

// HelpRows is the command's usage rows, or one row made of its canonical name
// and summary when it declares none.
func (c *Command[H]) HelpRows() []Row {
	if len(c.Rows) > 0 {
		return c.Rows
	}
	return []Row{{Syntax: c.Names[0], Desc: c.Summary}}
}

// Table renders rows as aligned plain text, the syntax padded to width
// columns (runes, so "…" counts as one).
func Table(rows []Row, indent string, width int) string {
	var b strings.Builder
	for _, row := range rows {
		gap := width - utf8.RuneCountInString(row.Syntax)
		if gap < 2 {
			gap = 2
		}
		b.WriteString(indent + row.Syntax + strings.Repeat(" ", gap) + row.Desc + "\n")
	}
	return b.String()
}
