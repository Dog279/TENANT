package tui

// The chat pane's Markdown renderer (TEN-514): a goldmark document tree (from
// internal/md, Tenant's copy) laid out as styled terminal text at a width:
// word wrap, hanging list indents, code panels kept whole, blockquote bars,
// task boxes and lipgloss tables, coloured by the palette's mdTheme. It lives
// in this package because only the terminal UI may draw with Charm libraries
// (internal/archtest).

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"tenant/internal/md"
	"tenant/internal/md/goldmark/ast"
	"tenant/internal/md/goldmark/extension"
	east "tenant/internal/md/goldmark/extension/ast"
	"tenant/internal/md/goldmark/renderer/html"
)

// mdTheme is the set of styles the renderer draws with. The zero value
// renders plain text; defaultMDTheme is the dark palette's look.
type mdTheme struct {
	Heading   lipgloss.Style // headings (all levels; H1 and H2 also get a rule)
	Strong    lipgloss.Style
	Emphasis  lipgloss.Style
	Code      lipgloss.Style // inline code
	CodeBlock lipgloss.Style // a code panel's lines
	CodeRule  lipgloss.Style // the panel's left rule and language label
	Quote     lipgloss.Style // a blockquote's bar and text
	Bullet    lipgloss.Style // list markers and task boxes
	Rule      lipgloss.Style // thematic breaks and heading rules
	Link      lipgloss.Style // link text
	URL       lipgloss.Style // the destination shown after link text
	TableHead lipgloss.Style
	TableLine lipgloss.Style // table borders
}

// defaultMDTheme is a readable dark-terminal palette: cyan for structure and
// links, dim grey for rules and URLs, bold for headings and strong text.
func defaultMDTheme() mdTheme {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	return mdTheme{
		Heading:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14")),
		Strong:    lipgloss.NewStyle().Bold(true),
		Emphasis:  lipgloss.NewStyle().Italic(true),
		Code:      lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
		CodeBlock: lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
		CodeRule:  dim,
		Quote:     dim,
		Bullet:    lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
		Rule:      lipgloss.NewStyle().Foreground(lipgloss.Color("238")),
		Link:      lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Underline(true),
		URL:       dim,
		TableHead: lipgloss.NewStyle().Bold(true),
		TableLine: lipgloss.NewStyle().Foreground(lipgloss.Color("238")),
	}
}

// markdownANSI renders src at width columns. Lines never exceed width except
// for a single word or code token wider than the column, which lipgloss
// breaks.
func markdownANSI(src string, width int, th mdTheme) string {
	if width < 8 {
		width = 8
	}
	source := []byte(src)
	r := &mdRenderer{src: source, th: th}
	lines := r.blocks(md.Parse(source), width)
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

type mdRenderer struct {
	src []byte
	th  mdTheme
}

// blocks renders n's block children, a blank line between blocks (none inside
// a tight list, whose items hand in tight=true).
func (r *mdRenderer) blocks(n ast.Node, width int) []string {
	var out []string
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		b := r.block(c, width)
		if len(b) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, b...)
	}
	return out
}

func (r *mdRenderer) block(n ast.Node, width int) []string {
	switch n := n.(type) {
	case *ast.Heading:
		text := r.inlines(n)
		lines := wrapStyled(r.th.Heading.Render(text), width)
		if n.Level <= 2 {
			lines = append(lines, r.th.Rule.Render(strings.Repeat("─", min(width, maxLineWidth(lines)))))
		}
		return lines
	case *ast.Paragraph:
		return wrapStyled(r.inlines(n), width)
	case *ast.CodeBlock:
		label, _ := n.Language(r.src)
		return r.codePanel(n.Value.Str(r.src), label, width)
	case *ast.Blockquote:
		inner := r.blocks(n, max(8, width-2)) // nested quotes never wrap at nothing
		for i, l := range inner {
			inner[i] = r.th.Quote.Render("│ ") + l
		}
		return inner
	case *ast.List:
		return r.list(n, width)
	case *ast.ThematicBreak:
		return []string{r.th.Rule.Render(strings.Repeat("─", width))}
	case *east.Table:
		return r.table(n, width)
	case *ast.LinkReferenceDefinition:
		return nil
	default:
		// A block kind this renderer doesn't know: its children, or its text.
		if n.HasChildren() && !isInlineContainer(n) {
			return r.blocks(n, width)
		}
		return wrapStyled(r.inlines(n), width)
	}
}

// isInlineContainer reports a block whose children are inline nodes.
func isInlineContainer(n ast.Node) bool {
	_, inline := n.FirstChild().(ast.InlineNode)
	return inline
}

func (r *mdRenderer) codePanel(code, label string, width int) []string {
	code = strings.TrimRight(code, "\n")
	var out []string
	bar := r.th.CodeRule.Render("│ ")
	if label != "" {
		out = append(out, r.th.CodeRule.Render("│ "+label))
	}
	for _, line := range strings.Split(code, "\n") {
		line = strings.ReplaceAll(line, "\t", "    ")
		for _, piece := range hardWrapCode(line, width-2) {
			out = append(out, bar+r.th.CodeBlock.Render(piece))
		}
	}
	return out
}

