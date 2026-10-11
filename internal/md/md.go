// Package md renders the Markdown a model writes for Tenant's two surfaces:
// HTML for the dashboard and styled text for the terminal app, from one
// in-house copy of goldmark (see goldmark/README.md). HTML inside the Markdown
// is text on both: the copy has no raw-HTML parser.
package md

import (
	"bytes"
	"html/template"
	"sync"

	"tenant/internal/md/ansi"
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

// ANSI renders src for a terminal pane width columns wide, in theme th.
func ANSI(src string, width int, th ansi.Theme) string {
	source := []byte(src)
	return ansi.Render(mdParser.Parse(source), source, width, th)
}
