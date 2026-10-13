// Package md parses the Markdown a model writes, from one in-house copy of
// goldmark (see goldmark/README.md), and renders it as HTML for the dashboard.
// The terminal app renders the same tree itself (internal/tui, which alone may
// draw with Charm libraries) through Parse. HTML inside the Markdown is text on
// both surfaces: the copy has no raw-HTML parser.
package md

import (
	"bytes"
	"html/template"
	"sync"

	"tenant/internal/md/goldmark/ast"
	"tenant/internal/md/goldmark/extension"
	"tenant/internal/md/goldmark/parser"
	"tenant/internal/md/goldmark/renderer/html"
)

var (
	setup    sync.Once
	mdParser parser.Parser
	htmlOut  html.Renderer
)

func init() {
	setup.Do(func() {
		mdParser = parser.New(parser.WithExtensions(extension.TableParser, extension.TaskListItemParser))
		htmlOut = html.New(html.WithExtensions(extension.TableHTMLRenderer, extension.TaskListItemHTMLRenderer))
	})
}

// HTML renders src for the dashboard. Every text node is escaped and link
// destinations with a script scheme are dropped, so the result is safe to
// place in a page as it is.
func HTML(src string) template.HTML {
	source := []byte(src)
	var buf bytes.Buffer
	if err := htmlOut.Render(&buf, source, mdParser.Parse(source)); err != nil {
		return template.HTML(template.HTMLEscapeString(src)) //nolint:gosec // escaped above
	}
	return template.HTML(buf.String()) //nolint:gosec // rendered from escaped text nodes
}

// Parse returns the document tree of source, with GFM tables and task boxes.
// Node values refer into source, so a renderer takes both.
func Parse(source []byte) ast.Node { return mdParser.Parse(source) }