func (r *mdRenderer) list(n *ast.List, width int) []string {
	var out []string
	num := n.Start
	if num == 0 {
		num = 1
	}
	i := 0
	for item := n.FirstChild(); item != nil; item = item.NextSibling() {
		marker := "•"
		if n.IsOrdered() {
			marker = strconv.Itoa(num) + "."
			num++
		}
		if status, ok := extension.TaskStatusOf(item); ok {
			marker = "○"
			if status == extension.TaskStatusCompleted {
				marker = "✓"
			}
		}
		indent := lipgloss.Width(marker) + 1
		body := r.itemBlocks(item, width-indent, n.IsTight)
		if len(body) == 0 {
			body = []string{""}
		}
		if i > 0 && !n.IsTight {
			out = append(out, "")
		}
		pad := strings.Repeat(" ", indent)
		for j, l := range body {
			if j == 0 {
				out = append(out, r.th.Bullet.Render(marker)+" "+l)
			} else {
				out = append(out, pad+l)
			}
		}
		i++
	}
	return out
}

// itemBlocks renders a list item's children; a tight list puts no blank line
// between an item's own paragraph and a nested list.
func (r *mdRenderer) itemBlocks(item ast.Node, width int, tight bool) []string {
	if !tight {
		return r.blocks(item, width)
	}
	var out []string
	for c := item.FirstChild(); c != nil; c = c.NextSibling() {
		out = append(out, r.block(c, width)...)
	}
	return out
}

func (r *mdRenderer) table(n *east.Table, width int) []string {
	t := table.New().Border(lipgloss.NormalBorder()).BorderStyle(r.th.TableLine).Width(width).Wrap(true)
	var headers []string
	var rows [][]string
	cells := func(row ast.Node) []string {
		var out []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			out = append(out, r.inlines(cell))
		}
		return out
	}
	for part := n.FirstChild(); part != nil; part = part.NextSibling() {
		switch part := part.(type) {
		case *east.TableHeader:
			headers = cells(part)
		case *east.TableBody: // v2 groups the body rows under one node
			for row := part.FirstChild(); row != nil; row = row.NextSibling() {
				rows = append(rows, cells(row))
			}
		case *east.TableRow:
			rows = append(rows, cells(part))
		}
	}
	t.Headers(headers...).Rows(rows...)
	th := r.th
	t.StyleFunc(func(row, _ int) lipgloss.Style {
		if row == table.HeaderRow {
			return th.TableHead.Padding(0, 1)
		}
		return lipgloss.NewStyle().Padding(0, 1)
	})
	return strings.Split(t.Render(), "\n")
}

// inlines renders n's inline children as one styled string. Soft line breaks
// become spaces (the wrap decides the lines); hard breaks stay.
func (r *mdRenderer) inlines(n ast.Node) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		r.inline(&b, c)
	}
	return b.String()
}

func (r *mdRenderer) inline(b *strings.Builder, n ast.Node) {
	switch n := n.(type) {
	case *ast.Text:
		b.WriteString(n.Value.Value(r.src))
		switch {
		case n.HardLineBreak():
			b.WriteByte('\n')
		case n.SoftLineBreak():
			b.WriteByte(' ')
		}
	case *ast.CodeSpan:
		// The value is already the spec's: line endings are spaces, one edge
		// space stripped; inner runs of spaces stay.
		b.WriteString(r.th.Code.Render(n.Value.Str(r.src)))
	case *ast.Strong:
		b.WriteString(r.th.Strong.Render(r.inlines(n)))
	case *ast.Emphasis:
		b.WriteString(r.th.Emphasis.Render(r.inlines(n)))
	case *ast.Link:
		text := r.inlines(n)
		dest := n.Destination.Value(r.src)
		b.WriteString(r.th.Link.Render(text))
		if dest != "" && dest != text && !html.IsDangerousURL(dest) {
			b.WriteString(r.th.URL.Render(" (" + dest + ")"))
		}
	case *ast.AutoLink:
		dest := n.Destination.Value(r.src)
		if html.IsDangerousURL(dest) {
			b.WriteString(dest) // shown, not styled as a link
			break
		}
		b.WriteString(r.th.Link.Render(dest))
	case *ast.Image:
		alt := r.inlines(n)
		if alt == "" {
			alt = "image"
		}
		b.WriteString(r.th.URL.Render(fmt.Sprintf("[%s: %s]", alt, n.Destination.Value(r.src))))
	default:
		if n.HasChildren() {
			b.WriteString(r.inlines(n))
		}
	}
}

// wrapStyled word-wraps a styled string to width, keeping the newlines it has.
func wrapStyled(s string, width int) []string {
	if s == "" {
		return nil
	}
	out := lipgloss.NewStyle().Width(width).Render(s)
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// hardWrapCode breaks one code line at width cells, with no regard for words.
// Cells, not runes: a wide character (CJK, fullwidth) takes two.
func hardWrapCode(line string, width int) []string {
	if width < 1 || lipgloss.Width(line) <= width {
		return []string{line}
	}
	var out []string
	var cur strings.Builder
	col := 0
	for _, r := range line {
		cw := lipgloss.Width(string(r))
		if col > 0 && col+cw > width {
			out = append(out, cur.String())
			cur.Reset()
			col = 0
		}
		cur.WriteRune(r)
		col += cw
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func maxLineWidth(lines []string) int {
	w := 0
	for _, l := range lines {
		w = max(w, lipgloss.Width(l))
	}
	return w
}
